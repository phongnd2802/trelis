#!/usr/bin/env bash
# Build and start the compose stack, waiting until every service is healthy
# or has completed. On failure, show what broke instead of hanging.
#
# Usage: scripts/stack-up.sh [extra `docker compose up` args]
#   WAIT_TIMEOUT=600 scripts/stack-up.sh    # seconds, default 300
#   BUILD=0 scripts/stack-up.sh             # use the already built trelis:dev image
#   COMPOSE_FILE=compose.dev.yaml scripts/stack-up.sh   # dependencies only (make dev-up)
set -uo pipefail

timeout=${WAIT_TIMEOUT:-300}
build_flag=--build
[ "${BUILD:-1}" = 0 ] && build_flag=--no-build

if docker compose up -d "$build_flag" --wait --wait-timeout "$timeout" "$@"; then
  docker compose ps -a
  exit 0
fi

echo >&2
echo "stack-up: compose did not become healthy within ${timeout}s" >&2
docker compose ps -a >&2
# Services that exited non-zero or are not running: show their recent logs.
docker compose ps -a --format '{{.Service}} {{.State}} {{.ExitCode}} {{.Health}}' |
  awk '($2 == "exited" && $3 != 0) || ($2 != "running" && $2 != "exited") || $4 == "unhealthy" { print $1 }' |
  sort -u |
  while read -r svc; do
    echo >&2
    echo "----- logs: $svc (last 40 lines) -----" >&2
    docker compose logs --no-color --tail 40 "$svc" >&2
  done
exit 1
