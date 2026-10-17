#!/usr/bin/env bash
# Check callback and pressure-hook descendants without changing the checkout.
set -euo pipefail
if [[ $# != 2 ]]; then
  printf 'Usage: %s GO_LRU_CHECKOUT NEW_OUTPUT\n' "$0" >&2
  exit 2
fi
checkout=$(CDPATH= cd -- "$1" && pwd)
output=$(python3 -c 'import pathlib,sys; print(pathlib.Path(sys.argv[1]).resolve())' "$2")
[[ ! -e "$output" && ! -L "$output" ]] || { printf 'Output already exists: %s\n' "$output" >&2; exit 2; }
[[ -f "$checkout/go.mod" && ! -e "$checkout/zz_review_hook_reentrancy_test.go" ]] || {
  printf 'Expected a go-lru checkout without zz_review_hook_reentrancy_test.go.\n' >&2; exit 2;
}
mkdir -p -- "$(dirname -- "$output")"
mkdir -- "$output"
cp -- "$0" "$output/executed-script.sh"
trap 'status=$?; printf "%s\n" "$status" > "$output/exit-status.txt"' EXIT
{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n'
  printf 'cd -- %q\n' "$checkout"
} > "$output/commands.sh"
git -C "$checkout" rev-parse HEAD > "$output/commit.txt"
go version > "$output/toolchain.txt"
cat > "$output/hook_test.go.txt" <<'GO'
package lru_test

import (
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	lru "github.com/google/go-lru"
)

// Keep every ancestor alive until its child returns.
//go:noinline
func reviewHookDescendant(depth int, fn func()) {
	if depth == 0 {
		fn()
		return
	}
	done := make(chan struct{})
	go func() { defer close(done); reviewHookDescendant(depth-1, fn) }()
	<-done
}

func reviewHookCache(t *testing.T, options ...lru.Option) lru.Cache[int] {
	t.Helper()
	switch os.Getenv("REVIEW_BACKEND") {
	case "map":
		return lru.NewMapCache[int](10, options...)
	case "radix":
		return lru.NewRadixCache[int](10, options...)
	case "arena":
		return lru.NewArenaRadixCache[int](10, options...)
	default:
		t.Fatal("unknown REVIEW_BACKEND")
		return nil
	}
}

func reviewHookDepth(t *testing.T) int {
	t.Helper()
	n, err := strconv.Atoi(os.Getenv("REVIEW_DEPTH"))
	if err != nil { t.Fatal(err) }
	return n
}

func TestReviewHookCallbackDescendant(t *testing.T) {
	depth := reviewHookDepth(t)
	var c lru.Cache[int]
	var calls atomic.Int32
	c = reviewHookCache(t, lru.WithOnEvictValue(func(v int, _ lru.EvictionReason) {
		calls.Add(1)
		if v == 1 { reviewHookDescendant(depth, func() { c.Delete("b") }) }
	}))
	_, _ = c.Put("a", 1)
	_, _ = c.Put("b", 2)
	done := make(chan struct{})
	go func() { defer close(done); c.Delete("a") }()
	select {
	case <-done:
		if n := calls.Load(); n != 2 { t.Fatalf("callbacks=%d, want 2", n) }
	case <-time.After(time.Second):
		t.Fatalf("depth %d: callback waits for descendant Delete, which never returns", depth)
	}
}

func TestReviewHookPressureDescendant(t *testing.T) {
	depth := reviewHookDepth(t)
	var c lru.Cache[int]
	var calls atomic.Int32
	var armed atomic.Bool
	c = reviewHookCache(t, lru.WithPressureFunc(func() float64 {
		calls.Add(1)
		if armed.CompareAndSwap(true, false) {
			reviewHookDescendant(depth, func() { _, _ = c.Put("inner", 2) })
		}
		return 0.1
	}))
	armed.Store(true)
	_, _ = c.Put("outer", 1)
	if n := calls.Load(); n != 1 {
		t.Fatalf("depth %d: pressure hook invoked %d times, want 1", depth, n)
	}
}
GO
gofmt -w "$output/hook_test.go.txt"
python3 - "$checkout" "$output" <<'PY'
import json
import pathlib
import sys
checkout, output = map(pathlib.Path, sys.argv[1:])
(output / "overlay.json").write_text(json.dumps({"Replace": {
    str(checkout / "zz_review_hook_reentrancy_test.go"): str(output / "hook_test.go.txt")
}}) + "\n")
PY
failures=0
for depth in 1 8 12; do
  for backend in map radix arena; do
    label="depth$depth-$backend"
    {
      printf '%q ' env "REVIEW_DEPTH=$depth" "REVIEW_BACKEND=$backend" go test -overlay "$output/overlay.json" -race -count=1 -timeout=10s -run '^TestReviewHook' .
      printf '\n'
    } >> "$output/commands.sh"
    if (cd -- "$checkout" && REVIEW_DEPTH=$depth REVIEW_BACKEND=$backend go test -overlay "$output/overlay.json" -race -count=1 -timeout=10s -run '^TestReviewHook' .) > "$output/$label.log" 2>&1; then
      status=0
    else
      status=$?
      failures=$((failures+1))
    fi
    printf '%s\t%s\n' "$label" "$status" >> "$output/status.tsv"
    printf '%s exit=%s\n' "$label" "$status"
    if [[ $status != 0 ]]; then cat "$output/$label.log"; fi
  done
done
[[ $failures == 0 ]]
