#!/usr/bin/env bash
# Drive Qwen Code through the local `qwen serve` daemon, so the user can watch
# and step into the same session (qwen attach --session <id>, or the Web Shell
# at the daemon URL). Used by /implement; see CLAUDE.md.
#
# Usage:
#   scripts/qwen.sh new                 # fresh session for one ticket, approval mode auto; prints its id
#   scripts/qwen.sh send <id> <file>    # send <file> as one prompt; refuses while the session is busy
#   scripts/qwen.sh wait <id>           # block until the prompt finishes; prints a line when Qwen
#                                       # waits on the user, fits Monitor or run_in_background
#   scripts/qwen.sh status <id>         # one-line JSON summary of the session
#   scripts/qwen.sh cancel <id>         # stop the running prompt
#
# Exit codes: 0 ok · 1 turn error · 2 wait timed out · 3 session busy ·
#             4 daemon or session unreachable · 64 usage
#
# Environment: QWEN_DAEMON_URL (default http://127.0.0.1:4170),
#              QWEN_WAIT_TIMEOUT seconds (default 1800).
# No X-Qwen-Client-Id header is sent: the daemon rejects ids it did not issue.
set -euo pipefail

url=${QWEN_DAEMON_URL:-http://127.0.0.1:4170}

usage() { sed -n '6,13p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 64; }

die() { echo "qwen.sh: $1" >&2; exit "${2:-4}"; }

# status_json prints the session's status, or exits 4.
status_json() {
  local s
  s=$(curl -fsS -m 5 "$url/session/$1/status" 2>/dev/null) || die "session $1 unreachable at $url (is qwen serve running?)"
  jq -e '.sessionId' >/dev/null <<<"$s" || die "unexpected status reply: ${s:0:200}"
  echo "$s"
}

summary() {
  jq -c '{sessionId, activeWorkState, hasActivePrompt, pendingPromptCount,
          waitingForUser: (.isWaitingForPermission or .isWaitingForUserQuestion), hasTurnError}'
}

cmd=${1:-}; shift || true
case "$cmd" in
  new)
    curl -fsS -m 5 "$url/health" >/dev/null 2>&1 || die "no daemon at $url: ask the user to run \`qwen serve\` in the repo"
    root=$(git rev-parse --show-toplevel)
    # sessionScope "thread" gives a fresh session; without it the daemon
    # hands back the workspace's shared one.
    reply=$(jq -n --arg cwd "$root" '{cwd: $cwd, sessionScope: "thread"}' |
      curl -fsS -m 60 -X POST "$url/session" -H 'Content-Type: application/json' --data-binary @-) ||
      die "could not create a session"
    id=$(jq -r '.sessionId // empty' <<<"$reply")
    [ -n "$id" ] || die "no sessionId in reply: ${reply:0:200}"
    curl -fsS -m 10 -X POST "$url/session/$id/approval-mode" \
      -H 'Content-Type: application/json' -d '{"mode":"auto"}' >/dev/null || die "could not set approval mode on $id"
    echo "$id"
    ;;
  send)
    [ $# -eq 2 ] || usage
    id=$1 file=$2
    [ -r "$file" ] || die "cannot read $file" 64
    s=$(status_json "$id")
    if jq -e '.hasActivePrompt or .pendingPromptCount > 0' >/dev/null <<<"$s"; then
      echo "qwen.sh: session $id is busy; wait for it first" >&2
      summary <<<"$s" >&2
      exit 3
    fi
    jq -n --rawfile t "$file" '{prompt: [{type: "text", text: $t}]}' |
      curl -fsS -m 30 -X POST "$url/session/$id/prompt" -H 'Content-Type: application/json' --data-binary @- |
      jq -r '"sent prompt \(.promptId)"'
    ;;
  wait)
    [ $# -eq 1 ] || usage
    id=$1
    deadline=$(( $(date +%s) + ${QWEN_WAIT_TIMEOUT:-1800} ))
    waiting=false
    while :; do
      s=$(status_json "$id")
      now_waiting=$(jq -r '.isWaitingForPermission or .isWaitingForUserQuestion' <<<"$s")
      if [ "$now_waiting" = true ] && [ "$waiting" = false ]; then
        echo "WAITING: Qwen needs the user (permission or question) in session $id"
      fi
      waiting=$now_waiting
      if jq -e '(.hasActivePrompt | not) and .pendingPromptCount == 0' >/dev/null <<<"$s"; then
        if jq -e '.hasTurnError' >/dev/null <<<"$s"; then
          echo "TURN ERROR in session $id"; exit 1
        fi
        echo "DONE: session $id is idle"; exit 0
      fi
      if [ "$(date +%s)" -ge "$deadline" ]; then
        echo "TIMEOUT: session $id still busy"; exit 2
      fi
      sleep 10
    done
    ;;
  status)
    [ $# -eq 1 ] || usage
    status_json "$1" | summary
    ;;
  cancel)
    [ $# -eq 1 ] || usage
    curl -fsS -m 10 -X POST "$url/session/$1/cancel" -H 'Content-Type: application/json' -d '{}' >/dev/null ||
      die "could not cancel $1"
    status_json "$1" | summary
    ;;
  *)
    usage
    ;;
esac
