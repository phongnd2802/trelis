#!/usr/bin/env bash
# Prove a regression test is red without the fix: put <file>... back as they
# are at <ref> (default main), run the test, then restore the working tree,
# even on Ctrl-C. The check passes only when the test FAILS on its own
# assertion; a build error, a skip, or no matching test fails the check.
#
# Usage:
#   scripts/red-check.sh [-r <ref>] <package> <test-regexp> <file>...
#   e.g. scripts/red-check.sh ./internal/application/repository/ 'TestSearchDense' \
#          internal/db/sqlc/chunks.sql.go
#
# <file>... are the fix: usually the changed source, or its generated output
# (sqlc, swag). A file absent at <ref> is removed for the run. DB tests need
# TRELIS_TEST_POSTGRES_DSN, as for go test.
#
# Exit codes: 0 red (the test failed on an assertion) · 1 the test passed ·
#             2 build failed · 3 the test was skipped or did not run · 64 usage
set -euo pipefail

usage() { sed -n '7,10p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 64; }

ref=main
if [ "${1:-}" = -r ]; then
  [ $# -ge 2 ] || usage
  ref=$2; shift 2
fi
[ $# -ge 3 ] || usage
pkg=$1 pattern=$2; shift 2

root=$(git rev-parse --show-toplevel)
cd "$root"
git rev-parse --verify --quiet "$ref^{commit}" >/dev/null || { echo "red-check.sh: unknown ref $ref" >&2; exit 64; }

backup=$(mktemp -d "$root/.scratch/red-check.XXXXXX")
restore() {
  for f in "${files[@]}"; do
    if [ -e "$backup/$f" ]; then
      mkdir -p "$(dirname "$f")"
      cp -p "$backup/$f" "$f"
    else
      rm -f "$f"
    fi
  done
  rm -rf "$backup"
}
files=()
trap restore EXIT INT TERM

for f in "$@"; do
  [ -e "$f" ] || { echo "red-check.sh: $f does not exist in the working tree" >&2; exit 64; }
  files+=("$f")
  mkdir -p "$backup/$(dirname "$f")"
  cp -p "$f" "$backup/$f"
  if git cat-file -e "$ref:$f" 2>/dev/null; then
    git show "$ref:$f" > "$f"
  else
    rm -f "$f"
  fi
done

set +e
out=$(go test -count=1 -v -run "$pattern" "$pkg" 2>&1)
status=$?
set -e

if grep -q -e '\[build failed\]' -e '\[setup failed\]' <<<"$out"; then
  printf '%s\n' "$out" | tail -20
  echo "red-check.sh: NOT RED: build failed at $ref; the test must fail on an assertion, not on a compile error" >&2
  exit 2
fi
if [ "$status" -eq 0 ]; then
  if grep -q -e 'no tests to run' -e '^--- SKIP' <<<"$out" && ! grep -q '^--- PASS' <<<"$out"; then
    printf '%s\n' "$out" | grep -e '--- SKIP' -e 'no tests to run' -e 'skip' | head -10
    echo "red-check.sh: NOT RED: no test ran (skipped, or nothing matches $pattern)" >&2
    exit 3
  fi
  echo "red-check.sh: NOT RED: $pattern passes with $* as at $ref" >&2
  exit 1
fi
printf '%s\n' "$out" | grep -e '^--- FAIL' -e '^    --- FAIL' -e '_test.go:[0-9]*:' | head -20
echo "red-check.sh: RED: $pattern fails with $* as at $ref"
