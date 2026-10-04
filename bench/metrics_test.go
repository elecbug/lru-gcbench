package bench

import (
	"math"
	"reflect"
	"runtime"
	"strconv"
	"testing"
)

func TestHistogramDeltaAndQuantiles(t *testing.T) {
	a := Histogram{BoundsSeconds: []string{"0", "0.001", "0.002", "+Inf"}, Counts: []uint64{10, 20, 0}}
	b := Histogram{BoundsSeconds: a.BoundsSeconds, Counts: []uint64{15, 24, 1}}
	d, err := histogramDelta(a, b)
	if err != nil || !reflect.DeepEqual(d.Counts, []uint64{5, 4, 1}) {
		t.Fatalf("delta %+v %v", d, err)
	}
	q := histogramQuantile(d, .5)
	if q == nil || q.LowerSeconds != 0 || q.UpperSeconds == nil || *q.UpperSeconds != .001 {
		t.Fatal(q)
	}
	if q = histogramQuantile(d, .99); q == nil || q.UpperSeconds != nil {
		t.Fatal("top bucket must be unbounded")
	}
	b.Counts[0] = 1
	if _, err = histogramDelta(a, b); err == nil {
		t.Fatal("counter regression accepted")
	}
	if histogramQuantile(Histogram{}, .99) != nil {
		t.Fatal("empty histogram should have no quantile")
	}
}
func TestLatencyBucketCoverage(t *testing.T) {
	values := []uint64{0, 1, 2, 7, 8, 9, 15, 16, 17, 31, 32, 1000, 1234567, 1 << 30, 1 << 62}
	for _, v := range values {
		var h latencyHistogram
		h.add(v)
		s := h.summary()
		if s.Samples != 1 || s.P99 == nil {
			t.Fatal(s)
		}
		lo := s.P99.LowerSeconds
		hi := *s.P99.UpperSeconds
		x := float64(v) * 1e-9
		if x < lo || x >= hi {
			t.Fatalf("%d not within [%g,%g)", v, lo, hi)
		}
	}
	var h latencyHistogram
	for i := 0; i < 10000; i++ {
		h.add(uint64(i))
	}
	s := h.summary()
	if s.Samples != 10000 {
		t.Fatal(s.Samples)
	}
	for i := 1; i < len(s.Histogram.BoundsSeconds); i++ {
		a, _ := strconv.ParseFloat(s.Histogram.BoundsSeconds[i-1], 64)
		b, _ := strconv.ParseFloat(s.Histogram.BoundsSeconds[i], 64)
		if b <= a || math.IsInf(b, 0) {
			t.Fatal("invalid latency bounds")
		}
	}
}
func TestCollectorAndHistogramOwnership(t *testing.T) {
	c, err := newCollector()
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	a, h := c.read(true)
	saved := append([]uint64(nil), h.Counts...)
	runtime.GC()
	b, _ := c.read(true)
	if !reflect.DeepEqual(saved, h.Counts) {
		t.Fatal("retained runtime-owned histogram memory")
	}
	if b.GCCycles < a.GCCycles || b.GCForcedCycles <= a.GCForcedCycles {
		t.Fatal("missing forced cycle")
	}
	if b.RuntimeManagedBytes != sub(b.RuntimeTotalBytes, b.HeapReleasedBytes) {
		t.Fatal("bad managed memory formula")
	}
	if runtime.GOOS == "linux" && (!b.RSSAvailable || b.RSSBytes == 0) {
		t.Fatal("Linux RSS unavailable")
	}
}
