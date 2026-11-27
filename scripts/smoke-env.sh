#!/usr/bin/env bash
# A throwaway Trelis next to the dev one, for smoke tests that must not leave
# rows in the dev database (Documents, Chunks, embeddings by a fake model).
# Same isolation the integration harness uses: its own database and its own
# Temporal task queues (two Trelis on one queue would take each other's
# ingest work) and its own port. The bucket is shared; its keys are per upload.
#
# Usage:
#   eval "$(scripts/smoke-env.sh up <name>)"   # create trelis_<name>, export its env
#   make dev                                   # or go run ./cmd/server; serves :8090
#   scripts/smoke-env.sh down <name>           # drop trelis_<name>
#
# <name> is lower-case letters, digits and underscores. Needs `make dev-up`.
set -euo pipefail

usage() { sed -n '7,11p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 64; }

[ $# -eq 2 ] || usage
cmd=$1 name=$2
[[ $name =~ ^[a-z0-9_]+$ ]] || { echo "smoke-env.sh: bad name $name" >&2; exit 64; }
db="trelis_$name"
psql() { docker compose -f compose.dev.yaml exec -T postgres psql -U trelis -d trelis -v ON_ERROR_STOP=1 -qAt "$@"; }

case "$cmd" in
  up)
    if [ "$(psql -c "SELECT 1 FROM pg_database WHERE datname = '$db'")" != 1 ]; then
      psql -c "CREATE DATABASE $db" >/dev/null
    fi
    # Trelis applies the migrations to the new database on start.
    cat <<EOF
export TRELIS_POSTGRES_DBNAME=$db
export TRELIS_SERVER_ADDR=127.0.0.1:8090
export TRELIS_TEMPORAL_TASK_QUEUE=$name-ingest
export TRELIS_TEMPORAL_MODEL_TASK_QUEUE=$name-models
export TRELIS_BASE_URL=http://127.0.0.1:8090
EOF
    ;;
  down)
    psql -c "DROP DATABASE IF EXISTS $db WITH (FORCE)" >/dev/null
    echo "dropped $db" >&2
    ;;
  *) usage ;;
esac
