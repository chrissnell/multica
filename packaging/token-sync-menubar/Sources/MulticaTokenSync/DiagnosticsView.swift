import SwiftUI

/// DiagnosticsView is the "why is it red" screen. Lists the recent ring
/// buffer with direction + duration + outcome, plus a copy-to-clipboard
/// button so a user pinging support can paste a self-contained summary.
struct DiagnosticsView: View {
    @Environment(AppModel.self) private var model

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            header
            Divider()
            recentRuns
            Spacer()
            HStack {
                Button("Copy diagnostics") { copyToClipboard() }
                Spacer()
                Text("Ring buffer holds \(AppModel.ringSize) most recent runs")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(20)
        .frame(minWidth: 640, minHeight: 480)
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 8) {
                Image(systemName: model.health.sfSymbol)
                    .foregroundStyle(model.health.tintColor)
                Text("Multica Token Sync — \(model.health.label)")
                    .font(.title2).bold()
            }
            if let err = model.setupError {
                Text("Setup error: \(err)")
                    .foregroundStyle(.red)
            }
            if let b = model.lastBrokerExpiresAt {
                Text("Broker expires: \(rfc3339(b))  •  \(until(b))")
            }
            if let k = model.lastKeychainExpiresAt {
                Text("Keychain expires: \(rfc3339(k))  •  \(until(k))")
            }
            Text("Namespace: \(model.config.namespace)  •  Secret: \(model.config.secretName)")
                .font(.footnote)
                .foregroundStyle(.secondary)
        }
    }

    private var recentRuns: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Recent syncs (newest first)")
                .font(.headline)
            if model.recent.isEmpty {
                Text("No syncs have run yet.").foregroundStyle(.secondary)
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 6) {
                        ForEach(model.recent.reversed()) { entry in
                            row(entry: entry)
                        }
                    }
                }
                .frame(maxHeight: .infinity)
            }
        }
    }

    /// row renders one ring-buffer entry with color-coded status. Errors are
    /// deliberately full-width so a long "OAuth access token has been
    /// revoked." doesn't get truncated — that string IS the diagnostic.
    private func row(entry: SyncEntry) -> some View {
        let o = entry.outcome
        return HStack(alignment: .top, spacing: 8) {
            Image(systemName: o.errorMessage == nil ? "checkmark.circle.fill" : "xmark.octagon.fill")
                .foregroundStyle(o.errorMessage == nil ? .green : .red)
            VStack(alignment: .leading, spacing: 2) {
                Text("\(rfc3339(o.at))  •  \(o.direction.rawValue)\(o.wrote ? "  •  wrote" : "")")
                    .font(.system(.body, design: .monospaced))
                if let msg = o.errorMessage {
                    Text(msg).font(.footnote).foregroundStyle(.red)
                }
            }
            Spacer()
        }
    }

    // MARK: - helpers

    /// localTimestamp renders a wall-clock date in the user's local zone
    /// with the zone abbreviation appended so a screenshot pasted into
    /// chat is still unambiguous. Named rfc3339 historically; kept for
    /// call-site brevity even though the format is no longer RFC 3339.
    private func rfc3339(_ d: Date) -> String {
        let f = DateFormatter()
        f.dateFormat = "yyyy-MM-dd HH:mm:ss zzz"
        f.timeZone = .current
        return f.string(from: d)
    }

    private func until(_ d: Date) -> String {
        let delta = d.timeIntervalSince(Date())
        if delta < 0 { return "expired" }
        let m = Int(delta / 60)
        if m < 60 { return "in \(m)m" }
        return "in \(m / 60)h \(m % 60)m"
    }

    /// copyToClipboard writes a self-contained plain-text summary of the
    /// current state to the pasteboard. The exact fields the user was
    /// asking about — expiries, skew, recent 20 syncs — are all there so
    /// they can paste into an issue or chat without a screenshot.
    private func copyToClipboard() {
        var lines: [String] = []
        lines.append("Multica Token Sync diagnostics — \(rfc3339(Date()))")
        lines.append("Health: \(model.health.label) (consecutiveFail=\(model.consecutiveFail))")
        if let b = model.lastBrokerExpiresAt { lines.append("Broker expires:   \(rfc3339(b))") }
        if let k = model.lastKeychainExpiresAt { lines.append("Keychain expires: \(rfc3339(k))") }
        lines.append("Namespace: \(model.config.namespace)  Secret: \(model.config.secretName)")
        lines.append("")
        lines.append("Recent syncs:")
        for entry in model.recent.reversed() {
            let o = entry.outcome
            let status = o.errorMessage == nil ? "ok" : "err"
            let extras = o.wrote ? " wrote" : ""
            lines.append("  \(rfc3339(o.at))  \(status)  \(o.direction.rawValue)\(extras)")
            if let m = o.errorMessage {
                lines.append("    error: \(m)")
            }
        }
        let text = lines.joined(separator: "\n")
        #if canImport(AppKit)
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
        #endif
    }
}

#if canImport(AppKit)
import AppKit
#endif
