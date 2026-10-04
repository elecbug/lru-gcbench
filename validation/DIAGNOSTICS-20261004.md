# Diagnostic follow-up — 2026-10-04

All **124 requested jobs completed successfully**, followed by all 22 profile analyses. The driver exited with status zero and the independent integrity audit passed. This is a diagnostic pilot with two repetitions per unprofiled condition and one per profile condition; it does not establish a new performance ranking.

A separate **four-job dense-trace follow-up also completed successfully**, bringing the total to **128 jobs**. Its instrumentation and operation budget differ; the 124-job scope and tables below remain separate from that supplement.

The [original performance study](PERFORMANCE-20261004.md) remains unchanged. Its observations motivated three follow-up questions: whether shared state explains poor concurrency scaling; which Arena allocation paths contribute during fill and pressure reclamation; and how instrumentation retention and local Compact effects alter measurements. The new controls distinguish these questions without assuming a particular lock, allocation site or GC mechanism is the cause.

## Implemented controls

- Five calibration arms separate no-buffer execution, retained-buffer-only execution, runtime sampling, request timing and their combination. Four arms allocate, touch and retain the same runtime-sample buffer capacity before baseline GC.
- Shared and independent cache modes use the same request-generation machinery. Independent mode creates a full configured cache per worker, so total capacity, fills and memory grow with worker count. A separate harness control bypasses the real cache while retaining key generation, payload work and bookkeeping; synthetic cache-utility metrics are omitted.
- Pressure controls retain the same 400,000-key request space under a 64 MiB soft limit: capacity 200,000 with pressure reclamation off/on, and capacity 100,000 with reclamation off. Periodic cache-stat observations track retention with their own timestamps and can acquire cache locks.
- CPU, mutex and block profiles cover shared and independent request phases. Arena allocation profiles examine the three pressure conditions and a separate 1,000,000-entry initial fill under an unlimited runtime-memory policy.
- Compact enabled/disabled controls use read-only requests after deleting 75% of a 100,000-entry cache. Both record the same workload-trigger position and observed retained entries. Timestamped request samples support fixed trigger windows and a separate selection of calls overlapping the actual Compact interval.

Reports now compute read-hit and read-miss throughput for each run before aggregation, separate pressure tier 1/tier 2, automatic-slack and explicit compaction counters, and expose Arena structural state. They label synthetic, independent, profiled and cache-observer conditions. Window rates are sampled estimates; incomplete request traces omit rate estimates. GC-pause p99 remains a secondary diagnostic with explicit event-count warnings.

See [DIAGNOSTICS.md](../docs/DIAGNOSTICS.md) for configuration fields, measurement boundaries and profile interpretation.

## Pilot scope and provenance

The recorded driver used `pilot` mode for 124 fresh worker processes: 102 unprofiled jobs and 22 profiled jobs. Unprofiled conditions have two independent repetitions; profile conditions have one. The full 532-job protocol was not run as part of this follow-up. The later [732-job study](REPEATED-20261004.md) provides repeated performance comparisons and profiles.

| Suite | Jobs requested | Pilot settings |
|---|---:|---|
| Calibration | 30 | Five arms × three backends × two repetitions; 2,000,000 requests |
| Shared/independent topology | 36 | Two modes × workers 1/4/8 × three backends × two repetitions; 1,000,000 requests |
| Harness control | 6 | Workers 1/4/8 × two repetitions; one synthetic backend label; 1,000,000 requests |
| Pressure controls | 18 | Three capacity/policy conditions × three backends × two repetitions; 2,000,000 requests |
| Compact windows | 12 | Enabled/disabled × three backends × two repetitions; 2,000,000 read-only requests |
| CPU/mutex/block profiles | 18 | Three profile kinds × shared/independent modes × three backends; four workers |
| Arena initial-fill allocation profile | 1 | 1,000,000 entries, 256-byte values, unlimited runtime-memory policy |
| Arena pressure allocation profiles | 3 | Three capacity/policy controls under 64 MiB |

Warmups are capped at 100,000 operations. Capacities, key spaces, payloads, runtime policies and instrumentation settings retain the full examples' values. Suites and worker processes run sequentially. Profile analysis runs only after timed jobs finish. The table above records this execution's operation budgets; the [pilot driver](../scripts/run-diagnostics.sh) implements the reduction from the example configurations.

| Item | Recorded value |
|---|---|
| Go | `go1.27.1`, linux/amd64 |
| CPU | Intel Xeon Gold 6434 |
| Upstream commit | `6c2b8fa056eb77549eb8d2254a9940f52d92f9ff` |
| Worker SHA256 | `c34e09ef233d186af71ab2c4300882ac847c483196e0cf5c018ba9f3f899d038` |
| Harness source SHA256 | `d47a6438d946b67659355f9f202f95584d05d0444683b39081dbfe8108db3186` |

The [diagnostic driver](../scripts/run-diagnostics.sh) runs the pilot suites sequentially and performs profile analysis after measurement. The [diagnostic guide](../docs/DIAGNOSTICS.md) explains the configuration controls and execution workflow.

## Validation completed before execution

The following repository checks passed before the diagnostic worker was built and timed runs began:

```sh
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

Tests cover matched calibration/control configurations, synthetic-metric suppression, exact per-run hit/miss rates, separate reclamation counters, cache-observation timestamps, bounded request traces and window summaries, profile generation and lifecycle, and existing report/comparison behavior. Passing these checks establishes tested implementation behavior; it does not establish benchmark precision or a causal performance explanation.

The integrity audit checked eight suites, 124 jobs, 371 phases and 22 profile records; it independently recomputed 42 request windows and 484 selected summary distributions. Worker/source provenance, workload accounting and request-trace accounting matched. There were no dropped runtime or request samples, and no forced-GC count deltas within timed natural-GC phases. Allocation-profile boundary collections occur outside those phase windows. These checks establish internal consistency, not significance or absence of observer effects. The [diagnostic verifier](../scripts/verify-diagnostics.py) implements the reusable checks.

## Preliminary observations

The following unprofiled values are descriptive medians of **two** worker results. They must not be pooled with the historical study or the profiled conditions.

### Calibration controls retain comparable buffer memory

Across backend/arm medians, baseline heap was approximately 149.50–149.73 KiB without retained buffers, compared with 2029.34–2032.31 KiB in the four retained-buffer arms. All four used the same 10,000-slot buffer capacity. These observations are consistent with the intended retention control; process heap is not byte-for-byte identical between fresh workers.

| Backend | Buffer only, Mops/s | Buffer + runtime, Mops/s | Buffer + request timing, Mops/s | Buffer + both, Mops/s |
|---|---:|---:|---:|---:|
| Map | 3.712 | 3.617 | 4.089 | 3.663 |
| Radix | 1.551 | 1.406 | 1.446 | 1.446 |
| Arena | 3.161 | 2.508 | 2.916 | 2.800 |

The controls now separate the intended instrumentation changes, but this short pilot still cannot assign a precise overhead percentage. In particular, Map request-timing-only throughput exceeded buffer-only throughput in these observations. More repetitions and longer measurement windows are needed before interpreting such differences.

### Shared state is a credible concurrency cost

GOMAXPROCS was fixed at four, with 1,000,000 requests per unprofiled run:

| Backend / topology | 1 worker, Mops/s | 4 workers, Mops/s | 8 workers, Mops/s |
|---|---:|---:|---:|
| Map shared | 2.933 | 1.141 | 0.692 |
| Map independent | 2.964 | 6.035 | 6.533 |
| Radix shared | 1.252 | 0.371 | 0.391 |
| Radix independent | 1.380 | 2.886 | 2.090 |
| Arena shared | 2.330 | 0.667 | 0.676 |
| Arena independent | 1.947 | 5.180 | 4.930 |
| Harness control, no real cache | 4.755 | 13.122 | 13.117 |

The runner control and independent caches scaled differently from the shared caches. This supports investigating shared-cache synchronization rather than assigning the entire regression to the runner. Independent caches retained 100,000, 400,000 and 800,000 total entries as worker count increased; shared caches retained 100,000. Their different live memory and GC activity remain a material confound. Several one-million-request phases were shorter than one second.

Separate mutex-profile runs used 2,000,000 requests, four workers and one profile per condition:

| Backend | Shared sampled mutex delay | Independent sampled mutex delay |
|---|---:|---:|
| Map | 3.25745 s | 504.71 µs |
| Radix | 16.13 s | 107.17 µs |
| Arena | 6.71 s | 523.70 µs |

Shared profiles attributed almost all sampled mutex delay to cache Get/Put unlock stacks. Independent profiles primarily showed small runtime/allocator contention. For Arena, shared Get and Put paths accounted for 69.86% and 30.13% of the sampled mutex delay respectively. This is evidence of actual cache contention in the profiled conditions. Sampled delay sums waiting across goroutines and is not request wall time; it does not quantify what fraction of the historical throughput loss came from locks. Block profiles also include worker joins, WaitGroups and sampler waits. Nested cumulative profile rows must not be added together.

### Pressure controls separate cache utility and dynamic recovery

All conditions use the same 400,000-key request space and 64 MiB soft limit. These unprofiled runs enable the periodic cache observer:

| Backend / condition | Mops/s | Million read hits/s | Hit rate | B/op | Tier 2 compactions |
|---|---:|---:|---:|---:|---:|
| Map 200k, off | 1.125 | 0.506 | 49.98% | 82.52 | 0 |
| Map 200k, on | 0.795 | 0.179 | 25.00% | 102.24 | 1 |
| Map 100k, off | 0.685 | 0.154 | 24.98% | 87.33 | 0 |
| Radix 200k, off | 0.550 | 0.248 | 49.98% | 82.37 | 0 |
| Radix 200k, on | 0.709 | 0.161 | 25.29% | 94.52 | 5.5 |
| Radix 100k, off | 0.822 | 0.185 | 24.98% | 87.72 | 0 |
| Arena 200k, off | 0.955 | 0.430 | 49.98% | 77.69 | 0 |
| Arena 200k, on | 0.869 | 0.199 | 25.47% | 149.56 | 5 |
| Arena 100k, off | 1.256 | 0.282 | 24.98% | 79.92 | 0 |

Fractional compaction counts are medians of two integer counts. Tier 1, automatic-slack and explicit compaction deltas were zero in these measured pressure phases. Read-hit rates are computed per run before taking the median; they do not multiply median throughput by median hit rate. Radix reclamation-on had higher total operation throughput than the large off condition but lower successful-read throughput. This illustrates why a memory/throughput result needs cache utility alongside it.

The small off control brings hit rate close to the dynamic-on condition without reproducing its allocation volume. For Arena, 149.56 B/op with dynamic reclamation exceeded both off controls, 77.69 and 79.92 B/op. Its median ending retention was 102,249 entries; the small off control retained 100,000. This narrows the additional-allocation question, without assuming their retention histories were identical or ignoring Stats-observer overhead.

### Allocation profiles locate Arena growth and rebuild work

The separate one-million-entry Arena fill profile recorded 891.175 runtime B/op, with zero compactions. Its sampled allocation delta attributed 55.32% to `allocateNode`, 28.53% to value payload creation, 8.17% to flat `Put` allocations and 5.37% to keys. This makes node-storage growth a concrete investigation target for initial construction; the profile does not establish a particular capacity-reservation fix.

The three Arena pressure allocation profiles recorded 77.718 B/op for 200k off, 136.639 B/op for 200k on, and 79.963 B/op for 100k off. The on run recorded four tier-2 compactions and 146,424 pressure evictions; both off runs recorded no compactions. In the on profile, `compactDataStructuresLocked` accounted for 18.88% of flat sampled allocated bytes and `allocateNode` for 17.73%. These disjoint flat sites total 36.61%; `shedAndCompactLocked` contributed another 3.62% at its own flat allocation site. Rebuild and subsequent node growth are therefore observed additional allocation paths, beyond the effect of simply retaining a smaller cache.

These are single **profiled** runs with forced profile-boundary collections and a disabled periodic cache observer. Their allocation figures differ from the unprofiled two-run medians above and must remain separate.

### Compact state is matched, but overlap samples are sparse

All twelve enabled/disabled runs observed exactly 25,000 entries before and after the trigger. Each retained 31,250 request observations with zero drops. None of the 42 fixed/overlap analysis windows was clipped by a phase boundary.

| Backend | Actual Compact duration, two enabled runs | Sampled calls overlapping Compact |
|---|---|---|
| Map | 12.422 ms, 4.981 ms | 0, 0 |
| Radix | 0.006860 ms, 0.006874 ms | 0, 0 |
| Arena | 14.822 ms, 12.467 ms | 0, 1 |

The read-only controls remove the earlier confound from writes growing the cache during Compact. They also reveal a measurement limitation: sampling every 64 requests almost entirely missed calls overlapping the single Compact. The one Arena observation cannot establish an overlap p99, and zero observations do not establish zero stalls.

Fixed 50 ms trigger windows still provide a local completion-rate observation. Arena's enabled windows each retained 818 request samples, versus 1,131 and 1,097 in the disabled runs; these are sampled completion counts rather than exact operation totals or population-tail estimates. A follow-up focused on individual Compact stalls should increase request-trace density and recalibrate its overhead.

## Supplemental dense Compact trace

The sparse overlap coverage motivated four additional Arena runs: Compact enabled/disabled with two repetitions each, 400,000 read-only requests, and `request_trace_every=1`. Capacity, 75% deletion, two request workers and the original worker binary remained unchanged. Both arms retained 400,000 observation slots, versus 31,250 actual slots in the original pilot, allocated before the request phase but after baseline GC. This is a separate instrumentation regime with additional timing and live-memory costs, not another repetition of the earlier experiment.

The supplemental audit passed: all 1,600,000 request observations were retained without drops, observed entries stayed at 25,000, fixed-window/histogram calculations matched and timed phases contained no forced-GC count increments. The two enabled Compact calls lasted 13.132 and 7.440 ms. Their overlapping request sets contained six and 127 calls respectively, including short calls during the Compact call interval; each set contained two calls longer than 1 ms. Those individual requests lasted **13.138/13.171 ms** and **7.445/7.460 ms**. The disabled controls' 50 ms trigger-window maxima were **0.150 and 0.208 ms**.

The denser trace therefore captured individual long requests associated with the Compact interval that sparse sampling had missed. These observations do not isolate lock-holding time or establish a stable population p99.

## Interpretation limits

Two repetitions are a diagnostic pilot, not a replacement for the original ten-repetition performance study. Some phases may be short or have few GC pauses, request-tail samples or observations near Compact. Profile results perturb the workload and use one process per condition; their throughput is not compared with unprofiled runs as an ordinary performance result.

Independent caches remove cross-worker sharing while increasing total live state. The harness control has no real cache. The pressure-control traces enable periodic Stats calls without a separate observer-off repetition matrix in this pilot, so their throughput includes possible observation locking. Smaller-capacity and dynamically reclaimed caches need not have identical retention histories or hit/miss composition even if final entry counts are similar.

Allocation-profile snapshots use forced collections outside the workload timer to publish delayed profile data. Their delta includes profile-boundary allocations visible in the stacks and is not exact natural-GC workload allocation accounting. Retained heap, scannable heap, GC CPU and allocation volume remain different quantities.

Compact-window rates scale timestamped request samples; they are not exact per-window operation counts. Overlapping-call latency has no throughput denominator. These closed-loop measurements omit an external arrival queue, and brief memory peaks can fall between periodic samples. The controls can support narrower hypotheses; none alone identifies the entire cause of a throughput difference.

## Run a new study

For a fresh repeated follow-up, use the [benchmark workflow](../scripts/run-benchmark.sh) from the repository root with a new run identifier and a clean upstream checkout:

```sh
make benchmark RUN_ID=my-new-study UPSTREAM_REPO=/path/to/go-lru
```

This builds and validates the tools before running the repeated protocol. It runs the larger study described in [REPEATED-20261004.md](REPEATED-20261004.md), not the short pilot summarized here. Use the [diagnostic driver](../scripts/run-diagnostics.sh) and [configuration guide](../docs/DIAGNOSTICS.md) when selecting a pilot instead.
