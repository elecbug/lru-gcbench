# Measurement protocol and limitations

## Experimental unit and provenance

One fresh OS worker process is the experimental unit. A suite runs workers sequentially and shuffles job order with a PRNG separate from the workload generator. Repetition index `r` uses `seed+r` across backends and runtime profiles. Repetitions therefore include execution variability and changes in the deterministic input stream; they do not replay one identical trace. Shared-cache interleavings remain subject to scheduling.

`run-paired` uses the same shuffled job list for baseline and candidate. It runs both versions of each job consecutively and alternates their order across pairs; the config seed determines which version runs first in the initial pair. Every invocation is a fresh process. `paired.json` records the schedule, and each side has its own manifest and raw results. Comparison is generated only when both suites finish without failed jobs and integrity validation succeeds. This reduces systematic execution-order bias without controlling host load, CPU frequency or thermal state.

The builder fingerprints `.go`, `.tmpl`, `go.mod` and `go.sum` inputs, excluding `.git`, `vendor`, `bin` and `results`. It records target commit/dirty state when available, the target and harness source hashes, and the build timestamp. The controller records the worker executable SHA256. The checkout must remain stable during compilation. This is a compilation-input fingerprint, not a hash of every repository byte; future `go:embed` assets would require extending the policy. Compilation uses `-mod=mod`, so vendor directories are not used.

Every run saves the resolved configuration as `config.json`, the actual controller executable/hash, arguments and working directory in `command.json`, progress in `run.log`, and a `reproduce.sh` script before worker execution. These records also remain on failed attempts. `run-paired` records these files at the pair root and in each side; only the root script replays the alternating schedule. Reproduction requires a fresh output path and the recorded binaries/configuration, or explicit binary-path overrides described in the README. No ambient environment dump is stored, and replay does not reconstruct the original machine or host load.

An API-fixture test checks generated code without executing the upstream implementation. The reference worker uses a distinct backend and provenance kind; it never represents MapCache, RadixCache or ArenaRadixCache. See [the validation record](../validation/VALIDATION.md) for the separation between these checks and actual-backend runs.

## Allocation and timing boundaries

Environment metadata, the bounded trace buffer and main result capacity are allocated before baseline GC. The trace buffer is touched before that collection. Only then is the target cache constructed. No global key/value corpus is retained. Values stay typed through the generated adapter.

Mixed worker goroutines, PRNG state and result buffers are prepared before the phase's start snapshot. Phase timing includes start-barrier release, workload execution, worker completion and fixed-size reduction. The operation loop includes key generation, random selection, cache/payload work and bookkeeping. These are end-to-end workload metrics. Boundary runtime sampling and Cache.Stats are outside the operation timer; counter windows span those snapshots and are slightly wider than the timer.

Optional request timing surrounds the adapter call and limited outcome bookkeeping, excluding key creation and random selection. Put timing includes allocation and touching of a new payload. Each worker samples every Nth request from a seed-derived offset; instrumentation does not change its workload PRNG stream. Request histogram buckets have eight intervals per factor of two. Reported p99 values are bucket bounds, not exact population quantiles.

Mixed workloads are closed-loop: a worker starts its next request only after its previous request completes. `workers` fixes concurrency, not offered requests per second. Sampled latencies describe those completed calls; they exclude an external arrival queue and cannot establish a service-level p99 under a fixed arrival rate. A stalled worker also starts fewer requests during the stall. Use a separate arrival-rate-controlled load generator when that is the question.

`examples/calibration.json` has five arms under the same workload and runtime settings. The first disables sampling, timing and sample-buffer retention. The remaining four set `retain_sample_buffer=true` and keep the same `max_samples`: buffer only, buffer plus runtime sampling, buffer plus request timing, and both. The retained buffer is allocated and touched before baseline GC even when `sample_interval="0"`. Buffer-only versus no-buffer observes the effect of retained instrumentation memory; runtime-only or timing-only versus buffer-only separates the enabled activities while holding buffer capacity fixed. Both-enabled observes their combined effect, which need not be additive. Fixed request histograms and other execution structures remain part of the harness. These controls do not establish zero observer effects, and faster instrumented throughput does not establish a speedup. Instrumentation is part of configuration identity; different arms remain separate report groups. The historical 2026-10-04 study used the earlier two-arm configuration saved with that run, not this revised protocol.

Histogram formatting and bulk result serialization occur after measurement phases. Boundary histogram copies and task objects still occupy process memory. Post-GC heap minus baseline is an approximate incremental process footprint; dividing by retained entries does not make it a precise cache-owned byte count.

## Scenarios and cache semantics

Capacity uses the default unit weight, so it counts entries. There is no custom WithWeigher. Entry capacity, Go's soft memory limit and OS/container memory limits are distinct.

Fills insert logical IDs `0..capacity-1`. Mixed reads and writes independently choose uniform keys from `[0,key_space)`; the default key space is twice capacity. Read misses do not insert. Writes construct a fresh value whether they insert or replace. Scalar values are pointer-free 16-byte values; byte values allocate and touch an independent slice on each Put.

`read_percent=90` means 90% of requests are reads; it does not target a 90% hit rate. With uniform accesses, a full cache and a key space twice its capacity, a hit rate near 50% is expected. Pressure shedding and transient retention can lower it. Report hit rate and retained entries alongside throughput and memory, especially when pressure reclamation differs.

Flat keys contain 16 ASCII hex bytes. Prefix keys use `tenant/<hex-digit>/objects/<16-hex-digits>` and group logical IDs by `id & 15`. Comparing flat and prefix keys changes both length and prefix sharing.

| Scenario | Phases and GC policy |
|---|---|
| `footprint` | Baseline GC, construct, fill, delete, Compact. Each operation phase is followed by a forced GC stored outside its timed/counter window. No periodic sampler, pressure reclamation or request timing; one worker. `warmup_ops` and `operations` are unused. |
| `steady` | Baseline GC, construct, fill, optional warmup, measured mixed workload. No forced GC after construction. `operations` is the measured workload size. |
| `reclaim` | Baseline GC, construct, fill, delete, sequential Compact, mixed recovery. Natural GC after construction. `operations` is recovery size; `warmup_ops` is unused. |
| `concurrent-compact` | Baseline GC, construct, fill, delete, mixed requests plus one explicit Compact. Natural GC after construction. Requires `operations >= 2*workers` and `latency_sample_every > 0`; `compact_mode="disabled"` keeps the trigger without calling Compact; `warmup_ops` is unused. |

For deletion scenarios, `delete_mode` defaults to `keys`: individual Delete calls remove the first `floor(capacity*delete_fraction)` logical IDs. With `delete_mode="prefix"`, one DeletePrefix call removes each selected tenant prefix in order from `0` upward. This requires prefix keys and a delete fraction that is a multiple of 1/16. The number of selected groups is `16*delete_fraction`; capacities not divisible by 16 can give a deleted-entry fraction slightly different from the requested fraction. Reported `delete`/`delete_prefix` operations count API calls, not removed entries. Pressure shedding may already have removed selected entries; deletion attempts still count.

The concurrent scenario triggers a compactor goroutine at `compact_at` of worker 0's request stream (default 0.5) and waits for both requests and the probe to finish. `compact_mode="disabled"` records the same trigger without calling Compact. `compact_probe` records the trigger and observed entries before/after; `concurrent_compaction` exists only for an actual call. These cache-stat observations are outside the Compact duration but can acquire locks while requests continue. Read-only matched controls keep retained state fixed after deletion, avoiding recovery writes changing the state to compact.

Optional `request_trace_every` sampling retains call start/end times, operation type and outcome up to `max_request_samples` per phase; dropped counts are explicit. `compact_window` defines fixed before-trigger, trigger-window and after-trigger intervals. These summaries bin sampled calls by completion time; estimated throughput scales their counts by the trace interval and divides by the actual window duration. Boundary-clipped windows are flagged. Missing samples and sparse sampling limit those estimates; dropped traces omit estimated rates. `compact-overlap` instead selects sampled calls whose call intervals intersect the actual Compact interval, including calls that wait across it; it reports latency but no throughput rate. See `request-trace.csv`, `compact-observations.csv` and raw phase windows. Whole-phase p99 still includes requests outside the Compact interval. These are closed-loop measurements without an external arrival queue.

The default pressure probe is retained. Disabling pressure reclamation sets both reclamation thresholds to `math.MaxFloat64`, avoiding a custom PressureFunc and its different execution path. Automatic structural/slack compaction may still occur. `examples/pressure.json` matches workload and runtime settings across pressure on/off cases; evaluate eviction counts, retained entries and hit rate together with memory.

Fills and deletion are single-threaded. Explicit Compact is sequential except in `concurrent-compact`; `workers` controls mixed-request concurrency. A sequential Compact phase checks entry-count preservation. The concurrent phase cannot apply that check because mixed requests can change retention while Compact runs.

## Runtime and normalized metrics

| Output | Source or denominator | Interpretation |
|---|---|---|
| `heap_objects_bytes` | `/memory/classes/heap/objects:bytes` | Object-space accounting; can contain uncollected garbage |
| `heap_live_bytes` | `/gc/heap/live:bytes` | Most recent GC live-heap estimate |
| `heap_goal_bytes` | `/gc/heap/goal:bytes` | Runtime GC heap goal |
| `heap_scan_bytes` | `/gc/scan/heap:bytes` | Scannable heap, not scan time or cache-owned memory |
| `runtime_managed_bytes` | Total minus heap/released | Soft-limit accounting quantity |
| `pressure_active_bytes` | Total minus heap/released minus heap/free | Active-memory quantity used by the inspected library probe |
| `gc_cpu_seconds` | `/cpu/classes/gc/total:cpu-seconds` | Cumulative runtime GC CPU estimate |
| `gc_assist_cpu_seconds` | `/cpu/classes/gc/mark/assist:cpu-seconds` | Cumulative assist CPU estimate |
| `gc_cycles` / `gc_forced_cycles` | `/gc/cycles/{total,forced}:gc-cycles` | Cumulative collection counts |
| `gc_pauses_delta` | `/sched/pauses/total/gc:seconds` | Delta of cumulative GC STW pause histograms |
| `rss_bytes` | Linux `/proc/self/statm` resident pages × page size | Process RSS, independent of Go heap attribution |
| `allocated_bytes_per_op` | Allocated byte delta / API or mixed-request count | Workload `B/op`, including harness allocations in the window |
| `allocated_objects_per_op` | Allocated object delta / operation count | Workload `allocs/op` |
| `gc_cpu_ns_per_op` | GC CPU delta × 1e9 / operation count | Present only in non-footprint phases with positive operations and no forced cycles |
| `post_gc_heap_delta_per_entry_bytes` | (Post-GC heap − baseline heap) / retained entries | Present only for a positive delta and positive retained-entry count |
| `read_hits_per_s` / `read_misses_per_s` | Observed hits or misses × 1e9 / phase duration | Compute per run, then aggregate; do not multiply medians of throughput and hit rate |
| `pressure_tier1_compactions_delta` / `pressure_tier2_compactions_delta` | Separate cache counter deltas | Keep pressure tiers distinct from automatic slack and explicit compaction |
| `arena_live_nodes_end` / `arena_free_nodes_end` / `arena_unallocated_capacity_end` | Arena cache stats at phase end | Structural occupancy, not cache-owned byte accounting |
| `concurrent_compaction_duration_ms` | Explicit Compact call duration | Present for the concurrent scenario, independent of whole-phase duration |

Per-op allocation metrics require positive operation counts. Fill counts Puts, delete counts Delete/DeletePrefix calls, sequential Compact counts one call, and mixed phases count Gets/Puts. These denominators make unlike phases unsuitable for a direct per-op ranking.

`gc_fraction_of_available_cpu` uses the runtime's available CPU-time delta, not an OS process CPU reading. Required missing runtime metrics fail explicitly rather than silently changing metric meaning. Histogram counts are deep-copied; counter regression or changed bucket layouts cause errors. Infinity bounds are serialized as strings; an unbounded upper quantile bound is null. No events means no quantile.

## Sample adequacy and median intervals

Reports include request sample counts (`get_latency_samples`, `put_latency_samples`), GC pause events (`gc_pause_events`), periodic sample counts, RSS sample counts and `periodic_max_gap_ms`. The gap includes phase boundaries. `job_dropped_samples` applies to the whole job and is repeated across its phase summaries.

Quality warnings flag:

- Fewer than 10 repetitions in a group.
- Measured, recovery or concurrent request phases shorter than one second.
- Enabled request timing with fewer than 1,000 samples for an operation type that occurred.
- Fewer than 100 GC pause events, or no observed GC cycles in a natural-GC workload window.
- Dropped periodic samples, or a phase with no retained periodic observations despite sampling being enabled.
- Mixed environment fingerprints within a summary group.

These thresholds are heuristics, not evidence that a result above them has sufficient precision. Increase operations to capture more runtime behavior and inspect raw counts. A short phase may finish before any GC cycle; absent pause data does not establish zero GC cost.

For performance studies, run a short pilot for every backend, then choose one fixed operation count that gives the fastest backend roughly 5–10 seconds of measured work. Use that same count across the comparison, and retain pilot results separately from the final repetitions. Do not stop each backend after a different number of operations to achieve equal durations: that changes its input stream and allocation volume. Fix the repetition count before examining final rankings; the supplied performance examples use 10 independent worker processes per group.

At a sampling interval of 128 requests, 2,000,000 requests and 10% writes produce about 1,563 Put observations per job, on average. Only about 16 observations then lie in the top 1% of the sampled distribution. Ten repetitions provide repeated estimates; they do not turn each run's p99 into a precise tail measurement. Increase operations or sample more frequently if tail latency is central, and recalibrate instrumentation when sampling settings change.

Each metric reports count, median, minimum and maximum. With at least six observations, `median_ci` provides an exact binomial/order-statistic interval whose coverage is at least 95%; achieved coverage is stored with the bounds. Fewer observations omit the interval because a finite interval of this kind cannot reach that coverage. The stated coverage assumes independent observations from the same continuous distribution; ties can make the interval conservative. Changing seeds, host drift, shared resource limits and consecutive A/B execution must be considered when applying that assumption. These intervals describe each group's median, not the difference between versions. They are not automatic significance or winner labels.

## Time series and HTML charts

By default the periodic sampler reads runtime metrics and Linux RSS without taking Cache.Stats locks. `sample_cache_stats=true` adds opt-in retention and pressure-counter observations. These may acquire cache locks, delay sampling and perturb contention, so they require a matched observer-off control when interpreting performance. The cache observation has its own `cache_elapsed_ns` timestamp; it is not atomic with runtime/RSS readings and can lie in a later phase if it waited for a lock. Empty cache cells in `samples.csv` mean no cache observation, not zero entries. Runtime, RSS and cache readings are consecutive observations.

The bounded buffer retains its first `max_samples` points and counts later drops. Phase endpoints remain available. Sampled peaks include retained periodic samples and boundary observations; they are lower bounds on true peaks. A phase shorter than the interval can have no periodic samples.

Offline HTML reports render metric tables, sample counts, quality warnings and per-job heap/RSS SVG charts with phase annotations and a shaded concurrent Compact window. Charts include baseline, phase and post-GC snapshots. When more than 600 observations are available, chart reduction preserves bucket heap/RSS extrema and endpoints; it does not alter raw results or peak calculations. Consult `samples.csv` and `raw/*.result.json` for analysis beyond the chart. Missing RSS is omitted rather than encoded as zero. cgroup metadata is best-effort from standard cgroup-v2 paths and does not cover every mount/namespace arrangement.

## Comparison protocol

Grouping includes backend, phase, all runtime settings and all case settings, while excluding source identity and repetition/seed. Only identical groups are aligned. Different settings are counted as unmatched rather than aligned by display name.

Within a matched group, comparison requires unique and identical successful repetition/seed identities, equal workload checksums and equal operation/read/write counts. Hits may differ and remain a measured outcome. Deterministic input streams do not imply deterministic concurrent cache interleavings. Static fill/delete/Compact phases have deterministic configuration-derived inputs and may retain a zero mixed-workload checksum.

Harness source hashes must match and be present by default; legacy reference-validation-only runs may have both hashes empty. Environment fingerprints include hostname, toolchain, platform, CPU description, kernel, recorded limits and build settings. `-allow-harness-diff` and `-allow-env-diff` explicitly relax their respective checks and are recorded. Neither disables workload checks or permits comparing reference-validation results with actual upstream results. Intentional workload or instrumentation changes remain different groups.

Comparison emits JSON, CSV and Markdown with differences of medians. Individual median intervals are available in each run's summary JSON/CSV and HTML, rather than the comparison rows. A zero baseline has no percentage change. Lower memory can result from more eviction, so retained entries and hit rate must accompany it. No version-difference confidence interval, p-value or automatic winner is inferred.

## Cache-operation microbenchmarks

The local upstream checkout already supplies per-operation benchmarks. To investigate method-level cost separately, select bounded cases and record each group independently. From `lru-gcbench/`, the study uses the three backends' `Nested_Depth2` Get/Put cases and a separate prefix-deletion run:

```sh
microbench_id=$(date +%Y%m%d-%H%M%S)-$$
./scripts/run-microbench-recorded.sh ../go-lru "results/micro-get-put-$microbench_id" \
  '^Benchmark_(Get|Put)_(MapCache|RadixCache|ArenaRadixCache)$/^Nested_Depth2$' 10 1s 2
./scripts/run-microbench-recorded.sh ../go-lru "results/micro-prefix-$microbench_id" \
  '^Benchmark_DeletePrefix_(MapCache|RadixCache|ArenaRadixCache)$/^Nested_Depth2$' 10 100x 2
```

The final arguments specify repetition count, benchmark duration or fixed iteration count, and Go benchmark CPU setting. `100x` makes each reported DeletePrefix measurement cover one cycle of 100 prefixes with 100 entries each. The upstream benchmark restores 10,000 entries with its timer stopped at the start of each cycle. An adaptive `1s` timed budget excludes that refill work and can take much longer in wall time; the fixed count bounds the requested measured work. Its ns/op averages across a shrinking cache over that deletion cycle.

Optional unit-weight Put and arena Compact measurements, also used by the study, are recorded separately:

```sh
./scripts/run-microbench-recorded.sh ../go-lru "results/micro-update-$microbench_id" \
  '^Benchmark_Put_UnitWeight_(Map|Radix|ArenaRadix)$' 10 1s 2
./scripts/run-microbench-recorded.sh ../go-lru "results/micro-compact-$microbench_id" \
  '^Benchmark_ArenaRadixCache_Compact$' 10 10x 2
```

These benchmarks reuse precomputed keys and scalar values. The default weighted Put case holds approximately 5,000 of its 10,000 keys and exercises eviction; the unit-weight Put case can hold all 10,000 and mostly updates existing keys after its initial fill. The Get case starts with all keys present. They are distinct from the mixed harness workload's key generation and fresh byte payloads. The Compact benchmark rebuilds 10,000 entries and deletes half with the timer stopped before every call. Its `reclaimed-B/op` output is the before/after forced-GC heap difference for the **last iteration only**, despite the per-op label; it is not an average across the 10 Compact calls.

The wrapper saves `benchmark.txt`, `environment.txt`, `exit-status.txt`, the actual `executed-script.sh`, its original `executed-command.sh`, and `reproduce.sh` together. Each output directory must be new. Its replay script invokes Go against the original source path again; preserve the recorded toolchain and checkout revision. Repeat the same command and toolchain against baseline/candidate checkouts, then use `benchstat` if available to compare the Go benchmark output. `benchstat` is optional and is not installed or invoked by this harness. These benchmark outputs and workloads are distinct from the runner's JSON results.

The microbenchmark wrapper explicitly sets `GOGC=100`, `GOMEMLIMIT=off`, `GOPROXY=off` and the recorded `GOCACHE`; replay restores that build-cache path. Required dependencies must already exist in the local module cache. These settings make its runtime assumptions explicit and avoid module-proxy downloads during a benchmark run.

## Boundary checks and scope

Before accepting a phase, the worker checks unit-weight size accounting, full non-pressure fill retention, expected non-pressure deletion survivors, entry preservation across sequential Compact, and agreement between harness read/hit counts and cache lookup counters. Failure marks the job unsuccessful. These checks complement upstream invariant, differential and fuzz tests; they do not replace them.

CPU, allocation, mutex and block profiles are available as explicit diagnostics; see [the diagnostic guide](DIAGNOSTICS.md). No external memory hog, CPU pinning, cgroup creation, Zipfian generator or scheduler control is included. The harness does not make process RSS a hard limit.

The adapter and runtime metric meanings are based on the local target's README, `cache.go`, `options.go`, `pressure.go`, `go.mod`, and Go runtime metrics/GC documentation. Exact source hashes and Go versions belong to each run's manifest/results. This harness is MIT-licensed; the separately linked upstream library retains Apache-2.0 licensing as described in [third-party notices](../THIRD_PARTY_NOTICES.md).
