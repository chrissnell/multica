import SwiftUI

/// StatusPanel is the SwiftUI view embedded at the top of the menubar
/// dropdown via NSHostingView. Everything above the first separator in the
/// menu is this view; the action items (Sync now, Quit, etc.) stay as
/// native NSMenuItems so they get the platform's hover/keyboard-shortcut
/// treatment for free.
///
/// TimelineView drives the "8s ago" counter and the "in 7h 53m" columns
/// so they tick without waiting for a model update. Explicit
/// `@Observable` observation on `model` handles all the state-driven
/// re-renders — direction changes, expiry-time updates, etc.
struct StatusPanel: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        // TimelineView at 1 Hz keeps the relative-time strings live.
        // A menu that's only visible for a few seconds at a time doesn't
        // strictly need this, but it's cheap and eliminates the "stale
        // countdown" edge case where the user opens the menu twice in
        // quick succession and sees the same "3s ago" both times.
        TimelineView(.periodic(from: .now, by: 1.0)) { _ in
            content
        }
    }

    private var content: some View {
        VStack(alignment: .leading, spacing: 8) {
            header

            Divider()

            Grid(alignment: .leadingFirstTextBaseline,
                 horizontalSpacing: 14,
                 verticalSpacing: 5) {
                row("Last sync", value: lastSyncText, tint: lastSyncTint)
                row("Broker",    value: brokerText,   tint: nil)
                row("Keychain",  value: keychainText, tint: nil)
                row("Skew",      value: skewText,     tint: nil)

                if model.consecutiveFail > 0 {
                    row("Failures",
                        value: "\(model.consecutiveFail) in a row",
                        tint: .red)
                }

                if let err = model.setupError {
                    row("Setup",
                        value: err,
                        tint: .red)
                }
            }
        }
        // No top pad: the menu already contributes a few points of its
        // own top inset. The bottom keeps a small gap so the divider
        // below breathes.
        .padding(EdgeInsets(top: 0, leading: 14, bottom: 6, trailing: 14))
        // The frame width tames the menu — NSMenu will otherwise resize
        // to its widest child, and an unusually long error string would
        // stretch the whole dropdown. 300pt is comfortably wider than a
        // full "in 12h 34m" cell without dominating the display.
        .frame(width: 300, alignment: .leading)
    }

    // MARK: - Sub-views

    private var header: some View {
        HStack(spacing: 8) {
            Circle()
                .fill(model.health.tintColor)
                .frame(width: 9, height: 9)
            Text(model.health.label)
                .font(.system(size: 13, weight: .semibold))
            Spacer()
        }
    }

    /// row lays out one label/value pair. Labels are muted-secondary,
    /// values are monospaced so time columns line up column-wise even
    /// though the Grid handles horizontal alignment — the monospace font
    /// makes "8s ago" and "3m ago" occupy the same slot without visual
    /// jitter as the counter ticks.
    private func row(_ label: String, value: String, tint: Color?) -> some View {
        GridRow {
            Text(label)
                .font(.system(size: 12))
                .foregroundStyle(.secondary)
                .gridColumnAlignment(.leading)
            Text(value)
                .font(.system(size: 12, design: .monospaced))
                .foregroundStyle(tint ?? .primary)
                .lineLimit(1)
                .truncationMode(.tail)
                .gridColumnAlignment(.leading)
        }
    }

    // MARK: - Value strings

    private var lastSyncText: String {
        guard let latest = model.latest?.outcome else { return "never" }
        let ago = agoNow(latest.at)
        if latest.errorMessage != nil { return "\(ago) — failed" }
        let dir = latest.direction.rawValue.capitalized
        return "\(ago) · \(dir)\(latest.wrote ? " · wrote" : "")"
    }

    private var lastSyncTint: Color? {
        guard let latest = model.latest?.outcome else { return .secondary }
        return latest.errorMessage != nil ? .red : nil
    }

    private var brokerText: String {
        guard let d = model.lastBrokerExpiresAt else { return "unknown" }
        return "\(hhmmLocal(d))  \(untilNow(d))"
    }

    private var keychainText: String {
        guard let d = model.lastKeychainExpiresAt else { return "unknown" }
        return "\(hhmmLocal(d))  \(untilNow(d))"
    }

    private var skewText: String {
        guard let b = model.lastBrokerExpiresAt,
              let k = model.lastKeychainExpiresAt else { return "—" }
        let delta = k.timeIntervalSince(b)
        if abs(delta) < 60 { return "in sync" }
        if delta > 0 { return "keychain +\(shortDuration(delta))" }
        return "broker +\(shortDuration(-delta))"
    }
}
