package bench

import (
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPhaseProfiler(t *testing.T) {
	for _, kind := range []string{"cpu", "allocs", "mutex", "block"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(profileDirEnv, dir)
			oldMem := runtime.MemProfileRate
			oldMutex := runtime.SetMutexProfileFraction(-1)
			p, err := prepareProfiler(Case{Profile: kind, ProfilePhase: "measured"})
			if err != nil {
				t.Fatal(err)
			}
			defer p.finish()
			if err := p.startPhase("warmup"); err != nil || p.started {
				t.Fatalf("profile enabled in an unselected phase: %v", err)
			}
			if err := p.startPhase("measured"); err != nil {
				t.Fatal(err)
			}
			allocations := make([][]byte, 32)
			for i := range allocations {
				allocations[i] = make([]byte, 32*1024)
				allocations[i][0] = byte(i)
			}
			// Exercise the CPU profiler and blocking profiler without asserting
			// exact sample counts, which depend on runtime scheduling.
			until := time.Now().Add(20 * time.Millisecond)
			for time.Now().Before(until) {
				runtime.KeepAlive(allocations)
			}
			done := make(chan struct{})
			go func() { time.Sleep(time.Millisecond); close(done) }()
			<-done
			if err := p.finishPhase("measured"); err != nil {
				t.Fatal(err)
			}
			if err := p.finish(); err != nil {
				t.Fatal(err)
			}
			if err := p.finish(); err != nil {
				t.Fatalf("cleanup is not idempotent: %v", err)
			}
			runtime.KeepAlive(allocations)
			if runtime.MemProfileRate != oldMem || runtime.SetMutexProfileFraction(-1) != oldMutex {
				t.Fatal("sampling settings were not restored")
			}
			meta := p.artifacts()
			want := 1
			if kind == "allocs" {
				want = 2
			}
			if !meta.Completed || !meta.Diagnostic || meta.Phase != "measured" || len(meta.Files) != want {
				t.Fatalf("wrong profile metadata: %+v", meta)
			}
			for _, name := range meta.Files {
				f, err := os.Open(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				reader, err := gzip.NewReader(f)
				if err != nil {
					f.Close()
					t.Fatal(err)
				}
				data, err := io.ReadAll(reader)
				reader.Close()
				f.Close()
				if err != nil || len(data) == 0 {
					t.Fatalf("invalid compressed pprof file %s: %v", name, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "profile.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProfilerRequiresExplicitDestination(t *testing.T) {
	t.Setenv(profileDirEnv, "")
	if _, err := prepareProfiler(Case{Profile: "allocs", ProfilePhase: "fill"}); err == nil {
		t.Fatal("profiling silently wrote to an unspecified destination")
	}
	if p, err := prepareProfiler(Case{}); err != nil || p.artifacts() != nil {
		t.Fatal("unprofiled runs acquired profile metadata")
	}
}

func TestProfilerMissingOrAbortedPhase(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "aborted"}[start], func(t *testing.T) {
			t.Setenv(profileDirEnv, t.TempDir())
			old := runtime.MemProfileRate
			p, err := prepareProfiler(Case{Profile: "allocs", ProfilePhase: "fill", ProfileRate: 16384})
			if err != nil {
				t.Fatal(err)
			}
			if runtime.MemProfileRate != 16384 {
				t.Fatal("allocation rate was not set before cache construction")
			}
			if start {
				if err := p.startPhase("fill"); err != nil {
					t.Fatal(err)
				}
			}
			err = p.finish()
			if !start && err == nil {
				t.Fatal("missing selected phase was silently accepted")
			}
			if p.artifacts().Completed || runtime.MemProfileRate != old {
				t.Fatal("incomplete profile marked complete or rate leaked")
			}
		})
	}
}

func TestProfileEnvironment(t *testing.T) {
	env := []string{"A=1", profileDirEnv + "=/inherited", "lrugcbench_profile_dir=/duplicate", "B=2"}
	if got := profileEnvironment(env, ""); !reflect.DeepEqual(got, []string{"A=1", "B=2"}) {
		t.Fatalf("inherited profile path leaked: %v", got)
	}
	if got := profileEnvironment(env, "/controlled"); !reflect.DeepEqual(got, []string{"A=1", "B=2", profileDirEnv + "=/controlled"}) {
		t.Fatalf("profile destination not controlled: %v", got)
	}
}

func TestProfileAnalysisScriptQuotesWorkerPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile's output")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(dir, "worker ' $(touch INJECTION) ; binary")
	if err := writeProfileInstructions(dir, worker, "allocs"); err != nil {
		t.Fatal(err)
	}
	// A fake go prints argv, proving the path reaches pprof as one literal
	// argument without executing any shell syntax embedded in the path.
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(dir, "analyze.sh"), "-nodecount=7")
	cmd.Dir = dir
	cmd.Env = OverrideEnv(os.Environ(), map[string]string{"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "LRUGCBENCH_WORKER": ""})
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %s: %v", output, err)
	}
	if !strings.Contains(string(output), "\n"+worker+"\n") || !strings.Contains(string(output), "-base="+dir+"/allocs-before.pprof\n") {
		t.Fatalf("script did not preserve pprof arguments: %s", output)
	}
	if _, err := os.Stat(filepath.Join(dir, "INJECTION")); !os.IsNotExist(err) {
		t.Fatal("shell syntax in the worker path was executed")
	}
}
