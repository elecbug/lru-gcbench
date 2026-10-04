package bench_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"example.com/lrugcbench/bench"
	"example.com/lrugcbench/internal/reference"
)

func integrationConfig() bench.Config {
	c := bench.Config{SchemaVersion: 1, Repetitions: 2, Seed: 2026, Timeout: "10s", Backends: []string{"reference"}, Runtimes: []bench.RuntimeConfig{{Name: "test", GOMAXPROCS: 2, GOGC: 100, GOMEMLIMIT: "64MiB"}}}
	base := bench.Case{Capacity: 1000, KeySpace: 2000, KeyKind: "prefix", ValueKind: "bytes", ValueBytes: 128, Workers: 1, Operations: 20000, WarmupOps: 1000, ReadPercent: 80, DeleteFraction: .75, SampleInterval: "1ms", MaxSamples: 128}
	for _, scenario := range []string{"footprint", "steady", "reclaim"} {
		v := base
		v.Name = scenario
		v.Scenario = scenario
		if scenario != "footprint" {
			v.Workers = 2
			v.LatencySampleEvery = 31
		}
		c.Cases = append(c.Cases, v)
	}
	return c
}
func buildReference(t *testing.T) string {
	t.Helper()
	name := "reference-worker"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", path, "../cmd/reference-worker")
	cmd.Env = bench.OverrideEnv(os.Environ(), map[string]string{"GOTOOLCHAIN": "local", "GOWORK": "off"})
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reference worker build: %v\n%s", err, b)
	}
	return path
}
func TestWorkerPhasesAndNaturalGC(t *testing.T) {
	c := integrationConfig()
	p := bench.Provenance{Kind: "reference-validation-only"}
	for _, v := range c.Cases {
		t.Run(v.Scenario, func(t *testing.T) {
			j := bench.Job{ID: "test", Backend: "reference", Case: v, Runtime: c.Runtimes[0], Seed: 4}
			r, err := bench.RunWorker(j, reference.Factory, p)
			if err != nil {
				t.Fatal(err)
			}
			if r.Phases[0].End.Cache.Entries != v.Capacity {
				t.Fatal("fill did not retain capacity")
			}
			for _, phase := range r.Phases {
				if phase.Name == "delete" && phase.End.Cache.Entries != 250 {
					t.Fatalf("delete retained %d", phase.End.Cache.Entries)
				}
				if v.Scenario == "footprint" {
					if phase.PostForcedGC == nil {
						t.Fatal("missing forced-GC snapshot")
					}
				}
				if v.Scenario != "footprint" && (phase.PostForcedGC != nil || phase.End.Runtime.GCForcedCycles != phase.Start.Runtime.GCForcedCycles) {
					t.Fatal("forced GC leaked into natural-GC phase")
				}
				if phase.Name == "measured" || phase.Name == "recovery" {
					if phase.Operations != uint64(v.Operations) {
						t.Fatal("wrong measured count")
					}
					hits := phase.End.Cache.GetHits - phase.Start.Cache.GetHits
					misses := phase.End.Cache.GetMisses - phase.Start.Cache.GetMisses
					if hits != phase.Hits || hits+misses != phase.Reads {
						t.Fatal("harness and cache counters disagree")
					}
				}
			}
			if _, err = json.Marshal(r); err != nil {
				t.Fatal("invalid JSON (possibly +Inf)", err)
			}
		})
	}
}
func TestSuiteSubprocessReportAndCompare(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	worker := buildReference(t)
	base := t.TempDir()
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	cfg := integrationConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m, err := bench.RunSuite(ctx, worker, cfg, a, "reference-A", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Jobs) != 6 {
		t.Fatal(len(m.Jobs))
	}
	_, results, err := bench.LoadSuite(a)
	if err != nil {
		t.Fatal(err)
	}
	pids := map[int]bool{}
	for _, r := range results {
		if r.PID == os.Getpid() {
			t.Fatal("worker ran in controller")
		}
		if pids[r.PID] {
			t.Fatal("worker process reused")
		}
		pids[r.PID] = true
		if r.Provenance.Kind != "reference-validation-only" {
			t.Fatal("mislabelled reference result")
		}
	}
	for _, name := range []string{"manifest.json", "summary.json", "summary.csv", "samples.csv", "report.md", "report.html"} {
		if st, err := os.Stat(filepath.Join(a, name)); err != nil || st.Size() == 0 {
			t.Fatal("missing report", name)
		}
	}
	report, _ := os.ReadFile(filepath.Join(a, "report.md"))
	if !strings.Contains(string(report), "NOT measurements of google/go-lru") {
		t.Fatal("missing reference warning")
	}
	if _, err = bench.RunSuite(ctx, worker, cfg, a, "overwrite", io.Discard); err == nil {
		t.Fatal("existing run overwritten")
	}
	if _, err = bench.RunSuite(ctx, worker, cfg, b, "reference-B", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err = bench.Compare(a, b, filepath.Join(base, "comparison"), false); err != nil {
		t.Fatal(err)
	}
	// Mismatched workload/backend is rejected, not silently routed to reference.
	bad := cfg
	bad.Backends = []string{"map"}
	if _, err = bench.RunSuite(ctx, worker, bad, filepath.Join(base, "bad"), "bad", io.Discard); err == nil {
		t.Fatal("reference accepted google backend")
	}
}
func TestTimeoutIsRecorded(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	worker := buildReference(t)
	cfg := integrationConfig()
	cfg.Repetitions = 1
	cfg.Cases = cfg.Cases[1:2]
	cfg.Timeout = "1ns"
	out := filepath.Join(t.TempDir(), "timeout")
	m, err := bench.RunSuite(context.Background(), worker, cfg, out, "timeout-test", io.Discard)
	if err == nil || len(m.Jobs) != 1 || m.Jobs[0].Status != "timeout" {
		t.Fatalf("timeout not recorded: %v %+v", err, m.Jobs)
	}
	if _, err = os.Stat(filepath.Join(out, "report.md")); err != nil {
		t.Fatal("failed run report missing")
	}
}
func TestRejectMalformedManifestPath(t *testing.T) {
	dir := t.TempDir()
	m := bench.Manifest{SchemaVersion: 1, Jobs: []bench.JobRecord{{Status: "ok", ResultFile: "../outside.json"}}}
	if err := bench.WriteJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bench.LoadSuite(dir); err == nil {
		t.Fatal("path traversal accepted")
	}
}

// Incorrect cache behavior must fail the experiment, not look like a speedup.
type brokenCache struct{}

func (brokenCache) Put(string, uint64) error { return nil }
func (brokenCache) Get(string) bool          { return false }
func (brokenCache) Delete(string) bool       { return false }
func (brokenCache) Compact()                 {}
func (brokenCache) Stats() bench.CacheStats  { return bench.CacheStats{MaxSize: 1000} }
func TestRejectIncorrectCacheOutcome(t *testing.T) {
	cfg := integrationConfig()
	j := bench.Job{ID: "broken", Backend: "broken", Case: cfg.Cases[0], Runtime: cfg.Runtimes[0]}
	factory := func(string, bench.Case) (bench.Cache, error) { return brokenCache{}, nil }
	if _, err := bench.RunWorker(j, factory, bench.Provenance{Kind: "test-only"}); err == nil {
		t.Fatal("incorrect fill accepted as benchmark result")
	}
}

func TestPrefixDeletionAndConcurrentCompactionPhases(t *testing.T) {
	for _, scenario := range []string{"footprint", "reclaim", "concurrent-compact"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := integrationConfig()
			c := cfg.Cases[0]
			c.Name = scenario
			c.Scenario = scenario
			c.Capacity = 101
			c.KeySpace = 202
			c.DeleteMode = "prefix"
			c.Operations = 5000
			c.LatencySampleEvery = 0
			if scenario == "concurrent-compact" {
				c.Workers = 2
				c.LatencySampleEvery = 1
			}
			j := bench.Job{ID: "prefix", Backend: "reference", Case: c, Runtime: cfg.Runtimes[0], Seed: 42}
			r, err := bench.RunWorker(j, reference.Factory, bench.Provenance{Kind: "reference-validation-only"})
			if err != nil {
				t.Fatal(err)
			}
			deletion := r.Phases[1]
			if deletion.Name != "delete_prefix" || deletion.Operations != 12 || deletion.End.Cache.Entries != 24 || deletion.End.Cache.EvictionsDeleted-deletion.Start.Cache.EvictionsDeleted != 77 {
				t.Fatalf("unexpected prefix deletion: %+v", deletion)
			}
			last := r.Phases[len(r.Phases)-1]
			if scenario == "concurrent-compact" {
				window := last.ConcurrentCompaction
				if last.Name != "concurrent" || window == nil {
					t.Fatal("missing concurrent compaction window")
				}
				if window.StartElapsedNS < last.Start.Runtime.ElapsedNS || window.EndElapsedNS > last.End.Runtime.ElapsedNS || window.DurationNS != window.EndElapsedNS-window.StartElapsedNS {
					t.Fatalf("window outside workload: %+v", window)
				}
				if last.End.Cache.CompactionsExplicit-last.Start.Cache.CompactionsExplicit != 1 {
					t.Fatal("expected exactly one explicit compaction")
				}
			}
		})
	}
}
