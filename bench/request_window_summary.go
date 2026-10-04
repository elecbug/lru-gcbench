package bench

import (
	"sort"
	"time"
)

// finalizeRequestTrace allocates summaries only after timed workloads finish.
// Fixed windows group completions, while compact-overlap groups requests whose
// execution intervals overlap Compact. Neither is a population tail estimate.
func finalizeRequestTrace(p *Phase, task *mixedTask, c Case) {
	trigger := task.compactionStart.Sub(task.origin).Nanoseconds()
	compactEnd := task.compactionEnd.Sub(task.origin).Nanoseconds()
	p.CompactProbe = &CompactionProbe{
		Executed: task.compactionExecuted, TriggerElapsedNS: trigger, EndElapsedNS: compactEnd,
		EntriesBefore: task.entriesBefore, EntriesAfter: task.entriesAfter,
	}
	p.ConcurrentCompaction = nil
	if task.compactionExecuted {
		p.ConcurrentCompaction = &CompactionWindow{StartElapsedNS: trigger, EndElapsedNS: compactEnd, DurationNS: compactEnd - trigger}
	}
	var total int
	for _, trace := range task.traces {
		total += len(trace)
	}
	p.RequestTrace = nil
	if total > 0 {
		p.RequestTrace = make([]RequestObservation, 0, total)
		for _, trace := range task.traces {
			p.RequestTrace = append(p.RequestTrace, trace...)
		}
		sort.SliceStable(p.RequestTrace, func(i, j int) bool {
			if p.RequestTrace[i].EndElapsedNS == p.RequestTrace[j].EndElapsedNS {
				return p.RequestTrace[i].StartElapsedNS < p.RequestTrace[j].StartElapsedNS
			}
			return p.RequestTrace[i].EndElapsedNS < p.RequestTrace[j].EndElapsedNS
		})
	}
	p.RequestTraceDropped = 0
	for _, dropped := range task.traceDropped {
		p.RequestTraceDropped += dropped
	}
	p.RequestWindows = nil
	if c.RequestTraceEvery <= 0 {
		return
	}
	width, err := time.ParseDuration(c.CompactWindow)
	if err != nil || width <= 0 {
		width = 50 * time.Millisecond
	}
	phaseStart := task.phaseStart.Sub(task.origin).Nanoseconds()
	phaseEnd := max(phaseStart, task.phaseEnd.Sub(task.origin).Nanoseconds())
	clip := func(value int64) int64 { return min(max(value, phaseStart), phaseEnd) }
	window := func(name string, start, end int64, overlap bool) {
		w := RequestWindowSummary{Name: name, StartElapsedNS: clip(start), EndElapsedNS: clip(end)}
		w.Clipped = w.StartElapsedNS != start || w.EndElapsedNS != end
		var get, put latencyHistogram
		for _, request := range p.RequestTrace {
			if w.EndElapsedNS <= w.StartElapsedNS {
				break
			}
			selected := request.EndElapsedNS >= w.StartElapsedNS && request.EndElapsedNS < w.EndElapsedNS
			if overlap {
				selected = request.StartElapsedNS < w.EndElapsedNS && request.EndElapsedNS > w.StartElapsedNS
			}
			if !selected {
				continue
			}
			w.Samples++
			latency := uint64(request.EndElapsedNS - request.StartElapsedNS)
			if request.Read {
				w.Reads++
				if request.Hit {
					w.Hits++
				}
				get.add(latency)
			} else {
				w.Writes++
				put.add(latency)
			}
		}
		w.GetLatency, w.PutLatency = get.summary(), put.summary()
		if duration := w.EndElapsedNS - w.StartElapsedNS; duration > 0 && !overlap && p.RequestTraceDropped == 0 {
			w.EstimatedOpsPerSecond = float64(w.Samples) * float64(c.RequestTraceEvery) * 1e9 / float64(duration)
		}
		p.RequestWindows = append(p.RequestWindows, w)
	}
	window("before-trigger", trigger-int64(width), trigger, false)
	window("trigger-window", trigger, trigger+int64(width), false)
	window("after-trigger", trigger+int64(width), trigger+2*int64(width), false)
	if task.compactionExecuted {
		window("compact-overlap", trigger, compactEnd, true)
	}
}
