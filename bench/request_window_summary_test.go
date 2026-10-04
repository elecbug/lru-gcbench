package bench

import (
	"math"
	"testing"
	"time"
)

func requestWindowTask() *mixedTask {
	origin := time.Unix(1_700_000_000, 0)
	return &mixedTask{
		origin: origin, phaseStart: origin.Add(10 * time.Millisecond), phaseEnd: origin.Add(125 * time.Millisecond),
		compactionStart: origin.Add(50 * time.Millisecond), compactionEnd: origin.Add(60 * time.Millisecond),
		compactionExecuted: true, entriesBefore: 123, entriesAfter: 124,
	}
}

func tracedRequest(startMS, endMS int64, read, hit bool) RequestObservation {
	return RequestObservation{StartElapsedNS: startMS * int64(time.Millisecond), EndElapsedNS: endMS * int64(time.Millisecond), Read: read, Hit: hit}
}

func TestRequestWindowsClipAndAssignCompletionBoundaries(t *testing.T) {
	task := requestWindowTask()
	task.traces = [][]RequestObservation{
		{tracedRequest(99, 100, false, false), tracedRequest(11, 20, true, true)},
		{tracedRequest(49, 50, false, false), tracedRequest(51, 55, true, false), tracedRequest(124, 125, true, true)},
	}
	var p Phase
	finalizeRequestTrace(&p, task, Case{RequestTraceEvery: 10})
	if p.CompactProbe == nil || !p.CompactProbe.Executed || p.CompactProbe.EntriesBefore != 123 || p.CompactProbe.EntriesAfter != 124 {
		t.Fatal("missing trigger state", p.CompactProbe)
	}
	if p.ConcurrentCompaction == nil || p.ConcurrentCompaction.DurationNS != int64(10*time.Millisecond) {
		t.Fatal("wrong Compact interval", p.ConcurrentCompaction)
	}
	for i := 1; i < len(p.RequestTrace); i++ {
		if p.RequestTrace[i-1].EndElapsedNS > p.RequestTrace[i].EndElapsedNS {
			t.Fatal("per-worker observations were not sorted")
		}
	}
	if len(p.RequestWindows) != 4 {
		t.Fatal("missing fixed/overlap windows", p.RequestWindows)
	}
	want := []struct {
		name       string
		start, end int64
		samples    uint64
		clipped    bool
		ops        float64
	}{
		{"before-trigger", 10, 50, 1, true, 250},
		{"trigger-window", 50, 100, 2, false, 400},
		{"after-trigger", 100, 125, 1, true, 400},
		{"compact-overlap", 50, 60, 1, false, 0},
	}
	for i, want := range want {
		got := p.RequestWindows[i]
		if got.Name != want.name || got.StartElapsedNS != want.start*int64(time.Millisecond) || got.EndElapsedNS != want.end*int64(time.Millisecond) || got.Samples != want.samples || got.Clipped != want.clipped || math.Abs(got.EstimatedOpsPerSecond-want.ops) > 1e-6 {
			t.Fatalf("window %d: %+v; want %+v", i, got, want)
		}
	}
	trigger := p.RequestWindows[1]
	if trigger.Reads != 1 || trigger.Writes != 1 || trigger.Hits != 0 || trigger.GetLatency.Samples != 1 || trigger.PutLatency.Samples != 1 {
		t.Fatal("request counts or latency histograms disagree", trigger)
	}
	q := trigger.GetLatency.P99
	if q == nil || q.UpperSeconds == nil || q.LowerSeconds > .004 || *q.UpperSeconds <= .004 {
		t.Fatal("4ms request fell outside its quantile bucket", q)
	}
	if p.RequestWindows[0].Hits != 1 {
		t.Fatal("read hit was lost")
	}
}

func TestCompactionOverlapIncludesCrossingRequestsOnly(t *testing.T) {
	task := requestWindowTask()
	task.traces = [][]RequestObservation{{
		tracedRequest(40, 50, true, false), // Ends at Compact start: no overlap.
		tracedRequest(60, 65, true, false), // Starts at Compact end: no overlap.
		tracedRequest(40, 110, true, true), // Completes after the fixed trigger window.
		tracedRequest(55, 80, false, false),
		tracedRequest(51, 52, true, false),
	}}
	var p Phase
	finalizeRequestTrace(&p, task, Case{RequestTraceEvery: 1, CompactWindow: "20ms"})
	w := p.RequestWindows[3]
	if w.Samples != 3 || w.Reads != 2 || w.Writes != 1 || w.Hits != 1 || w.EstimatedOpsPerSecond != 0 {
		t.Fatal("overlap used completion windows or fabricated throughput", w)
	}
	if w.GetLatency.P99 == nil || *w.GetLatency.P99.UpperSeconds <= .070 {
		t.Fatal("long crossing request was omitted from latency")
	}
}

func TestDisabledCompactStillHasMatchedTriggerWindows(t *testing.T) {
	task := requestWindowTask()
	task.compactionExecuted = false
	task.compactionEnd = task.compactionStart
	task.traces = [][]RequestObservation{{tracedRequest(51, 55, true, true)}}
	var p Phase
	finalizeRequestTrace(&p, task, Case{RequestTraceEvery: 1})
	if p.CompactProbe == nil || p.CompactProbe.Executed || p.ConcurrentCompaction != nil || len(p.RequestWindows) != 3 || p.RequestWindows[1].Samples != 1 {
		t.Fatal("disabled control lost trigger or claimed Compact", p)
	}
}

func TestDroppedOrDisabledRequestTrace(t *testing.T) {
	task := requestWindowTask()
	task.traces = [][]RequestObservation{{tracedRequest(51, 55, true, true)}}
	task.traceDropped = []uint64{2, 3}
	var p Phase
	finalizeRequestTrace(&p, task, Case{RequestTraceEvery: 10})
	if p.RequestTraceDropped != 5 {
		t.Fatal("missing trace drops")
	}
	for _, window := range p.RequestWindows {
		if window.EstimatedOpsPerSecond != 0 {
			t.Fatal("truncated trace produced a biased throughput estimate")
		}
	}
	task.traces, task.traceDropped = nil, nil
	finalizeRequestTrace(&p, task, Case{})
	if p.CompactProbe == nil || len(p.RequestTrace) != 0 || len(p.RequestWindows) != 0 || p.RequestTraceDropped != 0 {
		t.Fatal("disabled tracing did not preserve probe only", p)
	}
	finalizeRequestTrace(&p, task, Case{RequestTraceEvery: 10, CompactWindow: "1s"})
	if len(p.RequestWindows) != 4 || !p.RequestWindows[2].Clipped || p.RequestWindows[2].StartElapsedNS != p.RequestWindows[2].EndElapsedNS {
		t.Fatal("fully clipped window must have zero duration", p.RequestWindows)
	}
}
