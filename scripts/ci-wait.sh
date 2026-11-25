#!/usr/bin/env bash
# Wait for the latest GitHub Actions run on a branch and report each job.
# Exits 0 when the run succeeds, 1 when it fails, 2 on timeout.
#
# Usage: scripts/ci-wait.sh [branch]            # default: current branch
#   CI_WAIT_TIMEOUT=1800 scripts/ci-wait.sh     # seconds, default 1200
#   CI_WAIT_SHA=<sha> scripts/ci-wait.sh main   # wait for the run of this commit
set -euo pipefail

branch=${1:-$(git branch --show-current)}
sha=${CI_WAIT_SHA:-$(git rev-parse "origin/$branch" 2>/dev/null || git rev-parse HEAD)}
deadline=$(( $(date +%s) + ${CI_WAIT_TIMEOUT:-1200} ))

run_id=""
while :; do
  # gh's GraphQL-backed commands fail transiently; treat errors as "not yet".
  line=$(gh run list --branch "$branch" --commit "$sha" --limit 1 \
           --json databaseId,status,conclusion \
           --jq '.[0] | "\(.databaseId) \(.status) \(.conclusion)"' 2>/dev/null || true)
  if [ -n "$line" ] && [ "$line" != "null null null" ]; then
    read -r run_id status conclusion <<<"$line"
    [ "$status" = "completed" ] && break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "ci-wait: timed out waiting for ${run_id:-a run} on $branch@${sha:0:7}" >&2
    exit 2
  fi
  sleep 20
done

gh run view "$run_id" --json jobs --jq '.jobs[] | "\(.name): \(.conclusion)"'
echo "run $run_id on $branch@${sha:0:7}: $conclusion"
[ "$conclusion" = "success" ]
