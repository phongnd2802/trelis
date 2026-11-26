#!/usr/bin/env bash
# Check that every repo path docs/feature-map.md names in backticks exists.
#
# A backticked token counts as a path when its first segment is a tracked
# top-level file or directory (internal/, cmd/, migrations/, compose.yaml...);
# routes (/documents), config keys (server.swagger) and commands are left
# alone. Run from the repo root (make lint does).
#
# Usage: scripts/check-feature-map.sh
set -euo pipefail

map=docs/feature-map.md
top=$(git ls-files | cut -d/ -f1 | sort -u)

status=0
while read -r token; do
  first=${token%%/*}
  grep -qxF -- "$first" <<<"$top" || continue
  if [ ! -e "$token" ]; then
    echo "check-feature-map: $map names $token, which does not exist" >&2
    status=1
  fi
done < <(grep -oE '`[^` ]+`' "$map" | tr -d '`' | sort -u)
exit $status
