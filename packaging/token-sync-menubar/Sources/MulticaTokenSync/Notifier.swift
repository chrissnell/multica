import Foundation
import UserNotifications

/// Notifier wraps UserNotifications for the single case the app needs:
/// alerting on a healthy→failing transition. Deliberately not a general
/// notification API — every extra path is a new place to blast the user.
///
/// First run requests authorization (banner+sound only, no badge). If the
/// user denies, subsequent notifications are silently no-ops; the menubar
/// icon still reflects the state.
enum Notifier {
    /// Ask for notification permission early so the first health transition
    /// isn't the moment we surprise the user with a permission prompt.
    static func requestAuthorization() async {
        do {
            _ = try await UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound])
        } catch {
            // Silent — the health state is still visible in the menubar
            // icon, and there's no useful recovery for a denied request.
        }
    }

    /// notifyFailing fires when the state machine crosses into the failing
    /// zone (3+ consecutive failures). Cause is the last error message so
    /// the user can decide whether to open diagnostics or wait it out.
    static func notifyFailing(cause: String?) {
        let content = UNMutableNotificationContent()
        content.title = "Multica Token Sync is failing"
        content.body = cause ?? "Recent sync attempts have failed. Open the menubar for diagnostics."
        content.sound = .default

        let request = UNNotificationRequest(
            identifier: "multica.token-sync.failing.\(Date().timeIntervalSince1970)",
            content: content,
            trigger: nil // fire immediately
        )
        UNUserNotificationCenter.current().add(request) { _ in }
    }

    /// notifyRecovered fires when the state machine crosses back into
    /// healthy after having been failing. Deliberate: without a recovery
    /// notification, the user only ever hears bad news and can't tell
    /// whether the incident is over without opening the menubar.
    static func notifyRecovered() {
        let content = UNMutableNotificationContent()
        content.title = "Multica Token Sync recovered"
        content.body = "Sync is healthy again."
        content.sound = nil // less noisy than the failure notification

        let request = UNNotificationRequest(
            identifier: "multica.token-sync.recovered.\(Date().timeIntervalSince1970)",
            content: content,
            trigger: nil
        )
        UNUserNotificationCenter.current().add(request) { _ in }
    }
}
