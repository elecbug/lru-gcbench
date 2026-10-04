#!/usr/bin/env bash
# Run one suite and keep the exact launcher and command beside its results.
set -euo pipefail
if [[ $# -lt 4 || $# -gt 5 ]]; then
  printf 'Usage: %s CONTROLLER WORKER CONFIG NEW_OUTPUT [LABEL]\n' "$0" >&2
  exit 2
fi
controller=$1
worker=$2
config=$3
output=$4
label=${5:-performance}
if [[ -e "$output" ]]; then
  printf 'Output already exists: %s\n' "$output" >&2
  exit 2
fi
snapshot=$(mktemp)
cp -- "$0" "$snapshot"
command=("$controller" run -worker "$worker" -config "$config" -out "$output" -label "$label")
record_exit() {
  status=$?
  trap - EXIT
  if [[ -d "$output" ]]; then
    cp -- "$snapshot" "$output/executed-script.sh"
    {
      printf '#!/usr/bin/env bash\n# Exact original invocation; use reproduce.sh for a new output directory.\nset -euo pipefail\n'
      printf 'cd -- %q\n' "$PWD"
      printf '%q ' "${command[@]}"
      printf '\n'
    } > "$output/executed-command.sh"
    chmod +x "$output/executed-script.sh" "$output/executed-command.sh"
  fi
  rm -f -- "$snapshot"
  exit "$status"
}
trap record_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"${command[@]}"
