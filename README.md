# lru-gcbench

Measure memory, GC and workload throughput for `google/go-lru`'s `map`, `radix` and `arena` backends in separate worker processes. The controller uses the Go standard library; a generated, typed adapter links against your local go-lru checkout without modifying or fetching it.

[Measurement protocol](docs/METHODOLOGY.md) · [Validation record](validation/VALIDATION.md)

## Quick start

Run these commands from `lru-gcbench/`, with an existing checkout at `../go-lru`.

The controller requires Go 1.23+. Workers require the Go version declared in the target checkout's `go.mod`; the validated checkout declares Go 1.26 and was run with Go 1.27.1. Linux provides RSS measurements; RSS is omitted elsewhere. Git enables commit/dirty-state recording. Compiler downloads depend on Go's `GOTOOLCHAIN` setting and network availability.

```sh
go build -o bin/lrugcbench ./cmd/lrugcbench
run_id=$(date +%Y%m%d-%H%M%S)-$$
./bin/lrugcbench build -repo ../go-lru -source . -out "bin/worker-$run_id"
./bin/lrugcbench run -worker "bin/worker-$run_id" \
  -config examples/smoke.json -out "results/smoke-$run_id" -label smoke
```

This executes **18 fresh processes**: 3 backends × 3 cases × 2 repetitions. Open `report.html` or read `report.md` in the output directory. HTML includes metric/sample-count tables, quality warnings and heap/RSS plots with phase markers. It works offline without remote scripts.

Worker builds and runs refuse existing output paths. `report -dir <run>` regenerates derived reports from an existing run. Smoke runs establish functionality; warnings flag short phases and inadequate repetition/latency/GC sample counts before performance interpretation.

## Compare two versions

Build both workers using the same harness source and stable target checkouts:

```sh
comparison_id=$(date +%Y%m%d-%H%M%S)-$$
./bin/lrugcbench build -repo ../go-lru-base -source . -out "bin/base-$comparison_id"
./bin/lrugcbench build -repo ../go-lru-candidate -source . -out "bin/candidate-$comparison_id"
./bin/lrugcbench run-paired \
  -base-worker "bin/base-$comparison_id" \
  -candidate-worker "bin/candidate-$comparison_id" \
  -config examples/smoke.json -out "results/paired-$comparison_id"
```

Each pair runs the same job in baseline/candidate workers consecutively, alternating A/B and B/A order across pairs. All processes run sequentially. Results are saved under `base/`, `candidate/`, `comparison/`; `paired.json` records the schedule and status. Comparison is generated only after every job and the integrity checks succeed.

For existing separate runs:

```sh
./bin/lrugcbench compare -base results/base-run -candidate results/candidate-run \
  -out results/comparison-new
./bin/lrugcbench report -dir results/base-run
```

Comparison checks complete settings, successful repetition/seed identities, workload checksums and operation counts. Harness source hashes and environment fingerprints must also match by default. Explicit `-allow-harness-diff` and `-allow-env-diff` overrides are recorded and never bypass workload checks. Different configurations remain unmatched groups.

Outputs describe differences of medians. Individual median confidence intervals are available with sufficient samples; these are not significance tests of the version difference. Paired scheduling and matching fingerprints do not control temperature, CPU frequency or host contention.

## Choose an experiment

| Configuration | Purpose | Jobs |
|---|---|---:|
| [smoke.json](examples/smoke.json) | Basic actual-backend integration | 18 |
| [extended-smoke.json](examples/extended-smoke.json) | DeletePrefix reclamation and Compact during requests | 18 |
| [calibration.json](examples/calibration.json) | Matched instrumentation off/on workloads | 60 |
| [pressure.json](examples/pressure.json) | Pressure reclamation off/on at 32/64/128 MiB | 90 |
| [study.json](examples/study.json) | Wider GOGC/memory-limit study, 10 repetitions | 360 |
| [reference-validation.json](examples/reference-validation.json) | Harness validation without go-lru | 6 |

`GOMEMLIMIT` is a soft Go runtime limit, not an RSS cap. Start small. Read retained-entry counts, hit rates and pressure evictions alongside memory results.

| Scenario | Phases |
|---|---|
| `footprint` | Fill, delete, Compact; a forced GC follows each phase to measure retained memory |
| `steady` | Fill, warmup, measured mixed operations; natural GC after construction |
| `reclaim` | Fill, delete, Compact, mixed recovery; natural GC after construction |
| `concurrent-compact` | Fill, delete, mixed operations with one Compact inside the workload phase |

`delete_mode` defaults to `keys` (individual Delete); `prefix` uses one DeletePrefix per selected tenant group, requires `key_kind="prefix"` and a delete fraction in multiples of 1/16. Delete operation counts are API calls, not removed entries. `concurrent-compact` requires latency sampling and at least two operations per worker. Scheduling determines actual request/Compact contention.

Unknown configuration fields are rejected. Capacity is in entries, independent of `GOMEMLIMIT`. `workers` controls mixed-request concurrency, independently of GOMAXPROCS. Keys are generated per request with uniform accesses. Read misses never insert. Scalar values are 16 bytes; byte payloads are independently allocated and touched on each Put.

## Interpret the results

Throughput and allocations include key creation, random selection, payload creation and bookkeeping. Compare them with [upstream operation microbenchmarks](docs/METHODOLOGY.md#cache-operation-microbenchmarks) when attributing costs. Sampled request latency excludes key generation but includes Put payload allocation.

- `allocated_bytes_per_op` and `allocated_objects_per_op`: workload allocation cost.
- `gc_cpu_ns_per_op`: GC CPU per operation for natural-GC phases without forced collections.
- `post_gc_heap_delta_per_entry_bytes`: approximate incremental post-GC process heap per retained entry.
- Get/Put and GC p99: histogram bucket intervals; always inspect sample counts.
- Sampled heap/RSS peaks: lower bounds on the true peaks.

Set `sample_interval="0"` to disable periodic sampling and `latency_sample_every=0` to disable request timing. [calibration.json](examples/calibration.json) compares both disabled against both enabled under the same input stream. The bounded trace retains its earliest samples and reports later drops. Full boundaries, metric definitions and warning thresholds are in [METHODOLOGY.md](docs/METHODOLOGY.md).

A run contains `manifest.json` (config, order, provenance, status), `raw/` requests/results/stderr, `summary.json`, `summary.csv`, `samples.csv`, `report.md` and `report.html`. Failed or timed-out jobs keep diagnostics and never become zero-valued successful measurements; the command exits unsuccessfully if any job fails.

## Development and validation

```sh
go test -race ./...
go vet ./...
```

Build an actual worker and run smoke for upstream integration. `cmd/reference-worker` and `validation/reference-*` test the harness only and **are not google/go-lru performance evidence**. [VALIDATION.md](validation/VALIDATION.md) separates actual-backend validation from historical authoring-environment checks.

Code lives in `cmd/lrugcbench` (CLI), `bench` (execution, metrics, reports), `internal/buildworker` (typed adapter generation) and `internal/reference` (validation-only cache). `example.com/lrugcbench` is a local module identifier, not a published package claim.

## License

The benchmark harness is [MIT-licensed](LICENSE), copyright (c) 2026 elecbug. Separately built/linked `google/go-lru` remains under its upstream Apache-2.0 license. See [third-party notices](THIRD_PARTY_NOTICES.md) and the [preserved upstream license](licenses/google-go-lru-APACHE-2.0.txt). This independent project is not an official Google product.
