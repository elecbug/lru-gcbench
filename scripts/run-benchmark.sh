#!/usr/bin/env bash
# Build, validate, measure, and analyze one recorded repeated study.
set -euo pipefail

fail() {
  printf '%s\n' "$*" >&2
  exit 2
}

[[ $# == 0 ]] || fail 'Usage: make benchmark RUN_ID=NAME [UPSTREAM_REPO=/path/to/go-lru]'
run_id=${RUN_ID:-}
[[ "$run_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || \
  fail 'Set RUN_ID to a new name starting with a letter or digit and containing only letters, digits, dots, underscores, or hyphens.'

project=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd -- "$project"
controller="./bin/lrugcbench-$run_id"
worker="./bin/worker-$run_id"
smoke_dir="results/smoke-$run_id"
result_dir="results/repeated-$run_id"
workflow_dir="results/workflow-$run_id"

# Reject reused IDs before running tools or touching existing artifacts.
for artifact in "$controller" "$worker" "$smoke_dir" "$result_dir" "$workflow_dir"; do
  [[ ! -e "$artifact" && ! -L "$artifact" ]] || \
    fail "Artifact already exists: $artifact. Use a new RUN_ID."
done
for tool in go python3 git tee; do
  command -v "$tool" >/dev/null || fail "Required tool is missing: $tool"
done
python3 -c 'import sys; sys.version_info >= (3, 11) or sys.exit("Python 3.11 or newer is required.")'

upstream_repo=${UPSTREAM_REPO:-../go-lru}
[[ -d "$upstream_repo" && -f "$upstream_repo/go.mod" ]] || \
  fail "UPSTREAM_REPO must point to a go-lru checkout: $upstream_repo"
upstream_repo=$(CDPATH= cd -- "$upstream_repo" && pwd)
git -C "$upstream_repo" rev-parse --is-inside-work-tree >/dev/null
upstream_status=$(git -C "$upstream_repo" status --porcelain)
[[ -z "$upstream_status" ]] || \
  fail 'The repeated-study analyzer requires a clean upstream checkout. Use a clean UPSTREAM_REPO before starting.'

mkdir -p -- bin results
# Reserve the ID atomically, including against another identical invocation.
mkdir -- "$workflow_dir"
finish() {
  status=$?
  trap - EXIT
  printf '%s\n' "$status" > "$workflow_dir/workflow-exit-status.txt"
  if [[ "$status" == 0 ]]; then
    printf 'Benchmark workflow completed: %s\n' "$result_dir"
  else
    printf 'Benchmark workflow failed (exit %s). See %s/workflow.log; use a new RUN_ID to restart.\n' "$status" "$workflow_dir"
  fi | tee -a "$workflow_dir/workflow.log"
  exit "$status"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cp -- "$project/scripts/run-benchmark.sh" "$workflow_dir/executed-workflow.sh"
cp -- "$project/Makefile" "$workflow_dir/Makefile"
{
  printf '#!/usr/bin/env bash\n# Recorded effective invocation; choose a new RUN_ID to replay.\nset -euo pipefail\n'
  printf 'cd -- %q\n' "$project"
  printf 'RUN_ID=%q UPSTREAM_REPO=%q bash %q\n' "$run_id" "$upstream_repo" "$project/scripts/run-benchmark.sh"
} > "$workflow_dir/executed-command.sh"
{
  printf '#!/usr/bin/env bash\n# Commands actually attempted, in execution order.\nset -euo pipefail\n'
  printf 'cd -- %q\n' "$project"
} > "$workflow_dir/commands.sh"
chmod +x "$workflow_dir/executed-command.sh" "$workflow_dir/commands.sh"

run() {
  {
    printf '\n+ '
    printf '%q ' "$@"
    printf '\n'
  } | tee -a "$workflow_dir/workflow.log"
  printf '%q ' "$@" >> "$workflow_dir/commands.sh"
  printf '\n' >> "$workflow_dir/commands.sh"
  "$@" 2>&1 | tee -a "$workflow_dir/workflow.log"
}

run go version
run python3 --version
run git -C "$upstream_repo" rev-parse HEAD
run go test -race ./...
run go vet ./...
run python3 scripts/analyze-repeated.py --self-test
run go build -o "$controller" ./cmd/lrugcbench
run "$controller" build -repo "$upstream_repo" -source . -out "$worker"
run ./scripts/run-recorded.sh "$controller" "$worker" examples/smoke.json "$smoke_dir" smoke
run ./scripts/run-repeated-study.sh prepare "$controller" "$worker" "$result_dir"
run python3 scripts/analyze-repeated.py --plan-only "$result_dir"
run ./scripts/run-repeated-study.sh run "$result_dir"
run python3 scripts/analyze-repeated.py "$result_dir"
run python3 scripts/analyze-profile-repeats.py "$result_dir"
