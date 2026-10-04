# Measurement protocol and limitations

## Experimental unit and provenance

One fresh OS worker process is the experimental unit. A suite runs workers sequentially and shuffles job order with a PRNG separate from the workload generator. Repetition index `r` uses `seed+r` across backends and runtime profiles. Repetitions therefore include execution variability and changes in the deterministic input stream; they do not replay one identical trace. Shared-cache interleavings remain subject to scheduling.

`run-paired` uses the same shuffled job list for baseline and candidate. It runs both versions of each job consecutively and alternates their order across pairs; the config seed determines which version runs first in the initial pair. Every invocation is a fresh process. `paired.json` records the schedule, and each side has its own manifest and raw results. Comparison is generated only when both suites finish without failed jobs and integrity validation succeeds. This reduces systematic execution-order bias without controlling host load, CPU frequency or thermal state.

The builder fingerprints `.go`, `.tmpl`, `go.mod` and `go.sum` inputs, excluding `.git`, `vendor`, `bin` and `results`. It records target commit/dirty state when available, the target and harness source hashes, and the build timestamp. The controller records the worker executable SHA256. The checkout must remain stable during compilation. This is a compilation-input fingerprint, not a hash of every repository byte; future `go:embed` assets would require extending the policy. Compilation uses `-mod=mod`, so vendor directories are not used.

An API-fixture test checks generated code without executing the upstream implementation. The reference worker uses a distinct backend and provenance kind; it never represents MapCache, RadixCache or ArenaRadixCache. See [the validation record](../validation/VALIDATION.md) for the separation between these checks and actual-backend runs.

## Allocation and timing boundaries

Environment metadata, the bounded trace buffer and main result capacity are allocated before baseline GC. The trace buffer is touched before that collection. Only then is the target cache constructed. No global key/value corpus is retained. Values stay typed through the generated adapter.

Mixed worker goroutines, PRNG state and result buffers are prepared before the phase's start snapshot. Phase timing includes start-barrier release, workload execution, worker completion and fixed-size reduction. The operation loop includes key generation, random selection, cache/payload work and bookkeeping. These are end-to-end workload metrics. Boundary runtime sampling and Cache.Stats are outside the operation timer; counter windows span those snapshots and are slightly wider than the timer.

Optional request timing surrounds the adapter call and limited outcome bookkeeping, excluding key creation and random selection. Put timing includes allocation and touching of a new payload. Each worker samples every Nth request from a seed-derived offset; instrumentation does not change its workload PRNG stream. Request histogram buckets have eight intervals per factor of two. Reported p99 values are bucket bounds, not exact population quantiles.

`examples/calibration.json` holds workload settings fixed while turning both periodic sampling and request timing off/on. Its cases appear as separate groups because instrumentation belongs to the configuration identity. Compare their throughput and allocations in the same report; version comparison does not silently match different instrumentation settings. This estimates their combined overhead. Add a third case if separate clock and sampler overhead estimates are needed.

Histogram formatting and bulk result serialization occur after measurement phases. Boundary histogram copies and task objects still occupy process memory. Post-GC heap minus baseline is an approximate incremental process footprint; dividing by retained entries does not make it a precise cache-owned byte count.

## Scenarios and cache semantics

Capacity uses the default unit weight, so it counts entries. There is no custom WithWeigher. Entry capacity, Go's soft memory limit and OS/container memory limits are distinct.

Fills insert logical IDs `0..capacity-1`. Mixed reads and writes independently choose uniform keys from `[0,key_space)`; the default key space is twice capacity. Read misses do not insert. Writes construct a fresh value whether they insert or replace. Scalar values are pointer-free 16-byte values; byte values allocate and touch an independent slice on each Put.

Flat keys contain 16 ASCII hex bytes. Prefix keys use `tenant/<hex-digit>/objects/<16-hex-digits>` and group logical IDs by `id & 15`. Comparing flat and prefix keys changes both length and prefix sharing.

| Scenario | Phases and GC policy |
|---|---|
| `footprint` | Baseline GC, construct, fill, delete, Compact. Each operation phase is followed by a forced GC stored outside its timed/counter window. No periodic sampler, pressure reclamation or request timing; one worker. `warmup_ops` and `operations` are unused. |
| `steady` | Baseline GC, construct, fill, optional warmup, measured mixed workload. No forced GC after construction. `operations` is the measured workload size. |
| `reclaim` | Baseline GC, construct, fill, delete, sequential Compact, mixed recovery. Natural GC after construction. `operations` is recovery size; `warmup_ops` is unused. |
| `concurrent-compact` | Baseline GC, construct, fill, delete, mixed requests plus one explicit Compact. Natural GC after construction. Requires `operations >= 2*workers` and `latency_sample_every > 0`; `warmup_ops` is unused. |

For deletion scenarios, `delete_mode` defaults to `keys`: individual Delete calls remove the first `floor(capacity*delete_fraction)` logical IDs. With `delete_mode="prefix"`, one DeletePrefix call removes each selected tenant prefix in order from `0` upward. This requires prefix keys and a delete fraction that is a multiple of 1/16. The number of selected groups is `16*delete_fraction`; capacities not divisible by 16 can give a deleted-entry fraction slightly different from the requested fraction. Reported `delete`/`delete_prefix` operations count API calls, not removed entries. Pressure shedding may already have removed selected entries; deletion attempts still count.

The concurrent scenario triggers a compactor goroutine halfway through worker 0's request stream and waits for both requests and compaction to finish. The `concurrent` phase records `concurrent_compaction` start/end offsets and duration. That window lies inside the phase; scheduling may serialize the call with requests, and sparse latency sampling may miss individual stalls. Its phase metrics and latency distribution cover the whole mixed phase, not only requests overlapping Compact. Allocation and GC costs include the one Compact but use mixed-request count as their per-op denominator.

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

Each metric reports count, median, minimum and maximum. With at least six observations, `median_ci` provides an exact binomial/order-statistic interval whose coverage is at least 95%; achieved coverage is stored with the bounds. Fewer observations omit the interval because a finite interval of this kind cannot reach that coverage. The stated coverage assumes independent observations from the same continuous distribution; ties can make the interval conservative. Changing seeds, host drift, shared resource limits and consecutive A/B execution must be considered when applying that assumption. These intervals describe each group's median, not the difference between versions. They are not automatic significance or winner labels.

## Time series and HTML charts

The periodic sampler reads runtime metrics and Linux RSS without taking Cache.Stats locks. Compaction therefore does not directly block sampling on that lock, although scheduling may delay a sample. Cache state and eviction counters are recorded at phase boundaries. Runtime and RSS readings are consecutive observations rather than an atomic snapshot.

The bounded buffer retains its first `max_samples` points and counts later drops. Phase endpoints remain available. Sampled peaks include retained periodic samples and boundary observations; they are lower bounds on true peaks. A phase shorter than the interval can have no periodic samples.

Offline HTML reports render metric tables, sample counts, quality warnings and per-job heap/RSS SVG charts with phase annotations and a shaded concurrent Compact window. Charts include baseline, phase and post-GC snapshots. When more than 600 observations are available, chart reduction preserves bucket heap/RSS extrema and endpoints; it does not alter raw results or peak calculations. Consult `samples.csv` and `raw/*.result.json` for analysis beyond the chart. Missing RSS is omitted rather than encoded as zero. cgroup metadata is best-effort from standard cgroup-v2 paths and does not cover every mount/namespace arrangement.

## Comparison protocol

Grouping includes backend, phase, all runtime settings and all case settings, while excluding source identity and repetition/seed. Only identical groups are aligned. Different settings are counted as unmatched rather than aligned by display name.

Within a matched group, comparison requires unique and identical successful repetition/seed identities, equal workload checksums and equal operation/read/write counts. Hits may differ and remain a measured outcome. Deterministic input streams do not imply deterministic concurrent cache interleavings. Static fill/delete/Compact phases have deterministic configuration-derived inputs and may retain a zero mixed-workload checksum.

Harness source hashes must match and be present by default; legacy reference-validation-only runs may have both hashes empty. Environment fingerprints include hostname, toolchain, platform, CPU description, kernel, recorded limits and build settings. `-allow-harness-diff` and `-allow-env-diff` explicitly relax their respective checks and are recorded. Neither disables workload checks or permits comparing reference-validation results with actual upstream results. Intentional workload or instrumentation changes remain different groups.

Comparison emits JSON, CSV and Markdown with differences of medians. Individual median intervals are available in each run's summary JSON/CSV and HTML, rather than the comparison rows. A zero baseline has no percentage change. Lower memory can result from more eviction, so retained entries and hit rate must accompany it. No version-difference confidence interval, p-value or automatic winner is inferred.

## Cache-operation microbenchmarks

The local upstream checkout already supplies per-operation benchmarks. To investigate method-level cost separately, run its benchmark suite from that checkout. For example, from `lru-gcbench/`:

```sh
(cd ../go-lru && go test -run '^$' -bench '^Benchmark_(Get|Put|DeletePrefix)_' \
  -benchmem -count=10 -benchtime=1s .) > results/upstream-microbench.txt
```

Choose a fresh output filename to retain previous measurements. Repeat the same command and toolchain against baseline/candidate checkouts, then use `benchstat` if available to compare the Go benchmark output. `benchstat` is optional and is not installed or invoked by this harness. Review upstream benchmark setup when interpreting its allocation boundaries; these output formats and workloads are distinct from this runner's JSON results.

## Boundary checks and scope

Before accepting a phase, the worker checks unit-weight size accounting, full non-pressure fill retention, expected non-pressure deletion survivors, entry preservation across sequential Compact, and agreement between harness read/hit counts and cache lookup counters. Failure marks the job unsuccessful. These checks complement upstream invariant, differential and fuzz tests; they do not replace them.

No external memory hog, CPU pinning, cgroup creation, Zipfian generator, profiler or scheduler control is included. The harness does not make process RSS a hard limit.

The adapter and runtime metric meanings are based on the local target's README, `cache.go`, `options.go`, `pressure.go`, `go.mod`, and Go runtime metrics/GC documentation. Exact source hashes and Go versions belong to each run's manifest/results. This harness is MIT-licensed; the separately linked upstream library retains Apache-2.0 licensing as described in [third-party notices](../THIRD_PARTY_NOTICES.md).
