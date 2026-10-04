#!/usr/bin/env bash
# Execute the diagnostic protocols serially, preserving all commands and inputs.
set -euo pipefail
if [[ $# -lt 3 || $# -gt 4 ]]; then
  printf 'Usage: %s CONTROLLER WORKER NEW_OUTPUT [pilot|full]\n' "$0" >&2
  exit 2
fi
controller=$1
worker=$2
output=$3
mode=${4:-pilot}
if [[ "$mode" != pilot && "$mode" != full ]]; then
  printf 'Mode must be pilot or full\n' >&2
  exit 2
fi
if [[ -e "$output" ]]; then
  printf 'Output already exists: %s\n' "$output" >&2
  exit 2
fi
project=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p -- "$output/configs"
cp -- "$0" "$output/executed-driver.sh"
{
  printf '#!/usr/bin/env bash\n# Original invocation; choose a fresh output path before replay.\nset -euo pipefail\n'
  printf 'cd -- %q\n' "$PWD"
  printf '%q ' "$0" "$@"
  printf '\n'
} > "$output/executed-driver-command.sh"
trap 'status=$?; printf "%s\n" "$status" > "$output/driver-exit-status.txt"' EXIT
python3 - "$project" "$output" "$mode" <<'PY'
import json
import pathlib
import sys

project, output = map(pathlib.Path, sys.argv[1:3])
mode = sys.argv[3]
suites = ["calibration", "topology-study", "harness-control", "pressure-control",
          "compact-window", "profiling", "profiling-arena-fill", "profiling-arena"]
plan = {"mode": mode, "purpose": "Diagnostic validation; pilot repetitions are insufficient for performance conclusions.", "suites": []}
for name in suites:
    config = json.loads((project / "examples" / (name + ".json")).read_text())
    profiled = name.startswith("profiling")
    if mode == "pilot" and not profiled:
        config["repetitions"] = 2
    if mode == "pilot":
        limit = 1_000_000 if name in ("topology-study", "harness-control") else 2_000_000
        for case in config["cases"]:
            case["operations"] = min(case["operations"], limit)
            case["warmup_ops"] = min(case["warmup_ops"], 100_000)
    path = output / "configs" / (name + ".json")
    path.write_text(json.dumps(config, indent=2) + "\n")
    jobs = config["repetitions"] * len(config["backends"]) * len(config["runtimes"]) * len(config["cases"])
    plan["suites"].append({"name": name, "jobs": jobs, "profiled": profiled})
plan["total_jobs"] = sum(s["jobs"] for s in plan["suites"])
(output / "diagnostic-plan.json").write_text(json.dumps(plan, indent=2) + "\n")
print(f"Running {plan['total_jobs']} fresh worker processes ({mode}).", flush=True)
PY
for name in calibration topology-study harness-control pressure-control compact-window profiling profiling-arena-fill profiling-arena; do
  "$project/scripts/run-recorded.sh" "$controller" "$worker" \
    "$output/configs/$name.json" "$output/$name" "diagnostic-$mode-$name"
done

# Analyze profiles only after every timed job has completed.
{
  printf '#!/usr/bin/env bash\n# Exact profile analysis commands used after measurement.\nset -euo pipefail\n'
  printf 'cd -- %q\n' "$PWD"
} > "$output/analysis-commands.sh"
for suite in profiling profiling-arena-fill profiling-arena; do
  for analysis in "$output/$suite"/profiles/*/analyze.sh; do
    profile_dir=$(dirname -- "$analysis")
    printf '%q -nodecount=40 > %q\n' "$analysis" "$profile_dir/top.txt" >> "$output/analysis-commands.sh"
    "$analysis" -nodecount=40 > "$profile_dir/top.txt"
    printf '%q -cum -nodecount=40 > %q\n' "$analysis" "$profile_dir/cumulative.txt" >> "$output/analysis-commands.sh"
    "$analysis" -cum -nodecount=40 > "$profile_dir/cumulative.txt"
  done
done
printf 'Diagnostic results and executed scripts: %s\n' "$output"
