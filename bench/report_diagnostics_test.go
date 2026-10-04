package bench

import (
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRatesAggregatePerRunAndSeparatePressureTiers(t *testing.T) {
	var results []Result
	for i, hits := range []uint64{100, 0, 50} {
		r := Result{Job: Job{Backend: "arena", Repeat: i, Case: Case{Name: "pressure", Scenario: "steady"}}}
		p := Phase{Name: "measured", DurationNS: int64(i+1) * 1e9, Operations: 100, Reads: 100, Hits: hits}
		p.Start.Cache = CacheStats{CompactionsPressureTier1: 2, CompactionsPressureTier2: 5, CompactionsAutoSlack: 3}
		p.End.Cache = CacheStats{CompactionsPressureTier1: 5, CompactionsPressureTier2: 9, CompactionsAutoSlack: 10, ArenaLiveNodes: 18, ArenaFreeNodes: 2, ArenaUnallocatedCap: 80}
		r.Phases = []Phase{p}
		results = append(results, r)
	}
	groups := AggregateResults(results)
	if len(groups) != 1 {
		t.Fatal(groups)
	}
	m := groups[0].Metrics
	for key, want := range map[string]float64{"read_hits_per_s": 50.0 / 3, "read_misses_per_s": 50.0 / 3, "pressure_tier1_compactions_delta": 3, "pressure_tier2_compactions_delta": 4, "pressure_compactions_delta": 7, "auto_slack_compactions_delta": 7, "arena_live_nodes_end": 18, "arena_free_nodes_end": 2, "arena_unallocated_capacity_end": 80} {
		if math.Abs(m[key].Median-want) > 1e-9 {
			t.Errorf("%s = %g, want %g", key, m[key].Median, want)
		}
	}
	if m["read_hits_per_s"].Median == m["workload_ops_per_s"].Median*m["hit_rate"].Median {
		t.Fatal("read-hit throughput incorrectly calculated from summary medians")
	}
	zero := phaseMetrics(results[0], Phase{Reads: 1, Hits: 1})
	if _, ok := zero["read_hits_per_s"]; ok {
		t.Fatal("zero duration emitted a read rate")
	}
}

func TestHarnessControlOmitsSyntheticCacheMetrics(t *testing.T) {
	r := Result{Job: Job{Backend: "arena", Case: Case{CacheMode: "harness", Profile: "cpu", SampleCacheStats: true}}}
	p := Phase{Name: "measured", DurationNS: 1e9, Operations: 100, Reads: 90, Hits: 45}
	m := phaseMetrics(r, p)
	for _, key := range []string{"hit_rate", "read_hits_per_s", "read_misses_per_s", "cache_entries_end", "pressure_compactions_delta", "pressure_tier1_compactions_delta", "arena_live_nodes_end"} {
		if _, ok := m[key]; ok {
			t.Errorf("synthetic control emitted real-cache metric %s", key)
		}
	}
	if m["workload_ops_per_s"] != 100 {
		t.Fatal("harness throughput missing")
	}
	warnings := strings.Join(phaseQualityWarnings(r, p), "\n")
	for _, want := range []string{"HARNESS CONTROL", "PROFILED DIAGNOSTIC", "CACHE OBSERVER"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("missing diagnostic label %s", want)
		}
	}
}

func TestCacheObservationsUseTheirOwnTimestampAndDoNotInventZeros(t *testing.T) {
	r := Result{Samples: []RuntimeSample{
		{ElapsedNS: 1, CacheObserved: false},
		{ElapsedNS: 2, CacheObserved: true, CacheElapsedNS: 20, CacheEntries: 500},
		{ElapsedNS: 3, CacheObserved: true, CacheElapsedNS: 5, CacheEntries: 10},
		{ElapsedNS: -1, CacheObserved: true, CacheElapsedNS: 6, CacheEntries: 15},
	}}
	p := Phase{Start: Snapshot{Runtime: RuntimeSample{ElapsedNS: 0}}, End: Snapshot{Runtime: RuntimeSample{ElapsedNS: 10}}}
	m := phaseMetrics(r, p)
	if m["periodic_cache_sample_count"] != 2 || m["sampled_cache_entries_min"] != 10 || m["sampled_cache_entries_max"] != 15 {
		t.Fatal(m)
	}
	path := filepath.Join(t.TempDir(), "samples.csv")
	if err := writeTraceCSV(path, []Result{r}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range rows[0] {
		if name == "cache_entries" && (rows[1][i] != "" || rows[2][i] != "500") {
			t.Fatalf("missing cache observation confused with zero: %v", rows)
		}
	}
}

func TestWindowThroughputDoesNotTreatOverlapOrDroppedTraceAsRate(t *testing.T) {
	p := Phase{RequestWindows: []RequestWindowSummary{
		{Name: "trigger-window", StartElapsedNS: 0, EndElapsedNS: 1000, Samples: 5, EstimatedOpsPerSecond: 100},
		{Name: "compact-overlap", StartElapsedNS: 50, EndElapsedNS: 100, Samples: 2, EstimatedOpsPerSecond: 500},
	}}
	m := map[string]float64{}
	addWindowMetrics(m, p)
	if m["window_trigger_window_estimated_ops_per_s"] != 100 {
		t.Fatal(m)
	}
	if _, ok := m["window_compact_overlap_estimated_ops_per_s"]; ok {
		t.Fatal("overlapping calls do not define a throughput count")
	}
	p.RequestTraceDropped = 3
	m = map[string]float64{}
	addWindowMetrics(m, p)
	if _, ok := m["window_trigger_window_estimated_ops_per_s"]; ok {
		t.Fatal("incomplete trace emitted a throughput estimate")
	}
}
