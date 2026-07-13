#!/usr/bin/env bash
# packaging/token-sync-menubar/bundle/build-icon.sh — build AppIcon.icns
# from the Multica SVG logo. Run manually when the source SVG changes.
# The output icns is checked in so build.sh doesn't require ImageMagick.
#
# Toolchain:
#   qlmanage — macOS built-in QuickLook, rasterizes SVG → PNG
#   magick   — ImageMagick, composes rounded-corner mask + inset
#   sips     — Apple's built-in, downsamples to icon sizes
#   iconutil — Apple's built-in, packs an .iconset directory into .icns
#
# Why qlmanage instead of ImageMagick for SVG: ImageMagick's SVG
# delegate is rsvg-convert, which isn't a first-class macOS dependency.
# qlmanage ships with the OS and renders SVGs faithfully at whatever
# size we ask for.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="$HERE/icon-src/logo.svg"
OUT="$HERE/AppIcon.icns"
STAGE="$HERE/icon-src"
ICONSET="$STAGE/AppIcon.iconset"

if [[ ! -f "$SRC" ]]; then
  echo "error: $SRC not found. Fetch with:" >&2
  echo "  curl -sSL https://raw.githubusercontent.com/multica-ai/multica/main/docs/assets/logo-light.svg -o $SRC" >&2
  exit 1
fi

# Step 1 — rasterize SVG to 1024×1024 PNG via qlmanage. qlmanage always
# writes to <name>.png in the -o directory, so we rename after.
rm -f "$STAGE/logo.svg.png" "$STAGE/logo-1024.png"
qlmanage -t -s 1024 "$SRC" -o "$STAGE" >/dev/null
mv "$STAGE/logo.svg.png" "$STAGE/logo-1024.png"

# Step 2 — inset the logo so it doesn't touch the icon edge, then
# apply a squircle mask (radius = 22.37% of side, per Apple HIG).
# The `-shave` step trims 12% off each side so the burst has room to
# breathe inside the rounded frame; the `-mattecolor white -bordercolor
# white -border` step reintroduces the white surround at full 1024.
INSET="$STAGE/logo-inset.png"
magick "$STAGE/logo-1024.png" \
  -resize 800x800 \
  -background white -gravity center -extent 1024x1024 \
  "$INSET"

# Step 3 — build the rounded-corner mask and apply. The mask is a
# 1024×1024 solid rounded rectangle rendered by magick's `roundrectangle`
# draw op, then composed as the alpha channel of the inset PNG.
MASK="$STAGE/mask.png"
magick -size 1024x1024 xc:black \
  -fill white \
  -draw "roundrectangle 0,0 1023,1023 224,224" \
  "$MASK"

FRAMED="$STAGE/framed-1024.png"
magick "$INSET" "$MASK" \
  -compose CopyOpacity -composite \
  "$FRAMED"

# Step 4 — .iconset directory. Apple wants specific filenames; the
# retina @2x variants share pixel data with the next-larger base size.
rm -rf "$ICONSET"
mkdir -p "$ICONSET"
for s in 16 32 128 256 512; do
  sips -Z "$s" "$FRAMED" --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
  # @2x variant is the 2×-size PNG (e.g. icon_16x16@2x.png is 32px).
  s2=$((s * 2))
  sips -Z "$s2" "$FRAMED" --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
done
# 1024×1024 is delivered as the "512x512@2x" per icns convention.
# (There is no icon_1024x1024.png entry.)

iconutil -c icns "$ICONSET" -o "$OUT"
echo "Built $OUT ($(stat -f %z "$OUT") bytes)"
