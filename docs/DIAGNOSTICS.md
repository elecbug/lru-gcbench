# Diagnostic experiments

The completed [732-job repeated study](../validation/REPEATED-20261004.md) extends performance conditions to ten repetitions and profile conditions to six, with paired effect intervals and an explicit multiplicity limit.

The completed [diagnostic follow-up](../validation/DIAGNOSTICS-20261004.md) records 124 main pilot jobs and four separate dense Compact traces, their integrity audits, preliminary findings and sample limitations.

The original [performance study](../validation/PERFORMANCE-20261004.md) observed lower Arena GC CPU cost, lower shared-cache throughput with more request workers, extra allocation under pressure reclamation, and substantial memory recovery after Compact. Those observations motivate controls; they do not identify a particular lock, allocation site or GC mechanism. Retained heap, scannable heap, allocation volume, GC CPU and request throughput remain separate outcomes.

All examples save settings with the result. Build a fresh controller and worker, then run a selected configuration sequentially using the [recorded wrapper](PERFORMANCE.md#save-the-executed-script-with-every-run):

```sh
./scripts/run-recorded.sh ./bin/lrugcbench "bin/worker-$run_id" \
  examples/topology-study.json "results/topology-$run_id" topology
```

Pilot before a long run and choose a common operation count for the conditions being compared. Full repeated-study configurations use ten fresh processes per condition. Profile configurations use one process per diagnostic condition and do not support throughput rankings. A configuration's presence does not mean it has been run.

## Run the recorded diagnostic sequence

The driver runs all eight diagnostic suites sequentially and saves their effective configurations:

```sh
./scripts/run-diagnostics.sh ./bin/lrugcbench "bin/worker-$run_id" \
  "results/diagnostics-pilot-$run_id" pilot
```

Its interface is `scripts/run-diagnostics.sh CONTROLLER WORKER NEW_OUTPUT [pilot|full]`. The output path must be new; omitted mode defaults to `pilot`. Build both binaries before launching it and keep the host available for these measurements. Python 3 generates the resolved configurations; Go's `pprof` command analyzes the profiles after every timed job has finished.

Pilot mode runs **124 fresh worker processes**. Unprofiled suites use two repetitions; topology and harness controls use at most 1,000,000 requests, the other suites at most 2,000,000, and all warmups at most 100,000. Capacities, key spaces, payloads, runtime policies and instrumentation settings remain those of the full examples. The profile suites keep one repetition. These shortened runs check diagnostic behavior and provide preliminary observations; two repetitions do not establish reliable performance rankings or median intervals.

Use `full` explicitly to run the unmodified example configurations, currently **532 jobs**. This includes ten repetitions for unprofiled controls and one repetition for each profile condition. It does not automatically select a suitable duration from the pilot: if the pilot indicates insufficient measured time or samples, prepare a revised protocol first and preserve its configuration. The example files themselves are not changed by either mode.

The result root records:

- `diagnostic-plan.json`: driver mode, suite names, job counts and whether each suite is profiled.
- `configs/`: every exact configuration passed to a suite, including pilot reductions.
- `executed-driver.sh` and `executed-driver-command.sh`: the actual driver source and original invocation.
- `driver-exit-status.txt`: the driver's completion status, including a failed attempt.
- `analysis-commands.sh`: the profile analysis commands actually run after measurement.

Each suite additionally contains the recorded wrapper, original command, configuration, log, replay script and raw results. Each profile directory retains `analyze.sh`, `top.txt` sorted by flat cost, and `cumulative.txt` sorted by cumulative cost. A failed suite stops the driver; retained earlier results and the exit status show how far it completed. The original-command scripts record their original output paths, so use a new path when launching again; use each suite's `reproduce.sh` for a saved effective configuration. Whole-driver runs read the current example files, while per-suite replay reads the saved configuration.

## Freeze and run the repeated study

For the complete workflow, run `make benchmark RUN_ID=20261005-01` from `lru-gcbench/`. `UPSTREAM_REPO` defaults to `../go-lru`; set `UPSTREAM_REPO=/path/to/go-lru` to choose another clean upstream checkout. The command validates the run identifier and refuses existing binary/result paths. Use a new identifier for each attempt.

The workflow runs Go race/vet checks and analyzer self-tests, builds unique controller/worker binaries, runs 18 smoke jobs, prepares the study and expanded comparison plan, executes all 732 study jobs and profile extraction, then runs both analyzers. These stages execute sequentially even when Make is invoked with `-j`.

| Artifact | Path |
|---|---|
| Controller | `bin/lrugcbench-<RUN_ID>` |
| Worker | `bin/worker-<RUN_ID>` |
| Smoke results and replay scripts | `results/smoke-<RUN_ID>/` |
| Repeated measurements, profiles and analyses | `results/repeated-<RUN_ID>/` |
| Pipeline log, executed script, commands and status | `results/workflow-<RUN_ID>/` |

After preflight succeeds, the workflow saves `executed-workflow.sh`, `Makefile`, `executed-command.sh`, `commands.sh`, `workflow.log` and `workflow-exit-status.txt` in its pipeline directory. Exit status zero means the entire workflow, including both analyzers, completed. These records complement the per-suite execution and replay artifacts. A failed stage stops the workflow and preserves its recorded diagnostics; its output identifier cannot be reused. No timed suites or post-processing run concurrently within this command. Preserve both binaries at their recorded paths for later integrity checks and profile analysis.

To run the study stages manually with existing binaries, `scripts/run-repeated-study.sh` separates preparation from execution:

```sh
./scripts/run-repeated-study.sh prepare ./bin/lrugcbench "bin/worker-$run_id" \
  "results/repeated-$run_id"
python3 scripts/analyze-repeated.py --plan-only "results/repeated-$run_id"
./scripts/run-repeated-study.sh run "results/repeated-$run_id"
python3 scripts/analyze-repeated.py "results/repeated-$run_id"
python3 scripts/analyze-profile-repeats.py "results/repeated-$run_id"
```

Preparation requires a new output path and writes the 732-job `study-plan.json`, effective configurations, binary/configuration hashes, comparison policy and a saved suite wrapper. Execution verifies those hashes, records its own driver source and invocation, and runs suites serially. It refuses an output root that has already started. Per-suite replay scripts remain available for independently replaying saved configurations into fresh directories.

This protocol contains ten repetitions for unprofiled controls and six for each profile condition. Pressure observer-off and observer-on controls are separate suites. Dense Compact traces record every request for all three backends. CPU profiles use a larger request budget than mutex/block profiles, so attribution is analyzed by profile kind and normalized per operation where appropriate. Every profile gets full flat/cumulative text output after all timed jobs have finished.

The study plan declares the comparison policy before measurement. `scripts/analyze-repeated.py` provides the separate comparison-plan/analysis workflow; retain its generated plan and exact execution record alongside the driver artifacts. Same-seed statistical pairs follow globally shuffled suite execution, not adjacent temporal pairing. Individual median-effect intervals and exploratory adjusted sign tests have different interpretations; a large global family with ten pairs has little rejection power. Use effect sizes, coverage and profile consistency with the full limitations in the saved study record.

## Separate instrumentation effects

[calibration.json](../examples/calibration.json) has five arms, all with the same 100,000-entry workload, 25,000,000 requests and runtime policy:

| Arm | Retained sample buffer | Runtime sampling | Request timing |
|---|---|---|---|
| `no-buffer-no-timing` | No | Off | Off |
| `retained-buffer-only` | 10,000 slots | Off | Off |
| `buffer-runtime-sampling` | 10,000 slots | 20 ms | Off |
| `buffer-request-timing` | 10,000 slots | Off | Every 128 requests |
| `buffer-runtime-and-request` | 10,000 slots | 20 ms | Every 128 requests |

`retain_sample_buffer=true` allocates, touches and retains the sample capacity even when `sample_interval="0"`. Allocation precedes baseline GC. Buffer-only versus no-buffer examines retained instrumentation memory; runtime-only and request-only versus buffer-only examine the active instrumentation with the same retained capacity. Both-enabled checks their combined effect, which need not equal the sum of the isolated effects. Live memory, GC goals and scheduling can still interact. The old study's two-arm calibration remains historical evidence and must not be relabeled as this experiment.

## Separate runner costs from shared-cache costs

[topology-study.json](../examples/topology-study.json) fixes GOMAXPROCS at four and compares 1, 4 and 8 request workers under two modes. [harness-control.json](../examples/harness-control.json) supplies the third mode without duplicating synthetic work under three backend labels.

| `cache_mode` | Cache state | Interpretation |
|---|---|---|
| `shared` or omitted | All request workers use one cache | Includes synchronization of shared state |
| `independent` | Each worker uses a separate cache of the configured capacity | Removes cross-worker cache sharing but increases total retained state |
| `harness` | No cache storage or lookup | Measures key/PRNG/payload/runner work with synthetic outcomes |

Independent mode keeps **capacity per cache**, so four workers each at 100,000 entries retain up to 400,000 entries. Its fills, memory, GC and aggregate cache counters cover all caches. It is a control for sharing, not an equal-total-memory comparison. Request streams remain worker-specific and the overall operation budget remains fixed. Harness results are explicitly labeled; cache hit-rate, retention and eviction metrics are omitted because synthetic outcomes have no cache utility meaning. Their backend field is a launch identifier, not a measured implementation.

Use these conditions to narrow hypotheses. Better independent-cache scaling would support an effect of sharing, but its different memory/GC load still prevents attributing the entire difference to one lock. CPU, mutex and block profiles can then identify relevant call stacks. Eight workers with GOMAXPROCS four also introduces oversubscription; worker count is not a core count.

## Distinguish pressure policy from smaller retention

[pressure-control.json](../examples/pressure-control.json) uses a 64 MiB Go soft limit, 256-byte values and the same 400,000-key request space for:

- Capacity 200,000, pressure reclamation off.
- Capacity 200,000, pressure reclamation on.
- Capacity 100,000, pressure reclamation off.

The third condition controls for a cache that is smaller from the outset. It does not force identical instantaneous entries, eviction history or hit/miss composition. Keep the key space fixed so that shrinking capacity does not silently restore a 50% hit rate. Evaluate entries over time, hit rate and successful reads per second along with heap, throughput and allocation.

`read_hits_per_s` is computed from each run's observed hits and duration before aggregation. `read_misses_per_s` uses its misses. These are not products of median throughput, median read fraction and median hit rate. A cache-only benchmark assigns no database/network cost to a miss; those rates still do not predict end-to-end service latency.

Reports separate pressure tier 1, pressure tier 2, automatic slack and explicit compaction counters. Arena live/free/unallocated node counts describe structural state at phase end. Counters and allocation profiles help locate additional work but do not convert a correlation into exact per-compaction allocation cost.

This configuration opts into `sample_cache_stats=true` to record retention and pressure counters periodically. Stats calls can acquire locks and perturb the workload; create an otherwise identical observer-off configuration before comparing absolute throughput. Each cache reading carries its own timestamp, separate from runtime/RSS observations. A read that waited for a cache lock need not describe the heap observation taken earlier in that sampling iteration. Empty CSV fields mean no observation.

## Profile one phase at a time

[profiling.json](../examples/profiling.json) records CPU, mutex and block profiles for four workers using shared and independent caches. [profiling-arena.json](../examples/profiling-arena.json) records Arena allocation profiles for the three pressure-control conditions. [profiling-arena-fill.json](../examples/profiling-arena-fill.json) records 1,000,000-entry initial fill under an unlimited runtime-memory policy, matching the original study's scale and payload. Each job selects exactly one `profile` and an explicit `profile_phase` such as `fill` or `measured`.

Results include `profiles/<job-id>/profile.json`, profile files, an executable `analyze.sh` and notes. The analysis script embeds the recorded worker path. Keep that executable with the profiles or adjust the path if relocating it. The JSON records the selected phase/rate and whether recording completed. Failed profiles remain diagnostic artifacts and do not imply a successful measurement.

Examples of the underlying commands, using the actual paths from a result:

```sh
go tool pprof -top /path/to/worker /path/to/cpu.pprof
go tool pprof -top /path/to/worker /path/to/mutex.pprof
go tool pprof -top /path/to/worker /path/to/block.pprof
go tool pprof -top -sample_index=alloc_space \
  -base=/path/to/allocs-before.pprof /path/to/worker /path/to/allocs-after.pprof
```

The profiler's saved `analyze.sh` is preferable to manually guessing filenames. Profiled results are marked **PROFILED DIAGNOSTIC**. Sampling changes CPU, memory and scheduling; compare normal unprofiled repetitions separately. A mutex profile records sampled contention associated with lock-holder stacks, not a complete map of every lock's elapsed time. A block profile can also include runner barriers and channel waits. Read CPU, contention and allocation evidence together.

`profile_rate=0` uses defaults. Nonzero allocation rates are bytes between samples; smaller values increase detail and overhead. Mutex rates sample contention events; block rates are nanoseconds. CPU profiles use the runtime's fixed sampling behavior. The examples use a 64 KiB allocation rate, mutex rate one, and block rate 10,000 ns. Rates must remain recorded and comparable across diagnostic controls.

Allocation profiling starts before cache construction. Phase snapshots use two forced GCs before each profile snapshot, outside the workload timer, to publish delayed allocation-profile data. The saved before/after delta therefore differs from ordinary natural-GC performance runs and includes profiling-boundary/serialization allocations visible in their own stacks. Use it to locate allocation sources, not as an exact substitute for runtime allocated-byte deltas. CPU/mutex/block recording includes phase-boundary snapshots. No profile alone proves that a sampled allocation or contention site explains the entire measured throughput difference.

## Inspect Compact near its trigger

[compact-window.json](../examples/compact-window.json) fills 100,000 entries, deletes 75%, then issues read-only requests with two workers. Enabled and disabled arms use the same input budget, key space and trigger. Reads do not insert, so both preserve 25,000 entries through the request phase. This holds retention fixed while examining one explicit Compact; it answers a narrower question than write-heavy recovery.

`compact_at=0.5` triggers after half of worker zero's request stream. Both arms record `compact_probe` and entries before/after; only the enabled arm records an actual `concurrent_compaction` interval. The observed stats calls can acquire cache locks and are outside the Compact duration. Worker scheduling can delay the call or serialize it with requests.

`request_trace_every=64` records sampled adapter-call start/end times and outcomes, bounded by `max_request_samples=50000` per phase. The default example's `compact_window="50ms"` defines three fixed windows around the trigger: before-trigger, trigger-window, and after-trigger. Each uses completed sampled requests and its recorded duration. Estimated operations per second scales sampled counts by 64; it is not an exact throughput counter and is omitted if the trace overflowed. Windows clipped by the phase boundary and sparse latency samples produce warnings.

The separate `compact-overlap` selection contains sampled calls whose time intervals intersect the actual Compact call. It includes a call that began before Compact and waited until afterwards. It reports latency observations, not a throughput rate. The disabled control has no actual Compact overlap; use the fixed trigger windows to compare it with enabled. Whole-phase p99 can hide a brief stall, while a small overlap sample cannot establish a stable p99 either.

Inspect `request-trace.csv`, `compact-observations.csv`, raw `request_windows` and the report's window tables. Heap/RSS sample peaks remain observed lower bounds; a short Compact can finish between periodic samples. These closed-loop request measurements do not include an external arrival queue or demonstrate a service-level tail-latency guarantee.
