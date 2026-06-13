#!/usr/bin/env bash
# Helm template render tests. Each test invokes `helm template` against the
# chart with crafted values and greps the rendered output for expected
# (or forbidden) refs. No real cluster required.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="${REPO_ROOT}/packaging/helm/multica"

PASS=0
FAIL=0

assert_contains() {
  local name="$1" output="$2" needle="$3"
  if grep -qF -- "$needle" <<<"$output"; then
    echo "  PASS: $name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $name"
    echo "    expected to contain: $needle"
    FAIL=$((FAIL + 1))
  fi
}

assert_not_contains() {
  local name="$1" output="$2" needle="$3"
  if ! grep -qF -- "$needle" <<<"$output"; then
    echo "  PASS: $name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $name"
    echo "    expected NOT to contain: $needle"
    FAIL=$((FAIL + 1))
  fi
}

render() {
  helm template multica "$CHART" \
    --set image.registry=registry.test \
    --set image.pullSecret=test-secret \
    --set image.tag=test-tag \
    --set platform.postgres.user=u \
    --set platform.postgres.database=d \
    --set hostname=multica.test \
    "$@" 2>&1
}

test_migrate_hook_renders() {
  echo "test_migrate_hook_renders"
  local out
  out="$(render --show-only templates/platform/migrate-job.yaml)"
  assert_contains "kind: Job" "$out" "kind: Job"
  assert_contains "pre-upgrade hook" "$out" "helm.sh/hook: pre-install,pre-upgrade"
  assert_contains "hook-weight" "$out" "helm.sh/hook-weight"
  assert_contains "delete-policy" "$out" "before-hook-creation,hook-succeeded"
  assert_contains "uses backend image" "$out" "registry.test/multica-backend:test-tag"
  assert_contains "runs migrate command" "$out" "/app/migrate"
}

main() {
  test_migrate_hook_renders
  echo
  echo "Results: $PASS passed, $FAIL failed"
  [ "$FAIL" -eq 0 ]
}

main "$@"
