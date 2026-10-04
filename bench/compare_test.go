package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func comparisonFixture() Result {
	return Result{
		SchemaVersion: SchemaVersion,
		Job:           Job{ID: "job-1", Backend: "map", Repeat: 1, Seed: 42, Case: Case{Name: "steady"}, Runtime: RuntimeConfig{Name: "default"}},
		Provenance:    Provenance{Kind: "upstream-checkout", HarnessTreeSHA256: "harness-a"},
		Environment:   Environment{GoVersion: "test-go", Hostname: "same-host"},
		Phases:        []Phase{{Name: "measured", DurationNS: 1000, Operations: 100, Reads: 70, Writes: 30, Hits: 40, WorkloadChecksum: 99}},
	}
}

func writeComparisonFixture(t *testing.T, dir string, r Result, duplicate bool) {
	t.Helper()
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(filepath.Join(dir, "result.json"), r); err != nil {
		t.Fatal(err)
	}
	m := Manifest{SchemaVersion: SchemaVersion, Capabilities: Capabilities{Provenance: r.Provenance}, Jobs: []JobRecord{{Job: r.Job, Status: "ok", ResultFile: "result.json"}}}
	if duplicate {
		m.Jobs = append(m.Jobs, m.Jobs[0])
	}
	if err := WriteJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
}

func TestCompareIntegrity(t *testing.T) {
	tests := []struct {
		name      string
		base      func(*Result)
		candidate func(*Result)
		options   CompareOptions
		duplicate bool
		wantError string
	}{
		{name: "same input different hits accepted", candidate: func(r *Result) { r.Phases[0].Hits++ }},
		{name: "harness mismatch", candidate: func(r *Result) { r.Provenance.HarnessTreeSHA256 = "harness-b" }, wantError: "harness source hashes"},
		{name: "environment override cannot bypass harness", candidate: func(r *Result) { r.Provenance.HarnessTreeSHA256 = "harness-b" }, options: CompareOptions{AllowEnvironmentDiff: true}, wantError: "harness source hashes"},
		{name: "explicit harness override", candidate: func(r *Result) { r.Provenance.HarnessTreeSHA256 = "harness-b" }, options: CompareOptions{AllowHarnessDiff: true}},
		{name: "missing upstream harness", base: func(r *Result) { r.Provenance.HarnessTreeSHA256 = "" }, candidate: func(r *Result) { r.Provenance.HarnessTreeSHA256 = "" }, wantError: "harness source hashes"},
		{name: "legacy reference allowed", base: func(r *Result) { r.Provenance = Provenance{Kind: "reference-validation-only"} }, candidate: func(r *Result) { r.Provenance = Provenance{Kind: "reference-validation-only"} }},
		{name: "checksum mismatch", candidate: func(r *Result) { r.Phases[0].WorkloadChecksum++ }, wantError: "workload checksum"},
		{name: "checksum mismatch despite all overrides", candidate: func(r *Result) {
			r.Phases[0].WorkloadChecksum++
			r.Provenance.HarnessTreeSHA256 = "harness-b"
			r.Environment.Hostname = "different"
		}, options: CompareOptions{AllowHarnessDiff: true, AllowEnvironmentDiff: true}, wantError: "workload checksum"},
		{name: "operation count mismatch", candidate: func(r *Result) { r.Phases[0].Operations++ }, wantError: "operation counts"},
		{name: "read count mismatch", candidate: func(r *Result) { r.Phases[0].Reads++ }, wantError: "operation counts"},
		{name: "write count mismatch", candidate: func(r *Result) { r.Phases[0].Writes++ }, wantError: "operation counts"},
		{name: "repeat mismatch", candidate: func(r *Result) { r.Job.Repeat++ }, wantError: "repeat/seed sets"},
		{name: "environment mismatch", candidate: func(r *Result) { r.Environment.Hostname = "different" }, wantError: "environment differs"},
		{name: "harness override cannot bypass environment", candidate: func(r *Result) { r.Environment.Hostname = "different" }, options: CompareOptions{AllowHarnessDiff: true}, wantError: "environment differs"},
		{name: "explicit environment override", candidate: func(r *Result) { r.Environment.Hostname = "different" }, options: CompareOptions{AllowEnvironmentDiff: true}},
		{name: "duplicate replicate", duplicate: true, wantError: "duplicate replicate"},
		{name: "duplicate phase", candidate: func(r *Result) { r.Phases = append(r.Phases, r.Phases[0]) }, wantError: "duplicate replicate phase"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			base, candidate := comparisonFixture(), comparisonFixture()
			if tt.base != nil {
				tt.base(&base)
			}
			if tt.candidate != nil {
				tt.candidate(&candidate)
			}
			writeComparisonFixture(t, filepath.Join(dir, "base"), base, false)
			writeComparisonFixture(t, filepath.Join(dir, "candidate"), candidate, tt.duplicate)
			out := filepath.Join(dir, "comparison")
			err := CompareWithOptions(filepath.Join(dir, "base"), filepath.Join(dir, "candidate"), out, tt.options)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("got %v, wanted %q", err, tt.wantError)
				}
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Fatal("invalid comparison created output")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(out, "comparison.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result Comparison
			if err = json.Unmarshal(data, &result); err != nil {
				t.Fatal(err)
			}
			if result.HarnessOverride != tt.options.AllowHarnessDiff || result.EnvironmentOverride != tt.options.AllowEnvironmentDiff || result.BaselineHarnessSHA256 != base.Provenance.HarnessTreeSHA256 || result.CandidateHarnessSHA256 != candidate.Provenance.HarnessTreeSHA256 {
				t.Fatal("comparison lost override or harness provenance")
			}
			if err = CompareWithOptions(filepath.Join(dir, "base"), filepath.Join(dir, "candidate"), out, tt.options); err == nil {
				t.Fatal("comparison output overwritten")
			}
		})
	}
}

func TestPairedScheduleReproducibleAndAlternating(t *testing.T) {
	jobs := []Job{{ID: "one"}, {ID: "two"}, {ID: "three"}}
	starts := map[string]bool{}
	for seed := int64(0); seed < 10; seed++ {
		a, b := pairedSteps(jobs, seed), pairedSteps(jobs, seed)
		if len(a) != 2*len(jobs) {
			t.Fatal("incorrect step count")
		}
		starts[a[0].Side] = true
		for i := range a {
			if a[i] != b[i] {
				t.Fatal("seed did not reproduce schedule")
			}
			if a[i].Pair != i/2 || a[i].JobID != jobs[i/2].ID {
				t.Fatal("job order changed")
			}
			if i%2 == 1 && a[i].Side == a[i-1].Side {
				t.Fatal("pair repeated same side")
			}
			if i > 1 && i%2 == 0 && a[i].Side == a[i-2].Side {
				t.Fatal("starting side did not alternate")
			}
		}
	}
	if len(starts) != 2 {
		t.Fatal("seed never changed starting side")
	}
}
