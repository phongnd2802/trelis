#!/usr/bin/env bash
# The Trelis an Ablation Run talks to: the service built from this checkout,
# with its embedding role routed through the record/replay model proxy (#41),
# so a paid embedding is made once and every later run replays it for free.
# `--rerank` routes the rerank role the same way (#157), so a rerank call is
# paid once and replayed after that.
# It serves the dev database (the ingested Benchmark Corpus) on :8080, in
# place of `make dev`, and runs in the background until `down`.
#
# Usage:
#   scripts/eval-stack.sh up record [--rerank]   # paid calls go upstream once and are stored
#   scripts/eval-stack.sh up replay [--rerank]   # fixtures only: a missing one is an error, no key needed
#   scripts/eval-stack.sh down
#   scripts/eval-stack.sh status
#
# Environment:
#   EVAL_FIXTURES            fixture tree (default eval/fixtures/models, gitignored)
#   EVAL_PROXY_ADDR          proxy listen address (default 127.0.0.1:4100)
#   EVAL_EMBEDDING_UPSTREAM  record mode's embedding provider
#                            (default https://openrouter.ai/api/v1)
#   EVAL_RERANK_UPSTREAM     record mode's rerank provider, read with --rerank
#                            (default https://openrouter.ai/api/v1)
#   TRELIS_MODELS_EMBEDDING_API_KEY  record mode: the provider key, which
#                            Trelis sends and the proxy forwards
#   TRELIS_MODELS_RERANK_API_KEY     record mode with --rerank: the same for
#                            the rerank provider
#   .local/models.env        exported for `up` when present; the environment
#                            wins over the file (unlike `make dev`)
#   any other TRELIS_* setting is passed to Trelis unchanged
#
# Needs `make dev-up`. Logs and pids live in .scratch/eval-stack/.
set -euo pipefail

usage() {
  sed -n '/^# Usage:/,/^#$/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//' >&2
  exit 64
}

root=$(git rev-parse --show-toplevel)
cd "$root"

state=.scratch/eval-stack
fixtures=${EVAL_FIXTURES:-eval/fixtures/models}
proxy_addr=${EVAL_PROXY_ADDR:-127.0.0.1:4100}
upstream=${EVAL_EMBEDDING_UPSTREAM:-https://openrouter.ai/api/v1}
rerank_upstream=${EVAL_RERANK_UPSTREAM:-https://openrouter.ai/api/v1}
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

# report_dead <name> prints the tail of a dead process's log and exits.
report_dead() {
  local name=$1
  echo "eval-stack.sh: $name exited; last lines of $state/$name.log:" >&2
  tail -15 "$state/$name.log" >&2
  exit 1
}

# wait_for <seconds> <description> <command...> polls until the command
# succeeds, or reports the log of whatever died.
wait_for() {
  local seconds=$1 what=$2; shift 2
  local deadline=$(( $(date +%s) + seconds ))
  until "$@" >/dev/null 2>&1; do
    for name in proxy trelis; do
      if [ -f "$state/$name.pid" ] && ! alive "$name"; then
        report_dead "$name"
      fi
    done
    [ "$(date +%s)" -lt "$deadline" ] || { echo "eval-stack.sh: timed out waiting for $what" >&2; exit 1; }
    sleep 0.5
  done
}

cmd=${1:-}
case "$cmd" in
  up)
    [ $# -ge 2 ] && [ $# -le 3 ] || usage
    mode=$2
    rerank=false
    case "$mode" in record|replay) ;; *) usage ;; esac
    if [ $# -eq 3 ]; then
      case "$3" in
        --rerank) rerank=true ;;
        *) usage ;;
      esac
    fi
    # Export .local/models.env for `up`: a variable already in the environment
    # wins over the file, unlike `make dev`. A `bash -e` child does the source
    # because errexit is ignored in a command substitution tested by `||`, and
    # a failing line or syntax error must stop the import, not half-fill it.
    if [ -f .local/models.env ]; then
      models_env=$(
        bash -e -c '
          before=" $(compgen -e | tr "\n" " ") "
          set -a
          . ./.local/models.env
          set +a
          for name in $(compgen -e); do
            case "$before" in
              *" $name "*) continue ;;
            esac
            declare -p "$name" 2>/dev/null || true
          done
        '
      ) || { echo "eval-stack.sh: cannot read .local/models.env" >&2; exit 1; }
      eval "$models_env"
    fi
    if alive proxy || alive trelis; then
      echo "eval-stack.sh: already running; run \`scripts/eval-stack.sh down\` first" >&2; exit 1
    fi
    if curl -fsS -m 2 "$trelis_url/healthz" >/dev/null 2>&1; then
      echo "eval-stack.sh: something already serves $trelis_url (make dev?); stop it first" >&2; exit 1
    fi
    if curl -s -o /dev/null -m 2 "http://$proxy_addr/"; then
      echo "eval-stack.sh: something already serves http://$proxy_addr; stop it first" >&2; exit 1
    fi
    if [ "$mode" = record ] && [ -z "${TRELIS_MODELS_EMBEDDING_API_KEY:-}" ]; then
      echo "eval-stack.sh: record mode needs TRELIS_MODELS_EMBEDDING_API_KEY for the upstream provider" >&2; exit 1
    fi
    if [ "$mode" = record ] && [ "$rerank" = true ] && [ -z "${TRELIS_MODELS_RERANK_API_KEY:-}" ]; then
      echo "eval-stack.sh: record mode with --rerank needs TRELIS_MODELS_RERANK_API_KEY for the upstream provider" >&2; exit 1
    fi
    if [ "$rerank" = false ]; then
      if [ -n "${TRELIS_MODELS_RERANK_BASE_URL:-}" ] || grep -Eq '^[[:space:]]+rerank:' config/trelis.yaml; then
        echo "eval-stack.sh: warning: rerank is configured but not routed through the proxy, so rerank calls reach the provider unrecorded; pass --rerank" >&2
      fi
    fi
    mkdir -p "$state" "$fixtures"

    go build -o "$state/trelis" ./cmd/server
    uv sync --project eval -q

    proxy_args=(proxy --mode "$mode" --fixtures "$fixtures" --listen "$proxy_addr")
    if [ "$mode" = record ]; then
      proxy_args+=(--upstream "embedding=$upstream")
      if [ "$rerank" = true ]; then
        proxy_args+=(--upstream "rerank=$rerank_upstream")
      fi
    fi
    nohup eval/.venv/bin/trelis-eval "${proxy_args[@]}" > "$state/proxy.log" 2>&1 &
    echo $! > "$state/proxy.pid"
    # Any HTTP answer (a GET is a 405) means the proxy has bound its port.
    wait_for 20 "the proxy on $proxy_addr" curl -s -o /dev/null -m 2 "http://$proxy_addr/"
    alive proxy || report_dead proxy

    export TRELIS_MODELS_EMBEDDING_BASE_URL="http://$proxy_addr/embedding"
    if [ "$rerank" = true ]; then
      export TRELIS_MODELS_RERANK_BASE_URL="http://$proxy_addr/rerank"
    fi
    nohup "$state/trelis" -config config/trelis.yaml > "$state/trelis.log" 2>&1 &
    echo $! > "$state/trelis.pid"
    wait_for 90 "Trelis at $trelis_url/readyz" curl -fsS -m 2 "$trelis_url/readyz"

    routes="embedding via http://$proxy_addr/embedding"
    if [ "$rerank" = true ]; then
      routes="$routes, rerank via http://$proxy_addr/rerank"
    fi
    cat >&2 <<EOF
eval stack up ($mode): Trelis $trelis_url, $routes
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
