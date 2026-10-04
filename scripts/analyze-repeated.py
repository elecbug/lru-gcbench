#!/usr/bin/env python3
"""Predeclare and analyze repeat-matched cache experiments without rerunning them.

Run --plan-only after recording configs and before measurement. Normal analysis
requires that saved plan and completed suites. --self-test checks pure math only.
Profiles are audited as artifacts but excluded from performance inference.
"""

import argparse
import collections
import csv
import datetime
import hashlib
import heapq
import json
import math
from pathlib import Path
import shlex
import shutil
import statistics
import sys
import runpy

SCHEMA = 1
SUITES = (
    "calibration", "topology-study", "harness-control", "pressure-performance",
    "pressure-observer", "compact-dense", "profiling", "profiling-arena",
    "profiling-arena-fill",
)
PROFILE_SUITES = {"profiling", "profiling-arena", "profiling-arena-fill"}
CORE_METRICS = ("workload_ops_per_s", "allocated_bytes_per_op", "gc_cpu_ns_per_op")
REQUEST_PHASES = {"measured", "concurrent", "recovery"}
CASE_DEFAULTS = {
    "name": "", "cache_mode": "shared", "retain_sample_buffer": False,
    "sample_cache_stats": False, "profile": "", "profile_phase": "", "profile_rate": 0,
    "compact_mode": "enabled", "compact_at": .5, "compact_window": "50ms",
    "request_trace_every": 0, "max_request_samples": 0, "scenario": "steady",
    "capacity": 10000, "key_space": 0, "key_kind": "flat", "value_kind": "scalar",
    "value_bytes": 0, "pressure_reclamation": False, "workers": 1,
    "warmup_ops": 10000, "operations": 200000, "read_percent": 90,
    "delete_mode": "keys", "delete_fraction": .75, "sample_interval": "50ms",
    "max_samples": 2048, "latency_sample_every": 0,
}
METHOD = {
    "pairing": "Identical repeat and seed, with workload checksum/count checks except declared worker-count contrasts. Global shuffled execution is not adjacent temporal blocking.",
    "effect": "exp(median(log(candidate/baseline))); with even n this is the geometric mean of the two central paired ratios, not the ratio of separate medians.",
    "interval": "Smallest symmetric binomial/order-statistic interval with coverage >=95%, transformed from log ratios. At n=10, ranks2 and9 give97.8515625% coverage. These intervals are not simultaneous or multiplicity-adjusted.",
    "test": "Conservative two-sided sign test of median log ratio zero: min(1,2*P[Binomial(n,0.5)>=max(positive,negative)]). Exact ties remain in n.",
    "multiplicity": "Holm adjustment within each predeclared contrast-kind/metric family and also globally across all declared hypotheses. Non-estimable hypotheses count as p=1. Within-family control is not global control.",
    "missing_values": "Every contrast requires the complete matching repeat set. Any missing, nonfinite or nonpositive metric makes its log-ratio inference non-estimable; pairs are never silently dropped.",
    "assumptions": "Coverage requires independent repeat effects from a common distribution; fixed workload seeds, machine drift and shared host activity limit this interpretation. Findings remain specific to the recorded workloads.",
    "latency": "Whole-phase and window p99 values are histogram upper bounds of instrumented calls. Compact window maximum is an observed individual call, not a population tail parameter.",
    "profiles": "Profiled runs are excluded from performance contrasts and headline descriptives. Their profile kind, phase, rate, completion and files are audited; separate analysis normalizes sampled attribution per operation.",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def load(path):
    return json.loads(path.read_text())


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def dump(path, value):
    path.write_text(json.dumps(value, indent=2, allow_nan=False) + "\n")


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def normal_case(case):
    out = {**CASE_DEFAULTS, **case}
    for key in ("cache_mode", "compact_mode", "compact_window", "delete_mode"):
        if not out[key]:
            out[key] = CASE_DEFAULTS[key]
    if not out["compact_at"]:
        out["compact_at"] = .5
    if not out["key_space"]:
        out["key_space"] = out["capacity"] * 2
    return out


def selector(suite, backend, case, runtime):
    return {"suite": suite, "backend": backend, "case": case["name"],
            "runtime": runtime["name"], "phase": "concurrent" if case["scenario"] == "concurrent-compact" else "measured"}


def select_key(sel):
    return tuple(sel[field] for field in ("suite", "backend", "case", "runtime", "phase"))


def eligible_metrics(base_case, candidate_case, kind):
    a, b = normal_case(base_case), normal_case(candidate_case)
    names = list(CORE_METRICS)
    if a["cache_mode"] != "harness" and b["cache_mode"] != "harness" and a["read_percent"] > 0 and b["read_percent"] > 0:
        names.append("read_hits_per_s")
    if a["latency_sample_every"] and b["latency_sample_every"]:
        if a["read_percent"] and b["read_percent"]:
            names.append("get_p99_upper_seconds")
        if a["read_percent"] < 100 and b["read_percent"] < 100:
            names.append("put_p99_upper_seconds")
    if kind == "compact":
        names.extend(("window_trigger_ops_per_s", "window_trigger_get_p99_upper_seconds", "window_trigger_get_max_seconds"))
    return names


def generate_plan(root):
    configs, suites, contrasts = {}, [], []
    for name in SUITES:
        path = root / "configs" / (name + ".json")
        config = load(path)
        require(config["repetitions"] == (6 if name in PROFILE_SUITES else 10), "unexpected declared repeat count: " + name)
        configs[name] = config
        count = config["repetitions"] * len(config["cases"]) * len(config["runtimes"]) * len(config["backends"])
        suites.append({"name": name, "config_sha256": sha(path), "jobs": count, "profiled": name in PROFILE_SUITES})

    def add(kind, left, right, a, b, allowed=(), different_workload=False):
        require(left["runtime"] == right["runtime"] and left["phase"] == right["phase"], "incompatible planned phase/runtime")
        aa, bb = normal_case(a), normal_case(b)
        differences = {field for field in set(aa) | set(bb) if aa.get(field) != bb.get(field)}
        require(differences <= set(allowed), "unplanned case differences: " + str(sorted(differences - set(allowed))))
        for metric in eligible_metrics(a, b, kind):
            contrasts.append({"id": f"contrast-{len(contrasts) + 1:04d}", "kind": kind,
                              "family": kind + "/" + metric, "metric": metric,
                              "baseline": left, "candidate": right,
                              "allowed_case_differences": sorted(allowed),
                              "actual_case_differences": sorted(differences),
                              "allow_workload_difference": different_workload, "expected_pairs": 10})

    for name in SUITES:
        if name in PROFILE_SUITES or name == "harness-control":
            continue
        config = configs[name]
        require("map" in config["backends"], "backend baseline map missing")
        for case in config["cases"]:
            require(not case.get("profile"), "profile case in an unprofiled suite")
            for runtime in config["runtimes"]:
                for backend in ("radix", "arena"):
                    require(backend in config["backends"], "backend contrast missing")
                    add("backend", selector(name, "map", case, runtime), selector(name, backend, case, runtime), case, case)

    config = configs["topology-study"]
    by_topology = {(normal_case(c)["cache_mode"], c["workers"]): c for c in config["cases"]}
    for runtime in config["runtimes"]:
        for backend in config["backends"]:
            for workers in (1, 4, 8):
                a, b = by_topology[("shared", workers)], by_topology[("independent", workers)]
                add("topology", selector("topology-study", backend, a, runtime), selector("topology-study", backend, b, runtime), a, b, ("name", "cache_mode"))
            for mode in ("shared", "independent"):
                a = by_topology[(mode, 1)]
                for workers in (4, 8):
                    b = by_topology[(mode, workers)]
                    add("workers", selector("topology-study", backend, a, runtime), selector("topology-study", backend, b, runtime), a, b, ("name", "workers"), True)

    config = configs["harness-control"]
    by_worker = {c["workers"]: c for c in config["cases"]}
    for runtime in config["runtimes"]:
        for backend in config["backends"]:
            for workers in (4, 8):
                a, b = by_worker[1], by_worker[workers]
                add("harness-workers", selector("harness-control", backend, a, runtime), selector("harness-control", backend, b, runtime), a, b, ("name", "workers"), True)

    config = configs["calibration"]
    by_name = {c["name"]: c for c in config["cases"]}
    pairs = [("no-buffer-no-timing", "retained-buffer-only"),
             ("retained-buffer-only", "buffer-runtime-sampling"),
             ("retained-buffer-only", "buffer-request-timing"),
             ("retained-buffer-only", "buffer-runtime-and-request")]
    for runtime in config["runtimes"]:
        for backend in config["backends"]:
            for first, second in pairs:
                a, b = by_name[first], by_name[second]
                add("calibration", selector("calibration", backend, a, runtime), selector("calibration", backend, b, runtime), a, b,
                    ("name", "retain_sample_buffer", "sample_interval", "latency_sample_every"))

    for name in ("pressure-performance", "pressure-observer"):
        config = configs[name]
        by_setting = {(c["capacity"], c["pressure_reclamation"]): c for c in config["cases"]}
        for runtime in config["runtimes"]:
            for backend in config["backends"]:
                for target in ((200000, True), (100000, False)):
                    a, b = by_setting[(200000, False)], by_setting[target]
                    add("pressure", selector(name, backend, a, runtime), selector(name, backend, b, runtime), a, b, ("name", "capacity", "pressure_reclamation"))
    observer = configs["pressure-observer"]
    by_setting = {(c["capacity"], c["pressure_reclamation"]): c for c in observer["cases"]}
    for runtime in configs["pressure-performance"]["runtimes"]:
        for backend in configs["pressure-performance"]["backends"]:
            for a in configs["pressure-performance"]["cases"]:
                b = by_setting[(a["capacity"], a["pressure_reclamation"])]
                add("observer", selector("pressure-performance", backend, a, runtime), selector("pressure-observer", backend, b, runtime), a, b, ("name", "sample_cache_stats"))

    config = configs["compact-dense"]
    by_mode = {normal_case(c)["compact_mode"]: c for c in config["cases"]}
    for runtime in config["runtimes"]:
        for backend in config["backends"]:
            a, b = by_mode["disabled"], by_mode["enabled"]
            add("compact", selector("compact-dense", backend, a, runtime), selector("compact-dense", backend, b, runtime), a, b, ("name", "compact_mode"))
    require(sum(s["jobs"] for s in suites) == 732, "protocol differs from declared 732 jobs")
    return {"schema_version": SCHEMA, "suites": suites, "total_jobs": 732,
            "method": METHOD, "contrasts": contrasts}


def median_interval(values):
    n = len(values)
    if n < 6:
        return None
    ordered, tail, interval = sorted(values), 0, None
    denominator = 2 ** n
    for k in range(n // 2):
        tail += math.comb(n, k)
        coverage = 1 - 2 * tail / denominator
        if coverage < .95:
            break
        interval = {"lower": ordered[k], "upper": ordered[n - 1 - k], "coverage": coverage,
                    "order_indices_1based": [k + 1, n - k]}
    return interval


def distribution(values):
    require(all(math.isfinite(v) for v in values), "nonfinite descriptive metric")
    return {"n": len(values), "median": statistics.median(values), "min": min(values), "max": max(values),
            "median_ci": median_interval(values), "values": values}


def sign_test(log_ratios):
    positive, negative = sum(v > 0 for v in log_ratios), sum(v < 0 for v in log_ratios)
    n = len(log_ratios)
    tail = sum(math.comb(n, k) for k in range(max(positive, negative), n + 1)) / 2 ** n
    return {"p_value": min(1, 2 * tail), "positive": positive, "negative": negative,
            "ties": n - positive - negative, "n": n}


def holm(p_values):
    order = sorted(range(len(p_values)), key=p_values.__getitem__)
    adjusted, previous = [1.0] * len(p_values), 0.0
    for rank, index in enumerate(order):
        previous = max(previous, min(1, (len(order) - rank) * p_values[index]))
        adjusted[index] = previous
    return adjusted


def environment_identity(environment):
    out = dict(environment)
    out.pop("metric_names", None)
    out["build_settings"] = {k: v for k, v in environment.get("build_settings", {}).items() if not k.startswith("vcs")}
    return canonical(out)


def phase_metrics(result, phase):
    a, b = phase["start"]["runtime"], phase["end"]["runtime"]
    n, duration = phase["operations"], phase["duration_ns"]
    metrics = {"duration_ms": duration / 1e6, "workload_ops_per_s": n * 1e9 / duration,
               "end_heap_objects_bytes": b["heap_objects_bytes"], "end_heap_scan_bytes": b["heap_scan_bytes"],
               "gc_cycles_delta": b["gc_cycles"] - a["gc_cycles"],
               "gc_pause_events": sum(phase["gc_pauses_delta"]["counts"]),
               "get_latency_samples": phase["get_latency"]["samples"], "put_latency_samples": phase["put_latency"]["samples"]}
    if n:
        metrics["allocated_bytes_per_op"] = (b["alloc_bytes"] - a["alloc_bytes"]) / n
        metrics["allocated_objects_per_op"] = (b["alloc_objects"] - a["alloc_objects"]) / n
        if b["gc_forced_cycles"] == a["gc_forced_cycles"] and result["job"]["case"]["scenario"] != "footprint":
            metrics["gc_cpu_ns_per_op"] = max(0, b["gc_cpu_seconds"] - a["gc_cpu_seconds"]) * 1e9 / n
    synthetic = result["job"]["case"].get("cache_mode") == "harness"
    if not synthetic:
        start, end = phase["start"]["cache"], phase["end"]["cache"]
        metrics["cache_entries_end"] = end["entries"]
        for key, field in (("pressure_evictions_delta", "evictions_pressure"), ("pressure_tier1_compactions_delta", "compactions_pressure_tier1"),
                           ("pressure_tier2_compactions_delta", "compactions_pressure_tier2"), ("auto_slack_compactions_delta", "compactions_auto_slack")):
            metrics[key] = end[field] - start[field]
        if phase["reads"]:
            metrics["read_hits_per_s"] = phase["hits"] * 1e9 / duration
            metrics["read_misses_per_s"] = (phase["reads"] - phase["hits"]) * 1e9 / duration
            metrics["hit_rate"] = phase["hits"] / phase["reads"]
    for operation in ("get", "put"):
        for q in ("p50", "p95", "p99"):
            quantile = phase[operation + "_latency"].get(q)
            if quantile and quantile.get("upper_seconds") is not None:
                metrics[operation + "_" + q + "_upper_seconds"] = quantile["upper_seconds"]
    if "concurrent_compaction" in phase:
        metrics["compact_duration_ms"] = phase["concurrent_compaction"]["duration_ns"] / 1e6
    for window in phase.get("request_windows", []):
        if window["name"] == "trigger-window" and not window["clipped"]:
            metrics["window_trigger_ops_per_s"] = window.get("estimated_ops_per_second", 0)
            q = window["get_latency"].get("p99")
            if q and q.get("upper_seconds") is not None:
                metrics["window_trigger_get_p99_upper_seconds"] = q["upper_seconds"]
            lengths = (r["end_elapsed_ns"] - r["start_elapsed_ns"] for r in phase.get("request_trace", [])
                       if r["read"] and window["start_elapsed_ns"] <= r["end_elapsed_ns"] < window["end_elapsed_ns"])
            maximum = max(lengths, default=None)
            if maximum is not None:
                metrics["window_trigger_get_max_seconds"] = maximum / 1e9
    return metrics


def expected_jobs(config):
    jobs = {}
    for repeat in range(config["repetitions"]):
        for case in config["cases"]:
            for runtime in config["runtimes"]:
                for backend in config["backends"]:
                    ident = f"job-{len(jobs) + 1:05d}"
                    jobs[ident] = {"id": ident, "backend": backend, "repeat": repeat,
                                   "seed": config["seed"] + repeat, "case": case, "runtime": runtime}
    return jobs


def load_measurements(root, plan):
    records, raw_hashes, suite_records, worker_hashes, sources = {}, {}, [], set(), set()
    compact_observations = []
    verifier_path = root / "analyze-repeated-verifier.py"
    verify_windows = runpy.run_path(str(verifier_path))["verify_request_windows"]
    study = load(root / "study-plan.json")
    require(study["total_jobs"] == plan["total_jobs"], "driver and analysis plans disagree")
    require(sha(Path(study["worker"])) == study["worker_sha256"], "recorded worker binary changed")
    require(sha(Path(study["controller"])) == study["controller_sha256"], "recorded controller binary changed")
    for declared in plan["suites"]:
        name = declared["name"]
        directory = root / name
        manifest = load(directory / "manifest.json")
        require(manifest.get("finished_at") and not manifest.get("error"), "unfinished/failed suite: " + name)
        config = load(directory / "config.json")
        require(manifest["config"] == config, "resolved config mismatch: " + name)
        expected = expected_jobs(config)
        require(len(expected) == declared["jobs"] == len(manifest["jobs"]), "job count mismatch: " + name)
        planned = load(root / "configs" / (name + ".json"))
        require(len(planned["cases"]) == len(config["cases"]) and all(normal_case(a) == normal_case(b) for a, b in zip(planned["cases"], config["cases"])), "case changed during resolution: " + name)
        for field in ("repetitions", "seed", "runtimes", "backends"):
            require(planned[field] == config[field], "planned field changed: " + name + "/" + field)
        worker_hashes.add(manifest["worker_sha256"])
        sources.add(canonical(manifest["capabilities"]["provenance"]))
        seen, profiled, phases = set(), 0, 0
        for record in manifest["jobs"]:
            job = record["job"]
            context = name + "/" + job["id"]
            require(record["status"] == "ok" and not record.get("error"), "unsuccessful job: " + context)
            require(job["id"] not in seen and expected[job["id"]] == job, "duplicate/unexpected job: " + context)
            seen.add(job["id"])
            path = directory / record["result_file"]
            payload = path.read_bytes()
            raw_hashes[str(path.relative_to(root))] = hashlib.sha256(payload).hexdigest()
            result = json.loads(payload)
            del payload
            require(result["job"] == job == load(directory / "raw" / (job["id"] + ".request.json")), "job/request/result mismatch: " + context)
            require(result["provenance"] == manifest["capabilities"]["provenance"], "result provenance differs: " + context)
            require(result.get("dropped_samples", 0) == 0, "dropped runtime samples: " + context)
            case = normal_case(job["case"])
            require(bool(case["profile"]) == declared["profiled"], "profiling classification mismatch: " + context)
            if declared["profiled"]:
                profile = result.get("profiling")
                require(profile and profile["completed"] and profile["diagnostic"], "incomplete profile: " + context)
                require(profile["kind"] == case["profile"] and profile["phase"] == case["profile_phase"], "profile settings mismatch: " + context)
                expected_rate = case["profile_rate"] or {"cpu": 0, "allocs": 512 * 1024, "mutex": 1, "block": 1}[case["profile"]]
                require(profile["rate"] == expected_rate, "profile sampling rate differs: " + context)
                require(profile["directory"] == record["profile_dir"], "profile directory mismatch: " + context)
                profile_dir = directory / profile["directory"]
                require(load(profile_dir / "profile.json") == profile, "profile metadata differs: " + context)
                for artifact in profile["files"]:
                    require((profile_dir / artifact).is_file() and (profile_dir / artifact).stat().st_size > 0, "missing profile artifact: " + context)
                profiled += 1
            else:
                require(not result.get("profiling"), "unprofiled result includes profiler: " + context)
            phase_names = set()
            for phase in result["phases"]:
                phases += 1
                require(phase["name"] not in phase_names, "duplicate phase: " + context)
                phase_names.add(phase["name"])
                require(phase["duration_ns"] > 0, "nonpositive phase duration: " + context)
                require(phase.get("request_trace_dropped", 0) == 0, "dropped request trace: " + context)
                a, b = phase["start"]["runtime"], phase["end"]["runtime"]
                require(a["gc_forced_cycles"] == b["gc_forced_cycles"], "forced GC inside natural phase: " + context)
                if phase["name"] in REQUEST_PHASES or phase["name"] == "warmup":
                    n = case["warmup_ops"] if phase["name"] == "warmup" else case["operations"]
                    require(phase["operations"] == n == phase["reads"] + phase["writes"], "wrong request counts: " + context)
                if case["cache_mode"] == "harness":
                    require(phase["hits"] == 0 and phase["end"]["cache"]["entries"] == 0, "synthetic control claims cache activity: " + context)
                elif phase["name"] == "fill" and not case["pressure_reclamation"]:
                    multiplicity = case["workers"] if case["cache_mode"] == "independent" else 1
                    require(phase["end"]["cache"]["entries"] == case["capacity"] * multiplicity, "fill retention differs: " + context)
                if phase["name"] == "concurrent":
                    trace = phase.get("request_trace", [])
                    require(len(trace) == case["operations"] and case["request_trace_every"] == 1, "dense trace incomplete: " + context)
                    probe = phase["compact_probe"]
                    previous = (-1, -1)
                    trace_hits = 0
                    overlap_count, slow_count, longest = 0, 0, []
                    for request in trace:
                        ordering = (request["end_elapsed_ns"], request["start_elapsed_ns"])
                        require(previous <= ordering and a["elapsed_ns"] <= request["start_elapsed_ns"] <= request["end_elapsed_ns"] <= b["elapsed_ns"], "trace time/order mismatch: " + context)
                        previous = ordering
                        trace_hits += request["hit"]
                        if probe["executed"] and request["start_elapsed_ns"] < probe["end_elapsed_ns"] and request["end_elapsed_ns"] > probe["trigger_elapsed_ns"]:
                            duration = request["end_elapsed_ns"] - request["start_elapsed_ns"]
                            overlap_count += 1
                            slow_count += duration > 1_000_000
                            entry = (duration, request["start_elapsed_ns"], request["end_elapsed_ns"], request["hit"])
                            if len(longest) < 10:
                                heapq.heappush(longest, entry)
                            elif entry > longest[0]:
                                heapq.heapreplace(longest, entry)
                    require(trace_hits == phase["hits"], "dense trace hits differ: " + context)
                    require(probe["entries_before"] == probe["entries_after"] == 25000, "Compact retained state differs: " + context)
                    require(probe["executed"] == (case["compact_mode"] != "disabled"), "Compact control differs: " + context)
                    windows_checked = verify_windows(phase, job)
                    compact_observations.append({"suite": name, "job_id": job["id"], "backend": job["backend"],
                                                 "case": case["name"], "repeat": job["repeat"], "seed": job["seed"],
                                                 "probe": probe, "trace_count": len(trace), "windows_recomputed": windows_checked,
                                                 "compact_duration_ms": (probe["end_elapsed_ns"] - probe["trigger_elapsed_ns"]) / 1e6 if probe["executed"] else None,
                                                 "overlap_count": overlap_count, "overlap_calls_above_1ms": slow_count,
                                                 "longest_overlap_calls": [{"latency_ns": duration, "start_elapsed_ns": start, "end_elapsed_ns": end, "hit": hit}
                                                                           for duration, start, end, hit in sorted(longest, reverse=True)],
                                                 "windows": [{"name": w["name"], "clipped": w["clipped"], "samples": w["samples"],
                                                              "duration_ms": (w["end_elapsed_ns"] - w["start_elapsed_ns"]) / 1e6,
                                                              "get_p99_upper_ms": None if (w["get_latency"].get("p99") or {}).get("upper_seconds") is None else w["get_latency"]["p99"]["upper_seconds"] * 1000}
                                                             for w in phase.get("request_windows", [])]})
                if declared["profiled"]:
                    continue
                key = (name, job["backend"], case["name"], job["runtime"]["name"], phase["name"])
                replicate = (job["repeat"], job["seed"])
                condition = records.setdefault(key, {})
                require(replicate not in condition, "duplicate repeat/seed: " + context)
                condition[replicate] = {"job": job, "environment": environment_identity(result["environment"]),
                                        "workload": tuple(phase[field] for field in ("workload_checksum", "operations", "reads", "writes")),
                                        "metrics": phase_metrics(result, phase), "raw_result": str(path.relative_to(root)),
                                        "warnings": result.get("warnings", [])}
            del result
        require(len(list((directory / "raw").glob("*.result.json"))) == declared["jobs"], "unexpected result files: " + name)
        suite_records.append({"name": name, "jobs": len(seen), "phases": phases, "profiled_jobs": profiled,
                              "created_at": manifest["created_at"], "finished_at": manifest["finished_at"],
                              "worker_sha256": manifest["worker_sha256"], "manifest_sha256": sha(directory / "manifest.json")})
    require(len(worker_hashes) == 1 and len(sources) == 1, "mixed worker/source identity across suites")
    require(next(iter(worker_hashes)) == study["worker_sha256"], "suite worker differs from frozen study plan")
    for key, repeats in records.items():
        require(len({r["environment"] for r in repeats.values()}) == 1, "condition mixes runtime environments: " + str(key))
    provenance = json.loads(next(iter(sources)))
    require(provenance["kind"] == "upstream-checkout" and not provenance["target_dirty"], "unexpected target source")
    return records, {"suites": suite_records, "raw_result_sha256": raw_hashes,
                     "worker_sha256": next(iter(worker_hashes)), "provenance": provenance,
                     "compact_observations": compact_observations,
                     "request_window_verifier_sha256": sha(verifier_path),
                     "study_plan_sha256": sha(root / "study-plan.json"), "study_prepared_at": study["prepared_at"],
                     "hash_note": "Result digests are recorded by this analysis; manifests do not contain original per-result digests or numeric exit codes. Successful status implies the controller observed successful worker completion."}


def analyze_contrast(spec, records):
    left = records[select_key(spec["baseline"])]
    right = records[select_key(spec["candidate"])]
    require(set(left) == set(right) and len(left) == spec["expected_pairs"], "incomplete paired repeat set: " + spec["id"])
    pairs, reasons = [], []
    for replicate in sorted(left):
        a, b = left[replicate], right[replicate]
        aa, bb = normal_case(a["job"]["case"]), normal_case(b["job"]["case"])
        differences = {key for key in set(aa) | set(bb) if aa.get(key) != bb.get(key)}
        require(differences <= set(spec["allowed_case_differences"]), "case identity drift: " + spec["id"])
        require(a["job"]["runtime"] == b["job"]["runtime"] and a["environment"] == b["environment"], "runtime/environment mismatch: " + spec["id"])
        if not spec["allow_workload_difference"]:
            require(a["workload"] == b["workload"], "workload trace/count mismatch: " + spec["id"])
        else:
            require(a["workload"][1] == b["workload"][1], "worker contrast changed total operations: " + spec["id"])
        av, bv = a["metrics"].get(spec["metric"]), b["metrics"].get(spec["metric"])
        pair = {"repeat": replicate[0], "seed": replicate[1], "baseline": av, "candidate": bv,
                "baseline_raw": a["raw_result"], "candidate_raw": b["raw_result"]}
        if av is None or bv is None or not math.isfinite(av) or not math.isfinite(bv) or av <= 0 or bv <= 0:
            reasons.append({"repeat": replicate[0], "seed": replicate[1], "reason": "missing, nonfinite or nonpositive ratio metric"})
        else:
            pair["ratio"] = bv / av
            pair["log_ratio"] = math.log(bv) - math.log(av)
        pairs.append(pair)
    out = {**spec, "pairs": pairs, "n": len(pairs), "status": "not-estimable" if reasons else "estimated", "unavailable_pairs": reasons,
           "raw_p_value": 1.0, "inference_notes": []}
    if spec["allow_workload_difference"]:
        out["inference_notes"].append("Worker counts change per-worker PRNG streams; these are seed-matched workload distributions, not identical operation traces.")
    if reasons:
        out["inference_notes"].append("No log-ratio inference: every declared pair is required. The hypothesis remains in multiplicity adjustment with p=1.")
        return out
    logs = [p["log_ratio"] for p in pairs]
    ratio = math.exp(statistics.median(logs))
    ci = median_interval(logs)
    if ci:
        ci = {**ci, "lower": math.exp(ci["lower"]), "upper": math.exp(ci["upper"])}
    test = sign_test(logs)
    out.update({"median_paired_ratio_log_scale": ratio, "paired_change_percent": (ratio - 1) * 100,
                "paired_ratio_ci": ci, "sign_test": test, "raw_p_value": test["p_value"],
                "baseline_distribution": distribution([p["baseline"] for p in pairs]),
                "candidate_distribution": distribution([p["candidate"] for p in pairs])})
    return out


def write_csv(path, rows, fields):
    with path.open("w", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=fields)
        writer.writeheader()
        writer.writerows(rows)


def fmt(value):
    return "—" if value is None else f"{value:.6g}"


def write_outputs(root, plan, records, integrity):
    contrasts = [analyze_contrast(spec, records) for spec in plan["contrasts"]]
    globally = holm([row["raw_p_value"] for row in contrasts])
    families = collections.defaultdict(list)
    for i, row in enumerate(contrasts):
        row["global_holm_p_value"] = globally[i]
        row["global_family_size"] = len(contrasts)
        families[row["family"]].append(i)
    for indices in families.values():
        adjusted = holm([contrasts[i]["raw_p_value"] for i in indices])
        for index, p in zip(indices, adjusted):
            contrasts[index]["family_holm_p_value"] = p
            contrasts[index]["family_size"] = len(indices)
    descriptives = []
    for key, repeats in sorted(records.items()):
        if key[-1] not in REQUEST_PHASES:
            continue
        metrics = collections.defaultdict(list)
        for replicate in sorted(repeats):
            for name, value in repeats[replicate]["metrics"].items():
                metrics[name].append(value)
        descriptives.append({"selector": dict(zip(("suite", "backend", "case", "runtime", "phase"), key)),
                             "expected_repeats": 10, "repeats": [{"repeat": r, "seed": s} for r, s in sorted(repeats)],
                             "case": next(iter(repeats.values()))["job"]["case"],
                             "metrics": {metric: distribution(values) for metric, values in sorted(metrics.items())}})
    output = {"schema_version": SCHEMA, "completed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "plan_sha256": sha(root / "predeclared-comparisons.json"), "method": METHOD,
              "conditions": descriptives, "comparisons": contrasts, "integrity": integrity}
    dump(root / "repeated-analysis.json", output)
    dump(root / "repeated-integrity.json", integrity)
    rows, pair_rows = [], []
    for row in contrasts:
        ci = row.get("paired_ratio_ci") or {}
        rows.append({"id": row["id"], "kind": row["kind"], "family": row["family"],
                     "baseline": "/".join(select_key(row["baseline"])), "candidate": "/".join(select_key(row["candidate"])),
                     "metric": row["metric"], "n": row["n"], "status": row["status"],
                     "median_paired_ratio_log_scale": row.get("median_paired_ratio_log_scale"),
                     "paired_change_percent": row.get("paired_change_percent"), "ratio_ci_lower": ci.get("lower"),
                     "ratio_ci_upper": ci.get("upper"), "ratio_ci_coverage": ci.get("coverage"),
                     "raw_p_value": row["raw_p_value"], "family_holm_p_value": row["family_holm_p_value"],
                     "global_holm_p_value": row["global_holm_p_value"]})
        for pair in row["pairs"]:
            pair_rows.append({"contrast_id": row["id"], "metric": row["metric"], **pair})
    write_csv(root / "paired-comparisons.csv", rows, list(rows[0]))
    write_csv(root / "paired-observations.csv", pair_rows, ["contrast_id", "metric", "repeat", "seed", "baseline", "candidate", "baseline_raw", "candidate_raw", "ratio", "log_ratio"])
    descriptive_rows = []
    for condition in descriptives:
        for name, metric in condition["metrics"].items():
            ci = metric.get("median_ci") or {}
            descriptive_rows.append({**condition["selector"], "metric": name, "n": metric["n"], "median": metric["median"],
                                     "min": metric["min"], "max": metric["max"], "median_ci_lower": ci.get("lower"),
                                     "median_ci_upper": ci.get("upper"), "median_ci_coverage": ci.get("coverage")})
    write_csv(root / "condition-descriptives.csv", descriptive_rows, list(descriptive_rows[0]))
    text = ["# Repeated diagnostic study: paired analysis", "",
            f"Verified {sum(s['jobs'] for s in integrity['suites'])} fresh worker results; {sum(s['profiled_jobs'] for s in integrity['suites'])} profiled jobs remain separate from performance inference.", "",
            f"Predeclared hypotheses: {len(contrasts)}. Estimated: {sum(r['status'] == 'estimated' for r in contrasts)}. Missing or nonpositive metrics are reported without dropping pairs.", ""]
    text.extend([integrity["declaration_timing_note"], ""])
    for name, note in METHOD.items():
        text.extend([f"**{name.replace('_', ' ').capitalize()}:** {note}", ""])
    text.extend(["Ratios above1 mean the candidate's metric is higher. Throughput, allocation, memory and cache utility require different interpretations. Backend differences under pressure can include changed retention and hit/miss composition; independent caches multiply retained capacity by worker count.", "",
                 "## Unprofiled conditions", "", "| Suite | Backend | Case | Mops/s | GC CPU ns/op | B/op | Read hits/s | Entries |", "|---|---|---|---:|---:|---:|---:|---:|"])
    for condition in descriptives:
        s, metrics = condition["selector"], condition["metrics"]
        def med(name, divisor=1):
            return None if name not in metrics else metrics[name]["median"] / divisor
        text.append(f"| {s['suite']} | {s['backend']} | {s['case']} | {fmt(med('workload_ops_per_s', 1e6))} | {fmt(med('gc_cpu_ns_per_op'))} | {fmt(med('allocated_bytes_per_op'))} | {fmt(med('read_hits_per_s'))} | {fmt(med('cache_entries_end'))} |")
    text.extend(["", "## Paired throughput effects", "", "Other prespecified metrics and every raw pair are in the JSON/CSV artifacts. Intervals below are individual intervals; use the global Holm column for error control across the entire declared matrix.", "",
                 "| Kind | Baseline | Candidate | Ratio | Individual ratio interval | Family Holm p | Global Holm p |", "|---|---|---|---:|---|---:|---:|"])
    for row in contrasts:
        if row["metric"] != "workload_ops_per_s":
            continue
        ci = row.get("paired_ratio_ci")
        interval = "—" if not ci else f"{fmt(ci['lower'])}–{fmt(ci['upper'])} ({ci['coverage']:.4%})"
        left, right = "/".join(select_key(row["baseline"])), "/".join(select_key(row["candidate"]))
        text.append(f"| {row['kind']} | {left} | {right} | {fmt(row.get('median_paired_ratio_log_scale'))} | {interval} | {fmt(row['family_holm_p_value'])} | {fmt(row['global_holm_p_value'])} |")
    text.extend(["", "[Recorded plan](predeclared-comparisons.json) · [Full analysis](repeated-analysis.json) · [Paired metrics](paired-comparisons.csv) · [Individual pairs](paired-observations.csv) · [Condition distributions](condition-descriptives.csv) · [Integrity metadata](repeated-integrity.json) · [Executed analyzer](analyze-repeated-executed.py) · [Command](analyze-repeated-command.sh)", ""])
    (root / "REPEATED-ANALYSIS.md").write_text("\n".join(text))
    return {"jobs": sum(s["jobs"] for s in integrity["suites"]), "conditions": len(descriptives), "contrasts": len(contrasts),
            "estimated": sum(row["status"] == "estimated" for row in contrasts),
            "global_holm_p_below_005": sum(row["global_holm_p_value"] < .05 for row in contrasts)}


def record_command(root, plan_only):
    prefix = "analysis-plan" if plan_only else "analyze-repeated"
    source = Path(__file__).resolve()
    target = root / (prefix + "-executed.py")
    if source != target:
        shutil.copyfile(source, target)
    if not plan_only:
        dependency = source.parent / "verify-diagnostics.py"
        recorded_dependency = root / "analyze-repeated-verifier.py"
        if dependency.exists() and dependency.resolve() != recorded_dependency.resolve():
            shutil.copyfile(dependency, recorded_dependency)
        require(recorded_dependency.exists(), "recorded request-window verifier is missing")
    command = [sys.executable, str(source), *sys.argv[1:]]
    script = root / (prefix + "-command.sh")
    script.write_text("#!/usr/bin/env bash\nset -euo pipefail\ncd -- " + shlex.quote(str(Path.cwd())) + "\n" + shlex.join(command) + "\n")
    script.chmod(0o755)
    return {"command": command, "script_sha256": sha(target)}


def self_test():
    require(median_interval([1, 2, 3, 4, 5]) is None, "n<6 interval")
    ci = median_interval(list(range(1, 11)))
    require(ci["lower"] == 2 and ci["upper"] == 9 and ci["coverage"] == .978515625, "n10 interval ranks/coverage")
    require(sign_test([1] * 10)["p_value"] == .001953125, "unanimous sign test")
    require(sign_test([1] * 8 + [0] * 2)["p_value"] == .109375, "ties retained conservatively")
    require(sign_test([0] * 10)["p_value"] == 1, "all ties")
    require(holm([.01, .03, .04]) == [.03, .06, .06], "Holm step-down")
    require(math.isclose(math.exp(statistics.median([math.log(4), math.log(9)])), 6), "log median ratio")
    require(holm([.001953125] * 100)[0] == .1953125, "global multiplicity")
    base, candidate = {}, {}
    for repeat in range(10):
        a = {"job": {"case": {"name": "condition"}, "runtime": {"name": "runtime"}},
             "environment": "same", "workload": (repeat, 1000, 900, 100),
             "metrics": {"workload_ops_per_s": 100 + repeat}, "raw_result": "fixture"}
        b = {**a, "metrics": {"workload_ops_per_s": 2 * (100 + repeat)}}
        base[(repeat, 42 + repeat)], candidate[(repeat, 42 + repeat)] = a, b
    left = {"suite": "suite", "backend": "map", "case": "condition", "runtime": "runtime", "phase": "measured"}
    right = {**left, "backend": "arena"}
    spec = {"id": "self-test", "baseline": left, "candidate": right, "expected_pairs": 10,
            "allowed_case_differences": [], "allow_workload_difference": False, "metric": "workload_ops_per_s"}
    records = {select_key(left): base, select_key(right): candidate}
    result = analyze_contrast(spec, records)
    require(result["status"] == "estimated" and math.isclose(result["median_paired_ratio_log_scale"], 2), "paired raw ratios")
    candidate[(0, 42)]["metrics"]["workload_ops_per_s"] = 0
    result = analyze_contrast(spec, records)
    require(result["status"] == "not-estimable" and result["n"] == 10 and result["raw_p_value"] == 1, "zero metrics cannot drop pairs")
    candidate[(0, 42)]["workload"] = (999, 1000, 900, 100)
    try:
        analyze_contrast(spec, records)
    except ValueError:
        pass
    else:
        raise ValueError("workload identity guard failed")
    print("Pure-math self-checks passed.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", nargs="?", type=Path)
    parser.add_argument("--plan-only", action="store_true")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return 0
    if args.root is None:
        parser.error("ROOT is required unless --self-test is used")
    root = args.root.resolve()
    require(root.is_dir(), "result root does not exist")
    execution = record_command(root, args.plan_only)
    planned = generate_plan(root)  # Reads configs only, before any raw result access.
    plan_path = root / "predeclared-comparisons.json"
    if args.plan_only:
        if plan_path.exists():
            previous = load(plan_path)
            require(previous["protocol"] == planned, "refusing to replace a different predeclared analysis")
        else:
            dump(plan_path, {"created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "execution": execution, "protocol": planned})
        print(json.dumps({"status": "planned", "jobs": planned["total_jobs"], "hypotheses": len(planned["contrasts"]), "path": str(plan_path)}))
        return 0
    require(plan_path.exists(), "run --plan-only before measurement; analysis will not retrospectively create a predeclared plan")
    saved = load(plan_path)
    require(saved["protocol"] == planned, "saved configs or analysis contrasts changed since declaration")
    records, integrity = load_measurements(root, planned)
    integrity.update({"analysis_execution": execution, "plan_created_at": saved["created_at"], "status": "passed"})
    timestamp = lambda value: datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    first_measurement = min(timestamp(s["created_at"]) for s in integrity["suites"])
    require(timestamp(integrity["study_prepared_at"]) <= first_measurement, "study plan was not prepared before measurement")
    if timestamp(saved["created_at"]) > first_measurement:
        integrity["declaration_timing_note"] = "The study configurations and comparison policy were frozen before measurement. Deterministic enumeration of these hypotheses was saved after measurement began, using only the frozen configs and before any result analysis; this is not a claim that the enumerated JSON existed before the first job."
    else:
        integrity["declaration_timing_note"] = "The study configurations, comparison policy and enumerated hypotheses were saved before the first measurement job."
    summary = write_outputs(root, planned, records, integrity)
    print(json.dumps({"status": "completed", "root": str(root), **summary}))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, KeyError, OSError, json.JSONDecodeError) as error:
        print("Analysis refused: " + str(error), file=sys.stderr)
        sys.exit(1)
