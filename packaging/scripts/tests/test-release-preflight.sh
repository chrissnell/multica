#!/usr/bin/env bash
# Tests release-preflight.sh by stubbing git and gh. The script's PATH is
# rewritten so that the stubs are picked up instead of the real binaries.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SCRIPT="${REPO_ROOT}/packaging/scripts/release-preflight.sh"

PASS=0
FAIL=0

assert_eq() {
  local name="$1" actual="$2" expected="$3"
  if [ "$actual" = "$expected" ]; then
    echo "  PASS: $name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $name"
    echo "    expected: $expected"
    echo "    got:      $actual"
    FAIL=$((FAIL + 1))
  fi
}

assert_contains() {
  local name="$1" output="$2" needle="$3"
  if grep -qF -- "$needle" <<<"$output"; then
    echo "  PASS: $name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $name (expected to contain '$needle')"
    echo "    output: $output"
    FAIL=$((FAIL + 1))
  fi
}

setup_stubs() {
  local stubdir="$1"
  mkdir -p "$stubdir"
  # git stub: respond to specific argv patterns we care about.
  cat > "$stubdir/git" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "describe --tags --match v*-mk* --abbrev=0")
    echo "v0.4.0-mk10" ;;
  "log --merges --pretty=%s v0.4.0-mk10..HEAD")
    cat <<'LOG'
Merge pull request #42 from foo/bar
Merge pull request #43 from baz/qux
LOG
    ;;
  "log --oneline v0.4.0-mk10..HEAD")
    echo "abc123 commit a"
    echo "def456 commit b" ;;
  *) echo "unhandled git: $*" >&2; exit 99 ;;
esac
STUB
  chmod +x "$stubdir/git"
  # gh stub
  cat > "$stubdir/gh" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "pr view 42 --json title,body")
    cat <<'JSON'
{"title":"Add the widget","body":"## Summary\n- introduces widget X\n- bumps deps\n\n## Test plan\n- unit"}
JSON
    ;;
  "pr view 43 --json title,body")
    cat <<'JSON'
{"title":"Fix the bug","body":"## Summary\n- corrects regression in foo\n\n## Test plan\n- e2e"}
JSON
    ;;
  *) echo "unhandled gh: $*" >&2; exit 99 ;;
esac
STUB
  chmod +x "$stubdir/gh"
}

test_preflight_handles_escaped_quotes_in_pr_body() {
  echo "test_preflight_handles_escaped_quotes_in_pr_body"
  local stubs
  stubs=$(mktemp -d)
  mkdir -p "$stubs"
  cat > "$stubs/git" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "describe --tags --match v*-mk* --abbrev=0") echo "v0.4.0-mk10" ;;
  "log --merges --pretty=%s v0.4.0-mk10..HEAD") echo "Merge pull request #99 from x/y" ;;
  "log --oneline v0.4.0-mk10..HEAD") echo "abc commit a" ;;
  *) echo "unhandled git: $*" >&2; exit 99 ;;
esac
STUB
  chmod +x "$stubs/git"
  cat > "$stubs/gh" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  "pr view 99 --json title,body")
    # PR title and body both contain escaped quotes — old sed-based parser
    # truncated at the first \" and produced garbage.
    cat <<'JSON'
{"title":"fix: handle \"foo\" correctly","body":"## Summary\n- replaces sed with jq for the \"strict\" parser\n- adds an escape-quote regression test"}
JSON
    ;;
  *) echo "unhandled gh: $*" >&2; exit 99 ;;
esac
STUB
  chmod +x "$stubs/gh"
  local out
  out=$(PATH="$stubs:$PATH" "$SCRIPT" --no-confirm 2>&1)
  assert_contains "title with escaped quotes preserved" "$out" 'fix: handle "foo" correctly'
  assert_contains "body with escaped quotes preserved" "$out" 'replaces sed with jq for the "strict" parser'
  rm -rf "$stubs"
}

test_preflight_lists_prs() {
  echo "test_preflight_lists_prs"
  local stubs
  stubs=$(mktemp -d)
  setup_stubs "$stubs"
  local out
  out=$(PATH="$stubs:$PATH" "$SCRIPT" --no-confirm 2>&1)
  assert_contains "PR 42 title shown" "$out" "Add the widget"
  assert_contains "PR 43 title shown" "$out" "Fix the bug"
  assert_contains "PR 42 summary shown" "$out" "introduces widget X"
  assert_contains "PR 43 summary shown" "$out" "corrects regression in foo"
  assert_contains "count shown" "$out" "2 PRs"
  rm -rf "$stubs"
}

test_preflight_confirms_y() {
  echo "test_preflight_confirms_y"
  local stubs
  stubs=$(mktemp -d)
  setup_stubs "$stubs"
  echo "y" | PATH="$stubs:$PATH" "$SCRIPT" >/dev/null 2>&1
  local rc=$?
  rm -rf "$stubs"
  assert_eq "exit 0 on y" "$rc" "0"
}

test_preflight_aborts_on_n() {
  echo "test_preflight_aborts_on_n"
  local stubs
  stubs=$(mktemp -d)
  setup_stubs "$stubs"
  echo "n" | PATH="$stubs:$PATH" "$SCRIPT" >/dev/null 2>&1
  local rc=$?
  rm -rf "$stubs"
  assert_eq "exit non-zero on n" "$rc" "1"
}

test_preflight_aborts_on_blank() {
  echo "test_preflight_aborts_on_blank"
  local stubs
  stubs=$(mktemp -d)
  setup_stubs "$stubs"
  echo "" | PATH="$stubs:$PATH" "$SCRIPT" >/dev/null 2>&1
  local rc=$?
  rm -rf "$stubs"
  assert_eq "exit non-zero on empty input" "$rc" "1"
}

main() {
  test_preflight_handles_escaped_quotes_in_pr_body
  test_preflight_lists_prs
  test_preflight_confirms_y
  test_preflight_aborts_on_n
  test_preflight_aborts_on_blank
  echo
  echo "Results: $PASS passed, $FAIL failed"
  [ "$FAIL" -eq 0 ]
}

main "$@"
