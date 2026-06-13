#!/usr/bin/env bash
# Shows what would be shipped if you ran `make release` right now, then
# prompts y/N. Reads only from git and `gh`; never touches the cluster.
#
# Flags:
#   --no-confirm        Skip the prompt (used by tests and for dry runs).
#   --postgres          Note in the preflight that postgres is opted-in.

set -euo pipefail

NO_CONFIRM=0
POSTGRES=0
for arg in "$@"; do
  case "$arg" in
    --no-confirm) NO_CONFIRM=1 ;;
    --postgres) POSTGRES=1 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

LAST_TAG=$(git describe --tags --match 'v*-mk*' --abbrev=0)
COMMITS=$(git log --oneline "${LAST_TAG}..HEAD")
COMMIT_COUNT=$(printf '%s\n' "$COMMITS" | grep -c . || true)

# Extract PR numbers from merge subjects. Handles both "Merge pull request #N"
# (queue-merge) and "(#N)" (squash-merge) styles.
PR_NUMBERS=$(git log --merges --pretty=%s "${LAST_TAG}..HEAD" \
  | sed -nE 's/.*#([0-9]+).*/\1/p' \
  | sort -un)

PR_COUNT=0
if [ -n "$PR_NUMBERS" ]; then
  PR_COUNT=$(printf '%s\n' "$PR_NUMBERS" | wc -l | tr -d ' ')
fi

echo "── Release preflight ──────────────────────────────────────"
echo "Last shipped: ${LAST_TAG}"
echo "Range:        ${LAST_TAG}..HEAD  (${COMMIT_COUNT} commits, ${PR_COUNT} PRs)"
if [ "$POSTGRES" -eq 1 ]; then
  echo "Extras:       + postgres rebuild"
fi
echo

if [ -z "$PR_NUMBERS" ]; then
  echo "(no merged PRs since ${LAST_TAG})"
else
  while read -r pr; do
    [ -z "$pr" ] && continue
    local_json=$(gh pr view "$pr" --json title,body 2>/dev/null || echo '{}')
    title=$(printf '%s' "$local_json" | sed -nE 's/.*"title":"([^"]*)".*/\1/p')
    body=$(printf '%s' "$local_json" | sed -nE 's/.*"body":"([^"]*)".*/\1/p')
    summary=$(printf '%b' "${body//\\n/$'\n'}" \
      | awk 'BEGIN{p=0} /## Summary/{p=1; next} /^## /{p=0} p{print}' \
      | sed -E 's/^- /  • /')
    echo "#${pr}  ${title:-<no title>}"
    [ -n "$summary" ] && printf '%s\n' "$summary"
    echo
  done <<<"$PR_NUMBERS"
fi

echo "──────────────────────────────────────────────────────────"
printf 'Ship %d commits across %d PRs? [y/N] ' "$COMMIT_COUNT" "$PR_COUNT"

if [ "$NO_CONFIRM" -eq 1 ]; then
  echo "(skipped)"
  exit 0
fi

read -r answer
case "$answer" in
  y|Y|yes|YES) exit 0 ;;
  *) echo "Aborted."; exit 1 ;;
esac
