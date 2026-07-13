import AppKit
import SwiftUI

/// AppDelegate owns the NSStatusItem, its NSMenu, the diagnostics NSWindow,
/// and the sync-loop runner. We dropped SwiftUI's MenuBarExtra abstraction
/// because it force-templates label content (SF Symbols and Text alike),
/// which drops color and produces a black dot no matter what tint we set.
/// AppKit's NSStatusItem lets us hand it an NSImage with isTemplate=false
/// and draw whatever color we want.
///
/// The menu is native NSMenu — small enough that reimplementing it in
/// AppKit is cheaper than trying to embed SwiftUI inside NSMenu (which
/// SwiftUI doesn't support in any first-class way). The diagnostics
/// window, on the other hand, IS SwiftUI: it's rich content and benefits
/// from Observable-driven updates.
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let model = AppModel()
    private let runner = SyncLoopRunner()

    private var statusItem: NSStatusItem!
    private var refreshTimer: Timer?
    private var diagnosticsWindow: NSWindow?

    // The status rows are a single NSHostingView-backed NSMenuItem; the
    // SwiftUI view inside handles its own re-renders via @Observable +
    // TimelineView, so we don't touch it from applyState().
    private var syncNowItem: NSMenuItem!

    func applicationDidFinishLaunching(_ notification: Notification) {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        buildMenu()
        applyState()

        // Poll state every 2s. @Observable would let us use
        // withObservationTracking here, but the native menu doesn't need
        // reactive precision — a 2s tick is well under human perception
        // and less code than plumbing observation callbacks through.
        refreshTimer = Timer.scheduledTimer(withTimeInterval: 2.0, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.applyState() }
        }

        Task { [runner, model] in
            await runner.startIfNeeded(model: model)
        }
    }

    func applicationWillTerminate(_ notification: Notification) {
        refreshTimer?.invalidate()
    }

    // MARK: - Menu construction

    private func buildMenu() {
        let menu = NSMenu()
        menu.autoenablesItems = false

        // Status panel: one NSMenuItem whose custom view is the SwiftUI
        // StatusPanel. NSHostingView reports an intrinsic size derived
        // from StatusPanel's .frame modifier; NSMenu then sizes the row
        // to match. The panel handles its own reactive updates via
        // @Observable, so applyState() has nothing to do here.
        let panel = NSHostingView(rootView: StatusPanel().environment(model))
        panel.frame = NSRect(x: 0, y: 0, width: 300, height: 142)
        let panelItem = NSMenuItem()
        panelItem.view = panel
        menu.addItem(panelItem)

        menu.addItem(.separator())

        syncNowItem = NSMenuItem(title: "Sync now", action: #selector(syncNowClicked), keyEquivalent: "r")
        syncNowItem.target = self
        menu.addItem(syncNowItem)

        let diag = NSMenuItem(title: "Show diagnostics…", action: #selector(showDiagnosticsClicked), keyEquivalent: "d")
        diag.target = self
        menu.addItem(diag)

        let openLog = NSMenuItem(title: "Open log", action: #selector(openLogClicked), keyEquivalent: "")
        openLog.target = self
        menu.addItem(openLog)

        menu.addItem(.separator())

        let quit = NSMenuItem(title: "Quit", action: #selector(quitClicked), keyEquivalent: "q")
        quit.target = self
        menu.addItem(quit)

        statusItem.menu = menu
    }

    // MARK: - State application

    /// applyState pushes model → UI. Called on the timer and after each
    /// sync. Only the icon and the "Sync now" enabled state live here —
    /// the SwiftUI status panel handles the rest of the rendering itself.
    private func applyState() {
        // Icon: flat filled circle in the health color. isTemplate=false
        // is the whole reason we're not using MenuBarExtra — this is the
        // hook that keeps the color from being stripped.
        let icon = coloredCircle(color: model.health.nsColor, size: NSSize(width: 12, height: 12))
        icon.isTemplate = false
        statusItem.button?.image = icon
        statusItem.button?.imagePosition = .imageOnly

        syncNowItem.isEnabled = !model.running
    }

    // MARK: - Menu actions

    @objc private func syncNowClicked() {
        Task { [runner] in await runner.triggerNow() }
    }

    /// showDiagnosticsClicked creates a plain NSWindow hosting the SwiftUI
    /// DiagnosticsView. Cheap to keep around — recreating on every open
    /// resets the scroll position and any transient state, which is worse
    /// UX than the ~few KB the window retains.
    @objc private func showDiagnosticsClicked() {
        if diagnosticsWindow == nil {
            let host = NSHostingController(rootView: DiagnosticsView().environment(model))
            let window = NSWindow(contentViewController: host)
            window.title = "Multica Token Sync — Diagnostics"
            window.setContentSize(NSSize(width: 700, height: 500))
            window.styleMask = [.titled, .closable, .miniaturizable, .resizable]
            window.isReleasedWhenClosed = false
            window.center()
            diagnosticsWindow = window
        }
        NSApp.activate(ignoringOtherApps: true)
        diagnosticsWindow?.makeKeyAndOrderFront(nil)
    }

    @objc private func openLogClicked() {
        NSWorkspace.shared.open(logFileURL())
    }

    @objc private func quitClicked() {
        NSApp.terminate(nil)
    }
}

// MARK: - Icon drawing

/// coloredCircle draws a filled circle in the given color into an NSImage.
/// Inset by 1pt on each side so the shape doesn't visually touch the
/// menubar edge; the resulting image reads as a clean disc at menubar
/// text height.
func coloredCircle(color: NSColor, size: NSSize) -> NSImage {
    let image = NSImage(size: size)
    image.lockFocus()
    defer { image.unlockFocus() }
    let rect = NSRect(origin: .zero, size: size).insetBy(dx: 1, dy: 1)
    color.setFill()
    NSBezierPath(ovalIn: rect).fill()
    return image
}

extension HealthState {
    /// nsColor is the AppKit-side twin of tintColor (SwiftUI). Slightly
    /// desaturated greens/yellows/reds so the dot doesn't fight the
    /// menubar accent color — pure NSColor.systemGreen reads as a
    /// notification badge, which is louder than we want.
    var nsColor: NSColor {
        switch self {
        case .healthy: return NSColor(calibratedRed: 0.20, green: 0.72, blue: 0.32, alpha: 1.0)
        case .warning: return NSColor(calibratedRed: 0.95, green: 0.72, blue: 0.15, alpha: 1.0)
        case .failing: return NSColor(calibratedRed: 0.85, green: 0.22, blue: 0.20, alpha: 1.0)
        }
    }
}
