#!/usr/bin/env bash
# Tests for check-runtime-paths.sh: must succeed when every listed path
# exists in the repo, and must fail when any listed path is missing.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SCRIPT="${REPO_ROOT}/packaging/scripts/check-runtime-paths.sh"

PASS=0
FAIL=0

assert_eq() {
  local name="$1" actual="$2" expected="$3"
  if [ "$actual" = "$expected" ]; then
    echo "  PASS: $name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $name (expected '$expected', got '$actual')"
    FAIL=$((FAIL + 1))
  fi
}

test_passes_on_real_repo() {
  echo "test_passes_on_real_repo"
  (cd "$REPO_ROOT" && "$SCRIPT" >/dev/null 2>&1)
  assert_eq "exit code" "$?" "0"
}

test_fails_when_a_watched_path_is_missing() {
  echo "test_fails_when_a_watched_path_is_missing"
  local tmp
  tmp=$(mktemp -d)
  # Copy the script and only one of its watched paths into a fake repo
  mkdir -p "$tmp/packaging"
  cp "$SCRIPT" "$tmp/packaging/check-runtime-paths.sh"
  : > "$tmp/packaging/rust-version"   # only this one path exists
  (cd "$tmp" && ./packaging/check-runtime-paths.sh >/dev/null 2>&1)
  local rc=$?
  rm -rf "$tmp"
  assert_eq "exit code" "$rc" "1"
}

main() {
  test_passes_on_real_repo
  test_fails_when_a_watched_path_is_missing
  echo
  echo "Results: $PASS passed, $FAIL failed"
  [ "$FAIL" -eq 0 ]
}

main "$@"
