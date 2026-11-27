#!/usr/bin/env bash
# The DSN the Postgres-backed tests run against, or nothing when there is
# none (dbtest then skips them). make test and make test-linked call this.
#
# An explicit TRELIS_TEST_POSTGRES_DSN always wins. Otherwise the dev Postgres
# from compose.dev.yaml is used when it accepts connections, so the SQL tests
# run locally whenever `make dev-up` is up. Without a server it prints a
# one-line hint to stderr and nothing to stdout. A different server on that
# port is used as-is, so the tests fail (auth, missing database) rather than
# skip.
#
# TRELIS_TEST_POSTGRES_HOST and TRELIS_TEST_POSTGRES_PORT default to the
# address compose.dev.yaml publishes (127.0.0.1:5432). POSTGRES_USER and
# POSTGRES_PASSWORD come from the shell, then the .env next to compose.dev.yaml,
# then that file's development credentials, the same order compose uses.
#
# Usage: scripts/test-db-env.sh   # prints the DSN, or nothing
set -euo pipefail

if [ -n "${TRELIS_TEST_POSTGRES_DSN:-}" ]; then
  printf '%s\n' "$TRELIS_TEST_POSTGRES_DSN"
  exit 0
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
host=${TRELIS_TEST_POSTGRES_HOST:-127.0.0.1}
port=${TRELIS_TEST_POSTGRES_PORT:-5432}

# One KEY=VALUE from the .env compose reads, without sourcing the file: only
# the asked-for key reaches this shell. The last occurrence wins, as in
# compose, and one pair of surrounding quotes is stripped.
env_value() {
  local key=$1 line name value=''
  [ -f "$root/.env" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line%$'\r'}
    name=${line%%=*}
    name=${name%"${name##*[![:space:]]}"}
    [ "$name" = "$key" ] || continue
    value=${line#*=}
    value=${value#"${value%%[![:space:]]*}"}
    value=${value%"${value##*[![:space:]]}"}
    case $value in
      \'*\') value=${value#\'}; value=${value%\'} ;;
      \"*\") value=${value#\"}; value=${value%\"} ;;
    esac
  done < "$root/.env"
  printf '%s' "$value"
}

# An empty variable uses compose's default, so it does not fall through to
# .env.
if [ -n "${POSTGRES_USER+x}" ]; then
  user=$POSTGRES_USER
else
  user=$(env_value POSTGRES_USER)
fi
user=${user:-trelis}

if [ -n "${POSTGRES_PASSWORD+x}" ]; then
  password=$POSTGRES_PASSWORD
else
  password=$(env_value POSTGRES_PASSWORD)
fi
password=${password:-trelis}

# Percent-encode userinfo, keeping only the RFC 3986 unreserved characters: a
# password with @, :, /, ? or % would otherwise end it early or break the URL.
urlencode() {
  local LC_ALL=C s=$1 out='' c i
  for ((i = 0; i < ${#s}; i++)); do
    c=${s:i:1}
    case $c in
      [A-Za-z0-9._~-]) out+=$c ;;
      *) printf -v c '%%%02X' "'$c"; out+=$c ;;
    esac
  done
  printf '%s' "$out"
}

# One second is plenty on localhost and keeps `make test` quick when the dev
# stack is down. pg_isready speaks the protocol; the bare TCP connect is the
# fallback when it is not installed.
reachable=0
if command -v pg_isready >/dev/null 2>&1; then
  if pg_isready -h "$host" -p "$port" -t 1 >/dev/null 2>&1; then
    reachable=1
  fi
elif timeout 1 bash -c ": >/dev/tcp/$host/$port" 2>/dev/null; then
  reachable=1
fi

if [ "$reachable" = 0 ]; then
  printf 'test-db-env.sh: Postgres DB tests skipped, no server at %s:%s; start `make dev-up` or set TRELIS_TEST_POSTGRES_DSN\n' "$host" "$port" >&2
  exit 0
fi

printf 'postgres://%s:%s@%s:%s/trelis?sslmode=disable\n' \
  "$(urlencode "$user")" "$(urlencode "$password")" "$host" "$port"