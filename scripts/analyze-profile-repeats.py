#!/usr/bin/env python3
"""Summarize repeated diagnostic profiles without treating samples as repeats.

Usage: python3 scripts/analyze-profile-repeats.py RESULTS_ROOT

The controller/driver must finish every profiled job and then extract top.txt and
cumulative.txt with `go tool pprof -nodecount=0 -nodefraction=0 -unit=ns` (CPU,
mutex, block) or `-unit=B -sample_index=alloc_space -base=BEFORE` (allocations).
This script reads those existing files; it never invokes Go or a benchmark.
"""

import argparse
import csv
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shlex
import statistics
import sys

SUITES = ("profiling", "profiling-arena", "profiling-arena-fill")
CACHE = r"github\.com/google/go-lru\.\(\*(?:mapCache|radixCache|arenaRadix)\[go\.shape\.\[\]uint8\]\)\."
ARENA = r"github\.com/google/go-lru\.\(\*arenaRadix\[go\.shape\.\[\]uint8\]\)\."
SITES = {
    "rwmutex_lock": (r"sync\.\(\*RWMutex\)\.Lock", "RWMutex.Lock"),
    "rwmutex_unlock": (r"sync\.\(\*RWMutex\)\.Unlock", "RWMutex.Unlock"),
    "cache_unlock": (CACHE + "unlock", "Cache unlock"),
    "cache_get": (CACHE + "Get", "Cache Get"),
    "cache_put": (CACHE + "Put", "Cache Put"),
    "channel_receive": (r"runtime\.chanrecv1", "Controller channel wait"),
    "waitgroup": (r"sync\.\(\*WaitGroup\)\.Wait", "WaitGroup.Wait"),
    "timer_select": (r"runtime\.selectgo", "Timer/select wait"),
    "node_growth": (ARENA + "allocateNode", "Arena node growth"),
    "compaction": (ARENA + "compactDataStructuresLocked", "Arena compaction"),
    "pressure_shedding": (ARENA + "shedAndCompactLocked", "Arena pressure shedding"),
    "pressure_bookkeeping": (r"github\.com/google/go-lru\.\(\*pressureState\)\.markReclaimedLocked", "Pressure reclamation bookkeeping"),
    "payload": (r"main\.factory\.func2", "Payload allocation"),
    "key_generation": (r"example\.com/lrugcbench/bench\.MakeKey", "Key generation"),
    "radix_child_lookup": (r"github\.com/google/go-lru\.\(\*radixNode\[go\.shape\.\[\]uint8\]\)\.getChild", "Radix child lookup"),
    "arena_lookup": (ARENA + "getNodeKeyWithHash", "Arena lookup"),
    "map_lookup": (r"runtime\.mapaccess2_faststr", "String-map lookup"),
}
COUNTERS = ("evictions_capacity", "evictions_pressure", "compactions_explicit",
            "compactions_pressure_tier1", "compactions_pressure_tier2", "compactions_auto_slack")


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def quantity(value, unit):
    if value == "0":
        return 0.0
    match = re.fullmatch(r"(-?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)" + re.escape(unit), value)
    if not match:
        raise ValueError(f"Expected explicit {unit} quantity, got {value!r}")
    result = float(match.group(1))
    if not math.isfinite(result):
        raise ValueError(f"Nonfinite quantity: {value}")
    return result


def parse_profile(path, kind):
    text = path.read_text()
    unit = "B" if kind == "allocs" else "ns"
    expected_type = {"allocs": "alloc_space", "cpu": "cpu", "mutex": "delay", "block": "delay"}[kind]
    if f"Type: {expected_type}\n" not in text:
        raise ValueError(f"{path}: missing expected profile type {expected_type}")
    if re.search(r"^Dropped \d+ nodes|^Showing top ", text, re.M):
        raise ValueError(f"{path}: truncated profile; extract with -nodecount=0 -nodefraction=0")
    if not re.search(r"^\s*flat\s+flat%\s+sum%\s+cum\s+cum%\s*$", text, re.M):
        raise ValueError(f"{path}: missing pprof table header")
    total_match = re.search(r"^Showing nodes accounting for .*? of (\S+) total\s*$", text, re.M)
    if not total_match:
        raise ValueError(f"{path}: missing sampled total")
    total = quantity(total_match.group(1), unit)
    rows = []
    for line in text.splitlines():
        fields = line.split(maxsplit=5)
        if len(fields) != 6 or not fields[1].endswith("%") or not fields[4].endswith("%"):
            continue
        rows.append({
            "function": fields[5],
            "canonical_function": re.sub(r" \(inline\)$", "", fields[5]),
            "flat": quantity(fields[0], unit), "cumulative": quantity(fields[3], unit),
            "displayed_flat_percent": float(fields[1][:-1]),
            "displayed_cumulative_percent": float(fields[4][:-1]),
        })
    if total != 0 and not rows:
        raise ValueError(f"{path}: nonzero profile has no rows")
    # No pruning plus a complete table permits absence to mean zero. Require
    # flat quantities to account for the total, allowing pprof decimal rounding.
    if not math.isclose(sum(row["flat"] for row in rows), total, rel_tol=1e-7, abs_tol=max(1, len(rows))):
        raise ValueError(f"{path}: flat rows do not account for the full profile total")
    return {"type": expected_type, "unit": unit, "sampled_total": total,
            "sha256": sha256(path), "rows": rows, "complete_unpruned_table": True}


def select_sites(profile, operations):
    result = {}
    for name, (pattern, label) in SITES.items():
        matches = [row for row in profile["rows"] if re.fullmatch(pattern, row["canonical_function"])]
        if len(matches) > 1:
            raise ValueError(f"Ambiguous site {name}; refusing to sum overlapping cumulative frames")
        row = matches[0] if matches else {"flat": 0.0, "cumulative": 0.0, "function": None}
        result[name] = {"label": label, "present": bool(matches), "function": row["function"]}
        for cost in ("flat", "cumulative"):
            result[name][cost] = row[cost]
            result[name][cost + "_per_operation"] = row[cost] / operations
            result[name][cost + "_percent"] = 100 * row[cost] / profile["sampled_total"] if profile["sampled_total"] else 0.0
    return result


def distribution(values):
    if not values:
        raise ValueError("Cannot summarize zero repeats")
    return {"n": len(values), "median": statistics.median(values), "min": min(values), "max": max(values)}


def load_records(root):
    records, manifests = [], {}
    for suite in SUITES:
        directory = root / suite
        manifest = json.loads((directory / "manifest.json").read_text())
        if not manifest.get("finished_at") or any(job["status"] != "ok" for job in manifest["jobs"]):
            raise ValueError(f"{suite}: incomplete or failed jobs; do not silently summarize partial repetitions")
        manifests[suite] = manifest
        for record in manifest["jobs"]:
            relative = Path(record["result_file"])
            if relative.is_absolute() or ".." in relative.parts:
                raise ValueError(f"Unsafe result path: {relative}")
            path = directory / relative
            result = json.loads(path.read_text())
            job = result["job"]
            if job != record["job"] or result["provenance"] != manifest["capabilities"]["provenance"]:
                raise ValueError(f"{path}: job or provenance mismatch")
            meta = result.get("profiling")
            if not meta or not meta.get("completed") or not meta.get("diagnostic"):
                raise ValueError(f"{path}: missing completed diagnostic profile")
            relative_profile = Path(meta["directory"])
            if relative_profile.is_absolute() or ".." in relative_profile.parts:
                raise ValueError(f"Unsafe profile path: {relative_profile}")
            profile_dir = directory / relative_profile
            if meta["kind"] != job["case"]["profile"] or meta["phase"] != job["case"]["profile_phase"]:
                raise ValueError(f"{path}: profile settings disagree with job")
            phase = next(p for p in result["phases"] if p["name"] == meta["phase"])
            operations = phase["operations"]
            if operations <= 0:
                raise ValueError(f"{path}: nonpositive operation count")
            top = parse_profile(profile_dir / "top.txt", meta["kind"])
            cumulative = parse_profile(profile_dir / "cumulative.txt", meta["kind"])
            # Sorting is the only expected difference between these extracts.
            def facts(profile):
                return sorted((r["function"], r["flat"], r["cumulative"]) for r in profile["rows"])
            if top["sampled_total"] != cumulative["sampled_total"] or facts(top) != facts(cumulative):
                raise ValueError(f"{profile_dir}: flat/cumulative extracts disagree")
            counters = {k: phase["end"]["cache"][k] - phase["start"]["cache"][k] for k in COUNTERS}
            if any(value < 0 for value in counters.values()):
                raise ValueError(f"{path}: decreasing cache counter")
            allocated = phase["end"]["runtime"]["alloc_bytes"] - phase["start"]["runtime"]["alloc_bytes"]
            records.append({
                "suite": suite, "job_id": job["id"], "repeat": job["repeat"], "seed": job["seed"],
                "backend": job["backend"], "case_name": job["case"]["name"],
                "cache_mode": job["case"].get("cache_mode", "shared"),
                "case": job["case"], "runtime": job["runtime"], "profile_kind": meta["kind"],
                "phase": meta["phase"], "profile_rate": meta["rate"], "operations": operations,
                "phase_seconds": phase["duration_ns"] / 1e9,
                "runtime_allocated_bytes": allocated, "runtime_allocated_bytes_per_operation": allocated / operations,
                "start_entries": phase["start"]["cache"]["entries"], "end_entries": phase["end"]["cache"]["entries"],
                "counters": counters, "profile_directory": str(profile_dir.relative_to(root)),
                "result_file": str(path.relative_to(root)), "profile": top,
                "cumulative_extract_sha256": cumulative["sha256"],
                "sampled_total_per_operation": top["sampled_total"] / operations,
                "sites": select_sites(top, operations),
            })
    worker_hashes = {m["worker_sha256"] for m in manifests.values()}
    if len(worker_hashes) != 1:
        raise ValueError("Profile suites use different worker binaries")
    return records, manifests


def aggregate(records, manifests):
    grouped = {}
    for record in records:
        key = (record["suite"], record["backend"], record["case_name"], record["profile_kind"])
        grouped.setdefault(key, []).append(record)
    groups = []
    for key, members in sorted(grouped.items()):
        first = members[0]
        expected = manifests[first["suite"]]["config"]["repetitions"]
        if sorted(r["repeat"] for r in members) != list(range(expected)):
            raise ValueError(f"{key}: missing or duplicate repeat indices")
        for record in members:
            if record["case"] != first["case"] or record["runtime"] != first["runtime"] or record["operations"] != first["operations"]:
                raise ValueError(f"{key}: settings changed across repetitions")
        group = {k: first[k] for k in ("suite", "backend", "case_name", "cache_mode", "profile_kind", "phase", "operations", "profile_rate", "case", "runtime")}
        group["n"] = len(members)
        group["unit"] = first["profile"]["unit"]
        group["evidence"] = [r["profile_directory"] + "/top.txt" for r in sorted(members, key=lambda r: r["repeat"])]
        group["metrics"] = {k: distribution([r[k] for r in members]) for k in (
            "phase_seconds", "runtime_allocated_bytes_per_operation", "sampled_total_per_operation", "start_entries", "end_entries")}
        group["counters"] = {k: distribution([r["counters"][k] for r in members]) for k in COUNTERS}
        group["sites"] = {}
        for name, (_, label) in SITES.items():
            group["sites"][name] = {"label": label, "present_repeats": sum(r["sites"][name]["present"] for r in members)}
            for cost in ("flat", "cumulative"):
                for suffix in ("", "_per_operation", "_percent"):
                    metric = cost + suffix
                    group["sites"][name][metric] = distribution([r["sites"][name][metric] for r in members])
        groups.append(group)
    return groups


def cell(stats, digits=2):
    return f"{stats['median']:.{digits}f} [{stats['min']:.{digits}f}, {stats['max']:.{digits}f}]"


def markdown(groups, records, worker_hash):
    lines = ["# Repeated diagnostic profiles", "",
             f"This report covers {len(records)} fresh worker processes across {len(groups)} profile conditions. Each table reports **median [minimum, maximum] across independent process repetitions**. These ranges describe observed variability, not confidence intervals.", "",
             f"Worker SHA-256: `{worker_hash}`.", "",
             "Profiled runs are diagnostic and excluded from headline throughput/latency comparisons. CPU quantities are sampled process CPU nanoseconds; mutex/block quantities are aggregate sampled waiting nanoseconds across goroutines. Allocation quantities are sampled allocated bytes after subtracting the pre-phase allocation profile. Each quantity is divided by that run's recorded phase operation count before aggregation.", "",
             "Site percentages use that individual profile's sampled total as denominator, then are summarized across repetitions. Cumulative frames overlap; **do not add cumulative rows**. Missing sites count as zero only after full unpruned tables are verified. Zero means no samples attributed to that site, not proof that the code never ran. Profiles retain phase-boundary/sampling effects and allocation-profile publication GCs.", "",
             "Independent caches give every worker a complete cache, so retained memory and total capacity differ from shared mode. The controls help locate shared-state contention; they do not isolate every cause of a throughput difference.", ""]
    for kind, title, site in (("cpu", "CPU profiles", "rwmutex_lock"),
                              ("mutex", "Mutex profiles", "cache_unlock"),
                              ("block", "Block profiles", "rwmutex_lock")):
        selected = [g for g in groups if g["profile_kind"] == kind]
        lines += [f"## {title}", "",
                  f"The selected cumulative site is **{SITES[site][1]}**. All costs are ns per phase operation.", "",
                  "| Backend | Topology | n | Phase seconds | Total sampled ns/op | Selected site ns/op | Site % of sampled total |",
                  "|---|---|---:|---:|---:|---:|---:|"]
        for g in selected:
            s = g["sites"][site]
            lines.append(f"| {g['backend']} | {g['cache_mode']} | {g['n']} | {cell(g['metrics']['phase_seconds'])} | {cell(g['metrics']['sampled_total_per_operation'])} | {cell(s['cumulative_per_operation'])} | {cell(s['cumulative_percent'])} |")
        if kind == "mutex":
            lines += ["", "Mutex profiles attribute waiting to lock-holder release stacks. Cache unlock rows must not be interpreted as the waiting callers. Other runtime or collector mutexes can contribute to the profile total."]
        elif kind == "block":
            lines += ["", "Block profiles show waiting call stacks, but their totals also include controller channel waits, WaitGroup joins, and the sampler's timer/select waits. Those orchestration waits must not be presented as additional cache-request stalls."]
        lines += [""]
    cpu = [g for g in groups if g["profile_kind"] == "cpu"]
    lines += ["### Selected lookup CPU sites", "", "These cumulative costs identify lookup paths and overlap cache Get/Put totals.", "",
              "| Backend | Topology | Site | Sampled ns/op | % of sampled CPU |", "|---|---|---|---:|---:|"]
    for g in cpu:
        name = {"map": "map_lookup", "radix": "radix_child_lookup", "arena": "arena_lookup"}[g["backend"]]
        s = g["sites"][name]
        lines.append(f"| {g['backend']} | {g['cache_mode']} | {s['label']} | {cell(s['cumulative_per_operation'])} | {cell(s['cumulative_percent'])} |")
    allocs = [g for g in groups if g["profile_kind"] == "allocs"]
    lines += ["", "## Arena allocation profiles", "",
              "Runtime B/op comes from phase boundary counters. Sampled B/op comes from pprof before/after subtraction and can include profile-boundary serialization allocations. The quantities need not match exactly.", "",
              "| Condition | Phase | n | Runtime B/op | Sampled B/op |", "|---|---|---:|---:|---:|"]
    for g in allocs:
        lines.append(f"| {g['case_name']} | {g['phase']} | {g['n']} | {cell(g['metrics']['runtime_allocated_bytes_per_operation'])} | {cell(g['metrics']['sampled_total_per_operation'])} |")
    lines += ["", "### Allocation sites", "",
              "Rows use flat allocation sites except reclamation bookkeeping, whose cumulative path includes its sync.Map.Clear work. Do not add that cumulative row to another cumulative caller.", "",
              "| Condition | Site | Cost | Sampled B/op | % of sampled allocated bytes | Present profiles |",
              "|---|---|---|---:|---:|---:|"]
    for g in allocs:
        for name in ("node_growth", "compaction", "pressure_shedding", "pressure_bookkeeping", "payload", "key_generation"):
            s = g["sites"][name]
            cost = "cumulative" if name == "pressure_bookkeeping" else "flat"
            lines.append(f"| {g['case_name']} | {s['label']} | {cost} | {cell(s[cost + '_per_operation'])} | {cell(s[cost + '_percent'])} | {s['present_repeats']}/{g['n']} |")
    lines += ["", "### Retention and reclamation during profiled phases", "",
              "| Condition | Start entries | End entries | Pressure evictions | Tier 1 compactions | Tier 2 compactions | Auto-slack compactions |",
              "|---|---:|---:|---:|---:|---:|---:|"]
    for g in allocs:
        values = [cell(g['metrics'][key], 0) for key in ('start_entries', 'end_entries')]
        values += [cell(g['counters'][key], 0) for key in ('evictions_pressure', 'compactions_pressure_tier1', 'compactions_pressure_tier2', 'compactions_auto_slack')]
        lines.append('| ' + g['case_name'] + ' | ' + ' | '.join(values) + ' |')
    lines += ["", "Source interpretation: Arena `allocateNode` reuses the free list before growing the node slice. `compactDataStructuresLocked` can allocate an index mapping, replacement nodes, and a replacement hash map; later inserts can grow the compacted slice again. `shedAndCompactLocked` also allocates its returned eviction slice. These paths explain how cumulative allocation can rise while retained memory falls. Profile repetitions identify observed costs, not the causal benefit of an unimplemented optimization.", "",
              "## Reproducibility and evidence", "",
              "[Per-profile data and aggregate summaries](profile-repeat-summary.json) preserve all full-table rows, normalized selected sites, counters, settings, and input hashes. [Site CSV](profile-repeat-sites.csv) contains one row per selected site per independent profile. [Executed analysis script](profile-repeat-analysis-script.py), [invocation](profile-repeat-analysis-command.json), and [analysis replay](reproduce-profile-analysis.sh) reproduce this report from the saved extracts. Benchmark replay scripts and original pprof files remain in each suite/profile directory.", ""]
    for g in groups:
        links = ', '.join(f"[repeat {i + 1}]({path})" for i, path in enumerate(g['evidence']))
        lines.append(f"- {g['backend']} / {g['case_name']} / {g['profile_kind']}: {links}")
    lines += [""]
    return '\n'.join(lines)


def record_invocation(root):
    source = Path(__file__).resolve()
    captured = root / "profile-repeat-analysis-script.py"
    if source != captured:
        captured.write_bytes(source.read_bytes())
        captured.chmod(0o755)
    command = {"created_at": dt.datetime.now(dt.timezone.utc).isoformat(),
               "working_directory": os.getcwd(), "argv": sys.argv,
               "python_executable": sys.executable, "python_version": sys.version,
               "script_sha256": sha256(source), "results_root": str(root)}
    (root / "profile-repeat-analysis-command.json").write_text(json.dumps(command, indent=2) + '\n')
    script = "#!/bin/sh\nset -eu\nhere=$(CDPATH= cd -- \"$(dirname -- \"$0\")\" && pwd)\n"
    script += f"exec {shlex.quote(sys.executable)} \"$here/profile-repeat-analysis-script.py\" \"$here\"\n"
    replay = root / "reproduce-profile-analysis.sh"
    replay.write_text(script)
    replay.chmod(0o755)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("results_root", type=Path)
    args = parser.parse_args()
    root = args.results_root.resolve()
    if not root.is_dir():
        parser.error("results_root must be an existing study directory")
    record_invocation(root)
    records, manifests = load_records(root)
    groups = aggregate(records, manifests)
    worker_hash = next(iter(manifests.values()))["worker_sha256"]
    payload = {"schema_version": 1, "worker_sha256": worker_hash,
               "aggregation": "Per-process median/min/max after normalizing sampled cost by that phase's operation count; no pooled inference.",
               "groups": groups, "profiles": records}
    (root / "profile-repeat-summary.json").write_text(json.dumps(payload, indent=2) + '\n')
    with (root / "profile-repeat-sites.csv").open('w', newline='') as output:
        columns = ["suite", "job_id", "repeat", "seed", "backend", "case_name", "cache_mode", "profile_kind", "phase", "operations", "phase_seconds", "unit", "sampled_total", "site", "present", "function", "flat", "cumulative", "flat_per_operation", "cumulative_per_operation", "flat_percent", "cumulative_percent", "profile_directory"]
        writer = csv.DictWriter(output, fieldnames=columns)
        writer.writeheader()
        for record in records:
            for name, site in record["sites"].items():
                row = {key: record[key] for key in columns if key in record}
                row.update({key: site[key] for key in columns if key in site})
                row.update(site=name, unit=record["profile"]["unit"], sampled_total=record["profile"]["sampled_total"])
                writer.writerow(row)
    (root / "PROFILE-REPEATS.md").write_text(markdown(groups, records, worker_hash))
    print(f"Summarized {len(records)} independent profiles across {len(groups)} conditions: {root / 'PROFILE-REPEATS.md'}")


if __name__ == "__main__":
    main()
