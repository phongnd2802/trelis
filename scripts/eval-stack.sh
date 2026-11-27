#!/usr/bin/env bash
# The Trelis an Ablation Run talks to: the service built from this checkout,
# with its embedding role routed through the record/replay model proxy (#41),
# so a paid embedding is made once and every later run replays it for free.
# It serves the dev database (the ingested Benchmark Corpus) on :8080, in
# place of `make dev`, and runs in the background until `down`.
#
# Usage:
#   scripts/eval-stack.sh up record   # paid calls go upstream once and are stored
#   scripts/eval-stack.sh up replay   # fixtures only: a missing one is an error, no key needed
#   scripts/eval-stack.sh down
#   scripts/eval-stack.sh status
#
# Environment:
#   EVAL_FIXTURES            fixture tree (default eval/fixtures/models, gitignored)
#   EVAL_PROXY_ADDR          proxy listen address (default 127.0.0.1:4100)
#   EVAL_EMBEDDING_UPSTREAM  record mode's embedding provider
#                            (default https://openrouter.ai/api/v1)
#   TRELIS_MODELS_EMBEDDING_API_KEY  record mode: the provider key, which
#                            Trelis sends and the proxy forwards
#   any other TRELIS_* setting is passed to Trelis unchanged
#
# Needs `make dev-up`. Logs and pids live in .scratch/eval-stack/.
set -euo pipefail

usage() { sed -n '8,12p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 64; }

root=$(git rev-parse --show-toplevel)
cd "$root"
state=.scratch/eval-stack
fixtures=${EVAL_FIXTURES:-eval/fixtures/models}
proxy_addr=${EVAL_PROXY_ADDR:-127.0.0.1:4100}
upstream=${EVAL_EMBEDDING_UPSTREAM:-https://openrouter.ai/api/v1}
trelis_url=http://localhost:8080

alive() { [ -f "$state/$1.pid" ] && kill -0 "$(cat "$state/$1.pid")" 2>/dev/null; }

stop() {
  local name=$1 pid
  alive "$name" || { rm -f "$state/$name.pid"; return 0; }
  pid=$(cat "$state/$name.pid")
  kill -TERM "$pid"
  timeout 20 bash -c "while kill -0 $pid 2>/dev/null; do sleep 0.2; done" || kill -KILL "$pid" 2>/dev/null || true
  rm -f "$state/$name.pid"
  echo "stopped $name ($pid)" >&2
}

# wait_for <seconds> <description> <command...> polls until the command
# succeeds, or reports the log of whatever died.
wait_for() {
  local seconds=$1 what=$2; shift 2
  local deadline=$(( $(date +%s) + seconds ))
  until "$@" >/dev/null 2>&1; do
    for name in proxy trelis; do
      if [ -f "$state/$name.pid" ] && ! alive "$name"; then
        echo "eval-stack.sh: $name exited; last lines of $state/$name.log:" >&2
        tail -15 "$state/$name.log" >&2
        exit 1
      fi
    done
    [ "$(date +%s)" -lt "$deadline" ] || { echo "eval-stack.sh: timed out waiting for $what" >&2; exit 1; }
    sleep 0.5
  done
}

cmd=${1:-}
case "$cmd" in
  up)
    [ $# -eq 2 ] || usage
    mode=$2
    case "$mode" in record|replay) ;; *) usage ;; esac
    if alive proxy || alive trelis; then
      echo "eval-stack.sh: already running; run \`scripts/eval-stack.sh down\` first" >&2; exit 1
    fi
    if curl -fsS -m 2 "$trelis_url/healthz" >/dev/null 2>&1; then
      echo "eval-stack.sh: something already serves $trelis_url (make dev?); stop it first" >&2; exit 1
    fi
    if [ "$mode" = record ] && [ -z "${TRELIS_MODELS_EMBEDDING_API_KEY:-}" ]; then
      echo "eval-stack.sh: record mode needs TRELIS_MODELS_EMBEDDING_API_KEY for the upstream provider" >&2; exit 1
    fi
    mkdir -p "$state" "$fixtures"

    go build -o "$state/trelis" ./cmd/server
    uv sync --project eval -q

    proxy_args=(proxy --mode "$mode" --fixtures "$fixtures" --listen "$proxy_addr")
    [ "$mode" = record ] && proxy_args+=(--upstream "embedding=$upstream")
    nohup eval/.venv/bin/trelis-eval "${proxy_args[@]}" > "$state/proxy.log" 2>&1 &
    echo $! > "$state/proxy.pid"
    # Any HTTP answer (a GET is a 405) means the proxy has bound its port.
    wait_for 20 "the proxy on $proxy_addr" curl -s -o /dev/null -m 2 "http://$proxy_addr/"

    TRELIS_MODELS_EMBEDDING_BASE_URL="http://$proxy_addr/embedding" \
      nohup "$state/trelis" -config config/trelis.yaml > "$state/trelis.log" 2>&1 &
    echo $! > "$state/trelis.pid"
    wait_for 90 "Trelis at $trelis_url/readyz" curl -fsS -m 2 "$trelis_url/readyz"

    cat >&2 <<EOF
eval stack up ($mode): Trelis $trelis_url, embedding via http://$proxy_addr/embedding
fixtures: $fixtures ($(find "$fixtures" -type f -name '*.json' | wc -l) recorded)
logs: $state/trelis.log, $state/proxy.log; stop with scripts/eval-stack.sh down
EOF
    ;;
  down)
    [ $# -eq 1 ] || usage
    stop trelis
    stop proxy
    ;;
  status)
    [ $# -eq 1 ] || usage
    for name in proxy trelis; do
      if alive "$name"; then echo "$name: running ($(cat "$state/$name.pid"))"; else echo "$name: stopped"; fi
    done
    ;;
  *) usage ;;
esac
