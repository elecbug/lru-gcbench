package bench

import (
	"math"
	"sync/atomic"
	"testing"
)

func diagnosticConcurrentCase() Case {
	c := testCase()
	c.Scenario, c.Workers, c.Operations = "concurrent-compact", 3, 1001
	c.LatencySampleEvery = 17
	return c
}

func TestDiagnosticConfigurationValidation(t *testing.T) {
	invalid := []struct {
		name   string
		base   Case
		mutate func(*Case)
	}{
		{"unknown topology", testCase(), func(c *Case) { c.CacheMode = "sharded" }},
		{"independent compaction", diagnosticConcurrentCase(), func(c *Case) { c.CacheMode = "independent" }},
		{"harness reclamation", testCase(), func(c *Case) { c.CacheMode = "harness"; c.PressureReclamation = true }},
		{"harness footprint", testCase(), func(c *Case) { c.CacheMode = "harness"; c.Scenario = "footprint" }},
		{"cache observer without sampler", testCase(), func(c *Case) { c.SampleCacheStats = true }},
		{"cache observer with explicit zero sampler", testCase(), func(c *Case) { c.SampleCacheStats = true; c.SampleInterval = "0s" }},
		{"cache observer in footprint", testCase(), func(c *Case) { c.SampleCacheStats = true; c.SampleInterval = "10ms"; c.Scenario = "footprint" }},
		{"profile phase without mode", testCase(), func(c *Case) { c.ProfilePhase = "measured" }},
		{"profile rate without mode", testCase(), func(c *Case) { c.ProfileRate = 1 }},
		{"unknown profile", testCase(), func(c *Case) { c.Profile = "heap"; c.ProfilePhase = "measured" }},
		{"implicit profile phase", testCase(), func(c *Case) { c.Profile = "cpu" }},
		{"negative profile rate", testCase(), func(c *Case) { c.Profile = "allocs"; c.ProfilePhase = "measured"; c.ProfileRate = -1 }},
		{"custom CPU rate", testCase(), func(c *Case) { c.Profile = "cpu"; c.ProfilePhase = "measured"; c.ProfileRate = 100 }},
		{"nonexistent warmup", testCase(), func(c *Case) { c.Profile = "cpu"; c.ProfilePhase = "warmup"; c.WarmupOps = 0 }},
		{"wrong scenario profile", testCase(), func(c *Case) { c.Profile = "mutex"; c.ProfilePhase = "concurrent" }},
		{"wrong deletion phase", diagnosticConcurrentCase(), func(c *Case) { c.Profile = "allocs"; c.ProfilePhase = "delete_prefix" }},
		{"disabled steady compact", testCase(), func(c *Case) { c.CompactMode = "disabled" }},
		{"steady trace", testCase(), func(c *Case) { c.RequestTraceEvery = 1; c.MaxRequestSamples = c.Operations }},
		{"unknown compact mode", diagnosticConcurrentCase(), func(c *Case) { c.CompactMode = "auto" }},
		{"negative compact fraction", diagnosticConcurrentCase(), func(c *Case) { c.CompactAt = -.1 }},
		{"compact outside stream", diagnosticConcurrentCase(), func(c *Case) { c.CompactAt = 1 }},
		{"NaN compact fraction", diagnosticConcurrentCase(), func(c *Case) { c.CompactAt = math.NaN() }},
		{"infinite compact fraction", diagnosticConcurrentCase(), func(c *Case) { c.CompactAt = math.Inf(1) }},
		{"zero window", diagnosticConcurrentCase(), func(c *Case) { c.CompactWindow = "0" }},
		{"negative window", diagnosticConcurrentCase(), func(c *Case) { c.CompactWindow = "-1ms" }},
		{"oversized window", diagnosticConcurrentCase(), func(c *Case) { c.CompactWindow = "2h" }},
		{"malformed window", diagnosticConcurrentCase(), func(c *Case) { c.CompactWindow = "ten milliseconds" }},
		{"negative trace interval", diagnosticConcurrentCase(), func(c *Case) { c.RequestTraceEvery = -1 }},
		{"trace buffer without interval", diagnosticConcurrentCase(), func(c *Case) { c.MaxRequestSamples = 10 }},
		{"unbounded trace capacity", diagnosticConcurrentCase(), func(c *Case) { c.RequestTraceEvery = 1; c.MaxRequestSamples = 1000001 }},
		{"insufficient complete trace", diagnosticConcurrentCase(), func(c *Case) { c.RequestTraceEvery = 1; c.MaxRequestSamples = c.Operations - 1 }},
		{"uneven worker trace budget", diagnosticConcurrentCase(), func(c *Case) { c.Operations = 1000; c.RequestTraceEvery = 333; c.MaxRequestSamples = 3 }},
		{"trace with zero workers", diagnosticConcurrentCase(), func(c *Case) { c.Workers = 0; c.RequestTraceEvery = 1; c.MaxRequestSamples = 1001 }},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.base
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid diagnostic configuration accepted")
			}
		})
	}
	observed := testCase()
	observed.SampleCacheStats, observed.SampleInterval = true, "10ms"
	if err := observed.Validate(); err != nil {
		t.Fatalf("valid periodic cache observation rejected: %v", err)
	}

	// Verify every supported profiling phase remains reachable, including
	// prefix deletion; similarly named nonexistent phases must be rejected.
	for _, scenario := range []string{"steady", "footprint", "reclaim", "concurrent-compact"} {
		c := testCase()
		c.Scenario, c.Profile = scenario, "allocs"
		phases := []string{"fill"}
		switch scenario {
		case "steady":
			phases = append(phases, "warmup", "measured")
		case "footprint":
			phases = append(phases, "delete", "compact")
		case "reclaim":
			phases = append(phases, "delete", "compact", "recovery")
		case "concurrent-compact":
			c.LatencySampleEvery = 1
			phases = append(phases, "delete", "concurrent")
		}
		for _, phase := range phases {
			c.ProfilePhase = phase
			if err := c.Validate(); err != nil {
				t.Errorf("supported %s/%s profile rejected: %v", scenario, phase, err)
			}
		}
		if scenario != "steady" {
			c.KeyKind, c.DeleteMode, c.ProfilePhase = "prefix", "prefix", "delete_prefix"
			if err := c.Validate(); err != nil {
				t.Errorf("supported prefix deletion profile rejected: %v", err)
			}
		}
	}
}

type diagnosticCompactCache struct {
	*recordingCache
	compactCalls atomic.Int64
	requests     atomic.Int64
	firstStatsAt atomic.Int64
}

func (c *diagnosticCompactCache) Get(k string) bool {
	c.requests.Add(1)
	return c.recordingCache.Get(k)
}
func (c *diagnosticCompactCache) Compact() { c.compactCalls.Add(1) }
func (c *diagnosticCompactCache) Stats() CacheStats {
	c.firstStatsAt.CompareAndSwap(-1, c.requests.Load())
	return c.recordingCache.Stats()
}

func TestDisabledCompactRecordsMatchedTrigger(t *testing.T) {
	j := Job{Case: diagnosticConcurrentCase(), Seed: 77}
	j.Case.Workers, j.Case.Operations, j.Case.ReadPercent = 1, 1000, 100
	j.Case.CompactMode, j.Case.CompactAt = "disabled", .25
	c := &diagnosticCompactCache{recordingCache: newRecordingCache()}
	c.firstStatsAt.Store(-1)
	task := prepareMixedTask(c, j, j.Case.Operations, 9, true)
	counts, err := task.run()
	if err != nil {
		t.Fatal(err)
	}
	if counts.operations != 1000 || c.compactCalls.Load() != 0 || task.compactionExecuted {
		t.Fatal("disabled Compact executed or lost requests")
	}
	if c.firstStatsAt.Load() != 250 {
		t.Fatalf("marker triggered after %d requests, want 250", c.firstStatsAt.Load())
	}
	if task.compactionStart.Before(task.phaseStart) || task.compactionEnd.Before(task.compactionStart) || task.compactionEnd.After(task.phaseEnd) {
		t.Fatal("disabled control lost its matched trigger window")
	}
}

func TestRequestTracingPreservesWorkloadAndCompletePhase(t *testing.T) {
	for _, workers := range []int{1, 3, 7} {
		j := Job{Case: diagnosticConcurrentCase(), Seed: 77}
		j.Case.Workers, j.Case.Operations, j.Case.CompactMode = workers, 1031, "disabled"
		makeCache := func() *recordingCache {
			cache := newRecordingCache()
			// Keep all possible reads hits, independent of worker scheduling.
			for id := 0; id < j.Case.KeySpace; id++ {
				if err := cache.Put(MakeKey(uint64(id), j.Case.KeyKind), uint64(id)); err != nil {
					t.Fatal(err)
				}
			}
			return cache
		}
		plain, err := prepareMixedTask(makeCache(), j, j.Case.Operations, 9, true).run()
		if err != nil {
			t.Fatal(err)
		}
		j.Case.RequestTraceEvery, j.Case.MaxRequestSamples = 1, j.Case.Operations
		if err := j.Case.Validate(); err != nil {
			t.Fatal(err)
		}
		task := prepareMixedTask(makeCache(), j, j.Case.Operations, 9, true)
		traced, err := task.run()
		if err != nil {
			t.Fatal(err)
		}
		if plain.operations != traced.operations || plain.reads != traced.reads || plain.writes != traced.writes || plain.hits != traced.hits || plain.checksum != traced.checksum {
			t.Fatal("request tracing changed deterministic inputs or operation results")
		}
		var samples, reads, hits uint64
		for w, trace := range task.traces {
			if task.traceDropped[w] != 0 {
				t.Fatal("valid complete-phase budget dropped observations")
			}
			for _, sample := range trace {
				if sample.StartElapsedNS < task.phaseStart.Sub(task.origin).Nanoseconds() || sample.EndElapsedNS < sample.StartElapsedNS || sample.EndElapsedNS > task.phaseEnd.Sub(task.origin).Nanoseconds() {
					t.Fatal("request observation is outside its phase")
				}
				samples++
				if sample.Read {
					reads++
				}
				if sample.Hit {
					hits++
				}
			}
		}
		if samples != traced.operations || reads != traced.reads || hits != traced.hits {
			t.Fatal("full tracing lost or mislabeled requests")
		}
	}
}

func TestRequestTraceBudgetUnevenStreamsAndLargeInterval(t *testing.T) {
	for _, every := range []int{2, 333, math.MaxInt} {
		for _, seed := range []int64{1, 17, 99} {
			j := Job{Case: diagnosticConcurrentCase(), Seed: seed}
			j.Case.Operations, j.Case.RequestTraceEvery = 1000, every
			switch every {
			case 2:
				j.Case.MaxRequestSamples = 501 // 334/2 + ceil(333/2)*2.
			case 333:
				j.Case.MaxRequestSamples = 4 // ceil(334/333) + 333/333*2.
			default:
				j.Case.MaxRequestSamples = 3
			}
			if err := j.Case.Validate(); err != nil {
				t.Fatal(err)
			}
			task := prepareMixedTask(newRecordingCache(), j, j.Case.Operations, 9, true)
			if _, err := task.run(); err != nil {
				t.Fatal(err)
			}
			for w, dropped := range task.traceDropped {
				if dropped != 0 {
					t.Fatalf("worker %d dropped %d with a valid full-phase trace budget", w, dropped)
				}
			}
		}
	}
}
