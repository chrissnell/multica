#!/usr/bin/env bash
# packaging/token-sync-menubar/build.sh — build the Swift menubar app and
# assemble the .app bundle at ./build/Multica Token Sync.app.
#
# The Xcode-less approach: swift build produces a Mach-O executable in
# .build/<arch>/release/, which we drop into a hand-assembled bundle
# alongside Info.plist, then code-sign. Prefer a Developer ID identity so the
# app's designated requirement stays stable across rebuilds (keeps the
# keychain "Always Allow" grant from being invalidated); fall back to ad-hoc
# only when no such identity is available. Set CODESIGN_IDENTITY to override.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_NAME="Multica Token Sync"
APP="$HERE/build/$APP_NAME.app"
BUNDLE_ID="com.multica.token-sync"

cd "$HERE"

echo "==> swift build -c release"
swift build -c release

echo "==> resolving built binary"
BIN="$(swift build -c release --show-bin-path)/MulticaTokenSync"
if [[ ! -x "$BIN" ]]; then
  echo "error: built binary not found at $BIN" >&2
  exit 1
fi

echo "==> assembling $APP"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

cp "$BIN" "$APP/Contents/MacOS/MulticaTokenSync"
cp "$HERE/bundle/Info.plist" "$APP/Contents/Info.plist"

# PkgInfo is legacy but a well-formed .app has one. Contents = APPL????.
printf 'APPL????' > "$APP/Contents/PkgInfo"

echo "==> codesign"
# A STABLE signing identity is what keeps the "Always Allow" keychain grant
# from being invalidated on every rebuild. macOS pins the keychain ACL trust
# for the "Claude Code-credentials" item to the app's *designated
# requirement*. An ad-hoc signature's requirement is the raw cdhash, which
# changes on every build, so each reinstall drops the grant and the OS
# re-prompts for keychain access. A Developer ID identity yields a
# requirement keyed by bundle id + team, stable across rebuilds, so the grant
# sticks after a single "Always Allow" click (GRA-388).
#
# Identity resolution:
#   1. $CODESIGN_IDENTITY  (explicit override — name or SHA-1 hash)
#   2. first "Developer ID Application" identity in the login keychain
#   3. ad-hoc ("-") as a last resort, with a loud warning
IDENTITY="${CODESIGN_IDENTITY:-}"
if [[ -z "$IDENTITY" ]]; then
  IDENTITY="$(security find-identity -v -p codesigning 2>/dev/null \
    | awk -F'"' '/Developer ID Application/ {print $2; exit}')"
fi

if [[ -n "$IDENTITY" && "$IDENTITY" != "-" ]]; then
  echo "    using Developer ID identity: $IDENTITY"
  # --options runtime enables the hardened runtime (required only if this is
  # ever notarized; harmless otherwise). The keychain-ACL stability comes
  # from the identity itself, not from notarization.
  codesign --force --deep --options runtime \
    --sign "$IDENTITY" --identifier "$BUNDLE_ID" "$APP"
else
  echo "    WARNING: no Developer ID identity found; falling back to ad-hoc." >&2
  echo "    Ad-hoc builds re-trigger the 'Claude Code-credentials' keychain" >&2
  echo "    prompt after every rebuild. Set CODESIGN_IDENTITY or install a" >&2
  echo "    'Developer ID Application' certificate to make the grant persist." >&2
  codesign --force --deep --sign - --identifier "$BUNDLE_ID" "$APP"
fi

echo "==> designated requirement (stable => keychain grant persists):"
codesign -d -r- "$APP" 2>&1 | sed -n 's/^designated => /    /p'

echo ""
echo "Built: $APP"
echo ""
echo "Smoke test:"
echo "  open \"$APP\""
echo ""
echo "Install:"
echo "  ./install.sh"
