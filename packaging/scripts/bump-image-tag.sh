#!/usr/bin/env bash
# packaging/scripts/bump-image-tag.sh
#
# Bump packaging/image-tag — the single source of truth for the Multica
# self-hosted image tag (Harbor: registry.chrissnell.com/multica/multica-*).
#
# Default: increment the trailing -mk<N> suffix (e.g. v0.4.0-mk5 → v0.4.0-mk6).
# Use --base vX.Y.Z to switch base versions; the suffix resets to -mk1.
#
# This script does NOT build, push, commit, or tag. It only edits the file.
# After bumping:
#   1. git add packaging/image-tag && git commit -m "chore(images): bump to <tag>"
#   2. ./packaging/scripts/build-images.sh         (reads the pin automatically)
#   3. Update ~/kube/apps/multica/values.yaml image.tag → <new>
#   4. helm upgrade --install multica ...
#   5. (optional) git tag <new> && git push origin <new>
#
# Usage:
#   ./packaging/scripts/bump-image-tag.sh                 # mk+1
#   ./packaging/scripts/bump-image-tag.sh --base v0.5.0   # new base, -mk1
#   ./packaging/scripts/bump-image-tag.sh --print         # show next tag, no write
#   ./packaging/scripts/bump-image-tag.sh --set v0.4.0-mk9  # set explicit value

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PIN="$ROOT/packaging/image-tag"

BASE=""
SET_EXPLICIT=""
PRINT_ONLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --base) BASE="$2"; shift 2 ;;
    --set)  SET_EXPLICIT="$2"; shift 2 ;;
    --print) PRINT_ONLY=1; shift ;;
    -h|--help)
      grep '^#' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

[ -f "$PIN" ] || { echo "missing pin file: packaging/image-tag" >&2; exit 1; }
current="$(tr -d '[:space:]' < "$PIN")"
[ -n "$current" ] || { echo "packaging/image-tag is empty" >&2; exit 1; }

if [[ -n "$SET_EXPLICIT" ]]; then
  if [[ ! "$SET_EXPLICIT" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-mk[0-9]+$ ]]; then
    echo "--set value must match vX.Y.Z-mkN, got: $SET_EXPLICIT" >&2
    exit 2
  fi
  next="$SET_EXPLICIT"
elif [[ -n "$BASE" ]]; then
  if [[ ! "$BASE" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "--base value must match vX.Y.Z, got: $BASE" >&2
    exit 2
  fi
  next="${BASE}-mk1"
else
  if [[ ! "$current" =~ ^(v[0-9]+\.[0-9]+\.[0-9]+)-mk([0-9]+)$ ]]; then
    echo "current pin does not match vX.Y.Z-mkN: $current" >&2
    echo "use --base vX.Y.Z to reset, or --set vX.Y.Z-mkN to force." >&2
    exit 2
  fi
  base="${BASH_REMATCH[1]}"
  n="${BASH_REMATCH[2]}"
  next="${base}-mk$((n + 1))"
fi

if [[ "$PRINT_ONLY" -eq 1 ]]; then
  echo "$next"
  exit 0
fi

printf '%s\n' "$next" > "$PIN"
echo "packaging/image-tag: $current → $next"
