#!/usr/bin/env bash
# packaging/token-sync-menubar/build.sh — build the Swift menubar app and
# assemble the .app bundle at ./build/Multica Token Sync.app.
#
# The Xcode-less approach: swift build produces a Mach-O executable in
# .build/<arch>/release/, which we drop into a hand-assembled bundle
# alongside Info.plist. Ad-hoc code signing makes Gatekeeper let the app
# run locally without a paid developer certificate.

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
# AppIcon.icns is checked in — rebuild via bundle/build-icon.sh only
# when the source SVG changes. Copying the checked-in file avoids the
# ImageMagick+qlmanage dependency chain on a normal build.
if [[ -f "$HERE/bundle/AppIcon.icns" ]]; then
  cp "$HERE/bundle/AppIcon.icns" "$APP/Contents/Resources/AppIcon.icns"
fi

# PkgInfo is legacy but a well-formed .app has one. Contents = APPL????.
printf 'APPL????' > "$APP/Contents/PkgInfo"

echo "==> codesign"
# Prefer a Developer ID Application cert when one is installed on the
# build machine. Signing with a stable identity (as opposed to ad-hoc)
# gives the app a code identity that persists across rebuilds. That
# matters because macOS's per-item keychain ACL stores the trusted
# app's *identity*, not its bundle path — with ad-hoc signing, every
# rebuild gets a fresh CDHash, so every "Always Allow" the user
# clicked on the previous build is invalidated on the next build and
# they get re-prompted. Developer ID signing bakes in a certificate
# chain that stays constant across rebuilds, so ACL entries survive.
#
# You can pin a specific identity via CODESIGN_IDENTITY (SHA-1 or
# common-name substring); otherwise we auto-select the first
# "Developer ID Application" line from `security find-identity`. Fall
# back to ad-hoc if none is present so the build still works on a
# machine without an Apple developer account.
IDENTITY="${CODESIGN_IDENTITY:-}"
if [[ -z "$IDENTITY" ]]; then
  IDENTITY="$(security find-identity -v -p codesigning 2>/dev/null \
    | awk -F'"' '/Developer ID Application:/ {print $2; exit}')"
fi
if [[ -z "$IDENTITY" ]]; then
  echo "    no Developer ID Application cert found; falling back to ad-hoc"
  IDENTITY="-"
fi
echo "    identity: $IDENTITY"
codesign --force --deep --sign "$IDENTITY" --identifier "$BUNDLE_ID" "$APP"

echo ""
echo "Built: $APP"
echo ""
echo "Smoke test:"
echo "  open \"$APP\""
echo ""
echo "Install:"
echo "  ./install.sh"
