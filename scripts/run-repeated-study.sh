#!/usr/bin/env bash
# Freeze inputs before measurement, then execute fresh workers serially.
set -euo pipefail
usage() {
  printf 'Usage: %s prepare CONTROLLER WORKER NEW_OUTPUT\n       %s run OUTPUT\n' "$0" "$0" >&2
  exit 2
}
[[ $# -ge 1 ]] || usage
action=$1
project=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if [[ "$action" == prepare ]]; then
  [[ $# == 4 ]] || usage
  controller=$2 worker=$3 output=$4
  [[ ! -e "$output" ]] || { printf 'Output already exists: %s\n' "$output" >&2; exit 2; }
  mkdir -p -- "$output/configs"
  cp -- "$0" "$output/prepared-driver.sh"
  cp -- "$project/scripts/run-recorded.sh" "$output/recorded-run-suite.sh"
  python3 - "$project" "$controller" "$worker" "$output" <<'PY'
import hashlib
import json
import pathlib
import subprocess
import sys
from datetime import datetime, timezone

project, controller, worker, output = (pathlib.Path(p).resolve() for p in sys.argv[1:])
def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()
plan = {"schema_version": 1, "prepared_at": datetime.now(timezone.utc).isoformat(),
        "controller": str(controller), "controller_sha256": digest(controller),
        "worker": str(worker), "worker_sha256": digest(worker),
        "capabilities": json.loads(subprocess.check_output([str(worker), "--describe"])),
        "unprofiled_repetitions": 10, "profile_repetitions": 6,
        "purpose": "Repeated performance comparisons with separately repeated diagnostic profiles.",
        "comparison_policy": {
            "pairing": "Same seed and repeat; globally shuffled suite jobs, not adjacent temporal pairs.",
            "effect": "Median paired log ratio, exponentiated, with exact order-statistic interval.",
            "inference": "Two-sided exact paired sign tests, Holm adjustment by declared category/metric and globally; intervals are not simultaneous.",
            "categories": ["backend", "topology", "worker_scaling", "calibration", "pressure_policy", "pressure_observer", "compact"],
            "profile_policy": "Separate descriptive per-operation attribution; never rank profiled throughput against unprofiled throughput."
        }, "suites": []}
specs = [
    ("calibration", "calibration", 10, 6_000_000, 500_000),
    ("topology-study", "topology-study", 10, 8_000_000, 500_000),
    ("harness-control", "harness-control", 10, 16_000_000, 500_000),
    ("pressure-performance", "pressure-control", 10, 4_000_000, 100_000),
    ("pressure-observer", "pressure-control", 10, 4_000_000, 100_000),
    ("compact-dense", "compact-window", 10, 400_000, 0),
    ("profiling", "profiling", 6, None, 500_000),
    ("profiling-arena-fill", "profiling-arena-fill", 6, 1, 0),
    ("profiling-arena", "profiling-arena", 6, 4_000_000, 100_000),
]
for name, source, repeats, operations, warmup in specs:
    config = json.loads((project / "examples" / (source + ".json")).read_text())
    config["repetitions"] = repeats
    for case in config["cases"]:
        case["operations"] = operations if operations is not None else (20_000_000 if case["profile"] == "cpu" else 2_000_000)
        case["warmup_ops"] = warmup
        if name.startswith("pressure-"):
            case["sample_cache_stats"] = name == "pressure-observer"
        if name == "compact-dense":
            case["request_trace_every"] = 1
            case["max_request_samples"] = operations
    path = output / "configs" / (name + ".json")
    path.write_text(json.dumps(config, indent=2) + "\n")
    count = repeats * len(config["cases"]) * len(config["runtimes"]) * len(config["backends"])
    plan["suites"].append({"name": name, "jobs": count, "profiled": name.startswith("profiling"), "config_sha256": digest(path)})
plan["total_jobs"] = sum(s["jobs"] for s in plan["suites"])
assert plan["total_jobs"] == 732
(output / "study-plan.json").write_text(json.dumps(plan, indent=2) + "\n")
print(f"Prepared {plan['total_jobs']} jobs in {output}")
PY
  exit 0
fi
[[ "$action" == run && $# == 2 ]] || usage
output=$2
[[ -f "$output/study-plan.json" ]] || {
  printf 'Prepare the study and its comparison policy first.\n' >&2; exit 2;
}
[[ ! -e "$output/executed-driver.sh" ]] || { printf 'This study has already started.\n' >&2; exit 2; }
python3 - "$output" <<'PY'
import hashlib, json, pathlib, sys
root = pathlib.Path(sys.argv[1])
plan = json.loads((root / "study-plan.json").read_text())
def digest(p): return hashlib.sha256(p.read_bytes()).hexdigest()
for field in ("controller", "worker"):
    if digest(pathlib.Path(plan[field])) != plan[field + "_sha256"]:
        raise SystemExit(field + " changed since preparation")
for suite in plan["suites"]:
    if digest(root / "configs" / (suite["name"] + ".json")) != suite["config_sha256"]:
        raise SystemExit("config changed: " + suite["name"])
PY
controller=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["controller"])' "$output/study-plan.json")
worker=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["worker"])' "$output/study-plan.json")
cp -- "$0" "$output/executed-driver.sh"
{
  printf '#!/usr/bin/env bash\n# Exact invocation; original output cannot be reused.\nset -euo pipefail\n'
  printf 'cd -- %q\n' "$PWD"
  printf '%q ' "$0" "$@"
  printf '\n'
} > "$output/executed-driver-command.sh"
trap 'status=$?; printf "%s\n" "$status" > "$output/driver-exit-status.txt"' EXIT
for name in calibration topology-study harness-control pressure-performance pressure-observer compact-dense profiling profiling-arena-fill profiling-arena; do
  "$output/recorded-run-suite.sh" "$controller" "$worker" "$output/configs/$name.json" "$output/$name" "repeated-$name"
done
# No profile analysis overlaps a timed worker.
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  printf 'cd -- %q\n' "$PWD"
} > "$output/profile-analysis-commands.sh"
for suite in profiling profiling-arena-fill profiling-arena; do
  for analysis in "$output/$suite"/profiles/*/analyze.sh; do
    profile_dir=$(dirname -- "$analysis")
    unit=ns
    [[ "$suite" == profiling ]] || unit=B
    args=(-nodecount=0 -nodefraction=0 -edgefraction=0 "-unit=$unit")
    printf '%q ' "$analysis" "${args[@]}" >> "$output/profile-analysis-commands.sh"
    printf '> %q\n' "$profile_dir/top.txt" >> "$output/profile-analysis-commands.sh"
    "$analysis" "${args[@]}" > "$profile_dir/top.txt"
    printf '%q ' "$analysis" -cum "${args[@]}" >> "$output/profile-analysis-commands.sh"
    printf '> %q\n' "$profile_dir/cumulative.txt" >> "$output/profile-analysis-commands.sh"
    "$analysis" -cum "${args[@]}" > "$profile_dir/cumulative.txt"
  done
done
printf 'Repeated study completed: %s\n' "$output"
