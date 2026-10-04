package bench

import (
	"math"
	"sort"
)

// MedianInterval is a two-sided, distribution-free order-statistic interval.
// Coverage is the binomial coverage for independent, identically distributed
// continuous observations; ties can make the interval conservative.
type MedianInterval struct {
	Lower    float64 `json:"lower"`
	Upper    float64 `json:"upper"`
	Coverage float64 `json:"coverage"`
}

// medianInterval expects sorted observations. The narrowest symmetric rank
// interval with at least 95% binomial coverage is used. For n<6 even [min,max]
// has less than 95% coverage, so no finite interval is reported.
func medianInterval(v []float64) *MedianInterval {
	n := len(v)
	if n < 6 {
		return nil
	}
	logN, _ := math.Lgamma(float64(n + 1))
	tail := 0.0
	var ci *MedianInterval
	for k := 0; k < n/2; k++ {
		logK, _ := math.Lgamma(float64(k + 1))
		logRest, _ := math.Lgamma(float64(n - k + 1))
		tail += math.Exp(logN - logK - logRest - float64(n)*math.Ln2)
		coverage := 1 - 2*tail
		if coverage < .95 {
			break
		}
		ci = &MedianInterval{Lower: v[k], Upper: v[n-1-k], Coverage: coverage}
	}
	return ci
}

func addNormalizedMetrics(out map[string]float64, r Result, p Phase) {
	a, b := p.Start.Runtime, p.End.Runtime
	if event := p.ConcurrentCompaction; event != nil {
		out["concurrent_compaction_duration_ms"] = float64(event.DurationNS) / 1e6
	}
	if p.Operations > 0 {
		n := float64(p.Operations)
		out["allocated_bytes_per_op"] = float64(sub(b.AllocBytes, a.AllocBytes)) / n
		out["allocated_objects_per_op"] = float64(sub(b.AllocObjects, a.AllocObjects)) / n
		if r.Job.Case.Scenario != "footprint" && b.GCForcedCycles == a.GCForcedCycles {
			out["gc_cpu_ns_per_op"] = math.Max(0, b.GCCPUSeconds-a.GCCPUSeconds) * 1e9 / n
		}
	}
	if s := p.PostForcedGC; s != nil && s.Cache.Entries > 0 && s.Runtime.HeapObjectsBytes > r.Baseline.Runtime.HeapObjectsBytes {
		out["post_gc_heap_delta_per_entry_bytes"] = float64(s.Runtime.HeapObjectsBytes-r.Baseline.Runtime.HeapObjectsBytes) / float64(s.Cache.Entries)
	}
}

func pauseEvents(h Histogram) uint64 {
	var n uint64
	for _, v := range h.Counts {
		n += v
	}
	return n
}

func addCoverageMetrics(out map[string]float64, r Result, p Phase) {
	out["get_latency_samples"] = float64(p.GetLatency.Samples)
	out["put_latency_samples"] = float64(p.PutLatency.Samples)
	out["gc_pause_events"] = float64(pauseEvents(p.GCPauses))
	out["job_dropped_samples"] = float64(r.DroppedSamples)
	start, end := p.Start.Runtime.ElapsedNS, p.End.Runtime.ElapsedNS
	times := []int64{start, end}
	count, rssCount := 0, 0
	for _, s := range r.Samples {
		if s.ElapsedNS >= start && s.ElapsedNS <= end {
			count++
			times = append(times, s.ElapsedNS)
			if s.RSSAvailable {
				rssCount++
			}
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	var maxGap int64
	for i := 1; i < len(times); i++ {
		maxGap = max(maxGap, times[i]-times[i-1])
	}
	out["periodic_sample_count"] = float64(count)
	out["periodic_rss_sample_count"] = float64(rssCount)
	out["periodic_max_gap_ms"] = float64(maxGap) / 1e6
}

func requestPhase(name string) bool {
	return name == "measured" || name == "recovery" || name == "concurrent"
}

func phaseQualityWarnings(r Result, p Phase) []string {
	var warnings []string
	if requestPhase(p.Name) {
		if p.DurationNS < 1e9 {
			warnings = append(warnings, "Measured workload is shorter than 1 second; scheduling and startup effects may dominate.")
		}
		if r.Job.Case.LatencySampleEvery > 0 {
			if p.Reads > 0 && p.GetLatency.Samples < 1000 {
				warnings = append(warnings, "Get p99 has fewer than 1000 latency samples (or is unavailable).")
			}
			if p.Writes > 0 && p.PutLatency.Samples < 1000 {
				warnings = append(warnings, "Put p99 has fewer than 1000 latency samples (or is unavailable).")
			}
		}
		if r.Job.Case.Scenario != "footprint" {
			if sub(p.End.Runtime.GCCycles, p.Start.Runtime.GCCycles) == 0 {
				warnings = append(warnings, "No GC cycles observed; this phase does not establish GC behavior.")
			}
			if pauseEvents(p.GCPauses) < 100 {
				warnings = append(warnings, "GC-pause p99 has fewer than 100 pause events (or is unavailable); pause events are not GC cycles.")
			}
		}
	}
	if r.DroppedSamples > 0 {
		warnings = append(warnings, "Periodic samples were dropped in this job; the retained trace may omit later phases.")
	}
	if r.Job.Case.Scenario != "footprint" && r.Job.Case.SampleInterval != "0" && r.Job.Case.SampleInterval != "" {
		found := false
		for _, s := range r.Samples {
			if s.ElapsedNS >= p.Start.Runtime.ElapsedNS && s.ElapsedNS <= p.End.Runtime.ElapsedNS {
				found = true
				break
			}
		}
		if !found {
			warnings = append(warnings, "No periodic samples retained in this phase; sampled peaks use boundary observations only.")
		}
	}
	return warnings
}
