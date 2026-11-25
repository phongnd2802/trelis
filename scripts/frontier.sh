#!/usr/bin/env bash
# List open issues that are ready to start: no open blocker, no assignee.
# One GraphQL call per 100 issues instead of one REST call per issue.
#
# Usage:
#   scripts/frontier.sh                    # ready-for-agent + ready-for-human
#   scripts/frontier.sh ready-for-agent    # only that label
#   scripts/frontier.sh --all              # every open issue, with blocker counts
set -euo pipefail

label_filter=${1:-}
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
owner=${repo%/*}
name=${repo#*/}

gh api graphql --paginate -F owner="$owner" -F name="$name" -f query='
query($owner: String!, $name: String!, $endCursor: String) {
  repository(owner: $owner, name: $name) {
    issues(first: 100, after: $endCursor, states: OPEN, orderBy: {field: CREATED_AT, direction: ASC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        title
        issueDependenciesSummary { blockedBy }
        assignees(first: 1) { totalCount }
        labels(first: 20) { nodes { name } }
      }
    }
  }
}' --jq '.data.repository.issues.nodes[]
  | {n: .number, t: .title, b: .issueDependenciesSummary.blockedBy,
     a: .assignees.totalCount, l: [.labels.nodes[].name]}
  | @json' |
jq -r --arg f "$label_filter" '
  if $f == "--all" then
    "#\(.n)\tblocked_by=\(.b)\t[\(.l | join(","))]\t\(.t)"
  elif .b == 0 and .a == 0 and
       (if $f == "" then (.l | index("ready-for-agent") or index("ready-for-human"))
        else (.l | index($f)) end) then
    "#\(.n)\t[\(.l | join(","))]\t\(.t)"
  else empty end'
