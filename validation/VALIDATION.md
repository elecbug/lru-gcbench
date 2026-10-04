# Validation record — 2026-10-04

The subsequent [performance study record](PERFORMANCE-20261004.md) covers the rebuilt harness and longer experiment settings. The [732-job repeated study](REPEATED-20261004.md) documents later comparisons and profiling. The checks below document the earlier functional validation.

## Actual upstream integration

**The actual google/go-lru implementation was built and executed.** The historical authoring environment described below had supported only fixture/reference validation.

The original actual-backend smoke validation recorded:

| Evidence | Recorded result |
|---|---|
| Target | `github.com/google/go-lru` |
| Target commit | `6c2b8fa056eb77549eb8d2254a9940f52d92f9ff` |
| Target state | Clean |
| Target source SHA256 | `18204242453258526c05f13a9f61e8defb37346bbb80099d24ad69c649d8c346` |
| Runtime toolchain/platform | `go1.27.1 linux/amd64` |
| Actual backends | `map`, `radix`, `arena` |
| Original smoke result | 18/18 jobs with status `ok` |

This baseline preceded the comparison, reporting and scenario improvements. It established functional integration, not a performance ranking or validation of the later chart and warning features.

## Validation of the improvements

The improved source was validated with **Go 1.27.1 on Linux amd64**. A fresh worker was built against the same clean upstream commit shown above. Unit/integration tests cover comparison rejection paths, paired scheduling/failures, normalized metrics and median intervals, HTML output, prefix deletion and concurrent compaction.

| Improvement validation check | Recorded result |
|---|---|
| `go test -race -count=1 ./...` | PASS |
| `go vet ./...` | PASS |
| Actual worker build | PASS |
| Standard smoke | PASS — 18/18 jobs |
| Extended smoke: prefix footprint/reclaim and concurrent Compact | PASS — 18/18 jobs |
| Paired execution control | PASS — 18 baseline jobs and 18 candidate jobs |
| Reduced pressure validation | PASS — 12/12 jobs |
| HTML structure/assets | PASS — parsed output contains tables and one SVG per job, with no remote assets or rejected template URLs |
| License preservation | PASS — copied upstream license matches the original; upstream checkout remains clean |

The paired control uses **the same worker on both sides** to exercise orchestration and comparison integrity. It produces 1,038 metric rows with no unmatched groups. Observed differences describe repeated execution variability and are not evidence of a version change.

The improvement-validation worker had executable SHA256 `f8f66d77f67cf83e1b9d5b5ef5e828db2749ea1095ca0ab213f72f16af18d9b8` and harness source SHA256 `1b72614e257232be4ad9d5bec155176c76d857cc54e6d17d60ac9e6d5d76c22c`. These identify this validation build, not future builds after source changes.

The [reduced pressure config](improvements-20261004/pressure-validation.json) uses capacity 20,000, key space 40,000, 10,000 warmup operations, 100,000 measured operations, one repetition, limits of 12/32 MiB, and pressure reclamation off/on. Across each 12 MiB pressure-on job, recorded pressure evictions were:

| Backend | Pressure evictions |
|---|---:|
| map | 18,284 |
| radix | 18,258 |
| arena | 18,192 |

All pressure-off jobs recorded zero pressure evictions. At 32 MiB, both settings recorded zero. These checks establish that the pressure path was exercised; they do not rank backend memory efficiency. The eviction counts alone do not establish a memory saving at equal retention or hit rate. The then-current 90-job pressure study and 60-job calibration experiment were not run as part of this functional validation. The pressure example has since been expanded to 10 repetitions (180 jobs); later performance evidence is recorded separately.

Reproduce the main checks from `lru-gcbench/` using the [README commands](../README.md). Use the tracked [reduced pressure configuration](improvements-20261004/pressure-validation.json) for that check and build a fresh worker so the tested source matches the current harness.

## Historical authoring environment

The original harness was authored with `go1.23.2 linux/amd64`. GitHub cloning and Go-proxy module access were unavailable there, while the inspected target required Go 1.26. That environment inspected the public API and ran fixture/reference tests; it did not build or execute the actual upstream implementation.

The following table summarizes the checks performed in that authoring environment. These are historical observations, not claims that the current source produced identical test counts or coverage.

| Historical check | Recorded result |
|---|---|
| `go test -count=1 -json ./...` | PASS — 21 top-level tests, 24 including subtests, no failures |
| `go test -race -count=1 ./...` | PASS |
| `go vet ./...` | PASS |
| Generated worker syntax and API-only fixture compilation | PASS; fixture constructor panics and cannot validate upstream runtime behavior |
| Reference batches A and B | PASS — 6/6 fresh-process jobs each |
| Same-seed workload checksums | PASS |
| Effective GOMAXPROCS / GOMEMLIMIT recording | PASS |
| Forced-GC separation from timed windows | PASS |
| Bounded trace | PASS — no drops in those batches |
| Baseline/candidate comparison | PASS — no unmatched groups |
| Existing-output refusal, unsupported-backend rejection | PASS |
| Timeout/error reporting, invalid cache outcome refusal | PASS |
| Windows amd64 controller/reference cross-build | PASS |
| Windows runtime execution | NOT PERFORMED |
| Actual upstream build/run in that authoring environment | NOT PERFORMED |

Historical statement coverage was **83.5%** for `bench` and **73.6%** for the builder package. These are package-specific statement coverage figures for that source version, not current coverage or a whole-project percentage.

**Reference runs are not go-lru measurements.** They validate the harness's process, metric and report plumbing. The two-repetition reference batches support functional checks only. They establish no GC advantage or backend ranking.

## Interpretation limits

Smoke and reduced pressure checks establish that scenarios execute and boundary checks hold. They do not establish stable p99 estimates or statistically meaningful version differences. Inspect sample-quality warnings, actual pressure evictions, hit rates, retained entries and dropped samples before designing a longer experiment. Upstream invariant, differential and fuzz validation is a separate responsibility; harness boundary checks do not replace it.
