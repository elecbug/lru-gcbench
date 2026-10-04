#!/usr/bin/env python3
"""Summarize recorded benchmark suites without rerunning measurements.

Usage: python3 scripts/summarize-study.py results/STUDY_DIRECTORY
Incomplete main studies still produce artifacts and exit 1. Use
--allow-incomplete for a progress snapshot without changing the status labels.
"""

import argparse
import csv
import datetime
import io
import json
import math
import os
from pathlib import Path
import re
import shlex
import statistics
import sys
import tempfile
from urllib.parse import quote


MAIN_SUITES = ("calibration", "baseline", "footprint", "scalability", "pressure64", "reclaim")
MICRO_SUITES = {
    "micro-get-put": {f"Benchmark_{operation}_{backend}/Nested_Depth2" for operation in ("Get", "Put") for backend in ("MapCache", "RadixCache", "ArenaRadixCache")},
    "micro-update": {f"Benchmark_Put_UnitWeight_{backend}" for backend in ("Map", "Radix", "ArenaRadix")},
    "micro-prefix": {f"Benchmark_DeletePrefix_{backend}/Nested_Depth2" for backend in ("MapCache", "RadixCache", "ArenaRadixCache")},
    "micro-compact": {"Benchmark_ArenaRadixCache_Compact"},
}
MICRO_REPEATS = 10
REQUEST_PHASES = {"measured", "recovery", "concurrent"}
METRICS = (
    "duration_ms",
    "workload_ops_per_s",
    "allocated_bytes_per_op",
    "allocated_objects_per_op",
    "gc_cpu_ns_per_op",
    "gc_cycles_delta",
    "gc_pause_events",
    "hit_rate",
    "cache_entries_end",
    "end_heap_objects_bytes",
    "end_rss_bytes",
    "post_gc_heap_delta_per_entry_bytes",
    "post_gc_heap_scan_bytes",
    "get_latency_samples",
    "put_latency_samples",
    "get_p99_upper_seconds",
    "put_p99_upper_seconds",
    "gc_pause_p99_upper_seconds",
    "pressure_evictions_delta",
    "concurrent_compaction_duration_ms",
)
ROW_FIELDS = (
    "suite", "category", "status", "backend", "case", "runtime", "phase",
    "repeats", "expected_repeats", "environment_key", "warnings",
) + METRICS + (
    "benchmark", "go_cpu", "iterations_median", "iterations_per_repeat",
    "ns_per_op", "bytes_per_op", "allocs_per_op", "other_metrics_json",
)


def load_json(path):
    with path.open(encoding="utf-8") as stream:
        return json.load(stream)


def median(group, name):
    value = group.get("metrics", {}).get(name, {}).get("median")
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return ""
    return value if math.isfinite(value) else ""


def atomic_write(path, contents):
    with tempfile.NamedTemporaryFile("w", dir=path.parent, encoding="utf-8", newline="", delete=False) as stream:
        temporary = Path(stream.name)
        stream.write(contents)
    try:
        temporary.replace(path)
    finally:
        temporary.unlink(missing_ok=True)


def selected_phase(group, cases):
    phase = group.get("phase")
    scenario = cases.get(group.get("case"), {}).get("scenario")
    return phase in REQUEST_PHASES or (
        scenario == "footprint" and phase in {"fill", "delete", "delete_prefix", "compact"}
    ) or (scenario == "reclaim" and phase in {"delete", "delete_prefix", "compact"})


def expected_groups(config):
    for backend in config.get("backends", []):
        for case in config.get("cases", []):
            scenario = case.get("scenario")
            deletion = "delete_prefix" if case.get("delete_mode") == "prefix" else "delete"
            phases = {
                "footprint": ("fill", deletion, "compact"),
                "steady": ("measured",),
                "reclaim": (deletion, "compact", "recovery"),
                "concurrent-compact": ("concurrent",),
            }.get(scenario, ())
            for runtime in config.get("runtimes", []):
                for phase in phases:
                    yield backend, case.get("name"), runtime.get("name"), phase


def inspect_suite(path, category):
    suite = {"name": path.name, "category": category, "status": "INCOMPLETE", "notes": [], "launcher_notes": [], "rows": [], "ok": 0, "total": 0}
    if not path.is_dir():
        suite["notes"].append("Suite directory is missing; no measurements are available.")
        return suite
    # A launcher can fail after the controller has finalized all measurements.
    # Preserve that exception without relabeling successful worker jobs.
    launcher_status = path / "launcher-exit-status.txt"
    if launcher_status.is_file():
        try:
            status = int(launcher_status.read_text().strip())
            if status != 0:
                suite["launcher_notes"].append(f"Launcher exit status {status}; worker measurement completion is assessed independently. See launcher-note.txt for the recorded exception.")
        except (OSError, ValueError) as error:
            suite["launcher_notes"].append(f"Cannot read recorded launcher exit status: {error}")
    try:
        manifest = load_json(path / "manifest.json")
        config = manifest["config"]
        jobs = manifest["jobs"]
        repetitions = config["repetitions"]
        cases = {case["name"]: case for case in config["cases"]}
        suite["total"] = len(jobs)
        suite["ok"] = sum(job.get("status") == "ok" for job in jobs)
        suite["finished_at"] = manifest.get("finished_at")
        suite["provenance"] = manifest.get("capabilities", {}).get("provenance", {})
        suite["config"] = config
        suite["worker_sha256"] = manifest.get("worker_sha256", "")
        if not manifest.get("finished_at"):
            suite["notes"].append("Run has not finalized; these are partial results.")
        if manifest.get("error"):
            suite["notes"].append("Controller error: " + str(manifest["error"]))
        if suite["ok"] != len(jobs) or not jobs:
            counts = {}
            for job in jobs:
                status = job.get("status", "unknown")
                counts[status] = counts.get(status, 0) + 1
            suite["notes"].append("Job statuses: " + ", ".join(f"{key}={value}" for key, value in sorted(counts.items())))
        planned_jobs = repetitions * len(config["backends"]) * len(config["runtimes"]) * len(config["cases"])
        if len(jobs) != planned_jobs:
            suite["notes"].append(f"Manifest has {len(jobs)} jobs; configuration requires {planned_jobs}.")
        groups = load_json(path / "summary.json")
        if not isinstance(groups, list):
            raise ValueError("summary.json must contain an array")
        seen = set()
        for group in groups:
            if not selected_phase(group, cases):
                continue
            identity = tuple(group.get(field, "") for field in ("backend", "case", "runtime", "phase"))
            if identity in seen:
                suite["notes"].append("Duplicate summary group: " + "/".join(identity))
            seen.add(identity)
            repeats = group.get("replicates", [])
            repeat_ids = {replicate.get("repeat") for replicate in repeats}
            complete = len(repeats) == repetitions and repeat_ids == set(range(repetitions))
            warnings = list(group.get("quality_warnings", []))
            if not complete:
                warnings.append(f"Summary has {len(repeats)}/{repetitions} expected independent repeats, or invalid repeat IDs.")
                suite["notes"].append("Incomplete repeats for " + "/".join(identity))
            if group.get("mixed_environment"):
                warnings.append("Mixed environments: these medians do not represent one common execution environment.")
            row = {
                "suite": path.name, "category": category, "status": "COMPLETE" if complete else "INCOMPLETE",
                "backend": identity[0], "case": identity[1], "runtime": identity[2], "phase": identity[3],
                "repeats": len(repeats), "expected_repeats": repetitions,
                "environment_key": group.get("environment_key", ""), "warnings": " | ".join(warnings + suite["launcher_notes"]),
            }
            row.update({metric: median(group, metric) for metric in METRICS})
            suite["rows"].append(row)
        for missing in sorted(set(expected_groups(config)) - seen):
            suite["notes"].append("Missing summary group: " + "/".join(missing))
        if not suite["notes"]:
            suite["status"] = "COMPLETE"
        else:
            for row in suite["rows"]:
                row["status"] = "INCOMPLETE"
        suite["rows"].sort(key=lambda row: (row["case"], row["runtime"], row["phase"], row["backend"]))
    except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
        suite["notes"].append(f"Cannot read complete suite artifacts: {error}")
        for row in suite["rows"]:
            row["status"] = "INCOMPLETE"
    return suite


def inspect_micro_suite(path, expected):
    suite = {"name": path.name, "category": "microbenchmark", "status": "INCOMPLETE", "notes": [], "rows": [], "ok": 0, "total": len(expected) * MICRO_REPEATS, "leaf_count": 0, "expected_leaves": len(expected), "exit_status": "missing"}
    if not path.is_dir():
        suite["notes"].append("Microbenchmark directory is missing; no measurements are available.")
        return suite
    try:
        suite["exit_status"] = int((path / "exit-status.txt").read_text().strip())
        if suite["exit_status"] != 0:
            suite["notes"].append(f"Recorded command exited with status {suite['exit_status']}.")
    except (OSError, ValueError) as error:
        suite["notes"].append(f"Run has no valid exit-status.txt; it may still be running: {error}")
    for name in ("environment.txt", "executed-script.sh", "executed-command.sh", "reproduce.sh", "run.log"):
        if not (path / name).is_file():
            suite["notes"].append(f"Missing execution artifact: {name}")
    try:
        text = (path / "benchmark.txt").read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        suite["notes"].append(f"Cannot read benchmark.txt: {error}")
        if isinstance(suite["exit_status"], int) and suite["exit_status"] != 0:
            suite["status"] = "FAILED"
        return suite
    if not re.search(r"^PASS\s*$", text, re.MULTILINE):
        suite["notes"].append("benchmark.txt does not contain a final PASS marker.")
    groups = {}
    for line_number, line in enumerate(text.splitlines(), 1):
        if not line.startswith("Benchmark"):
            continue
        parts = line.split()
        try:
            if len(parts) < 4 or (len(parts) - 2) % 2:
                raise ValueError("expected name, iterations, and value/unit pairs")
            name = parts[0]
            iterations = int(parts[1])
            if iterations <= 0:
                raise ValueError("iteration count must be positive")
            metrics = {}
            for offset in range(2, len(parts), 2):
                value, unit = float(parts[offset]), parts[offset + 1]
                if not math.isfinite(value) or unit in metrics:
                    raise ValueError("nonfinite metric or duplicate unit")
                metrics[unit] = value
            if not {"ns/op", "B/op", "allocs/op"} <= metrics.keys():
                raise ValueError("missing ns/op, B/op, or allocs/op")
            groups.setdefault(name, []).append((iterations, metrics))
        except (ValueError, IndexError) as error:
            suite["notes"].append(f"Cannot parse benchmark.txt line {line_number}: {error}")
    normalized = {re.sub(r"-\d+$", "", name) for name in groups}
    suite["leaf_count"] = len(groups)
    for name in sorted(expected - normalized):
        suite["notes"].append("Missing expected benchmark leaf: " + name)
    for name in sorted(normalized - expected):
        suite["notes"].append("Unexpected benchmark leaf: " + name)
    if len(groups) != len(expected):
        suite["notes"].append(f"Found {len(groups)}/{len(expected)} expected unique benchmark full names; CPU variants are separate names.")
    for name, samples in sorted(groups.items()):
        leaf = re.sub(r"-\d+$", "", name)
        warnings = []
        if len(samples) != MICRO_REPEATS:
            warnings.append(f"Observed {len(samples)}/{MICRO_REPEATS} expected benchmark repeats.")
            suite["notes"].append(f"Repeat count for {name}: {len(samples)}/{MICRO_REPEATS}.")
        if leaf not in expected:
            warnings.append("This leaf was not part of the recorded study plan.")
        iterations = [sample[0] for sample in samples]
        units = sorted({unit for _, metrics in samples for unit in metrics})
        medians = {}
        other = {}
        for unit in units:
            values = [metrics[unit] for _, metrics in samples if unit in metrics]
            medians[unit] = statistics.median(values)
            if len(values) != len(samples):
                warnings.append(f"{unit} is present in only {len(values)}/{len(samples)} repeats.")
            if unit not in {"ns/op", "B/op", "allocs/op"}:
                other[unit] = {"median": medians[unit], "n": len(values), "values": values}
        if "reclaimed-B/op" in other:
            warnings.append("Upstream reclaimed-B/op is the heap delta from the final benchmark iteration, not an average reclaimed value per operation; its median here is across benchmark repeats.")
        cpu_match = re.search(r"-(\d+)$", name)
        row = {
            "suite": path.name, "category": "microbenchmark", "status": "INCOMPLETE",
            "benchmark": name, "go_cpu": int(cpu_match.group(1)) if cpu_match else 1,
            "repeats": len(samples), "expected_repeats": MICRO_REPEATS,
            "iterations_median": statistics.median(iterations), "iterations_per_repeat": json.dumps(iterations),
            "ns_per_op": medians["ns/op"], "bytes_per_op": medians["B/op"], "allocs_per_op": medians["allocs/op"],
            "other_metrics_json": json.dumps(other, sort_keys=True) if other else "", "warnings": " | ".join(warnings),
        }
        suite["rows"].append(row)
        suite["ok"] += len(samples)
    if not suite["notes"]:
        suite["status"] = "COMPLETE"
    elif isinstance(suite["exit_status"], int) and suite["exit_status"] != 0:
        suite["status"] = "FAILED"
    for row in suite["rows"]:
        row["status"] = suite["status"]
    return suite


def cell(value):
    return str(value).replace("|", "\\|").replace("\n", " ").replace("\r", " ")


def number(row, key, divisor=1, digits=3):
    value = row.get(key, "")
    return "—" if value == "" else f"{value / divisor:,.{digits}f}"


def table(lines, headers, rows):
    lines.append("| " + " | ".join(headers) + " |")
    lines.append("| " + " | ".join("---" for _ in headers) + " |")
    for row in rows:
        lines.append("| " + " | ".join(cell(value) for value in row) + " |")
    lines.append("")


def render_report(study, suites, micro_suites, command):
    main = [suite for suite in suites if suite["category"] == "main"]
    complete = all(suite["status"] == "COMPLETE" for suite in main + micro_suites)
    lines = ["# Benchmark study", "", f"Status: **{'COMPLETE' if complete else 'INCOMPLETE'}**. Generated at {datetime.datetime.now(datetime.timezone.utc).isoformat()}.", ""]
    lines += [
        "Harness results below are medians across independent worker repeats from each suite's `summary.json`. "
        "Standalone Go microbenchmarks are summarized separately from their recorded benchmark output. "
        "No aggregate ranking or statistical significance is inferred. Different workloads, runtimes, and instrumentation settings must be interpreted separately.", "",
        "Request p99 values are medians of per-run histogram bucket upper bounds, not exact or pooled percentiles. "
        "Latency sample counts are per-run medians. A dash means the metric is unavailable or disabled. "
        "Post-GC B/entry is retained heap above the empty-process baseline divided by retained entries; "
        "it includes payloads. Heap and RSS are end-of-phase snapshots. "
        "Workload throughput includes key generation, random selection, and payload allocation.", "",
        "Read percentage is the configured request mix, not the observed hit rate. "
        "Memory reductions under pressure must be read alongside hit rate, retained entries, and pressure evictions. "
        "Short deletion/compaction phases need not meet the multi-second target for sustained workloads.", "",
        "## Execution record", "", "The exact analysis invocation was:", "", "```sh", f"cd {shlex.quote(os.getcwd())}", command, "```", "",
        "[Executed analysis script](analysis-script.py) · [Machine-readable medians](study-summary.csv)", "",
        "Each suite links its recorded launcher, resolved configuration, reproduction script, and full report. "
        "Pilot suites appear separately and are excluded from the main-study completion status.", "", "## Main suites", "",
    ]
    table(lines, ["Suite", "Status", "Successful jobs", "Notes"], [
        [suite["name"], suite["status"], f"{suite['ok']}/{suite['total']}", "; ".join(suite["notes"] + suite["launcher_notes"]) or "Finalized"]
        for suite in main
    ])
    lines.extend(["### Standalone microbenchmark status", ""])
    table(lines, ["Suite", "Status", "Exit status", "Unique leaves", "Recorded repeats", "Notes"], [
        [suite["name"], suite["status"], suite["exit_status"], f"{suite['leaf_count']}/{suite['expected_leaves']}", f"{suite['ok']}/{suite['total']}", "; ".join(suite["notes"]) or "Finalized"]
        for suite in micro_suites
    ])
    for category, title in (("main", "Main measurements"), ("pilot", "Pilot measurements (exploratory)"), ("additional", "Additional measurements")):
        selected = [suite for suite in suites if suite["category"] == category]
        if not selected:
            continue
        lines.extend([f"## {title}", ""])
        for suite in selected:
            name = suite["name"]
            prefix = quote(name, safe="")
            lines.extend([f"### {cell(name)} — {suite['status']}", ""])
            links = []
            for filename, label in (("report.html", "Full report"), ("config.json", "Resolved config"), ("executed-script.sh", "Executed launcher"), ("executed-command.sh", "Executed command"), ("reproduce.sh", "Reproduce"), ("command.json", "Command metadata"), ("run.log", "Run log"), ("launcher-note.txt", "Launcher exception note"), ("launcher-exit-status.txt", "Launcher exit status")):
                if (study / name / filename).is_file():
                    links.append(f"[{label}]({prefix}/{filename})")
            if links:
                lines.extend([" · ".join(links), ""])
            if suite["notes"]:
                lines.extend(["; ".join(cell(note) for note in suite["notes"]), ""])
            if suite["launcher_notes"]:
                lines.extend(["Launcher record: " + "; ".join(cell(note) for note in suite["launcher_notes"]), ""])
            config = suite.get("config", {})
            if config:
                runtimes = "; ".join(f"{runtime['name']}: P={runtime['gomaxprocs']}, GOGC={runtime['gogc']}, GOMEMLIMIT={runtime['gomemlimit']}" for runtime in config.get("runtimes", []))
                lines.extend([f"Configured repeats: {config.get('repetitions')}. {cell(runtimes)}.", ""])
                provenance = suite.get("provenance", {})
                lines.extend([f"Target commit: `{provenance.get('target_commit', 'unavailable')}`. Worker SHA-256: `{suite.get('worker_sha256', 'unavailable')}`.", ""])
            rows = suite["rows"]
            if not rows:
                lines.extend(["No selected phase summaries are available.", ""])
                continue
            ids = {id(row): str(index + 1) for index, row in enumerate(rows)}
            table(lines, ["ID", "Backend / case / runtime / phase", "Repeats", "s", "Mops/s", "B/op", "GC ns/op", "Hit %"], [
                [ids[id(row)], " / ".join(row[key] for key in ("backend", "case", "runtime", "phase")), f"{row['repeats']}/{row['expected_repeats']}", number(row, "duration_ms", 1000), number(row, "workload_ops_per_s", 1e6), number(row, "allocated_bytes_per_op"), number(row, "gc_cpu_ns_per_op"), number(row, "hit_rate", 0.01, 2)]
                for row in rows
            ])
            table(lines, ["ID", "Entries", "Heap MiB", "RSS MiB", "Post-GC B/entry", "Pressure evictions", "GC cycles"], [
                [ids[id(row)], number(row, "cache_entries_end", digits=0), number(row, "end_heap_objects_bytes", 2**20), number(row, "end_rss_bytes", 2**20), number(row, "post_gc_heap_delta_per_entry_bytes"), number(row, "pressure_evictions_delta", digits=0), number(row, "gc_cycles_delta", digits=1)]
                for row in rows
            ])
            if any(row["phase"] in REQUEST_PHASES for row in rows):
                table(lines, ["ID", "Get samples", "Put samples", "Get p99 upper µs", "Put p99 upper µs", "Pause events", "Pause p99 upper µs"], [
                    [ids[id(row)], number(row, "get_latency_samples", digits=0), number(row, "put_latency_samples", digits=0), number(row, "get_p99_upper_seconds", 1e-6), number(row, "put_p99_upper_seconds", 1e-6), number(row, "gc_pause_events", digits=0), number(row, "gc_pause_p99_upper_seconds", 1e-6)]
                    for row in rows if row["phase"] in REQUEST_PHASES
                ])
            warnings = {}
            for row in rows:
                if row["warnings"]:
                    warnings.setdefault(row["warnings"], []).append(ids[id(row)])
            if warnings:
                lines.extend(["Quality warnings (row IDs):", ""])
                lines.extend(f"- {', '.join(row_ids)}: {cell(warning)}" for warning, row_ids in warnings.items())
                lines.append("")
            else:
                lines.extend(["No harness quality warnings were emitted for these selected phases. This does not establish statistical significance.", ""])
    lines.extend([
        "## Standalone Go microbenchmarks", "",
        "These are medians across 10 Go benchmark repeats per full benchmark name. "
        "A `-cpu` suffix is preserved as part of the name. Repeats within a `go test` invocation share a process; "
        "they are not the isolated-worker repetitions used by the harness. "
        "Iteration counts are benchmark calibration counts, not independent samples. "
        "All iteration counts and additional reported metrics are preserved in the CSV and raw output.", "",
        "The recorded study uses time-based runs for Get/Put and update, 100 iterations per repeat for DeletePrefix, "
        "and 10 iterations per repeat for Compact. The setup, timer boundaries, and allocation semantics "
        "are those of the upstream benchmarks. Results from these microbenchmarks should not be equated with harness workload throughput.", "",
    ])
    for suite in micro_suites:
        name = suite["name"]
        prefix = quote(name, safe="")
        lines.extend([f"### {cell(name)} — {suite['status']}", ""])
        links = []
        for filename, label in (("benchmark.txt", "Raw benchmark output"), ("exit-status.txt", "Exit status"), ("environment.txt", "Environment"), ("executed-script.sh", "Executed launcher"), ("executed-command.sh", "Executed command"), ("reproduce.sh", "Reproduce"), ("run.log", "Run log")):
            if (study / name / filename).is_file():
                links.append(f"[{label}]({prefix}/{filename})")
        if links:
            lines.extend([" · ".join(links), ""])
        if suite["notes"]:
            lines.extend(["; ".join(cell(note) for note in suite["notes"]), ""])
        rows = suite["rows"]
        if not rows:
            lines.extend(["No parseable microbenchmark measurements are available.", ""])
            continue
        table(lines, ["Benchmark full name", "Repeats", "Iterations/repeat", "ns/op", "B/op", "allocs/op", "Other metric medians"], [
            [row["benchmark"], f"{row['repeats']}/{row['expected_repeats']}", number(row, "iterations_median", digits=1), number(row, "ns_per_op"), number(row, "bytes_per_op"), number(row, "allocs_per_op"), "; ".join(f"{unit}: {value['median']:,.3f} (n={value['n']})" for unit, value in json.loads(row["other_metrics_json"] or "{}").items()) or "—"]
            for row in rows
        ])
        warnings = sorted({row["warnings"] for row in rows if row["warnings"]})
        if warnings:
            lines.extend(["Caveats:", ""])
            lines.extend("- " + cell(warning) for warning in warnings)
            lines.append("")
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("study_directory", type=Path)
    parser.add_argument("--allow-incomplete", action="store_true", help="exit successfully for a partial snapshot; retain INCOMPLETE labels")
    args = parser.parse_args()
    study = args.study_directory.resolve()
    if not study.is_dir():
        parser.error(f"study directory does not exist: {study}")
    suites = [inspect_suite(study / name, "main") for name in MAIN_SUITES]
    micro_suites = [inspect_micro_suite(study / name, leaves) for name, leaves in MICRO_SUITES.items()]
    for path in sorted(study.iterdir()):
        if path.is_dir() and path.name not in MAIN_SUITES and (path / "manifest.json").is_file():
            category = "pilot" if "pilot" in path.name.lower() else "additional"
            suites.append(inspect_suite(path, category))
    output = io.StringIO(newline="")
    writer = csv.DictWriter(output, fieldnames=ROW_FIELDS)
    writer.writeheader()
    for suite in suites + micro_suites:
        if suite["rows"]:
            writer.writerows(suite["rows"])
        else:
            writer.writerow({"suite": suite["name"], "category": suite["category"], "status": suite["status"], "warnings": " | ".join(suite["notes"] + suite.get("launcher_notes", []))})
    command = shlex.join([sys.executable, *sys.argv])
    source = Path(__file__).read_text(encoding="utf-8")
    atomic_write(study / "analysis-script.py", source)
    (study / "analysis-script.py").chmod(0o755)
    atomic_write(study / "study-summary.csv", output.getvalue())
    atomic_write(study / "STUDY.md", render_report(study, suites, micro_suites, command))
    incomplete = [suite["name"] for suite in suites + micro_suites if suite["category"] in {"main", "microbenchmark"} and suite["status"] != "COMPLETE"]
    print(f"Wrote {study / 'STUDY.md'} and study-summary.csv; captured analysis-script.py.")
    if incomplete:
        print("Main study INCOMPLETE: " + ", ".join(incomplete), file=sys.stderr)
        return 0 if args.allow_incomplete else 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
