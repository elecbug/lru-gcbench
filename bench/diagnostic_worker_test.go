package bench_test

import (
	"testing"

	"example.com/lrugcbench/bench"
	"example.com/lrugcbench/internal/reference"
)

func TestWorkerTopologyAccounting(t *testing.T) {
	cfg := integrationConfig()
	for _, mode := range []string{"shared", "independent", "harness"} {
		t.Run(mode, func(t *testing.T) {
			c := cfg.Cases[1]
			c.CacheMode, c.Workers = mode, 3
			c.Operations, c.WarmupOps = 1001, 101
			j := bench.Job{ID: "topology", Backend: "reference", Case: c, Runtime: cfg.Runtimes[0], Seed: 42}
			calls := 0
			factory := func(backend string, c bench.Case) (bench.Cache, error) {
				calls++
				return reference.Factory(backend, c)
			}
			r, err := bench.RunWorker(j, factory, bench.Provenance{Kind: "test-only"})
			if err != nil {
				t.Fatal(err)
			}
			copies, expectedCalls := 1, 1
			if mode != "shared" {
				copies = c.Workers
			}
			if mode == "independent" {
				expectedCalls = copies
			}
			if mode == "harness" {
				expectedCalls = 0
			}
			if calls != expectedCalls {
				t.Fatalf("factory calls %d, want %d", calls, expectedCalls)
			}
			fill := r.Phases[0]
			if fill.Operations != uint64(c.Capacity*copies) || fill.Writes != fill.Operations {
				t.Fatalf("fill accounting: %+v", fill)
			}
			wantEntries := c.Capacity * copies
			if mode == "harness" {
				wantEntries = 0
			}
			if fill.End.Cache.Entries != wantEntries {
				t.Fatalf("entries %d, want %d", fill.End.Cache.Entries, wantEntries)
			}
			measured := r.Phases[len(r.Phases)-1]
			if measured.Operations != uint64(c.Operations) || measured.Reads+measured.Writes != measured.Operations {
				t.Fatal("request work multiplied or lost")
			}
			if mode == "harness" && measured.Hits != 0 {
				t.Fatal("synthetic adapter must always miss")
			}
		})
	}
}

func TestWorkerMatchedCompactTrace(t *testing.T) {
	cfg := integrationConfig()
	for _, mode := range []string{"enabled", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			c := cfg.Cases[1]
			c.Scenario, c.CompactMode = "concurrent-compact", mode
			c.ReadPercent, c.WarmupOps, c.Operations = 100, 0, 10000
			c.RequestTraceEvery, c.MaxRequestSamples, c.CompactWindow = 7, 1430, "1ms"
			j := bench.Job{ID: "compact", Backend: "reference", Case: c, Runtime: cfg.Runtimes[0], Seed: 42}
			r, err := bench.RunWorker(j, reference.Factory, bench.Provenance{Kind: "test-only"})
			if err != nil {
				t.Fatal(err)
			}
			p := r.Phases[len(r.Phases)-1]
			if p.CompactProbe == nil || p.CompactProbe.Executed != (mode == "enabled") {
				t.Fatal("missing or incorrect probe")
			}
			if p.CompactProbe.EntriesBefore != 250 || p.CompactProbe.EntriesAfter != 250 {
				t.Fatal("matched cache state changed")
			}
			if (p.ConcurrentCompaction != nil) != (mode == "enabled") {
				t.Fatal("disabled probe reported as Compact call")
			}
			if len(p.RequestTrace) < 1400 || p.RequestTraceDropped != 0 {
				t.Fatal("incomplete trace")
			}
			for _, sample := range p.RequestTrace {
				if !sample.Read || sample.StartElapsedNS < p.Start.Runtime.ElapsedNS || sample.EndElapsedNS > p.End.Runtime.ElapsedNS {
					t.Fatal("trace outside collector timebase or read-only workload")
				}
			}
			wantWindows := 3
			if mode == "enabled" {
				wantWindows++
			}
			if len(p.RequestWindows) != wantWindows {
				t.Fatalf("windows %d, want %d", len(p.RequestWindows), wantWindows)
			}
		})
	}
}
