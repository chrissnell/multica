import Foundation
import Observation
import SwiftUI

/// HealthState is the traffic-light shown as the menubar icon color. Derived
/// from the consecutive-failure count; the thresholds match the Go
/// menubar's original design (1-2 = warning, 3+ = failing) so a green→red
/// transition means "you should look at this".
enum HealthState {
    case healthy, warning, failing

    var label: String {
        switch self {
        case .healthy: return "Healthy"
        case .warning: return "Warning"
        case .failing: return "Failing"
        }
    }

    var sfSymbol: String {
        switch self {
        case .healthy: return "circle.fill"
        case .warning: return "exclamationmark.triangle.fill"
        case .failing: return "xmark.octagon.fill"
        }
    }

    var tintColor: Color {
        switch self {
        case .healthy: return .green
        case .warning: return .yellow
        case .failing: return .red
        }
    }

    /// menubarGlyph is a plain Unicode filled-circle character (U+25CF) —
    /// intentionally not the colored emoji "🟢🟡🔴" which have Apple's
    /// gradient/shading texture. A monochrome glyph paired with
    /// `.foregroundStyle(tintColor)` renders as a flat solid dot in the
    /// menubar (Text respects foreground color even though Image is
    /// force-templated to black).
    var menubarGlyph: String { "●" }
}

/// SyncEntry is the ring-buffer element. Explicit `id` so SwiftUI diffing
/// doesn't confuse two syncs that happened at the same second.
struct SyncEntry: Identifiable, Equatable {
    let id = UUID()
    let outcome: SyncOutcome
}

/// AppModel is the single source of truth for the UI. All mutation happens
/// on the MainActor; the sync loop hands outcomes off via
/// `MainActor.run { model.record(...) }`.
///
/// The ring buffer is capped at 20 — enough for a diagnostics glance but
/// bounded so a long-lived menubar doesn't accumulate memory. Ordering is
/// oldest-first; `latest` reads the tail.
@MainActor
@Observable
final class AppModel {
    static let ringSize = 20
    static let failingThreshold = 3

    var recent: [SyncEntry] = []
    var consecutiveFail: Int = 0
    var lastBrokerExpiresAt: Date?
    var lastKeychainExpiresAt: Date?
    var lastSyncAt: Date?
    var running: Bool = false // "Sync now" grays out while true
    var setupError: String?   // fatal init error (missing kubeconfig, etc.)
    var config: SyncConfig = SyncConfig()

    var health: HealthState {
        if consecutiveFail >= Self.failingThreshold { return .failing }
        if consecutiveFail > 0 { return .warning }
        return .healthy
    }

    var latest: SyncEntry? { recent.last }

    /// record consumes a sync outcome, updates derived state, and returns
    /// the previous → new health transition so the caller can decide
    /// whether to fire a notification.
    func record(_ outcome: SyncOutcome) -> (from: HealthState, to: HealthState) {
        let prev = health
        if outcome.errorMessage != nil {
            consecutiveFail += 1
        } else {
            consecutiveFail = 0
            if let b = outcome.brokerExpiresAt { lastBrokerExpiresAt = b }
            if let k = outcome.keychainExpiresAt { lastKeychainExpiresAt = k }
        }
        lastSyncAt = outcome.at
        recent.append(SyncEntry(outcome: outcome))
        if recent.count > Self.ringSize {
            recent.removeFirst(recent.count - Self.ringSize)
        }
        return (prev, health)
    }
}
