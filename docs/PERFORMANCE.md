# Performance study guide

Use fresh actual-backend workers, run suites sequentially on an otherwise idle host, and preserve every configuration and launch script with its results. Smoke results establish integration only. A configuration in `examples/` describes an experiment; it is not evidence that the experiment has been run.

The completed [2026-10-04 study record](../validation/PERFORMANCE-20261004.md) contains 450 repeated-study jobs, 12 pilot jobs and 130 standalone microbenchmark measurements, with the actual build, results and interpretation limits.

The [732-job repeated follow-up](../validation/REPEATED-20261004.md) supplies ten-repeat performance controls, six-repeat profiles, paired effect intervals and dense Compact traces. Its [prepare/run workflow](DIAGNOSTICS.md#freeze-and-run-the-repeated-study) freezes effective configurations and executable hashes; it preserves the original study separately.

## Recommended sequence

| Order | Configuration | Question | Primary measurements |
|---|---|---|---|
| 1 | [calibration.json](../examples/calibration.json) | How do buffer retention, runtime sampling and request timing change results? | Five matched controls; throughput, B/op and allocs/op |
| 2 | [performance.json](../examples/performance.json) | How do backends behave under a fixed, unconstrained workload? | Throughput, GC CPU/op, sampled Get/Put latency and hit rate |
| 3 | [footprint.json](../examples/footprint.json) | How does retained memory scale with entry count and value representation? | Post-GC bytes/entry, scan bytes and retained entries after fill/delete/Compact |
| 4 | [scalability.json](../examples/scalability.json) | What changes with 1, 4 and 8 request workers? | Throughput, request latency, GC CPU and hit rate at fixed GOMAXPROCS |
| 5 | [pressure.json](../examples/pressure.json) | What does pressure reclamation trade for lower memory? | Memory, pressure evictions, retained entries, hit rate and throughput |
| 6 | [reclaim-study.json](../examples/reclaim-study.json) | How do key deletion, prefix deletion and Compact affect recovery? | Deletion/Compact time, memory recovery and latency during requests |

[study.json](../examples/study.json) is a wider mixed matrix for later exploration. Its runtime profiles, concurrency and pressure settings differ from the narrow baseline. Do not attribute a difference between those configurations to the backend alone.

## Establish a baseline

The narrow baseline fixes capacity at 100,000 entries, key space at 200,000, independently allocated values at 256 bytes, reads at 90%, one request worker, GOMAXPROCS at 2, GOGC at 100, and no Go memory limit. Pressure reclamation is disabled. The three backends each run 10 repetitions.

Run a one-repetition pilot before a long study. Inspect the `measured` phase duration for every backend and select one common operation count that gives the fastest backend roughly 5–10 seconds of work. Keep that count in the final saved configuration for all backends. Increase the count if the resulting GC or request samples are still inadequate. The example's operation count is a starting point, not an automatic duration guarantee.

Calibration must use the baseline's workload, operation count and runtime settings. The current five-arm configuration separates buffer retention from active runtime sampling and request timing: no buffer/timing, retained buffer only, buffer plus runtime sampling, buffer plus request timing, and buffer plus both. The four retained-buffer arms allocate and touch the same 10,000 slots before baseline GC. Compare each activity against buffer-only, and compare buffer-only against no-buffer to examine the retained-memory effect. Their effects can interact through GC and scheduling; faster instrumented throughput does not establish a speedup or zero overhead. The historical study's saved two-arm calibration remains unchanged and cannot establish these separated costs. Changing sampling, topology or concurrency requires a suitable calibration.

Decide the final repetition count in advance. Use the same seed policy and matched settings across backends; use `run-paired` for before/after library versions. Do not run multiple suites concurrently, because their CPU, memory bandwidth and GC contention would become part of each other's workload. Record machine changes and background load that materially affect a run.

## Save the executed script with every run

Build a fresh controller and worker with the [README commands](../README.md#quick-start), then use the recorded wrapper for each chosen configuration. For example, from `lru-gcbench/` with `run_id` and the worker from that build:

```sh
./scripts/run-recorded.sh ./bin/lrugcbench "bin/worker-$run_id" \
  examples/calibration.json "results/calibration-$run_id" calibration
./scripts/run-recorded.sh ./bin/lrugcbench "bin/worker-$run_id" \
  examples/performance.json "results/performance-$run_id" baseline
```

Each output directory retains `executed-script.sh` (the wrapper source that ran), `executed-command.sh` (the original benchmark command), effective `config.json`, `command.json`, `run.log`, `reproduce.sh`, raw measurements and reports. Keep a derived pilot/subset configuration under its own name and pass it through the same wrapper. For individual Go benchmarks, use `scripts/run-microbench-recorded.sh` as shown in the [microbenchmark instructions](METHODOLOGY.md#cache-operation-microbenchmarks).

Use a result's `reproduce.sh` with a new output directory to rerun it. A harness replay uses the recorded configuration and original binaries; it does not rebuild sources or recreate the host. A microbenchmark replay invokes Go again against the original checkout path, so preserve the recorded toolchain and source revision. The README lists harness binary path overrides and paired-run behavior. Generated results are ignored by Git; the saved scripts and logs remain available in the local result directory.

For the recorded study layout, with `calibration/`, `baseline/`, `footprint/`, `scalability/`, `pressure64/`, `reclaim/` and the four `micro-get-put/`, `micro-update/`, `micro-prefix/`, `micro-compact/` directories, generate a combined summary after the suites finish:

```sh
python3 scripts/summarize-study.py results/performance-20261004
```

This writes `STUDY.md`, `study-summary.csv` and a copy of the executed `analysis-script.py`; the Markdown report records the exact analysis invocation and working directory. The CSV contains both harness and microbenchmark rows, while the report keeps their results and pilot evidence separate. Missing or unsuccessful main suites or selected microbenchmark cases cause a nonzero exit while preserving the report. Use `--allow-incomplete` only for an explicitly incomplete progress snapshot. The generated summary does not replace each suite's raw results and sample-quality warnings.

## Memory and pressure experiments

The footprint matrix covers 100,000 and 1,000,000 entries with scalar and 256-byte values. Forced GC occurs after each phase. Interpret retained heap and scan bytes at those boundaries; do not use forced-GC footprint results to rank natural-GC request throughput.

The pressure matrix uses 200,000 entries and 256-byte values with 32, 64 and 128 MiB soft limits, and matching pressure reclamation off/on cases. Values alone require approximately 48.8 MiB at full retention, before keys, cache structures and runtime overhead. **The 32 MiB case is a stress condition:** with pressure reclamation off, the requested retained payload already exceeds the soft limit. It can cause sustained GC pressure and a much longer runtime. Run it deliberately after higher-limit pilots, retain failures/timeouts, and do not treat it as an ordinary capacity baseline.

`GOMEMLIMIT` is a runtime soft limit, not an RSS limit. Lower heap/RSS can reflect additional eviction. Always read the corresponding retained entries, pressure evictions and hit rate. `read_percent=90` controls the read/write mix; with uniform access over twice the cache capacity, it does not imply a 90% hit rate.

For pressure exploration, derive a clearly named subset configuration instead of silently changing the full example. Preserve that effective configuration with the run and report its capacity and limits. A reduced-capacity pressure result is evidence for that reduced workload only.

## Requests, deletion and compaction

Compare scalability groups at their common GOMAXPROCS, and compare each group's 1/4/8-worker results. The narrow baseline uses a different GOMAXPROCS, so it is a separate experiment. More request workers are not equivalent to more CPU cores.

The reclaim study holds the payload, capacity and runtime fixed. Its sequential cases compare individual Delete and grouped DeletePrefix, both followed by Compact and one-worker recovery. A third case uses individual deletion and two request workers with Compact during the mixed phase. It is a concurrency experiment, so its whole-phase throughput also reflects the worker-count change. API-call counts differ: one DeletePrefix call can remove many entries. Compare deletion elapsed time and removed/remaining entries, not just operations per second across the two deletion modes.

The concurrent Compact call runs once per job. Whole-phase p99 includes requests outside its interval, and sparse sampling may miss individual stalls. Use the recorded Compact interval and heap/RSS plot to interpret it. These are closed-loop request measurements, not a service-level latency guarantee under an external arrival rate.

## Read a result

Start with job status and quality warnings, then examine durations, sample counts and repetition intervals before drawing conclusions. In particular:

- Throughput and B/op include key generation, random selection, payload allocation and workload bookkeeping. Use the [upstream microbenchmarks](METHODOLOGY.md#cache-operation-microbenchmarks) to investigate individual cache methods.
- Get/Put p99 values are histogram intervals from sampled calls. At 2,000,000 operations, 90% reads and sampling every 128 requests, a job has only about 1,563 Put samples on average.
- A median confidence interval describes one group's repeated measurements. Overlapping or non-overlapping intervals do not replace a statistical test of a version difference.
- Sampled memory peaks are lower bounds. A dropped trace or large sampling gap weakens peak and timing conclusions.
- Memory comparisons need retained-entry counts and hit rates, especially under pressure. Inspect post-delete/post-Compact phases separately from initial fill.

Keep pilot, functional validation and final study results distinct. Report actual configuration, successful/failed jobs and remaining quality warnings with any numerical finding. Full metric definitions and measurement boundaries are in [METHODOLOGY.md](METHODOLOGY.md).

## Diagnose the observed tradeoffs

Use [DIAGNOSTICS.md](DIAGNOSTICS.md) for the follow-up controls motivated by the original study. Shared/independent/harness topologies and per-phase profiles help locate concurrency costs without assuming a lock is the cause. A smaller-capacity pressure-off control helps separate dynamic reclamation from retaining fewer entries. Read-only Compact enabled/disabled cases keep state comparable while examining local request windows. These new configurations are protocols, not evidence that a bottleneck has been identified or eliminated. Preserve the historical study separately from new diagnostic results.
