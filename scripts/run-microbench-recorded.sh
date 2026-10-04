#!/usr/bin/env bash
# Standalone Go microbenchmarks, with exact scripts and environment metadata.
set -euo pipefail
if [[ $# -lt 3 || $# -gt 6 ]]; then
  printf 'Usage: %s REPO NEW_OUTPUT BENCH_REGEX [COUNT=10] [BENCHTIME=1s] [CPU=1]\n' "$0" >&2
  exit 2
fi
repo=$(cd -- "$1" && pwd)
output=$2
pattern=$3
repeats=${4:-10}
benchtime=${5:-1s}
cpus=${6:-1}
mkdir -p -- "$(dirname -- "$output")"
mkdir -- "$output"
output=$(cd -- "$output" && pwd)
cp -- "$0" "$output/executed-script.sh"
cache_dir=$(go env GOCACHE)
command=(env "GOCACHE=$cache_dir" GOPROXY=off GOGC=100 GOMEMLIMIT=off go test -run '^$' -bench "$pattern" -benchmem -count="$repeats" -benchtime="$benchtime" -cpu="$cpus" .)
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  printf 'cd -- %q\n' "$repo"
  printf '%q ' "${command[@]}"
  printf '\n'
} > "$output/executed-command.sh"
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  printf 'if [[ $# -ne 1 ]]; then printf "Usage: %%s NEW_OUTPUT\\n" "$0" >&2; exit 2; fi\n'
  printf 'script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)\n'
  printf 'export GOCACHE=%q\n' "$cache_dir"
  printf 'exec bash "$script_dir/executed-script.sh" %q "$1" %q %q %q %q\n' "$repo" "$pattern" "$repeats" "$benchtime" "$cpus"
} > "$output/reproduce.sh"
chmod +x "$output/"*.sh
(
  cd -- "$repo"
  go version
  go env GOOS GOARCH GOAMD64 GOFLAGS GOTOOLCHAIN
  printf 'GOCACHE=%s\nGOPROXY=off\n' "$cache_dir"
  printf 'GOGC=100\nGOMEMLIMIT=off\nGOMAXPROCS (-cpu)=%s\n' "$cpus"
  git rev-parse HEAD
  git status --short
) > "$output/environment.txt" 2>&1
status=0
(cd -- "$repo" && "${command[@]}") > "$output/benchmark.txt" 2> "$output/run.log" || status=$?
printf '%s\n' "$status" > "$output/exit-status.txt"
exit "$status"
