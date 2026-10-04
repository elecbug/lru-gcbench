# Performance study — 2026-10-04

This record separates pilot measurements, functional validation and repeated performance experiments. Generated logs, scripts and raw results live under [`results/performance-20261004/`](../results/performance-20261004/) and are excluded from Git. Artifact links resolve only in a workspace containing that run.

## Completed experiment matrix

All six repeated harness suites completed **450/450 measurement jobs** successfully:

| Suite | Successful jobs | Selected conditions |
|---|---:|---|
| [Calibration](#calibration) | 60/60 | Instrumentation off/on; 25 million measured operations |
| [Baseline](#baseline) | 30/30 | One request worker; 25 million measured operations |
| [Footprint](#footprint) | 120/120 | 100k/1m entries; scalar/256-byte values |
| [Concurrency](#concurrency) | 90/90 | 1/4/8 request workers; GOMAXPROCS 4; 10 million operations |
| [Pressure](#memory-pressure) | 60/60 | 64 MiB profile only; reclamation off/on; 2 million operations |
| [Reclamation](#deletion-and-compaction) | 90/90 | Individual/prefix deletion and concurrent Compact; 10 million operations |

The [baseline/calibration pilot](../results/performance-20261004/pilot/manifest.json) and [pressure pilot](../results/performance-20261004/pressure-pilot/manifest.json) each completed 6/6 jobs, separately from the 450 repeated-study jobs. Four upstream microbenchmark invocations also completed successfully, recording **130 measurements across 13 selected benchmark cases**. The calibration launcher exception described below occurred after its measurements completed.

The [initial study script](../results/performance-20261004/run-study.sh) and [resume script](../results/performance-20261004/resume-study.sh) record suite order. Each suite additionally preserves its own executed launcher, exact benchmark command, resolved configuration and replay script.

The strict [combined report](../results/performance-20261004/STUDY.md) is **COMPLETE**; [study-summary.csv](../results/performance-20261004/study-summary.csv) includes both harness and microbenchmark medians. The [integrity report](../results/performance-20261004/integrity-check.json), produced by the saved [verification script](../results/performance-20261004/verify-study.py), passed checks for all **462 harness jobs**, recorded configurations, source/binary hashes and matched input operation/read/write/checksum tuples. Hits remain measured outcomes and are not required to match. The saved [analysis script](../results/performance-20261004/analysis-script.py) and its exact invocation in `STUDY.md` reproduce the combined analysis.

## Build and environment

The study uses the actual `google/go-lru` backends, built from a clean checkout. The [pilot manifest](../results/performance-20261004/pilot/manifest.json) records:

| Field | Value |
|---|---|
| Target commit | `6c2b8fa056eb77549eb8d2254a9940f52d92f9ff` |
| Target source SHA256 | `18204242453258526c05f13a9f61e8defb37346bbb80099d24ad69c649d8c346` |
| Harness source SHA256 | `77e9695b15a20a396b61c337a7cf13b53e14dd3c2801cf4f6ba325c249b4608c` |
| Worker executable SHA256 | `f4c344f3a4340971dece80fa977642ff6663400b313d19822efbd38fe13edb79` |
| Toolchain/platform | `go1.27.1 linux/amd64` |
| Processor | Intel Xeon Gold 6434; 32 logical CPUs visible |
| Kernel | `7.0.0-28-generic` |
| Backends | `map`, `radix`, `arena` |

The controller and worker were built after successful `go test -race ./...` and `go vet ./...`. The [executed validation/build script](../results/performance-20261004/validate-build.sh), [race log](../results/performance-20261004/test-race.log), [vet log](../results/performance-20261004/vet.log) and [worker build log](../results/performance-20261004/build.log) are preserved. The vet log is empty on success.

These are sequential runs on one host. Runtime settings are explicit in each saved configuration; CPU frequency, temperature and unrelated host activity are not controlled by the harness. The recorded toolchain and hashes identify this build, not later source edits.

## Pilot and final settings

The [baseline/calibration pilot](../results/performance-20261004/pilot/report.html) completed 6/6 jobs: all three backends, instrumentation off/on, one repetition, 100,000 entries, a 200,000-key space, 256-byte values, 90% reads and 2,000,000 measured operations. GOMAXPROCS was 2, GOGC was 100, the Go memory limit was off and pressure reclamation was disabled. The fastest pilot completed approximately 4.01 million operations per second.

That pilot was used to choose a common **25,000,000 measured operations** for both final calibration and baseline configurations, targeting at least roughly five seconds on the fastest backend. Warmup remains 500,000 operations. All backends use the same operation count and 10 fresh-process repetitions. With sampling every 128 requests, this workload should collect approximately 19,531 Put observations per job on average; actual counts and phase durations determine the interpretation.

Scalability and reclamation configurations use 10,000,000 mixed operations per job. Their different concurrency and phases make them separate experiments. Footprint has forced GC after fill/delete/Compact and does not use mixed-operation duration. The full pressure example includes an intentionally severe 32 MiB profile; it is not implied to have been run by the baseline pilot.

The pilot's exact [executed script](../results/performance-20261004/pilot/executed-script.sh), [benchmark command](../results/performance-20261004/pilot/executed-command.sh), [effective configuration](../results/performance-20261004/pilot/config.json), [log](../results/performance-20261004/pilot/run.log) and [replay script](../results/performance-20261004/pilot/reproduce.sh) are stored beside its report. Every completed suite launched through the recorded wrapper has the same artifact set.

## Calibration

The [calibration suite](../results/performance-20261004/calibration/report.html) completed all **60/60 measurement jobs**: 10 repetitions of instrumentation off/on for each backend, at 25,000,000 operations per job. Measured throughput medians were:

| Backend | Instrumentation off, million ops/s | Instrumentation on, million ops/s |
|---|---:|---:|
| map | 3.237 | 3.579 |
| radix | 1.452 | 1.466 |
| arena | 2.667 | 2.871 |

The median confidence intervals overlap widely. The higher on-case medians do **not** establish a beneficial speedup or zero instrumentation cost. The on case also retains a 10,000-slot runtime-sample buffer allocated and touched before baseline GC; the off case does not. That live memory can change the GC heap goal and scheduling even though its allocation is outside the timed delta. These measurements include the combined effects of timing, sampling, retained tracer memory and execution variability; they do not identify which caused the observed differences. Arena recorded fewer than 100 measured GC pause events in 3/10 off runs and 8/10 on runs, limiting GC-tail interpretation.

There was a launcher exception after the controller successfully finished all 60 jobs: its shell script was edited while Bash waited for the controller, causing a syntax error when Bash resumed reading it. The original launcher source was restored from a byte-identical pilot snapshot, and the executed benchmark command was reconstructed from `command.json`; no measurements were rerun or altered. The [launcher note](../results/performance-20261004/calibration/launcher-note.txt) and [launcher exit status](../results/performance-20261004/calibration/launcher-exit-status.txt) preserve this distinction between successful measurements and launcher exit status 2. Later suites use the frozen wrapper via the [resume script](../results/performance-20261004/resume-study.sh).

## Baseline

The [baseline suite](../results/performance-20261004/baseline/report.html) completed **30/30 jobs**, using the common 100,000-entry, 256-byte, 90%-read configuration above. All measured phases lasted 6.4–18.3 seconds. The [saved summary](../results/performance-20261004/baseline/summary.json) reports:

| Backend | Median million ops/s | Throughput median interval, million ops/s | Allocated B/op | GC CPU ns/op |
|---|---:|---:|---:|---:|
| map | 3.443 | 3.078–3.685 | 80.67 | 55.76 |
| radix | 1.437 | 1.368–1.513 | 83.02 | 62.99 |
| arena | 2.947 | 2.540–3.138 | 76.42 | 13.07 |

The intervals use the harness's order-statistic procedure with achieved coverage 97.85% for 10 observations. They describe each group's median, not a confidence interval for the difference between backends. Map has the highest observed throughput median in this workload; arena has the lowest observed allocation and GC CPU medians. These are workload-level observations on this host, not isolated cache-method costs or a ranking across other workloads. Map and arena's throughput intervals overlap.

Every backend retained all 100,000 entries, recorded zero pressure evictions and had a median hit rate of approximately 50.00%. Median sampled-call counts were 175,748 Gets and 19,564.5 Puts per job, with no dropped periodic samples. Arena observed only 94–98 measured GC pause events per run, so all 10 runs retained the GC-p99 sample warning; map and radix had no measured-phase quality warnings. The lower measured GC CPU cost does not remove that limitation on GC-tail estimates.

The [effective configuration](../results/performance-20261004/baseline/config.json), [executed launcher](../results/performance-20261004/baseline/executed-script.sh), [benchmark command](../results/performance-20261004/baseline/executed-command.sh), [run log](../results/performance-20261004/baseline/run.log) and [replay script](../results/performance-20261004/baseline/reproduce.sh) were recorded normally.

## Footprint

The [footprint suite](../results/performance-20261004/footprint/report.html) completed **120/120 jobs**: 100,000 and 1,000,000 entries, scalar and 256-byte values, all three backends and 10 repetitions. Its forced-GC boundaries measure retained process memory separately from natural-GC workload throughput.

For the **1,000,000-entry, 256-byte-value case**, fill retained exactly 1,000,000 entries; deleting 75% left exactly 250,000, and Compact preserved that count. Median results were:

| Backend | Fill incremental post-GC B/entry | Fill post-GC scan MiB | After delete, post-GC heap MiB | After Compact, post-GC heap MiB |
|---|---:|---:|---:|---:|
| map | 423.77 | 113.38 | 141.07 | 101.22 |
| radix | 401.23 | 119.41 | 95.95 | 95.95 |
| arena | 408.23 | 94.12 | 194.93 | 97.24 |

Bytes per entry subtract the pre-construction baseline; heap and scan columns are whole-process post-GC observations. Radix retained the least heap in this case. Arena's scan footprint was smaller, while its retained heap after deletion fell substantially only after Compact. Map also released retained heap after Compact. These results distinguish total retained heap from scannable heap; they do not establish a universal memory winner or isolate the cause of the separate baseline's GC CPU differences.

The full [summary](../results/performance-20261004/footprint/summary.json), [configuration](../results/performance-20261004/footprint/config.json), [executed script](../results/performance-20261004/footprint/executed-script.sh) and [replay script](../results/performance-20261004/footprint/reproduce.sh) retain evidence for every footprint case.

## Concurrency

The [scalability suite](../results/performance-20261004/scalability/report.html) completed **90/90 jobs**, with GOMAXPROCS fixed at 4 and 10,000,000 measured operations per job. Each cell below lists medians in **1 / 4 / 8 request-worker** order:

| Backend | Throughput, million ops/s (1 / 4 / 8) | Sampled Put p99 upper bucket bound, µs (1 / 4 / 8) |
|---|---:|---:|
| map | 2.727 / 0.841 / 0.709 | 3.46 / 131.07 / 212.99 |
| radix | 1.303 / 0.384 / 0.384 | 5.89 / 589.82 / 1,114.11 |
| arena | 2.353 / 0.679 / 0.666 | 6.40 / 262.14 / 491.52 |

Increasing request concurrency did not improve throughput for this shared-cache workload and substantially increased sampled Put latency. Retention stayed at 100,000 entries and median hit rates remained near 50%. These observations do not identify the cause of the concurrency limit; no CPU or lock profile was collected. GOMAXPROCS differs from the narrow baseline, so its throughput is not a direct one-variable comparison with that earlier table.

Latency values summarize sampled per-run histogram bounds, not exact population quantiles or externally queued requests. The one-worker map and arena phases had median durations of approximately 3.7 and 4.3 seconds, respectively. All 90 runs retained the fewer-than-100-GC-pause-events warning, so this suite does not establish stable GC p99 estimates.

See the [summary](../results/performance-20261004/scalability/summary.json), [configuration](../results/performance-20261004/scalability/config.json), [executed script](../results/performance-20261004/scalability/executed-script.sh) and [replay script](../results/performance-20261004/scalability/reproduce.sh) for full evidence.

## Memory pressure

The [64 MiB pressure subset](../results/performance-20261004/pressure64/report.html) completed **60/60 jobs**: 200,000-entry capacity, a 400,000-key space, 256-byte values, 90% reads, 2,000,000 measured operations, GOMAXPROCS 2 and pressure reclamation off/on. Only the 64 MiB runtime profile was selected for this repeated study; the full example's 32 and 128 MiB profiles were not run here.

Each cell lists medians with pressure reclamation **off / on**:

| Backend | Million ops/s | Entries at end | Hit rate, % | Heap objects at end, MiB |
|---|---:|---:|---:|---:|
| map | 1.051 / 0.881 | 200,000 / 100,000 | 50.01 / 25.06 | 84.57 / 44.23 |
| radix | 0.556 / 0.845 | 200,000 / 102,279 | 50.01 / 25.39 | 81.12 / 43.61 |
| arena | 1.005 / 0.989 | 200,000 / 102,002 | 50.01 / 25.46 | 81.47 / 45.62 |

Reclamation-on jobs recorded median pressure evictions of approximately 148,000–151,000 during measurement; off jobs recorded zero. The lower heap accompanies roughly half as many retained entries and a hit rate near 25%, rather than 50%. It therefore describes an eviction/memory tradeoff, not a memory saving at equal retention. Radix's higher throughput median with reclamation enabled also occurs under that changed retention and hit rate.

The off-case heap-object measurements exceed the configured 64 MiB soft limit, illustrating that `GOMEMLIMIT` does not impose a hard heap or RSS cap. These are end-of-phase object-space observations that can include uncollected garbage, not forced-GC retained-heap measurements. Measured phases lasted approximately 1.4–5.1 seconds. All arena-on and radix-on runs had fewer than 100 GC pause events; those GC-p99 warnings remain, even though no phase fell below the report's one-second threshold.

The exact [subset configuration](../results/performance-20261004/pressure64/config.json), [summary](../results/performance-20261004/pressure64/summary.json), [executed script](../results/performance-20261004/pressure64/executed-script.sh) and [replay script](../results/performance-20261004/pressure64/reproduce.sh) preserve the selected conditions.

## Deletion and compaction

The [reclamation suite](../results/performance-20261004/reclaim/report.html) completed **90/90 jobs**. Both sequential deletion cases started with 100,000 entries, removed 75,000 and retained 25,000 through the subsequent Compact call. Individual deletion makes 75,000 Delete calls; prefix deletion makes 12 DeletePrefix calls. They select different key subsets with the same survivor count. Median total durations were:

| Backend | Individual deletion, ms | Prefix deletion, ms | Sequential Compact after individual deletion, ms | Compact during requests, ms |
|---|---:|---:|---:|---:|
| map | 16.578 | 25.820 | 2.730 | 24.587 |
| radix | 38.997 | 6.446 | 0.00385 | 0.00694 |
| arena | 33.762 | 10.770 | 4.391 | 60.451 |

Prefix deletion completed sooner than the individual-delete loop for radix and arena in these selected cases; map's prefix deletion took longer. Compare elapsed time and removed entries rather than API calls per second across the two modes. The sequential Compact call runs before one-worker recovery, at 25,000 entries. The concurrent case starts its mixed phase at 25,000 entries, uses two request workers and regrows to 100,000 entries by the end, with Compact triggered inside that evolving workload. Its call duration therefore does not isolate the effect of contention at equal cache state.

Whole-phase latency summaries include requests outside the single Compact interval, and periodic/request sampling may miss a brief stall. Some short deletion and Compact phases have no periodic observations; their memory peaks use boundary samples only. Every mixed recovery/concurrent run retains the fewer-than-100-GC-pause-events warning. Consult the recorded Compact interval and per-job charts before attributing a latency change to compaction.

The [summary](../results/performance-20261004/reclaim/summary.json), [configuration](../results/performance-20261004/reclaim/config.json), [executed script](../results/performance-20261004/reclaim/executed-script.sh) and [replay script](../results/performance-20261004/reclaim/reproduce.sh) preserve the measurements and exact conditions.

## Upstream microbenchmarks

All four selected upstream benchmark invocations returned exit status 0 and `PASS`: Get/Put produced 60 rows, unit-weight Put 30, DeletePrefix 30 and arena Compact 10. Each selected case has 10 repeats at `-cpu=2`. Repeats within a `go test` invocation share a process, unlike the harness's fresh-process repetitions. Medians are:

| Operation / selected case | map ns/op | radix ns/op | arena ns/op |
|---|---:|---:|---:|
| Get, `Nested_Depth2` | 34.525 | 144.100 | 82.165 |
| Weighted Put, `Nested_Depth2` | 639.550 | 1,381.000 | 1,113.000 |
| Unit-weight Put, mostly updates after fill | 53.015 | 167.850 | 106.200 |
| DeletePrefix, `Nested_Depth2`, fixed 100 iterations | 164,979.000 | 4,363.000 | 20,705.500 |
| Arena Compact, fixed 10 iterations | — | — | 2,263,377.500 |

The Get and unit-weight Put cases reported median 0 B/op and 0 allocs/op for all backends. Weighted Put medians were map 104 B/op and 2 allocs/op, radix 118 B/op and 3 allocs/op, and arena 23 B/op and 2 allocs/op. These benchmarks reuse keys and scalar values. Weighted Put exercises eviction, unit-weight Put mostly updates retained keys, and Get starts with all keys present; they do not measure the same work as the mixed harness workload.

DeletePrefix's 100-iteration measurement traverses 100 prefixes of 100 entries each, with refill excluded from timing. Arena Compact's median call cost was approximately **2.263 ms**, rebuilding 10,000 entries and deleting half outside each timed call. Its reported `reclaimed-B/op` median was **803,304 bytes across the 10 repeats' final-iteration heap deltas**. Despite the unit label, this is not average reclamation per Compact call. Its setup and cache size also differ from the harness's reclamation scenarios.

Raw results are preserved for [Get/Put](../results/performance-20261004/micro-get-put/benchmark.txt), [unit-weight Put](../results/performance-20261004/micro-update/benchmark.txt), [DeletePrefix](../results/performance-20261004/micro-prefix/benchmark.txt) and [Compact](../results/performance-20261004/micro-compact/benchmark.txt). Each directory includes its executed launcher/command, environment, exit status and replay script; the [combined report](../results/performance-20261004/STUDY.md#standalone-go-microbenchmarks) links every artifact. Exact bounded commands and upstream timing details are in the [methodology](../docs/METHODOLOGY.md#cache-operation-microbenchmarks).

## Interpretation

Throughput and allocation figures include key generation, random selection, payload allocation and workload bookkeeping. Read percentage is not hit percentage. Compare memory with retained entries, hit rate and pressure evictions, particularly when reclamation settings differ.

Request latency is sampled, histogram-bucketed and measured in a closed-loop workload. It excludes an external request queue. Individual group median intervals do not establish a version-change significance test. GC p99 needs sufficient observed pause events; a long workload or ten repetitions alone does not ensure it. Keep the one-repetition pilot separate from final performance estimates.

See the [performance study guide](../docs/PERFORMANCE.md) and [measurement protocol](../docs/METHODOLOGY.md) for complete boundaries and reproduction details.
