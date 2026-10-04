package bench

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
)

const profileDirEnv = "LRUGCBENCH_PROFILE_DIR"

// ProfileArtifacts describes diagnostic artifacts, never headline measurements.
// Directory is relative to the enclosing suite output directory.
type ProfileArtifacts struct {
	Kind       string   `json:"kind"`
	Phase      string   `json:"phase"`
	Rate       int      `json:"rate"`
	Directory  string   `json:"directory"`
	Files      []string `json:"files"`
	Diagnostic bool     `json:"diagnostic"`
	Completed  bool     `json:"completed"`
	Notes      []string `json:"notes"`
}

// phaseProfiler is process-global, just like RunWorker. Profiling is supported
// in fresh worker processes, not in an application already using these profilers.
type phaseProfiler struct {
	meta         ProfileArtifacts
	dir          string
	started      bool
	active       bool
	finished     bool
	finishErr    error
	cpuFile      *os.File
	oldMemRate   int
	oldMutexRate int
}

func prepareProfiler(c Case) (*phaseProfiler, error) {
	p := &phaseProfiler{}
	if c.Profile == "" {
		return p, nil
	}
	if c.ProfilePhase == "" || c.ProfileRate < 0 {
		return nil, fmt.Errorf("profiling requires a phase and a nonnegative rate")
	}
	rate := c.ProfileRate
	switch c.Profile {
	case "cpu":
		if rate != 0 {
			return nil, fmt.Errorf("CPU profiling uses the Go runtime's fixed sampling rate")
		}
	case "allocs":
		if rate == 0 {
			rate = 512 * 1024
		}
	case "mutex", "block":
		if rate == 0 {
			rate = 1
		}
	default:
		return nil, fmt.Errorf("unsupported profile %q", c.Profile)
	}
	p.dir = os.Getenv(profileDirEnv)
	if !filepath.IsAbs(p.dir) {
		return nil, fmt.Errorf("profile jobs require an absolute %s set by the controller", profileDirEnv)
	}
	st, err := os.Stat(p.dir)
	if err != nil {
		return nil, fmt.Errorf("profile directory: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("profile destination is not a directory")
	}
	p.meta = ProfileArtifacts{
		Kind: c.Profile, Phase: c.ProfilePhase, Rate: rate,
		Directory: filepath.ToSlash(filepath.Join("profiles", filepath.Base(p.dir))),
		Files:     []string{}, Diagnostic: true,
		Notes: []string{"Profiled runs are diagnostic: do not compare their throughput or latency with unprofiled headline measurements. Profiles include phase boundary snapshots."},
	}
	if c.Profile == "allocs" {
		p.oldMemRate = runtime.MemProfileRate
		runtime.MemProfileRate = rate
		p.meta.Notes = append(p.meta.Notes,
			"The allocation sampling rate is set before the baseline GC and cache construction. Two forced GCs before each profile publish delayed allocation records outside the timed phase.",
			"Use allocs-after.pprof with -base=allocs-before.pprof and -sample_index=alloc_space (or alloc_objects). The difference includes profile boundary/serialization allocations; inspect workload stacks rather than treating its total as exact phase allocation accounting. Later phases are also diagnostic because these forced GCs perturb runtime state.")
	} else if c.Profile == "mutex" || c.Profile == "block" {
		p.meta.Notes = append(p.meta.Notes,
			"Sampling is enabled only around the selected phase in a fresh worker. Profiles report accumulated sampled contention, including concurrent background activity; blocking or mutex contention may outlast the phase boundary.")
		if c.Profile == "block" {
			p.meta.Notes = append(p.meta.Notes, "Block sampling is disabled after this phase. Go exposes no getter for its previous sampling rate; this mode requires a fresh worker with block profiling initially disabled.")
		}
	}
	return p, nil
}

func (p *phaseProfiler) startPhase(name string) error {
	if p == nil || p.meta.Kind == "" || name != p.meta.Phase {
		return nil
	}
	if p.started {
		return fmt.Errorf("profile phase %q started more than once", name)
	}
	p.started, p.active = true, true
	switch p.meta.Kind {
	case "cpu":
		f, err := os.OpenFile(filepath.Join(p.dir, "cpu.pprof"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return err
		}
		if err = pprof.StartCPUProfile(f); err != nil {
			f.Close()
			return err
		}
		p.cpuFile = f
	case "allocs":
		runtime.GC()
		runtime.GC()
		return p.writeProfile("allocs", "allocs-before.pprof")
	case "mutex":
		p.oldMutexRate = runtime.SetMutexProfileFraction(p.meta.Rate)
	case "block":
		runtime.SetBlockProfileRate(p.meta.Rate)
	}
	return nil
}

func (p *phaseProfiler) finishPhase(name string) error {
	if p == nil || !p.active || name != p.meta.Phase {
		return nil
	}
	p.active = false
	var err error
	switch p.meta.Kind {
	case "cpu":
		if p.cpuFile != nil {
			pprof.StopCPUProfile()
			err = p.cpuFile.Close()
			p.cpuFile = nil
			p.meta.Files = append(p.meta.Files, "cpu.pprof")
		}
	case "allocs":
		runtime.GC()
		runtime.GC()
		err = p.writeProfile("allocs", "allocs-after.pprof")
	case "mutex":
		runtime.SetMutexProfileFraction(p.oldMutexRate)
		err = p.writeProfile("mutex", "mutex.pprof")
	case "block":
		runtime.SetBlockProfileRate(0)
		err = p.writeProfile("block", "block.pprof")
	}
	p.meta.Completed = err == nil
	return err
}

func (p *phaseProfiler) writeProfile(kind, name string) error {
	f, err := os.OpenFile(filepath.Join(p.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	err = pprof.Lookup(kind).WriteTo(f, 0)
	err = errors.Join(err, f.Close())
	if err == nil {
		p.meta.Files = append(p.meta.Files, name)
	}
	return err
}

// finish is idempotent and also closes an active profile after an aborted phase.
// Call explicitly on success to surface write errors, as well as with defer for
// all early returns. A stopped worker may leave a partial CPU profile on disk.
func (p *phaseProfiler) finish() error {
	if p == nil || p.meta.Kind == "" {
		return nil
	}
	if p.finished {
		return p.finishErr
	}
	p.finished = true
	if p.active {
		p.finishErr = p.finishPhase(p.meta.Phase)
		p.meta.Completed = false
	}
	if p.meta.Kind == "allocs" {
		runtime.MemProfileRate = p.oldMemRate
	}
	if !p.started {
		p.finishErr = errors.Join(p.finishErr, fmt.Errorf("profile phase %q was not executed", p.meta.Phase))
	}
	p.finishErr = errors.Join(p.finishErr, WriteJSON(filepath.Join(p.dir, "profile.json"), p.meta))
	return p.finishErr
}

func (p *phaseProfiler) artifacts() *ProfileArtifacts {
	if p == nil || p.meta.Kind == "" {
		return nil
	}
	m := p.meta
	m.Files = append([]string(nil), m.Files...)
	m.Notes = append([]string(nil), m.Notes...)
	return &m
}

func writeProfileInstructions(dir, worker, kind string) error {
	var script strings.Builder
	script.WriteString("#!/bin/sh\n# Analyze the saved diagnostic profile; this does not rerun the benchmark.\nset -eu\n")
	script.WriteString("here=$(CDPATH= cd -- \"$(dirname -- \"$0\")\" && pwd)\n")
	fmt.Fprintf(&script, "worker=${LRUGCBENCH_WORKER:-%s}\n", shellQuote(worker))
	if kind == "allocs" {
		script.WriteString("exec go tool pprof -top -sample_index=alloc_space -base=\"$here/allocs-before.pprof\" \"$@\" \"$worker\" \"$here/allocs-after.pprof\"\n")
	} else {
		fmt.Fprintf(&script, "exec go tool pprof -top \"$@\" \"$worker\" \"$here/%s.pprof\"\n", kind)
	}
	if err := os.WriteFile(filepath.Join(dir, "analyze.sh"), []byte(script.String()), 0755); err != nil {
		return err
	}
	note := "# Diagnostic profile\n\nRun `./analyze.sh` for a text summary. The script preserves the worker binary path; set `LRUGCBENCH_WORKER` to its matching relocated copy if needed. Pass additional pprof flags as arguments. The enclosing suite's `reproduce.sh` reruns the recorded experiment into a fresh output directory.\n\nProfiling perturbs execution; throughput and latency from this run are diagnostic, not headline performance estimates. See `profile.json` for phase, sampling rate, files, completion state, and limitations. A failed or interrupted job can retain incomplete profiles.\n"
	if kind == "allocs" {
		note += "\nThe script subtracts the before profile from the after profile using allocation bytes (`alloc_space`). These are sampled cumulative profiles, not live-heap snapshots. Two forced GCs outside each phase boundary flush delayed records. Their difference also includes profile boundary/serialization work, which can be identified by its stacks. Do not interpret the after profile alone as allocations of the selected phase.\n"
	}
	return os.WriteFile(filepath.Join(dir, "README.md"), []byte(note), 0644)
}

// Do not let a user's inherited path redirect or enable worker profile output.
func profileEnvironment(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(key, profileDirEnv) {
			out = append(out, entry)
		}
	}
	if dir != "" {
		out = append(out, profileDirEnv+"="+dir)
	}
	return out
}
