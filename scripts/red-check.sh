#!/usr/bin/env bash
# Prove a regression test is red without the fix: put <file>... back as they
# are at <ref> (default main), run the test, then restore the working tree,
# even on Ctrl-C. The check passes only when the test FAILS on its own
# assertion; a build error, a skip, or no matching test fails the check.
#
# Usage:
#   scripts/red-check.sh [-r <ref>] <package> <test-regexp> <file>...
#   scripts/red-check.sh [-r <ref>] --each <package> <TestA,TestB,...> <file>...
#   scripts/red-check.sh [-r <ref>] --py <node-id>[,<node-id>...] <file>...
#
#   -r may appear anywhere among the arguments.
#   e.g. scripts/red-check.sh ./internal/application/repository/ 'TestSearchDense' \
#          internal/db/sqlc/chunks.sql.go
#        scripts/red-check.sh -r 6a15a6c --py \
#          eval/tests/test_model_proxy.py::test_serve_surfaces_a_taken_port_as_os_error \
#          eval/src/trelis_eval/model_proxy.py
#
# <file>... are the fix: usually the changed source, or its generated output
# (sqlc, swag). A file absent at <ref> is removed for the run.
#
# --each runs each named Go test on its own (-run '^TestA$'), prints one
# verdict line per test, and exits 0 only when every test is red; otherwise
# the first non-red code below wins (build failed, then skipped, then passed).
# The single-test form is the same path with one pattern.
#
# --py runs pytest in eval/ once per node id, with a JUnit XML report in the
# backup directory; scripts/red-check-junit.py classifies it. A node id is
# repo-relative under eval/, e.g. eval/tests/test_x.py::test_name. RED means
# a <failure> whose exception is AssertionError, or pytest's own Failed
# (pytest.fail, or a failed pytest.raises: both are the test's own
# expectation); any other exception (an AttributeError escaping pytest.raises
# included), an <error> (setup or collection), a skip, an unmatched node id or
# a report with no readable exception type fails the check. A node id that
# matches several cases is red only when every case fails on an assertion; its
# verdict reports the counts.
#
# Go DB tests get the same DSN as make test from scripts/test-db-env.sh.
#
# Exit codes: 0 red (the test failed on its own assertion) · 1 the test passed ·
#             2 not red: a Go build, a setup/collection error, any other
#             exception, or pytest rejecting the run · 3 skipped, or no test
#             matched · 64 red-check.sh usage
set -euo pipefail

usage() {
  awk '/^# Usage:/{seen=1} seen && /^#$/ {exit} seen {sub(/^# ?/, ""); print}' "$0" >&2
  exit 64
}

# -r is an option wherever it appears; everything else keeps its order.
ref=main
args=()
while [ $# -gt 0 ]; do
  case $1 in
    -r)
      [ $# -ge 2 ] || usage
      ref=$2
      shift 2
      ;;
    --each|--py)
      args+=("$1")
      shift
      ;;
    --)
      shift
      while [ $# -gt 0 ]; do
        args+=("$1")
        shift
      done
      ;;
    -*)
      echo "red-check.sh: unknown option $1" >&2
      usage
      ;;
    *)
      args+=("$1")
      shift
      ;;
  esac
done
set -- "${args[@]}"

mode=one
case "${1:-}" in
  --each)
    [ $# -ge 4 ] || usage
    mode=each
    pkg=$2
    names=$3
    shift 3
    ;;
  --py)
    [ $# -ge 3 ] || usage
    mode=py
    nodes=$2
    shift 2
    ;;
  *)
    [ $# -ge 3 ] || usage
    mode=one
    pkg=$1
    pattern=$2
    shift 2
    ;;
esac
[ $# -ge 1 ] || usage

root=$(git rev-parse --show-toplevel)
cd "$root"
git rev-parse --verify --quiet "$ref^{commit}" >/dev/null || { echo "red-check.sh: unknown ref $ref" >&2; exit 64; }
# The same DSN make test uses: an explicit TRELIS_TEST_POSTGRES_DSN wins,
# otherwise the dev Postgres when it is up. Without one a DB test skips, which
# this script reports as not red.
if [ "$mode" != py ]; then
  export TRELIS_TEST_POSTGRES_DSN="$(scripts/test-db-env.sh)"
fi

# Validate every argument before touching the working tree.
for f in "$@"; do
  case $f in
    --each|--py) echo "red-check.sh: only one mode may be given" >&2; exit 64 ;;
  esac
  [ -e "$f" ] || { echo "red-check.sh: $f does not exist in the working tree" >&2; exit 64; }
done

node_ids=()
if [ "$mode" = py ]; then
  IFS=, read -r -a raw_nodes <<<"$nodes"
  for node in "${raw_nodes[@]}"; do
    node=${node#"${node%%[![:space:]]*}"}
    node=${node%"${node##*[![:space:]]}"}
    [ -n "$node" ] || { echo "red-check.sh: --py needs a node id" >&2; exit 64; }
    case $node in
      eval/*) ;;
      *)
        echo "red-check.sh: --py node ids are repo-relative under eval/, got $node" >&2
        exit 64
        ;;
    esac
    path=${node%%::*}
    case "/$path/" in
      */../*)
        echo "red-check.sh: --py node id escapes eval/: $node" >&2
        exit 64
        ;;
    esac
    rel_path=${path#eval/}
    case $rel_path in
      ''|/*)
        echo "red-check.sh: --py node id must name a path under eval/: $node" >&2
        exit 64
        ;;
    esac
    node_ids+=("$node")
  done
  [ "${#node_ids[@]}" -ge 1 ] || usage
fi

go_names=()
go_patterns=()
legacy=0
if [ "$mode" = one ]; then
  go_names=("$pattern")
  go_patterns=("$pattern")
  legacy=1
elif [ "$mode" = each ]; then
  IFS=, read -r -a raw_tests <<<"$names"
  for t in "${raw_tests[@]}"; do
    t=${t#"${t%%[![:space:]]*}"}
    t=${t%"${t##*[![:space:]]}"}
    [ -n "$t" ] || { echo "red-check.sh: --each needs a test name" >&2; exit 64; }
    go_names+=("$t")
    go_patterns+=("^$t$")
  done
  [ "${#go_names[@]}" -ge 1 ] || usage
fi

mkdir -p "$root/.scratch"
backup=$(mktemp -d "$root/.scratch/red-check.XXXXXX")
files=()
restore() {
  trap - EXIT INT TERM
  for f in "${files[@]}"; do
    # files only ever holds paths whose backup succeeded, so this never
    # deletes an unbacked file.
    [ -e "$backup/$f" ] || continue
    mkdir -p "$(dirname "$f")"
    cp -p "$backup/$f" "$f"
  done
  rm -rf "$backup"
}
trap restore EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for f in "$@"; do
  mkdir -p "$backup/$(dirname "$f")"
  cp -p "$f" "$backup/$f"
  files+=("$f")
  if git cat-file -e "$ref:$f" 2>/dev/null; then
    git show "$ref:$f" > "$f"
  else
    rm -f "$f"
  fi
done

# classify_go <run-regexp> runs one Go test and sets go_verdict to red |
# passed | skipped | build; go_out keeps its output for the caller to quote.
classify_go() {
  local pattern=$1 status
  set +e
  go_out=$(go test -count=1 -v -run "$pattern" "$pkg" 2>&1)
  status=$?
  set -e
  if grep -q -e '\[build failed\]' -e '\[setup failed\]' <<<"$go_out"; then
    go_verdict=build
  elif [ "$status" -eq 0 ]; then
    if grep -q -e 'no tests to run' -e '^--- SKIP' <<<"$go_out" && ! grep -q '^--- PASS' <<<"$go_out"; then
      go_verdict=skipped
    else
      go_verdict=passed
    fi
  else
    go_verdict=red
  fi
}

# exit_ladder <hard> <skip> <pass>: 2 > 3 > 1 > 0.
exit_ladder() {
  if [ "$1" -eq 1 ]; then exit 2; fi
  if [ "$2" -eq 1 ]; then exit 3; fi
  if [ "$3" -eq 1 ]; then exit 1; fi
  exit 0
}

if [ "$mode" = one ] || [ "$mode" = each ]; then
  any_hard=0 any_skip=0 any_pass=0
  build_shown=0
  for i in "${!go_names[@]}"; do
    name=${go_names[$i]}
    classify_go "${go_patterns[$i]}"
    out=$go_out
    if [ "$legacy" -eq 1 ]; then
      case $go_verdict in
        build)
          any_hard=1
          printf '%s\n' "$out" | tail -20
          echo "red-check.sh: NOT RED: build failed at $ref; the test must fail on an assertion, not on a compile error" >&2
          ;;
        skipped)
          any_skip=1
          printf '%s\n' "$out" | grep -e '--- SKIP' -e 'no tests to run' -e 'skip' | head -10
          echo "red-check.sh: NOT RED: no test ran (skipped, or nothing matches $pattern)" >&2
          ;;
        passed)
          any_pass=1
          echo "red-check.sh: NOT RED: $pattern passes with $* as at $ref" >&2
          ;;
        red)
          printf '%s\n' "$out" | grep -e '^--- FAIL' -e '^    --- FAIL' -e '_test.go:[0-9]*:' | head -20
          echo "red-check.sh: RED: $pattern fails with $* as at $ref"
          ;;
      esac
    else
      case $go_verdict in
        build)
          any_hard=1
          if [ "$build_shown" -eq 0 ]; then
            printf '%s\n' "$out" | tail -20
            build_shown=1
          fi
          echo "red-check.sh: NOT RED: build failed: $name with $* as at $ref" >&2
          ;;
        skipped)
          any_skip=1
          printf '%s\n' "$out" | grep -e '--- SKIP' -e 'no tests to run' -e 'skip' | head -10
          echo "red-check.sh: NOT RED: skipped: $name with $* as at $ref" >&2
          ;;
        passed)
          any_pass=1
          echo "red-check.sh: NOT RED: passed: $name with $* as at $ref" >&2
          ;;
        red)
          printf '%s\n' "$out" | grep -e '^--- FAIL' -e '^    --- FAIL' -e '_test.go:[0-9]*:' | head -20
          echo "red-check.sh: RED: $name fails with $* as at $ref"
          ;;
      esac
    fi
  done
  exit_ladder "$any_hard" "$any_skip" "$any_pass"
fi

# --py: one pytest run per node id, classified from its JUnit XML report.
any_error=0 any_skip=0 any_pass=0
i=0
for node in "${node_ids[@]}"; do
  rel=${node#eval/}
  i=$((i + 1))
  xml="$backup/pytest-$i.xml"
  log="$backup/pytest-$i.log"
  set +e
  ( cd eval && uv run pytest -p no:cacheprovider -q --junitxml="$xml" "$rel" ) >"$log" 2>&1
  status=$?
  set -e
  verdict=$(python3 -I "$root/scripts/red-check-junit.py" "$xml" "$status" "$log")
  IFS=$'\x1f' read -r verdict kind detail counts <<<"$verdict"
  suffix=""
  if [ -n "$counts" ]; then
    suffix=" ($counts)"
  fi
  case $verdict in
    RED)
      if [ -n "$detail" ]; then printf '  %s\n' "$detail"; fi
      echo "red-check.sh: RED: $node fails on $kind$suffix with $* as at $ref"
      ;;
    "NOT RED")
      case $kind in
        passed)
          any_pass=1
          echo "red-check.sh: NOT RED: passed: $node$suffix with $* as at $ref" >&2
          ;;
        skipped)
          any_skip=1
          if [ -n "$detail" ]; then printf '  %s\n' "$detail"; fi
          echo "red-check.sh: NOT RED: skipped: $node$suffix with $* as at $ref" >&2
          ;;
        "no test")
          any_skip=1
          echo "red-check.sh: NOT RED: no test: $node matched nothing, with $* as at $ref" >&2
          ;;
        *)
          any_error=1
          if [ -n "$detail" ]; then printf '  %s\n' "$detail" >&2; fi
          echo "red-check.sh: NOT RED: $kind: $node$suffix with $* as at $ref" >&2
          ;;
      esac
      ;;
  esac
done
exit_ladder "$any_error" "$any_skip" "$any_pass"
