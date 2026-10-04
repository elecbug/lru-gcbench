#!/usr/bin/env python3
"""Independently audit recorded diagnostics after all measurement has stopped.

Usage: verify-diagnostics.py RESULT_ROOT [AUDIT_JSON]
Only Python's standard library is required. The audit records a copy of itself,
its command, and SHA-256 digests of the raw results it actually verified.
"""

import argparse
import collections
import datetime
import gzip
import hashlib
import json
import math
import pathlib
import re
import shlex
import shutil
import statistics
import sys

PILOT_COUNTS = {
    "calibration": 30, "topology-study": 36, "harness-control": 6,
    "pressure-control": 18, "compact-window": 12, "profiling": 18,
    "profiling-arena-fill": 1, "profiling-arena": 3,
}
MASK64 = (1 << 64) - 1


def read_json(path):
    return json.loads(path.read_text())


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def close(actual, expected):
    return math.isclose(actual, expected, rel_tol=1e-10, abs_tol=1e-12)


def mix64(value):
    value &= MASK64
    value ^= value >> 30
    value = value * 0xbf58476d1ce4e5b9 & MASK64
    value ^= value >> 27
    value = value * 0x94d049bb133111eb & MASK64
    return value ^ (value >> 31)


def duration_ns(value):
    units = {"ns": 1, "us": 1000, "µs": 1000, "ms": 10**6, "s": 10**9, "m": 60 * 10**9, "h": 3600 * 10**9}
    parts = re.findall(r"([0-9]+(?:\.[0-9]+)?)(ns|us|µs|ms|s|m|h)", value)
    require("".join(a + b for a, b in parts) == value, "unsupported duration: " + value)
    return int(sum(float(number) * units[unit] for number, unit in parts))


def expected_jobs(config):
    jobs = {}
    for repeat in range(config["repetitions"]):
        for case in config["cases"]:
            for runtime in config["runtimes"]:
                for backend in config["backends"]:
                    ident = f"job-{len(jobs) + 1:05d}"
                    jobs[ident] = {"id": ident, "backend": backend, "repeat": repeat,
                                   "seed": config["seed"] + repeat, "runtime": runtime, "case": case}
    return jobs


def verify_histogram(summary, latencies, context):
    require(summary["samples"] == len(latencies), context + ": sample count")
    histogram = summary["histogram"]
    if not latencies:
        require(not histogram.get("counts"), context + ": nonempty empty histogram")
        require(not any(summary.get(q) for q in ("p50", "p95", "p99")), context + ": empty quantiles")
        return
    bounds = [float(v) for v in histogram["bounds_seconds"]]
    counts = histogram["counts"]
    require(len(counts) == 496 and len(bounds) == 497, context + ": bucket count")
    actual = [0] * 496
    for ns in latencies:
        if ns < 8:
            bucket = ns
        else:
            exponent = ns.bit_length() - 1
            bucket = 8 + (exponent - 3) * 8 + (ns >> (exponent - 3)) - 8
        actual[bucket] += 1
    require(counts == actual, context + ": latency histogram mismatch")
    for index, bound in enumerate(bounds):
        expected = index * 1e-9 if index < 8 else (8 + (index - 8) % 8) * (2 ** ((index - 8) // 8)) * 1e-9
        require(close(bound, expected), context + ": latency bucket boundary")
    for name, quantile in (("p50", .5), ("p95", .95), ("p99", .99)):
        rank, cumulative = math.ceil(quantile * len(latencies)), 0
        for index, count in enumerate(counts):
            cumulative += count
            if cumulative >= rank:
                reported = summary[name]
                require(close(reported["lower_seconds"], bounds[index]) and close(reported["upper_seconds"], bounds[index + 1]), context + ": " + name)
                break


def verify_request_windows(phase, job):
    case = job["case"]
    trace = phase.get("request_trace", [])
    require(phase.get("request_trace_dropped", 0) == 0, "dropped request trace")
    require(trace == sorted(trace, key=lambda item: (item["end_elapsed_ns"], item["start_elapsed_ns"])), "request trace ordering")
    start = phase["start"]["runtime"]["elapsed_ns"]
    end = phase["end"]["runtime"]["elapsed_ns"]
    for request in trace:
        require(start <= request["start_elapsed_ns"] <= request["end_elapsed_ns"] <= end, "request outside phase snapshot bounds")
        require(request["read"] or not request["hit"], "write reported a hit")
    every = case.get("request_trace_every", 0)
    expected_count = 0
    if every:
        for worker in range(case["workers"]):
            operations = case["operations"] // case["workers"] + (worker < case["operations"] % case["workers"])
            seed = mix64(job["seed"] ^ ((worker + 1) * 0x9e3779b97f4a7c15 & MASK64) ^ 0x41)
            offset = mix64(seed ^ 0xaad319) % every
            expected_count += max(0, (operations - 1 - offset) // every + 1)
    require(len(trace) == expected_count, "request trace does not cover complete phase")
    probe = phase["compact_probe"]
    enabled = case.get("compact_mode") != "disabled"
    require(probe["executed"] == enabled, "probe mode mismatch")
    trigger, compact_end = probe["trigger_elapsed_ns"], probe["end_elapsed_ns"]
    require(start <= trigger <= compact_end <= end, "probe outside phase bounds")
    retained = case["capacity"] - int(case["capacity"] * case["delete_fraction"])
    if case["read_percent"] == 100 and not case["pressure_reclamation"]:
        require(probe["entries_before"] == retained == probe["entries_after"], "read-only Compact changed the matched retained state")
    compact = phase.get("concurrent_compaction")
    if enabled:
        require(compact == {"start_elapsed_ns": trigger, "end_elapsed_ns": compact_end, "duration_ns": compact_end - trigger}, "Compact interval mismatch")
    else:
        require(not compact, "disabled control claims Compact interval")
        require(phase["end"]["cache"]["compactions_explicit"] == phase["start"]["cache"]["compactions_explicit"], "disabled control ran Compact")
    windows = phase.get("request_windows", [])
    if not every:
        require(not windows, "disabled tracing has windows")
        return 0
    width = duration_ns(case.get("compact_window") or "50ms")
    expected = [("before-trigger", trigger - width, trigger), ("trigger-window", trigger, trigger + width), ("after-trigger", trigger + width, trigger + 2 * width)]
    if enabled:
        expected.append(("compact-overlap", trigger, compact_end))
    require(len(windows) == len(expected), "window count mismatch")
    for window, (name, left, right) in zip(windows, expected):
        require(window["name"] == name, "window name mismatch")
        lo, hi = window["start_elapsed_ns"], window["end_elapsed_ns"]
        require(start <= lo <= hi <= end, name + ": bounds outside phase")
        if not window["clipped"]:
            require((lo, hi) == (left, right), name + ": exact bounds mismatch")
        else:
            require((lo, hi) != (left, right), name + ": spurious clipped flag")
            require(lo >= min(left, hi) and hi <= max(right, lo), name + ": clipping expanded interval")
        overlap = name == "compact-overlap"
        chosen = [r for r in trace if lo < hi and ((r["start_elapsed_ns"] < hi and r["end_elapsed_ns"] > lo) if overlap else lo <= r["end_elapsed_ns"] < hi)]
        reads = [r for r in chosen if r["read"]]
        writes = [r for r in chosen if not r["read"]]
        require(window["samples"] == len(chosen) and window["reads"] == len(reads) and window["writes"] == len(writes) and window["hits"] == sum(r["hit"] for r in reads), name + ": counts mismatch")
        throughput = len(chosen) * every * 1e9 / (hi - lo) if hi > lo and not overlap else 0
        require(close(window.get("estimated_ops_per_second", 0), throughput), name + ": sampled throughput mismatch")
        verify_histogram(window["get_latency"], [r["end_elapsed_ns"] - r["start_elapsed_ns"] for r in reads], name + " Get")
        verify_histogram(window["put_latency"], [r["end_elapsed_ns"] - r["start_elapsed_ns"] for r in writes], name + " Put")
    return len(windows)


def verify_profiles(directory, record, result):
    profile = result.get("profiling")
    case = result["job"]["case"]
    if not case.get("profile"):
        require(not profile and not record.get("profile_dir"), "unexpected profile")
        return 0
    require(profile and profile["completed"] and profile["diagnostic"], "incomplete profile")
    require(profile["kind"] == case["profile"] and profile["phase"] == case["profile_phase"], "wrong profile settings")
    require(profile["directory"] == record["profile_dir"], "profile directory mismatch")
    target = directory / profile["directory"]
    require(read_json(target / "profile.json") == profile, "profile metadata mismatch")
    expected = ["allocs-before.pprof", "allocs-after.pprof"] if profile["kind"] == "allocs" else [profile["kind"] + ".pprof"]
    require(profile["files"] == expected, "profile file list mismatch")
    for name in expected:
        require(bool(gzip.decompress((target / name).read_bytes())), "empty or invalid compressed profile")
    for name in ("analyze.sh", "top.txt", "cumulative.txt"):
        require((target / name).is_file() and (target / name).stat().st_size > 0, "missing profile analysis " + name)
    for name in ("top.txt", "cumulative.txt"):
        require("Type:" in (target / name).read_text(), "pprof did not produce a parsed text profile")
    return 1


def verify_suite(root, item, inventory):
    name, expected_count = item["name"], item["jobs"]
    directory = root / name
    manifest = read_json(directory / "manifest.json")
    config = read_json(directory / "config.json")
    planned = read_json(root / "configs" / (name + ".json"))
    require(manifest["config"] == config, "manifest/config mismatch")
    for key, value in planned.items():
        if key in ("cases", "runtimes"):
            require(len(value) == len(config[key]), "planned matrix length mismatch")
            for original, resolved in zip(value, config[key]):
                require(all(resolved.get(field, False if isinstance(v, bool) else 0 if isinstance(v, (int, float)) else "") == v for field, v in original.items()), "planned settings changed during resolution")
        else:
            require(config[key] == value, "planned config changed: " + key)
    expected = expected_jobs(config)
    require(len(expected) == expected_count == len(manifest["jobs"]), "planned job count mismatch")
    require(manifest.get("finished_at") and not manifest.get("error"), "suite did not finish successfully")
    for artifact in ("executed-script.sh", "executed-command.sh", "reproduce.sh", "run.log", "command.json", "summary.json", "report.md", "report.html"):
        require((directory / artifact).is_file(), "missing run artifact: " + artifact)
    command = read_json(directory / "command.json")
    replay = command["replay_command"]
    worker = pathlib.Path(replay[replay.index("-worker") + 1])
    require(sha256(worker) == manifest["worker_sha256"], "worker binary hash mismatch")
    require(sha256(pathlib.Path(command["process_executable"])) == command["process_sha256"], "controller binary hash mismatch")
    provenance = manifest["capabilities"]["provenance"]
    require(provenance["kind"] == "upstream-checkout" and not provenance["target_dirty"], "unexpected target provenance")
    metrics = collections.defaultdict(lambda: collections.defaultdict(list))
    replicates = collections.defaultdict(list)
    seen, profiles, request_windows, raw_phases = set(), 0, 0, 0
    for record in manifest["jobs"]:
        job = record["job"]
        ident = job["id"]
        context = name + "/" + ident
        try:
            require(ident not in seen and job == expected[ident], "duplicate or unexpected job")
            seen.add(ident)
            require(record["status"] == "ok" and not record.get("error"), "worker status not ok")
            if "exit_code" in record:
                require(record["exit_code"] == 0, "nonzero worker exit code")
            request = read_json(directory / "raw" / (ident + ".request.json"))
            path = directory / record["result_file"]
            result = read_json(path)
            require(job == request == result["job"], "request/result/manifest job mismatch")
            require(result["provenance"] == provenance, "result provenance mismatch")
            require(result.get("dropped_samples", 0) == 0, "dropped runtime samples")
            digest = sha256(path)
            if record.get("result_sha256"):
                require(digest == record["result_sha256"], "result digest mismatch")
            inventory[str(path.relative_to(root))] = digest
            case = job["case"]
            synthetic = case.get("cache_mode") == "harness"
            multiplicity = case["workers"] if case.get("cache_mode") == "independent" else 1
            phase_names = []
            for phase in result["phases"]:
                raw_phases += 1
                phase_names.append(phase["name"])
                group = (job["backend"], case["name"], job["runtime"]["name"], phase["name"])
                require(phase["duration_ns"] > 0, "nonpositive phase duration")
                require(phase["end"]["runtime"]["gc_forced_cycles"] == phase["start"]["runtime"]["gc_forced_cycles"], "forced GC inside natural phase")
                require(phase.get("request_trace_dropped", 0) == 0, "dropped request samples")
                if phase["name"] in ("measured", "recovery", "concurrent", "warmup"):
                    operations = case["warmup_ops"] if phase["name"] == "warmup" else case["operations"]
                    require(phase["operations"] == operations == phase["reads"] + phase["writes"], "wrong workload operation count")
                    require(0 <= phase["hits"] <= phase["reads"], "invalid hit count")
                if synthetic:
                    require(phase["hits"] == 0 and phase["end"]["cache"]["entries"] == 0, "synthetic control reported cache state")
                elif phase["name"] == "fill" and not case["pressure_reclamation"]:
                    require(phase["end"]["cache"]["entries"] == case["capacity"] * multiplicity, "fill did not retain expected total capacity")
                if not synthetic:
                    require(phase["end"]["cache"]["max_size"] == case["capacity"] * multiplicity, "cache capacity accounting")
                if phase["name"] == "concurrent":
                    request_windows += verify_request_windows(phase, job)
                replicates[group].append({"repeat": job["repeat"], "seed": job["seed"]})
                metrics[group]["workload_ops_per_s"].append(phase["operations"] * 1e9 / phase["duration_ns"])
                if phase["reads"] and not synthetic:
                    metrics[group]["read_hits_per_s"].append(phase["hits"] * 1e9 / phase["duration_ns"])
                    metrics[group]["read_misses_per_s"].append((phase["reads"] - phase["hits"]) * 1e9 / phase["duration_ns"])
            require(len(phase_names) == len(set(phase_names)), "duplicate phase names")
            profiles += verify_profiles(directory, record, result)
        except Exception as error:
            raise AssertionError(context + ": " + str(error)) from error
    require(len(list((directory / "raw").glob("*.result.json"))) == expected_count, "unexpected raw result count")
    require(len(list((directory / "raw").glob("*.request.json"))) == expected_count, "unexpected raw request count")
    summary = read_json(directory / "summary.json")
    require(len(summary) == len(metrics), "summary group count mismatch")
    compared = 0
    for aggregate in summary:
        key = tuple(aggregate[field] for field in ("backend", "case", "runtime", "phase"))
        require(key in metrics, "unexpected summary group")
        require(sorted(aggregate["replicates"], key=lambda r: r["repeat"]) == sorted(replicates[key], key=lambda r: r["repeat"]), "summary repeats differ")
        for metric, values in metrics[key].items():
            actual = aggregate["metrics"][metric]
            require(actual["n"] == len(values) and close(actual["median"], statistics.median(values)) and close(actual["min"], min(values)) and close(actual["max"], max(values)), "summary distribution differs: " + str(key) + "/" + metric)
            compared += 1
        if aggregate.get("cache_mode") == "harness":
            require(not any(metric in aggregate["metrics"] for metric in ("hit_rate", "read_hits_per_s", "read_misses_per_s", "cache_entries_end")), "synthetic cache metrics not suppressed")
    return {"name": name, "jobs": len(seen), "phases": raw_phases, "profiles": profiles,
            "request_windows_recomputed": request_windows, "summary_distributions_recomputed": compared,
            "worker_sha256": manifest["worker_sha256"], "provenance": provenance}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("result_root", type=pathlib.Path)
    parser.add_argument("audit_json", nargs="?", type=pathlib.Path)
    args = parser.parse_args()
    root = args.result_root.resolve()
    output = args.audit_json.resolve() if args.audit_json else root / "integrity-audit.json"
    output.parent.mkdir(parents=True, exist_ok=True)
    script = pathlib.Path(__file__).resolve()
    recorded_script = output.with_name(output.stem + "-script.py")
    if script != recorded_script:
        shutil.copyfile(script, recorded_script)
    command = [sys.executable, str(script), *sys.argv[1:]]
    command_file = output.with_name(output.stem + "-command.sh")
    command_file.write_text("#!/usr/bin/env bash\nset -euo pipefail\ncd -- " + shlex.quote(str(pathlib.Path.cwd())) + "\n" + shlex.join(command) + "\n")
    command_file.chmod(0o755)
    audit = {"schema_version": 1, "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
             "result_root": str(root), "command": command, "script_sha256": sha256(recorded_script),
             "status": "failed", "suites": [], "raw_result_sha256": {}, "errors": [],
             "limitations": ["Manifests record successful worker status, not numeric exit codes or original result digests; this audit records a new SHA-256 inventory.",
                             "Exact mixed-task phase boundary timestamps are not exported. Unclipped window bounds are recomputed exactly; clipped bounds are checked against phase snapshots and used to independently recompute counts, rates and latency histograms.",
                             "Integrity checks do not establish statistical significance or remove profiling/observer overhead."]}
    try:
        plan = read_json(root / "diagnostic-plan.json")
        require({s["name"] for s in plan["suites"]} == set(PILOT_COUNTS), "expected eight diagnostic suites")
        require(len(plan["suites"]) == 8, "duplicate suites")
        if plan["mode"] == "pilot":
            require({s["name"]: s["jobs"] for s in plan["suites"]} == PILOT_COUNTS and plan["total_jobs"] == 124, "pilot plan is not the expected 124-job matrix")
        require((root / "driver-exit-status.txt").read_text().strip() == "0", "driver or profile analysis failed")
        for item in plan["suites"]:
            audit["suites"].append(verify_suite(root, item, audit["raw_result_sha256"]))
        require(sum(s["jobs"] for s in audit["suites"]) == plan["total_jobs"], "total job count mismatch")
        require(len({s["worker_sha256"] for s in audit["suites"]}) == 1, "mixed worker binaries")
        require(len({json.dumps(s["provenance"], sort_keys=True) for s in audit["suites"]}) == 1, "mixed source provenance")
        if plan["mode"] == "pilot":
            require(sum(s["profiles"] for s in audit["suites"]) == 22, "expected 22 complete profile analyses")
        audit["status"] = "passed"
    except Exception as error:
        audit["errors"].append(str(error))
    audit["finished_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    output.write_text(json.dumps(audit, indent=2) + "\n")
    print(json.dumps({"status": audit["status"], "suites": len(audit["suites"]), "jobs": sum(s["jobs"] for s in audit["suites"]), "profiles": sum(s["profiles"] for s in audit["suites"]), "audit": str(output), "errors": audit["errors"]}))
    return 0 if audit["status"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())
