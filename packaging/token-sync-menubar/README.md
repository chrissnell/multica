# multica-token-sync (menubar)

Native macOS menubar app that reconciles the Claude OAuth broker Secret
in the Multica cluster with the local Claude Code Keychain entry. Replaces
the launchd-driven `multica-token-sync --once` polling unit with a
long-running app that surfaces sync state and diagnostics at a glance.

## What it does

Same bidirectional reconciler shipped in
`server/cmd/multica-token-sync` (Go), ported to Swift and driven from a
SwiftUI menubar app:

- Reads the broker's state `Secret` (`multica-claude-oauth-broker`) via
  the Kubernetes REST API, using the client-cert mTLS credentials from
  `~/.kube/config`.
- Reads / writes the local Keychain entry
  (`Claude Code-credentials` / `$USER`) via `Security.framework`.
- On each 5-min tick, decides:
  - **pull** — broker is ahead of keychain → overwrite keychain
  - **push** — keychain is ahead of broker → patch the Secret so the
    broker's next tick reseeds and exchanges the fresh refresh_token
  - **noop** — fingerprints match, no write needed

Menubar dot is green / yellow / red for healthy / warning / failing.
Native macOS notification fires on healthy → failing and failing →
healthy transitions.

## Requirements

- macOS 14 (Sonoma) or newer
- Swift 5.9 toolchain (Xcode 15+ or standalone Swift)
- `~/.kube/config` with a client-cert user for the target cluster

## Build

```bash
./build.sh
```

Runs `swift build -c release`, assembles the `.app` bundle at
`build/Multica Token Sync.app`, and code-signs it.

Smoke test without installing:

```bash
open "build/Multica Token Sync.app"
```

### Code signing and the recurring keychain prompt

The app reads the `Claude Code-credentials` login-keychain item. macOS ties
the item's "Always Allow" grant to the accessing app's *designated
requirement*. An **ad-hoc** signature's requirement is the raw code hash,
which changes on every rebuild — so each reinstall silently invalidates the
grant and macOS re-prompts:

> *Multica Token Sync wants to access key "Claude Code-credentials"…*

Signing with a stable **Developer ID Application** identity fixes this: its
requirement is keyed by bundle id + team, so it survives rebuilds and updates.
Click **Always Allow** once after the first Developer-ID-signed install and
the prompt stops for good.

`build.sh` auto-selects the first `Developer ID Application` identity in your
login keychain. Override or pin it explicitly:

```bash
CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)" ./build.sh
# or by cert SHA-1:
CODESIGN_IDENTITY=ABCDEF0123... ./build.sh
```

If no Developer ID identity is found the build falls back to ad-hoc signing
and prints a warning; the app still runs but the keychain prompt will keep
returning after each rebuild.

> Notarization is **not** required — the keychain grant only depends on a
> stable signing identity, not on notarization or Gatekeeper approval.

## Install

```bash
./install.sh          # copy to /Applications, load LaunchAgent
./install.sh status   # launchctl print
./install.sh uninstall
```

`install` unloads the legacy `com.multica.token-sync` LaunchAgent (the Go
`--once` polling unit) but leaves its `.plist` on disk so you can roll
back. To also delete that:

```bash
./install.sh uninstall-legacy
```

## Layout

```
Package.swift                  # SwiftPM manifest
Sources/MulticaTokenSync/
  MulticaTokenSyncApp.swift    # @main + sync loop runner
  AppDelegate.swift            # NSStatusItem + NSMenu + diagnostics window
  StatusPanel.swift            # SwiftUI status rows embedded in the menu
  DiagnosticsView.swift        # SwiftUI diagnostics window
  AppModel.swift               # @Observable state (ring buffer, health)
  SyncEngine.swift             # bidirectional reconciler (Swift port)
  KubeClient.swift             # k8s API over mTLS via AsyncHTTPClient+NIOSSL
  KubeConfig.swift             # kubeconfig YAML parser (Yams)
  Keychain.swift               # Security.framework generic-password wrapper
  Notifier.swift               # UserNotifications wrapper
  OAuthTypes.swift             # BrokerState / KeychainPayload / SyncOutcome
  Formatters.swift             # shared date/duration formatting
bundle/Info.plist              # .app bundle Info.plist (LSUIElement)
launchd/…                      # LaunchAgent for autostart
build.sh                        # build + assemble .app
install.sh                      # install / uninstall / status
```

## Why AppKit and not `MenuBarExtra`

`MenuBarExtra` force-templates its label image, which strips color from
SF Symbols and even from colored Text. Getting a real green dot in the
menubar requires an `NSStatusItem` with `NSImage.isTemplate = false`, so
the app drops out to `AppKit` for the status item and its menu. The
diagnostics window is still SwiftUI (via `NSHostingController`).
