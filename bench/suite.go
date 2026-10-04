package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

type JobRecord struct {
	Job        Job    `json:"job"`
	Status     string `json:"status"`
	ResultFile string `json:"result_file,omitempty"`
	StderrFile string `json:"stderr_file,omitempty"`
	Error      string `json:"error,omitempty"`
}
type Manifest struct {
	SchemaVersion int          `json:"schema_version"`
	Label         string       `json:"label"`
	CreatedAt     time.Time    `json:"created_at"`
	FinishedAt    *time.Time   `json:"finished_at,omitempty"`
	WorkerSHA256  string       `json:"worker_sha256"`
	Capabilities  Capabilities `json:"capabilities"`
	Config        Config       `json:"config"`
	Jobs          []JobRecord  `json:"jobs"`
}

func OverrideEnv(env []string, values map[string]string) []string {
	out := make([]string, 0, len(env)+len(values))
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		found := false
		for v := range values {
			if strings.EqualFold(v, k) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, e)
		}
	}
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	return out
}
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".json-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func FileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func describe(ctx context.Context, worker string) (Capabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var cap Capabilities
	cmd := exec.CommandContext(ctx, worker, "--describe")
	b, err := cmd.Output()
	if err != nil {
		return cap, fmt.Errorf("worker --describe: %w", err)
	}
	if err = DecodeStrict(strings.NewReader(string(b)), &cap); err != nil {
		return cap, err
	}
	if cap.SchemaVersion != SchemaVersion || len(cap.Backends) == 0 {
		return cap, fmt.Errorf("incompatible worker protocol")
	}
	return cap, nil
}

// suiteRunner owns the durable state for one worker. A paired run advances two
// runners one job at a time; a regular run advances just one.
type suiteRunner struct {
	manifest Manifest
	worker   string
	out      string
	timeout  time.Duration
	failed   int
}

func prepareSuite(ctx context.Context, worker string, c Config, out, label string) (*suiteRunner, error) {
	s := &suiteRunner{manifest: Manifest{SchemaVersion: 1, Label: label, CreatedAt: time.Now().UTC(), Config: c}}
	if err := c.Validate(); err != nil {
		return s, err
	}
	var err error
	s.worker, err = filepath.Abs(worker)
	if err != nil {
		return s, err
	}
	s.manifest.WorkerSHA256, err = FileHash(s.worker)
	if err != nil {
		return s, err
	}
	s.manifest.Capabilities, err = describe(ctx, s.worker)
	if err != nil {
		return s, err
	}
	for _, b := range c.Backends {
		if !slices.Contains(s.manifest.Capabilities.Backends, b) {
			return s, fmt.Errorf("worker cannot run %q; supports %v", b, s.manifest.Capabilities.Backends)
		}
	}
	s.out, err = filepath.Abs(out)
	if err != nil {
		return s, err
	}
	s.timeout, _ = time.ParseDuration(c.Timeout)
	for _, j := range c.Jobs() {
		s.manifest.Jobs = append(s.manifest.Jobs, JobRecord{Job: j, Status: "pending"})
	}
	return s, nil
}

// Mkdir reserves the final directory atomically, including against another
// controller starting at the same time. Existing run artifacts are never mixed.
func reserveOutputDir(out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return err
	}
	if err := os.Mkdir(out, 0755); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("output directory already exists; refusing to mix runs: %s", out)
		}
		return err
	}
	return nil
}

func (s *suiteRunner) save() error {
	return WriteJSON(filepath.Join(s.out, "manifest.json"), s.manifest)
}

func (s *suiteRunner) initialize() error {
	if err := reserveOutputDir(s.out); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(s.out, "raw"), 0755); err != nil {
		return err
	}
	return s.save()
}

func (s *suiteRunner) runJob(ctx context.Context, i int, log io.Writer) error {
	rec := &s.manifest.Jobs[i]
	j := rec.Job
	fmt.Fprintf(log, "[%s %d/%d] %s %s %s %s repeat=%d\n", s.manifest.Label, i+1, len(s.manifest.Jobs), j.ID, j.Backend, j.Case.Name, j.Runtime.Name, j.Repeat+1)
	rec.StderrFile = filepath.ToSlash(filepath.Join("raw", j.ID+".stderr.log"))
	rec.ResultFile = filepath.ToSlash(filepath.Join("raw", j.ID+".result.json"))
	runErr, timedOut := s.executeJob(ctx, rec)
	if runErr != nil {
		s.failed++
		rec.Status = "failed"
		if timedOut {
			rec.Status = "timeout"
		}
		if ctx.Err() != nil {
			rec.Status = "cancelled"
		}
		rec.Error = runErr.Error()
		rec.ResultFile = ""
		fmt.Fprintf(log, "  %s: %v (see %s)\n", rec.Status, runErr, rec.StderrFile)
	} else {
		rec.Status = "ok"
	}
	return s.save()
}

func (s *suiteRunner) executeJob(ctx context.Context, rec *JobRecord) (error, bool) {
	j := rec.Job
	requestPath := filepath.Join(s.out, "raw", j.ID+".request.json")
	if err := WriteJSON(requestPath, j); err != nil {
		return err, false
	}
	final := filepath.Join(s.out, rec.ResultFile)
	tmp := final + ".partial"
	request, err := json.Marshal(j)
	if err != nil {
		return err, false
	}
	output, err := os.Create(tmp)
	if err != nil {
		return err, false
	}
	stderr, err := os.Create(filepath.Join(s.out, rec.StderrFile))
	if err != nil {
		output.Close()
		return err, false
	}
	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, s.worker)
	gc := strconv.Itoa(j.Runtime.GOGC)
	if j.Runtime.GOGC == -1 {
		gc = "off"
	}
	cmd.Env = OverrideEnv(os.Environ(), map[string]string{"GOMAXPROCS": strconv.Itoa(j.Runtime.GOMAXPROCS), "GOGC": gc, "GOMEMLIMIT": j.Runtime.GOMEMLIMIT})
	cmd.Stdin = strings.NewReader(string(request))
	cmd.Stdout = output
	cmd.Stderr = stderr
	runErr := cmd.Run()
	timedOut := runCtx.Err() == context.DeadlineExceeded
	if e := output.Close(); runErr == nil {
		runErr = e
	}
	if e := stderr.Close(); runErr == nil {
		runErr = e
	}
	if runErr == nil {
		result, e := readResult(tmp)
		if e == nil && (result.SchemaVersion != SchemaVersion || !reflect.DeepEqual(result.Job, j) || !reflect.DeepEqual(result.Provenance, s.manifest.Capabilities.Provenance)) {
			e = fmt.Errorf("worker returned mismatched job/provenance")
		}
		if e == nil {
			e = os.Rename(tmp, final)
		}
		runErr = e
	}
	return runErr, timedOut
}

func (s *suiteRunner) finish(ctx context.Context) error {
	if ctx.Err() != nil {
		for i := range s.manifest.Jobs {
			rec := &s.manifest.Jobs[i]
			if rec.Status == "pending" {
				rec.Status = "cancelled"
				rec.Error = ctx.Err().Error()
			}
		}
	}
	now := time.Now().UTC()
	s.manifest.FinishedAt = &now
	var failed error
	if s.failed > 0 {
		failed = fmt.Errorf("%d job(s) failed; successful jobs are still reported", s.failed)
	}
	saveErr := s.save()
	reportErr := Report(s.out)
	return errors.Join(saveErr, reportErr, ctx.Err(), failed)
}

func RunSuite(ctx context.Context, worker string, c Config, out, label string, log io.Writer) (Manifest, error) {
	s, err := prepareSuite(ctx, worker, c, out, label)
	if err != nil {
		return s.manifest, err
	}
	if err = s.initialize(); err != nil {
		return s.manifest, err
	}
	for i := range s.manifest.Jobs {
		if ctx.Err() != nil {
			break
		}
		if err = s.runJob(ctx, i, log); err != nil {
			break
		}
	}
	err = errors.Join(err, s.finish(ctx))
	return s.manifest, err
}
func readResult(path string) (Result, error) {
	var r Result
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	if err = DecodeStrict(io.LimitReader(f, 256<<20), &r); err != nil {
		return r, err
	}
	return r, nil
}
func LoadSuite(dir string) (Manifest, []Result, error) {
	var m Manifest
	f, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, nil, err
	}
	err = DecodeStrict(f, &m)
	f.Close()
	if err != nil {
		return m, nil, err
	}
	if m.SchemaVersion != 1 {
		return m, nil, fmt.Errorf("unsupported manifest schema")
	}
	var results []Result
	for _, rec := range m.Jobs {
		if rec.Status != "ok" {
			continue
		}
		// Do not allow untrusted manifests to read files outside their run directory.
		name := filepath.FromSlash(rec.ResultFile)
		if !filepath.IsLocal(name) {
			return m, nil, fmt.Errorf("unsafe result path: %q", name)
		}
		r, err := readResult(filepath.Join(dir, name))
		if err != nil {
			return m, nil, err
		}
		if !reflect.DeepEqual(r.Job, rec.Job) || !reflect.DeepEqual(r.Provenance, m.Capabilities.Provenance) {
			return m, nil, fmt.Errorf("result identity mismatch: %s", name)
		}
		results = append(results, r)
	}
	return m, results, nil
}
