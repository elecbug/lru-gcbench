package bench_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"example.com/lrugcbench/bench"
)

func TestSuiteProfileArtifacts(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	worker := buildReference(t)
	cfg := integrationConfig()
	cfg.Repetitions = 1
	base := cfg.Cases[1]
	base.Capacity, base.KeySpace = 100, 200
	base.WarmupOps, base.Operations = 50, 5000
	cfg.Cases = nil
	for _, kind := range []string{"cpu", "allocs", "mutex", "block"} {
		c := base
		c.Name, c.Profile, c.ProfilePhase = kind, kind, "measured"
		cfg.Cases = append(cfg.Cases, c)
	}
	out := filepath.Join(t.TempDir(), "profile suite")
	inherited := filepath.Join(t.TempDir(), "inherited")
	t.Setenv("LRUGCBENCH_PROFILE_DIR", inherited)
	m, err := bench.RunSuite(context.Background(), worker, cfg, out, "profile-test", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, results, err := bench.LoadSuite(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatalf("missing diagnostic results: %d", len(results))
	}
	for i, result := range results {
		meta := result.Profiling
		if meta == nil || !meta.Completed || !meta.Diagnostic || meta.Directory != m.Jobs[i].ProfileDir {
			t.Fatalf("missing or mismatched profile metadata: %+v", meta)
		}
		for _, p := range result.Phases {
			if p.Start.Runtime.GCForcedCycles != p.End.Runtime.GCForcedCycles {
				t.Fatalf("profile publication GC leaked inside %s's counter window", p.Name)
			}
		}
		profileDir := filepath.Join(out, meta.Directory)
		for _, name := range append(append([]string(nil), meta.Files...), "analyze.sh", "README.md", "profile.json") {
			if st, err := os.Stat(filepath.Join(profileDir, name)); err != nil || st.Size() == 0 {
				t.Fatalf("missing %s: %v", name, err)
			}
		}
		// Parse the real protobuf profile with Go's pprof, including the
		// before/after allocation subtraction used by the saved script.
		cmd := exec.Command("sh", filepath.Join(profileDir, "analyze.sh"), "-nodecount=2")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pprof could not read %s: %v\n%s", meta.Kind, err, output)
		}
	}
	if _, err := os.Stat(inherited); !os.IsNotExist(err) {
		t.Fatal("inherited environment redirected profile output")
	}

	// The selected phase must be explicit, not silently defaulted to the
	// whole process (which includes cache fill and workload warmup).
	bad := cfg
	bad.Cases = append([]bench.Case(nil), cfg.Cases[:1]...)
	bad.Cases[0].ProfilePhase = ""
	if _, err := bench.RunSuite(context.Background(), worker, bad, filepath.Join(t.TempDir(), "invalid"), "invalid", io.Discard); err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("missing profile phase accepted: %v", err)
	}
}
