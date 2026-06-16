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

test_image_helper_tag_only() {
  echo "test_image_helper_tag_only"
  local out
  out="$(render --show-only templates/platform/backend-deployment.yaml)"
  assert_contains "tag-only ref" "$out" "image: registry.test/multica-backend:test-tag"
  assert_not_contains "no digest suffix" "$out" "@sha256"
}

test_image_helper_with_digest() {
  echo "test_image_helper_with_digest"
  local digest="sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc1"
  local out
  out="$(render --show-only templates/platform/backend-deployment.yaml \
                --set image.digests.backend="${digest}")"
  assert_contains "digest pinned ref" "$out" "image: registry.test/multica-backend:test-tag@${digest}"
}

test_controller_image_helper_with_digest() {
  echo "test_controller_image_helper_with_digest"
  local digest="sha256:1111111111111111111111111111111111111111111111111111111111111111"
  local out
  out="$(render --show-only templates/runtime/controller-deployment.yaml \
                --set image.digests.controller="${digest}" \
                --set runtime.enabled=true \
                --set runtime.mode=controller \
                --set runtime.workspaceId=ws-test)"
  assert_contains "controller digest pinned" "$out" "@${digest}"
}

test_claude_broker_image_helper_with_digest() {
  echo "test_claude_broker_image_helper_with_digest"
  local digest="sha256:2222222222222222222222222222222222222222222222222222222222222222"
  local out
  out="$(render --show-only templates/runtime/claude-broker-deployment.yaml \
                --set image.digests.claudeBroker="${digest}" \
                --set runtime.enabled=true \
                --set runtime.mode=controller \
                --set runtime.claudeBroker.enabled=true \
                --set runtime.workspaceId=ws-test)"
  assert_contains "claude-broker digest pinned" "$out" "@${digest}"
}

test_repocache_image_helper_with_digest() {
  echo "test_repocache_image_helper_with_digest"
  local digest="sha256:3333333333333333333333333333333333333333333333333333333333333333"
  local out
  out="$(render --show-only templates/runtime/repocache-deployment.yaml \
                --set image.digests.repocache="${digest}" \
                --set runtime.enabled=true \
                --set runtime.mode=controller \
                --set runtime.repocache.enabled=true \
                --set runtime.workspaceId=ws-test)"
  assert_contains "repocache digest pinned" "$out" "@${digest}"
}

test_migrate_hook_uses_digest() {
  echo "test_migrate_hook_uses_digest"
  local digest="sha256:4444444444444444444444444444444444444444444444444444444444444444"
  local out
  out="$(render --show-only templates/platform/migrate-job.yaml \
                --set image.digests.backend="${digest}")"
  assert_contains "migrate hook uses digest" "$out" "@${digest}"
}

main() {
  test_image_helper_tag_only
  test_image_helper_with_digest
  test_controller_image_helper_with_digest
  test_claude_broker_image_helper_with_digest
  test_repocache_image_helper_with_digest
  test_migrate_hook_renders
  test_migrate_hook_uses_digest
  echo
  echo "Results: $PASS passed, $FAIL failed"
  [ "$FAIL" -eq 0 ]
}

main "$@"
