package bench

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDistributionMedianInterval(t *testing.T) {
	if d := distribution(nil); d.N != 0 || d.MedianCI != nil {
		t.Fatal(d)
	}
	if d := distribution([]float64{3, 1, 2}); d.Median != 2 || d.MedianCI != nil {
		t.Fatal(d)
	}
	d := distribution([]float64{6, 5, 4, 3, 2, 1})
	if d.Median != 3.5 || d.MedianCI == nil || d.MedianCI.Lower != 1 || d.MedianCI.Upper != 6 || math.Abs(d.MedianCI.Coverage-.96875) > 1e-12 {
		t.Fatal(d)
	}
	d = distribution([]float64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1})
	if d.MedianCI == nil || d.MedianCI.Lower != 2 || d.MedianCI.Upper != 9 || math.Abs(d.MedianCI.Coverage-.978515625) > 1e-12 {
		t.Fatal(d)
	}
	// Far-tail binomial probabilities underflow at this size; computing each
	// log probability must still produce a central interval, not [min,max].
	v := make([]float64, 2000)
	for i := range v {
		v[i] = float64(i)
	}
	d = distribution(v)
	if d.MedianCI == nil || d.MedianCI.Coverage < .95 || d.MedianCI.Lower < 900 || d.MedianCI.Upper > 1100 {
		t.Fatal(d.MedianCI)
	}
}

func TestPhaseNormalizedMetricsAndCoverage(t *testing.T) {
	r := Result{Job: Job{Case: Case{Scenario: "steady"}}, Baseline: Snapshot{Runtime: RuntimeSample{HeapObjectsBytes: 100}}, DroppedSamples: 3,
		Samples: []RuntimeSample{{ElapsedNS: 200000000, RSSAvailable: true}, {ElapsedNS: 400000000}, {ElapsedNS: 900000000, RSSAvailable: true}, {ElapsedNS: 2000000000}}}
	p := Phase{Name: "measured", DurationNS: 1e9, Operations: 20, Start: Snapshot{Runtime: RuntimeSample{AllocBytes: 100, AllocObjects: 10}}, End: Snapshot{Runtime: RuntimeSample{ElapsedNS: 1e9, AllocBytes: 300, AllocObjects: 50, GCCPUSeconds: .000001}}, PostForcedGC: &Snapshot{Runtime: RuntimeSample{HeapObjectsBytes: 200}, Cache: CacheStats{Entries: 10}}, GetLatency: LatencySummary{Samples: 3}, PutLatency: LatencySummary{Samples: 4}, GCPauses: Histogram{BoundsSeconds: []string{"0", "1", "+Inf"}, Counts: []uint64{2, 3}}}
	m := phaseMetrics(r, p)
	for key, want := range map[string]float64{"allocated_bytes_per_op": 10, "allocated_objects_per_op": 2, "gc_cpu_ns_per_op": 50, "post_gc_heap_delta_per_entry_bytes": 10, "get_latency_samples": 3, "put_latency_samples": 4, "gc_pause_events": 5, "periodic_sample_count": 3, "periodic_rss_sample_count": 2, "periodic_max_gap_ms": 500, "job_dropped_samples": 3} {
		if math.Abs(m[key]-want) > 1e-8 {
			t.Errorf("%s = %g; want %g", key, m[key], want)
		}
	}
	p.Operations = 0
	p.PostForcedGC.Cache.Entries = 0
	m = phaseMetrics(r, p)
	for _, key := range []string{"allocated_bytes_per_op", "allocated_objects_per_op", "gc_cpu_ns_per_op", "post_gc_heap_delta_per_entry_bytes"} {
		if _, ok := m[key]; ok {
			t.Errorf("unexpected %s for zero denominator", key)
		}
	}
	p.Operations = 20
	p.PostForcedGC.Cache.Entries = 10
	p.PostForcedGC.Runtime.HeapObjectsBytes = 99
	r.Job.Case.Scenario = "footprint"
	m = phaseMetrics(r, p)
	if _, ok := m["post_gc_heap_delta_per_entry_bytes"]; ok {
		t.Error("reported nonpositive retained-heap delta")
	}
	if _, ok := m["gc_cpu_ns_per_op"]; ok {
		t.Error("reported footprint as natural GC")
	}
	r.Job.Case.Scenario = "steady"
	p.End.Runtime.GCForcedCycles = 1
	if _, ok := phaseMetrics(r, p)["gc_cpu_ns_per_op"]; ok {
		t.Error("reported forced GC as natural GC")
	}
}

func TestQualityWarningsReflectMissingObservations(t *testing.T) {
	r := Result{Job: Job{Case: Case{Scenario: "steady", LatencySampleEvery: 10, SampleInterval: "10ms"}}, DroppedSamples: 1}
	p := Phase{Name: "measured", DurationNS: 200000000, Reads: 1000, Writes: 200}
	warnings := strings.Join(phaseQualityWarnings(r, p), "\n")
	for _, want := range []string{"shorter than 1 second", "Get p99", "Put p99", "No GC cycles", "100 pause events", "dropped", "No periodic samples"} {
		if !strings.Contains(warnings, want) {
			t.Errorf("missing %q in %s", want, warnings)
		}
	}
	m := phaseMetrics(r, p)
	if m["gc_pause_events"] != 0 {
		t.Fatal(m)
	}
	if _, ok := m["gc_pause_p99_lower_seconds"]; ok {
		t.Error("no events reported as a quantile")
	}
	r.Job.Case.Scenario = "footprint"
	r.DroppedSamples = 0
	p.Name = "fill"
	if w := phaseQualityWarnings(r, p); len(w) != 0 {
		t.Fatal("expected footprint instrumentation policy was flagged", w)
	}
}

func TestChartDownsamplingPreservesExtremaAndEndpoints(t *testing.T) {
	samples := make([]RuntimeSample, 10000)
	for i := range samples {
		samples[i] = RuntimeSample{ElapsedNS: int64(i), HeapObjectsBytes: 500, RSSBytes: 600, RSSAvailable: true}
	}
	samples[1234].HeapObjectsBytes = 1
	samples[2345].HeapObjectsBytes = 9999
	samples[4567].RSSBytes = 2
	samples[6789].RSSBytes = 8888
	got := chartSamples(samples, 600)
	if len(got) > 600 || got[0].ElapsedNS != 0 || got[len(got)-1].ElapsedNS != 9999 {
		t.Fatal("bounds or endpoints lost", len(got))
	}
	found := map[int64]bool{}
	for _, s := range got {
		found[s.ElapsedNS] = true
	}
	for _, ns := range []int64{1234, 2345, 4567, 6789} {
		if !found[ns] {
			t.Errorf("extremum %d lost", ns)
		}
	}
	if len(samples) != 10000 || samples[2345].HeapObjectsBytes != 9999 {
		t.Fatal("input was changed")
	}
}

func TestHTMLReportEscapesLabelsAndShowsPhases(t *testing.T) {
	label := `<script>alert("label")</script>`
	r := Result{Job: Job{ID: label, Backend: "lru", Case: Case{Name: label, Scenario: "concurrent-compact"}}, Phases: []Phase{{Name: "delete_prefix", Start: Snapshot{Runtime: RuntimeSample{ElapsedNS: 1e9}}, End: Snapshot{Runtime: RuntimeSample{ElapsedNS: 2e9, HeapObjectsBytes: 4096}}}, {Name: "concurrent", Start: Snapshot{Runtime: RuntimeSample{ElapsedNS: 2e9}}, End: Snapshot{Runtime: RuntimeSample{ElapsedNS: 3e9, HeapObjectsBytes: 2048}}, ConcurrentCompaction: &CompactionWindow{StartElapsedNS: 2100000000, EndElapsedNS: 2200000000, DurationNS: 100000000}}}}
	path := filepath.Join(t.TempDir(), "report.html")
	if err := writeHTMLReport(path, Manifest{Label: label}, []Result{r}, AggregateResults([]Result{r})); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("label is not escaped")
	}
	for _, want := range []string{"<table>", "<svg ", "delete_prefix", "concurrent Compact", "Sampling coverage", "quality warnings", "lower bounds"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	c := makeMemoryChart(r)
	if len(c.Phases) != 2 || len(c.Events) != 1 || c.Events[0].Start != "2.100" || c.Events[0].End != "2.200" {
		t.Fatal(c)
	}
	if strings.Contains(c.HeapPath, "NaN") || strings.Contains(c.HeapPath, "Inf") {
		t.Fatal("nonfinite path", c.HeapPath)
	}
	empty := makeMemoryChart(Result{})
	if strings.Contains(empty.HeapPath, "NaN") || strings.Contains(empty.HeapPath, "Inf") {
		t.Fatal("zero span has nonfinite path", empty)
	}
}
