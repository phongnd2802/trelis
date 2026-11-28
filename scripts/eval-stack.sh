#!/usr/bin/env bash
# The Trelis an Ablation Run talks to: the service built from this checkout,
# with its embedding role routed through the record/replay model proxy (#41),
# so a paid embedding is made once and every later run replays it for free.
# `--rerank` routes the rerank role the same way (#157), so a rerank call is
# paid once and replayed after that.
# It serves the dev database (the ingested Benchmark Corpus) on :8080, in
# place of `make dev`, and runs in the background until `down`.
#
# Before it starts the proxy or Trelis, `up` prints each routed role's model
# and how many fixtures are already recorded for that role and model (#166).
# The model is part of every request, so a changed one turns a record run into
# paid calls and a replay run into misses: a role whose fixtures were all
# recorded with another model warns, since every call for the configured
# model will be paid (record) or miss (replay).
#
# Usage:
#   scripts/eval-stack.sh up record [--rerank]   # paid calls go upstream once and are stored
#   scripts/eval-stack.sh up replay [--rerank]   # fixtures only: a missing one is an error, no key needed
#   scripts/eval-stack.sh down
#   scripts/eval-stack.sh status
#
# `up` prints the line an Ablation Run needs, `export TRELIS_EVAL_MODEL_MODE=<mode>`
# (record with `up record`, replay with `up replay`): the harness records the
# mode in its results header, and the report shows latency only for live and
# passthrough runs: a record run mixes recorded upstream calls with replayed
# ones, and a replay run replays every call.
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
#   TRELIS_MODELS_<ROLE>_MODEL  the model Trelis will send for a routed role;
#                            `up` reports it, falling back to that role's
#                            `model:` under `models:` in config/trelis.yaml
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

# role_model <role> prints the model Trelis will send for one role:
# TRELIS_MODELS_<ROLE>_MODEL when the environment names one, otherwise the
# role's `model:` under `models:` in config/trelis.yaml, otherwise nothing.
role_model() {
  local name value
  name="TRELIS_MODELS_$(printf '%s' "$1" | tr '[:lower:]' '[:upper:]')_MODEL"
  value=${!name:-}
  if [ -n "$value" ]; then
    printf '%s\n' "$value"
  else
    configured_model "$1"
  fi
}

# configured_model <role> prints models.<role>.model from config/trelis.yaml,
# or nothing. The file is YAML and the harness has no YAML library, so this
# small reader walks only the plain mapping layout the file uses: the
# top-level `models:` block, uncommented role keys, a `model:` under the role.
configured_model() {
  eval/.venv/bin/python - config/trelis.yaml "$1" <<'PY'
import sys

config, role = sys.argv[1], sys.argv[2]
try:
    with open(config, encoding="utf-8") as handle:
        lines = handle.readlines()
except OSError:
    sys.exit(0)
in_models = False
role_indent = None
in_role = False
for raw in lines:
    line = raw.split("#", 1)[0].rstrip()
    stripped = line.strip()
    if not stripped:
        continue
    indent = len(line) - len(line.lstrip())
    key, _, value = stripped.partition(":")
    if indent == 0:
        in_models = key == "models"
        role_indent = None
        in_role = False
    elif in_models:
        if role_indent is None:
            role_indent = indent
        if indent == role_indent:
            in_role = key == role
        elif in_role and key == "model":
            print(value.strip().strip("'\""))
            break
PY
}

# The three helpers below read `trelis-eval proxy fixtures` output, which is
# one unindented line per role and request model: "<role> <model>: N fixtures".

# fixture_count <role> <model> <summary> prints the fixtures recorded for one
# role and model, or 0.
fixture_count() {
  awk -v prefix="$1 $2: " '
    index($0, prefix) == 1 { count = $(NF - 1) }
    END { print (count == "" ? 0 : count) }
  ' <<<"$3"
}

# fixture_word <count> prints "fixture" or "fixtures" for a count.
fixture_word() {
  if [ "$1" -eq 1 ]; then
    printf 'fixture\n'
  else
    printf 'fixtures\n'
  fi
}

# role_fixture_count <role> <summary> prints every fixture recorded for a role.
role_fixture_count() {
  awk -v role="$1" '$1 == role { total += $(NF - 1) } END { print total + 0 }' <<<"$2"
}

# role_models <role> <summary> prints the models that role's fixtures used,
# comma-separated.
role_models() {
  awk -v role="$1" '
    $1 == role {
      rest = $0
      sub(/^[^ ]+ /, "", rest)
      sub(/: [0-9]+ fixtures?$/, "", rest)
      models = models (models == "" ? "" : ", ") rest
    }
    END { print models }
  ' <<<"$2"
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

    uv sync --project eval -q

    # What a paid run is about to send. The model travels in every request, so
    # a changed one misses every recorded fixture and a record run pays again:
    # show each routed role's model and the fixtures already answering it.
    # A broken fixture tree stops here with the harness's one-line error,
    # before the proxy or Trelis starts.
    fixtures_summary=$(eval/.venv/bin/trelis-eval proxy fixtures --fixtures "$fixtures") || exit 1
    routed_roles=(embedding)
    if [ "$rerank" = true ]; then
      routed_roles+=(rerank)
    fi
    echo "model and recorded fixtures per routed role:" >&2
    for role in "${routed_roles[@]}"; do
      model=$(role_model "$role")
      total=$(role_fixture_count "$role" "$fixtures_summary")
      noun=$(fixture_word "$total")
      if [ -z "$model" ]; then
        echo "  $role: model: unknown; $total $noun recorded under this role" >&2
        continue
      fi
      count=$(fixture_count "$role" "$model" "$fixtures_summary")
      echo "  $role: model $model; $count $(fixture_word "$count") recorded for it" >&2
      if [ "$count" -eq 0 ] && [ "$total" -gt 0 ]; then
        recorded=$(role_models "$role" "$fixtures_summary")
        if [ "$mode" = record ]; then
          echo "eval-stack.sh: warning: $role has $total $noun but none for model $model; every $role call will be paid (recorded models: $recorded)" >&2
        else
          echo "eval-stack.sh: warning: $role has $total $noun but none for model $model; every $role call will fail as a missing fixture (recorded models: $recorded)" >&2
        fi
      fi
    done

    go build -o "$state/trelis" ./cmd/server

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
for trelis-eval runs: export TRELIS_EVAL_MODEL_MODE=$mode
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
