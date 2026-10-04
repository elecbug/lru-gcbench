package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"time"
)

func WorkerMain(factory Factory, caps Capabilities) int {
	if len(os.Args) == 2 && os.Args[1] == "--describe" {
		if err := json.NewEncoder(os.Stdout).Encode(caps); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "worker accepts --describe or one Job JSON on stdin")
		return 2
	}
	var j Job
	if err := DecodeStrict(os.Stdin, &j); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if !slices.Contains(caps.Backends, j.Backend) {
		fmt.Fprintln(os.Stderr, "unsupported backend:", j.Backend)
		return 2
	}
	if err := j.Case.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if j.Runtime.GOMAXPROCS < 1 || j.Runtime.GOGC < -1 {
		fmt.Fprintln(os.Stderr, "invalid runtime settings")
		return 2
	}
	limit, err := MemoryLimit(j.Runtime.GOMEMLIMIT)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	runtime.GOMAXPROCS(j.Runtime.GOMAXPROCS)
	debug.SetGCPercent(j.Runtime.GOGC)
	debug.SetMemoryLimit(limit)
	r, err := RunWorker(j, factory, caps.Provenance)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err = json.NewEncoder(os.Stdout).Encode(r); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func RunWorker(j Job, factory Factory, source Provenance) (Result, error) {
	// RunWorker is process-global by design; never invoke it concurrently in one process.
	if j.Runtime.GOMAXPROCS < 1 || j.Runtime.GOGC < -1 {
		return Result{}, fmt.Errorf("invalid runtime settings")
	}
	limit, err := MemoryLimit(j.Runtime.GOMEMLIMIT)
	if err != nil {
		return Result{}, err
	}
	oldProcs := runtime.GOMAXPROCS(j.Runtime.GOMAXPROCS)
	oldGC := debug.SetGCPercent(j.Runtime.GOGC)
	oldLimit := debug.SetMemoryLimit(limit)
	defer runtime.GOMAXPROCS(oldProcs)
	defer debug.SetGCPercent(oldGC)
	defer debug.SetMemoryLimit(oldLimit)
	r := Result{SchemaVersion: 1, Job: j, PID: os.Getpid(), StartedAt: time.Now().UTC(), Provenance: source, Environment: environment(j)}
	if err := j.Case.Validate(); err != nil {
		return r, err
	}
	profiler, err := prepareProfiler(j.Case)
	if err != nil {
		return r, err
	}
	defer profiler.finish()
	c, err := newCollector()
	if err != nil {
		return r, err
	}
	defer c.close()
	interval, _ := time.ParseDuration(j.Case.SampleInterval)
	if j.Case.Scenario == "footprint" {
		interval = 0
	}
	t := newTracer(c, interval, j.Case.MaxSamples, j.Case.RetainSampleBuffer)
	defer func() { runtime.KeepAlive(t.points) }()
	r.Phases = make([]Phase, 0, 5)
	var counts [5]opCounts
	// Static metadata and large trace buffers are live before this single baseline GC.
	runtime.GC()
	baseline, _ := c.read(false)
	r.Baseline = Snapshot{Runtime: baseline}
	cache, err := newCacheForJob(j, factory)
	if err != nil {
		return r, err
	}
	if j.Case.SampleCacheStats {
		t.startWithCache(c, interval, cache)
	} else {
		t.start(c, interval)
	}
	capacity := j.Case.Capacity * cacheMultiplicity(j.Case)
	synthetic := j.Case.CacheMode == "harness"
	stopped := false
	defer func() {
		if !stopped {
			t.finish()
		}
		runtime.KeepAlive(cache)
	}()
	snapshot := func() (Snapshot, Histogram) {
		m, h := c.read(true)
		return Snapshot{Runtime: m, Cache: cache.Stats()}, h
	}
	var pauseStarts [5]Histogram
	var pauseEnds [5]Histogram
	run := func(name string, fn func() (opCounts, error), forcedGC bool) error {
		idx := len(r.Phases)
		if err := profiler.startPhase(name); err != nil {
			return fmt.Errorf("%s profile: %w", name, err)
		}
		start, a := snapshot()
		then := time.Now()
		ops, err := fn()
		duration := time.Since(then).Nanoseconds()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		end, b := snapshot()
		if err := profiler.finishPhase(name); err != nil {
			return fmt.Errorf("%s profile: %w", name, err)
		}
		// Cheap boundary checks: reject invalid outcomes instead of publishing fast-but-wrong results.
		if !synthetic && (end.Cache.CurrentSize != uint64(end.Cache.Entries) || end.Cache.MaxSize != uint64(capacity)) {
			return fmt.Errorf("%s: unexpected unit-weight capacity accounting", name)
		}
		if !synthetic && !j.Case.PressureReclamation && name == "fill" && end.Cache.Entries != capacity {
			return fmt.Errorf("fill: retained %d entries, expected %d", end.Cache.Entries, capacity)
		}
		if !j.Case.PressureReclamation && (name == "delete" || name == "delete_prefix") {
			want := j.Case.Capacity - deletedEntryCount(j.Case)
			if end.Cache.Entries != want {
				return fmt.Errorf("delete: retained %d entries, expected %d", end.Cache.Entries, want)
			}
		}
		if name == "compact" && end.Cache.Entries != start.Cache.Entries {
			return fmt.Errorf("Compact changed entry count: %d -> %d", start.Cache.Entries, end.Cache.Entries)
		}
		if !synthetic && (name == "warmup" || name == "measured" || name == "recovery" || name == "concurrent") {
			hits := sub(end.Cache.GetHits, start.Cache.GetHits)
			misses := sub(end.Cache.GetMisses, start.Cache.GetMisses)
			if hits != ops.hits || hits+misses != ops.reads {
				return fmt.Errorf("%s: cache and harness lookup counters disagree", name)
			}
		}
		p := Phase{Name: name, DurationNS: duration, Operations: ops.operations, Reads: ops.reads, Writes: ops.writes, Hits: ops.hits,
			WorkloadChecksum: ops.checksum, Start: start, End: end}
		if forcedGC {
			runtime.GC()
			post, _ := snapshot()
			p.PostForcedGC = &post
		}
		pauseStarts[idx] = a
		pauseEnds[idx] = b
		counts[idx] = ops
		r.Phases = append(r.Phases, p)
		return nil
	}
	fillCopies := cacheMultiplicity(j.Case)
	if synthetic {
		fillCopies = j.Case.Workers
	}
	fill := func() (opCounts, error) {
		var o opCounts
		for i := 0; i < j.Case.Capacity; i++ {
			if err := cache.Put(MakeKey(uint64(i), j.Case.KeyKind), uint64(i)); err != nil {
				return o, err
			}
			o.operations += uint64(fillCopies)
			o.writes += uint64(fillCopies)
		}
		return o, nil
	}
	deletePhase := "delete"
	var prefixCache PrefixCache
	if j.Case.DeleteMode == "prefix" {
		var ok bool
		prefixCache, ok = cache.(PrefixCache)
		if !ok {
			return r, fmt.Errorf("backend does not support DeletePrefix")
		}
		deletePhase = "delete_prefix"
	}
	deleteEntries := func() (opCounts, error) {
		var o opCounts
		if prefixCache != nil {
			for group := 0; group < int(j.Case.DeleteFraction*16); group++ {
				prefixCache.DeletePrefix("tenant/" + string("0123456789abcdef"[group]) + "/objects/")
				o.operations++
			}
			return o, nil
		}
		n := deletedEntryCount(j.Case)
		for i := 0; i < n; i++ {
			cache.Delete(MakeKey(uint64(i), j.Case.KeyKind))
			o.operations++
		}
		return o, nil
	}
	compact := func() (opCounts, error) { cache.Compact(); return opCounts{operations: 1}, nil }
	static := j.Case.Scenario == "footprint"
	if err = run("fill", fill, static); err != nil {
		return r, err
	}
	var compactTask *mixedTask
	compactPhase := -1
	switch j.Case.Scenario {
	case "footprint", "reclaim":
		if err = run(deletePhase, deleteEntries, static); err != nil {
			return r, err
		}
		if err = run("compact", compact, static); err != nil {
			return r, err
		}
		if !static {
			task := prepareMixed(cache, j, j.Case.Operations, 0x41)
			task.origin = c.origin
			if err = run("recovery", task.run, false); err != nil {
				return r, err
			}
		}
	case "concurrent-compact":
		if err = run(deletePhase, deleteEntries, false); err != nil {
			return r, err
		}
		task := prepareMixedTask(cache, j, j.Case.Operations, 0x41, true)
		task.origin = c.origin
		if err = run("concurrent", task.run, false); err != nil {
			return r, err
		}
		compactTask, compactPhase = task, len(r.Phases)-1
		r.Warnings = append(r.Warnings, "The Compact probe is triggered at compact_at (default 0.5) of worker 0's operation stream. Enabled and disabled probes observe cache size at both boundaries; their scheduling and observation work is included in the phase.")

	case "steady":
		if j.Case.WarmupOps > 0 {
			task := prepareMixed(cache, j, j.Case.WarmupOps, 0x17)
			task.origin = c.origin
			if err = run("warmup", task.run, false); err != nil {
				return r, err
			}
		}
		task := prepareMixed(cache, j, j.Case.Operations, 0x41)
		task.origin = c.origin
		if err = run("measured", task.run, false); err != nil {
			return r, err
		}
	}
	t.finish()
	stopped = true
	r.Samples = t.points[:t.used]
	r.DroppedSamples = t.dropped
	if err := profiler.finish(); err != nil {
		return r, err
	}
	r.Profiling = profiler.artifacts()
	if r.Profiling != nil {
		r.Warnings = append(r.Warnings, "PROFILED DIAGNOSTIC RUN: profiling and profile boundary work perturb runtime state; exclude this run from unprofiled performance comparisons.")
	}
	if compactTask != nil {
		finalizeRequestTrace(&r.Phases[compactPhase], compactTask, j.Case)
		if j.Case.RequestTraceEvery > 0 {
			r.Warnings = append(r.Warnings, "Request windows use sparse, timestamped adapter calls and fixed elapsed-time intervals around the probe. Window throughput estimates sampled completions; overlapping-call latency has no throughput estimate. Clipped or sparsely sampled windows and individual stalls require inspecting the raw trace.")
		}
	}
	// Delay histogram formatting/report allocations until ALL measurement phases end.
	for i := range r.Phases {
		d, err := histogramDelta(pauseStarts[i], pauseEnds[i])
		if err != nil {
			return r, err
		}
		r.Phases[i].GCPauses = d
		r.Phases[i].GetLatency = counts[i].get.summary()
		r.Phases[i].PutLatency = counts[i].put.summary()
	}
	r.Warnings = append(r.Warnings, "Throughput includes on-demand key generation, payload creation, PRNG and harness bookkeeping; it is not a cache-only microbenchmark.")
	if synthetic {
		r.Warnings = append(r.Warnings, "SYNTHETIC HARNESS CONTROL: the selected backend is bypassed. Per-worker adapters always miss and retain only the latest payload; cache retention, hit rate and cache counters are not comparable to real caches.")
	} else if j.Case.CacheMode == "independent" {
		r.Warnings = append(r.Warnings, "Independent topology gives each worker a complete cache with the configured capacity and keyspace. Total capacity and fill operations multiply by workers; retained memory is not matched to the shared-cache condition.")
	}
	if source.Kind != "upstream-checkout" {
		r.Warnings = append(r.Warnings, "REFERENCE VALIDATION ONLY: these are not google/go-lru measurements.")
	}
	if j.Case.Workers > 1 {
		r.Warnings = append(r.Warnings, "Per-worker operation streams are reproducible; OS scheduling and shared-cache interleaving are not.")
	}
	if j.Case.LatencySampleEvery > 0 {
		r.Warnings = append(r.Warnings, "Sampled request latency includes adapter/payload work but excludes key generation; quantiles are histogram bucket intervals.")
	}
	if t.dropped > 0 {
		r.Warnings = append(r.Warnings, "Trace buffer filled: later periodic samples were dropped. Increase max_samples or the sampling interval.")
	}
	if interval > 0 {
		r.Warnings = append(r.Warnings, "Periodic runtime sampling can miss transient peaks; RSS is a process-wide Linux-only observation.")
		if j.Case.SampleCacheStats {
			r.Warnings = append(r.Warnings, "Periodic cache Stats observation is opt-in and may acquire cache locks. Cache observation completion has a separate timestamp from the runtime sample; observer work can affect throughput and contention.")
		}
	}
	runtime.KeepAlive(cache)
	return r, nil
}

// Prefix groups are assigned by ID modulo 16. This exact count also handles
// capacities that are not divisible by the number of tenant groups.
func deletedEntryCount(c Case) int {
	if c.DeleteMode == "prefix" {
		groups := int(c.DeleteFraction * 16)
		return (c.Capacity/16)*groups + min(c.Capacity%16, groups)
	}
	return int(float64(c.Capacity) * c.DeleteFraction)
}
