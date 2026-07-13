#!/usr/bin/env bash
# packaging/token-sync-menubar/install.sh — install/uninstall the menubar
# app and its LaunchAgent. Run as the operator (no sudo).
#
# On install:
#  1. Copies the built .app into /Applications.
#  2. Unloads the legacy launchctl unit (com.multica.token-sync) if present.
#  3. Loads the new LaunchAgent that opens the .app on login.

set -euo pipefail

CMD="${1:-install}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_NAME="Multica Token Sync"
APP_SRC="$HERE/build/$APP_NAME.app"
APP_DST="/Applications/$APP_NAME.app"
NEW_LABEL="com.multica.token-sync.app"
OLD_LABEL="com.multica.token-sync"
PLIST_SRC="$HERE/launchd/$NEW_LABEL.plist"
PLIST_DST="$HOME/Library/LaunchAgents/$NEW_LABEL.plist"
OLD_PLIST_DST="$HOME/Library/LaunchAgents/$OLD_LABEL.plist"

case "$CMD" in
  install)
    if [[ ! -d "$APP_SRC" ]]; then
      echo "error: $APP_SRC not found. Run ./build.sh first." >&2
      exit 1
    fi
    echo "==> stopping legacy launchd unit ($OLD_LABEL) if present"
    launchctl bootout "gui/$(id -u)/$OLD_LABEL" 2>/dev/null || true
    # Do not delete the legacy plist automatically — user may want to
    # keep it around for rollback. `install.sh uninstall-legacy` clears it.

    echo "==> copying app to $APP_DST"
    # rsync preserves timestamps/perms and doesn't leave stale files.
    rsync -a --delete "$APP_SRC/" "$APP_DST/"

    echo "==> installing new LaunchAgent to $PLIST_DST"
    mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
    sed "s|__USER_HOME__|$HOME|g" "$PLIST_SRC" > "$PLIST_DST"
    launchctl bootout "gui/$(id -u)/$NEW_LABEL" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$PLIST_DST"

    echo ""
    echo "Installed. The app should be running in your menubar."
    echo "Logs:            $HOME/Library/Logs/multica-token-sync.log"
    echo "launchd stdout:  $HOME/Library/Logs/multica-token-sync-launchd.log"
    echo "Status:          $0 status"
    ;;
  uninstall)
    launchctl bootout "gui/$(id -u)/$NEW_LABEL" 2>/dev/null || true
    rm -f "$PLIST_DST"
    rm -rf "$APP_DST"
    echo "Uninstalled ${APP_NAME} and $NEW_LABEL."
    ;;
  uninstall-legacy)
    launchctl bootout "gui/$(id -u)/$OLD_LABEL" 2>/dev/null || true
    rm -f "$OLD_PLIST_DST"
    echo "Removed legacy $OLD_LABEL LaunchAgent."
    ;;
  status)
    launchctl print "gui/$(id -u)/$NEW_LABEL" 2>&1 | head -30 || true
    ;;
  *)
    echo "usage: $0 [install|uninstall|uninstall-legacy|status]" >&2
    exit 2
    ;;
esac
