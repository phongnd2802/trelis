#!/usr/bin/env bash
# Check that NOTICE and the Go sources agree on what came from WeKnora.
#
# A Go file that carries a Tencent copyright line was copied or derived from
# WeKnora, so NOTICE must list it; every Go path NOTICE lists must exist and
# carry that line. Run from the repo root (make lint does).
#
# Usage: scripts/check-notice.sh
set -euo pipefail

marker='Copyright (C) 2026 Tencent'

# Go files that say they came from WeKnora.
attributed=$(git ls-files '*.go' | xargs grep -lF "$marker" | sort -u)
# Go paths NOTICE lists: the indented "internal/…​.go" or "cmd/…​.go" lines
# under "Files and parts". The "from …" lines name WeKnora's own paths.
listed=$(grep -E '^    (internal|cmd)/[^ ]+\.go' NOTICE | awk '{print $1}' | sort -u)

status=0
while read -r f; do
  [ -n "$f" ] || continue
  echo "check-notice: $f has a Tencent copyright header but NOTICE does not list it" >&2
  status=1
done < <(comm -23 <(echo "$attributed") <(echo "$listed"))
while read -r f; do
  [ -n "$f" ] || continue
  if [ ! -f "$f" ]; then
    echo "check-notice: NOTICE lists $f, which does not exist" >&2
  else
    echo "check-notice: NOTICE lists $f, which has no \"$marker\" header" >&2
  fi
  status=1
done < <(comm -13 <(echo "$attributed") <(echo "$listed"))
exit $status
