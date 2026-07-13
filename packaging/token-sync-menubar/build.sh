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

# PkgInfo is legacy but a well-formed .app has one. Contents = APPL????.
printf 'APPL????' > "$APP/Contents/PkgInfo"

echo "==> ad-hoc codesign"
# Ad-hoc signing (identity "-") is enough for a locally-run app that talks
# to a private cluster over mTLS. If we ever ship this to other operators
# we'll swap in a Developer ID cert here.
codesign --force --deep --sign - --identifier "$BUNDLE_ID" "$APP"

echo ""
echo "Built: $APP"
echo ""
echo "Smoke test:"
echo "  open \"$APP\""
echo ""
echo "Install:"
echo "  ./install.sh"
