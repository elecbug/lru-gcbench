package bench

import (
	"fmt"
	"sort"
	"strings"
)

type chartBand struct {
	Name, X, Width, Start, End string
	LabelY                     int
}
type chartTick struct{ X, Label string }
type memoryChart struct {
	Label, HeapPath, RSSPath, MaxMiB string
	Observations, Plotted, Periodic  int
	Dropped                          uint64
	Phases, Events                   []chartBand
	Ticks                            []chartTick
}

// chartSamples bounds rendered size without altering recorded data. Each bucket
// retains heap and available RSS extrema; global endpoints are always retained.
func chartSamples(samples []RuntimeSample, limit int) []RuntimeSample {
	if len(samples) <= limit || limit < 6 {
		return samples
	}
	buckets := (limit - 2) / 4
	indexes := map[int]bool{0: true, len(samples) - 1: true}
	for bucket := 0; bucket < buckets; bucket++ {
		start := 1 + (len(samples)-2)*bucket/buckets
		end := 1 + (len(samples)-2)*(bucket+1)/buckets
		if start == end {
			continue
		}
		minHeap, maxHeap, minRSS, maxRSS := start, start, -1, -1
		for i := start; i < end; i++ {
			if samples[i].HeapObjectsBytes < samples[minHeap].HeapObjectsBytes {
				minHeap = i
			}
			if samples[i].HeapObjectsBytes > samples[maxHeap].HeapObjectsBytes {
				maxHeap = i
			}
			if samples[i].RSSAvailable {
				if minRSS < 0 || samples[i].RSSBytes < samples[minRSS].RSSBytes {
					minRSS = i
				}
				if maxRSS < 0 || samples[i].RSSBytes > samples[maxRSS].RSSBytes {
					maxRSS = i
				}
			}
		}
		indexes[minHeap], indexes[maxHeap] = true, true
		if minRSS >= 0 {
			indexes[minRSS], indexes[maxRSS] = true, true
		}
	}
	keys := make([]int, 0, len(indexes))
	for i := range indexes {
		keys = append(keys, i)
	}
	sort.Ints(keys)
	out := make([]RuntimeSample, 0, len(keys))
	for _, i := range keys {
		out = append(out, samples[i])
	}
	return out
}

func makeMemoryChart(r Result) memoryChart {
	c := memoryChart{Label: r.Job.ID + " — " + r.Job.Backend + " / " + r.Job.Case.Name + " / " + r.Job.Runtime.Name + fmt.Sprintf(" / repeat %d", r.Job.Repeat), Periodic: len(r.Samples), Dropped: r.DroppedSamples}
	samples := make([]RuntimeSample, 0, len(r.Samples)+len(r.Phases)*3+1)
	samples = append(samples, r.Baseline.Runtime)
	samples = append(samples, r.Samples...)
	for _, p := range r.Phases {
		samples = append(samples, p.Start.Runtime, p.End.Runtime)
		if p.PostForcedGC != nil {
			samples = append(samples, p.PostForcedGC.Runtime)
		}
	}
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].ElapsedNS < samples[j].ElapsedNS })
	start, end := samples[0].ElapsedNS, samples[len(samples)-1].ElapsedNS
	span := float64(end) - float64(start)
	if span <= 0 {
		span = 1
	}
	var peak uint64 = 1
	for _, s := range samples {
		peak = max(peak, s.HeapObjectsBytes)
		if s.RSSAvailable {
			peak = max(peak, s.RSSBytes)
		}
	}
	x := func(ns int64) float64 { return 60 + 880*max(0, min(1, (float64(ns)-float64(start))/span)) }
	y := func(bytes uint64) float64 { return 200 - 176*float64(bytes)/float64(peak) }
	pos := func(v float64) string { return fmt.Sprintf("%.3f", v) }
	c.MaxMiB = fmt.Sprintf("%.2f", float64(peak)/(1<<20))
	for i := 0; i <= 4; i++ {
		c.Ticks = append(c.Ticks, chartTick{X: pos(60 + 880*float64(i)/4), Label: fmt.Sprintf("%.3f", (float64(start)+span*float64(i)/4)/1e9)})
	}
	band := func(name string, from, to int64, lane int) chartBand {
		return chartBand{Name: name, X: pos(x(from)), Width: pos(max(0, x(to)-x(from))), Start: pos(float64(from) / 1e9), End: pos(float64(to) / 1e9), LabelY: 235 + lane*15}
	}
	for i, p := range r.Phases {
		c.Phases = append(c.Phases, band(p.Name, p.Start.Runtime.ElapsedNS, p.End.Runtime.ElapsedNS, i%3))
		if event := p.ConcurrentCompaction; event != nil {
			c.Events = append(c.Events, band("concurrent Compact", event.StartElapsedNS, event.EndElapsedNS, 0))
		}
	}
	c.Observations = len(samples)
	samples = chartSamples(samples, 600)
	c.Plotted = len(samples)
	var heap, rss strings.Builder
	rssStarted := false
	for i, s := range samples {
		command := "L"
		if i == 0 {
			command = "M"
		}
		fmt.Fprintf(&heap, "%s%s,%s ", command, pos(x(s.ElapsedNS)), pos(y(s.HeapObjectsBytes)))
		if !s.RSSAvailable {
			rssStarted = false
			continue
		}
		command = "L"
		if !rssStarted {
			command = "M"
			rssStarted = true
		}
		fmt.Fprintf(&rss, "%s%s,%s ", command, pos(x(s.ElapsedNS)), pos(y(s.RSSBytes)))
	}
	c.HeapPath, c.RSSPath = heap.String(), rss.String()
	return c
}
