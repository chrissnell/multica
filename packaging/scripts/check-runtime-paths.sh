#!/usr/bin/env bash
# Asserts that every path the release workflow watches for "runtime needs
# rebuild?" still exists in the repo. CI runs this; failure means someone
# moved or renamed a watched file without updating the list below (or this
# script).
#
# If you intentionally remove a watched path, remove it from the list here in
# the same change. If you add a new toolchain input (a new pinned-version
# file, a new packaging/docker/runtime/* file), add it to the list.

set -euo pipefail

WATCHED=(
  "packaging/rust-version"
  "packaging/kotlin-version"
  "packaging/claude-code-version"
  "packaging/gh-version"
  "packaging/kubectl-version"
  "packaging/helm-version"
  "packaging/pnpm-version"
  "packaging/ktlint-version"
  "packaging/golangci-lint-version"
  "packaging/rclone-version"
  "packaging/wrangler-version"
  "packaging/protoc-version"
  "packaging/docker/runtime"
  "packaging/scripts/build-images.sh"
)

missing=()
for p in "${WATCHED[@]}"; do
  if [ ! -e "$p" ]; then
    missing+=("$p")
  fi
done

if [ ${#missing[@]} -ne 0 ]; then
  echo "ERROR: watched runtime paths are missing from the repo:" >&2
  for p in "${missing[@]}"; do
    echo "  - $p" >&2
  done
  echo >&2
  echo "Either the path was renamed/moved (update WATCHED in $0 to match)," >&2
  echo "or it was deleted (remove it from WATCHED)." >&2
  exit 1
fi

# Echo for use in release.yml's diff step
printf '%s\n' "${WATCHED[@]}"
