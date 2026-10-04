# Draft maintainer discussion: process-isolated memory/GC experiments

## Proposed scope

Provide an opt-in experiment runner to compare how the three go-lru backends trade retained memory, GC CPU, throughput and cache utility under matched workloads and Go runtime memory limits.

A separate worker links a typed adapter against a local checkout. A standard-library-only controller launches a fresh process per trial, preserving raw results and source provenance. Production cache code and core-module runtime dependencies are unchanged.

The implemented scope includes static post-GC footprint, natural-GC steady/reclamation workloads, tenant-prefix deletion, Compact during mixed requests, paired A/B execution, comparison integrity checks, sample-quality warnings and offline heap/RSS charts. Normalized metrics and individual median intervals aid interpretation without producing automatic significance or winner claims.

## Relationship to existing benchmarks

This runner includes workload/key/value generation in operation throughput. Existing upstream Go benchmarks remain useful for method-level cost and allocation analysis. The runner brings process-level memory pressure, GC events, retained entries and source provenance together. Its calibration configuration measures the combined cost of periodic sampling and request timing.

Before proposing upstream integration, discuss location, maintenance scope, configuration format, reference-worker packaging and licensing. The current standalone harness is MIT-licensed; the target go-lru library retains its Apache-2.0 license and notices. An upstream contribution would need to follow the destination project's contribution requirements.

## Validation accompanying a proposal

Actual map/radix/arena workers have been built and executed with Go 1.27.1 against a clean checkout at `6c2b8fa056eb77549eb8d2254a9940f52d92f9ff`. The [validation record](../validation/VALIDATION.md) distinguishes current integration evidence from earlier Go 1.23.2 fixture/reference checks. Historical reference results demonstrate harness behavior only.

A proposal should identify the exact commands, worker and source hashes, machine limits and raw result paths for the submitted implementation. A successful smoke suite is functional evidence, not a backend performance ranking. Validate pressure eviction under a suitably sized workload and inspect hit rate/retention before making a pressure-related claim. Upstream correctness and invariant tests remain separate from these harness boundary checks.

## Reproducible performance evidence

Build baseline and candidate workers from stable checkouts with the same harness source. Use `run-paired` to alternate version order while preserving matching workloads; keep instrumentation and runtime settings identical. Increase operation count and repetitions until the report's short-phase and low-sample warnings have been investigated. Review raw counts and the assumptions of each median interval.

Publish retained entries and hit rates beside memory figures, and preserve raw data supporting graphs. Avoid attributing a workload throughput change to one cache method without examining the existing microbenchmarks. The [methodology](METHODOLOGY.md) documents timing windows, denominators and the remaining sources of variability.
