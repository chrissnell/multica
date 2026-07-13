import Foundation
import Security

/// Errors surfaced by the keychain store. `.notFound` is expected on first
/// run when the CLI hasn't ever authenticated on this Mac; callers should
/// treat it as "keychain empty, pull from broker" rather than as a fatal.
enum KeychainError: Error, LocalizedError {
    case notFound(OSStatus)
    case unexpectedFormat
    case read(OSStatus)
    case write(OSStatus)
    case securityCLI(status: Int32, message: String)

    var errorDescription: String? {
        switch self {
        case .notFound(let s):
            return "keychain entry not found (OSStatus \(s))"
        case .unexpectedFormat:
            return "keychain entry present but not readable as bytes"
        case .read(let s):
            return "keychain read failed (OSStatus \(s): \(secErrorMessage(s)))"
        case .write(let s):
            return "keychain write failed (OSStatus \(s): \(secErrorMessage(s)))"
        case .securityCLI(let status, let msg):
            return "/usr/bin/security add-generic-password exited \(status): \(msg)"
        }
    }
}

/// KeychainStore reads and writes a single generic-password entry keyed by
/// (service, account). Deliberately mirrors the `/usr/bin/security` layout
/// the Go binary used so the existing entry — which was originally written
/// by that binary — is readable here without re-adding.
///
/// First-read UX: macOS' keychain ACL on entries written by
/// `security add-generic-password` is permissive to the writing user's
/// processes by default, so the Swift app should be able to read/update the
/// entry without prompting. If a prompt does appear on first run, choosing
/// "Always Allow" persists the ACL for future syncs.
struct KeychainStore {
    /// Read the generic-password entry as raw bytes. Throws `.notFound` if
    /// absent; other OSStatuses become `.read` so callers can distinguish
    /// "empty" from "broken".
    func read(service: String, account: String) throws -> Data {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecItemNotFound {
            throw KeychainError.notFound(status)
        }
        guard status == errSecSuccess else {
            throw KeychainError.read(status)
        }
        guard let data = item as? Data else {
            throw KeychainError.unexpectedFormat
        }
        return data
    }

    /// Write the entry via /usr/bin/security. Deliberately NOT
    /// SecItemUpdate: on macOS, SecItemUpdate replaces the item's ACL
    /// with just the caller (this app), which then makes every other
    /// process reading the entry — most notably /usr/bin/security when
    /// the multica daemon spawns a claude subprocess — trip a keychain
    /// prompt on every sync. `security add-generic-password -U`
    /// preserves the existing ACL that the Claude Code CLI's own login
    /// installed, so subsequent reads by other processes stay silent.
    /// This is the exact call the Go binary made before the menubar
    /// rewrite; matching it bit-for-bit avoids reintroducing the exact
    /// class of user-visible regression that motivated moving off
    /// SecItemUpdate in the first place.
    ///
    /// Password bytes travel on argv, not stdin, so we hit the code path
    /// in `security` that handles blobs larger than PASS_MAX (Claude's
    /// credentials blob comfortably exceeds it, and the stdin path
    /// silently truncates). macOS restricts argv visibility to the
    /// process owner by default, so this leaks nothing to any other
    /// user; the value is already in the user's keychain, which the
    /// same owner can read directly.
    func write(service: String, account: String, data: Data) throws {
        guard let value = String(data: data, encoding: .utf8) else {
            throw KeychainError.unexpectedFormat
        }
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/security")
        // -A marks the item accessible from any app without warning.
        // Without it, the ACL is scoped to just the writing process
        // (/usr/bin/security), and every OTHER process that later reads
        // via `security find-generic-password` — Claude Code CLI at
        // startup, multica-daemon-spawned agents, etc. — trips a
        // keychain prompt on every rotation. The single-user Mac case
        // treats "any app the current user runs" as trusted anyway
        // (they can read the login keychain directly), so -A doesn't
        // widen the effective threat model; it just stops the loop of
        // Always-Allow clicks that never stick.
        process.arguments = [
            "add-generic-password",
            "-s", service,
            "-a", account,
            "-U",
            "-A",
            "-w", value,
        ]
        let errPipe = Pipe()
        process.standardError = errPipe
        process.standardOutput = Pipe()
        do {
            try process.run()
        } catch {
            throw KeychainError.write(errSecInternalError)
        }
        process.waitUntilExit()
        if process.terminationStatus != 0 {
            let errData = errPipe.fileHandleForReading.readDataToEndOfFile()
            let msg = String(data: errData, encoding: .utf8) ?? "unknown"
            throw KeychainError.securityCLI(status: process.terminationStatus, message: msg.trimmingCharacters(in: .whitespacesAndNewlines))
        }
    }
}

/// secErrorMessage maps common OSStatus codes to human-readable strings so
/// diagnostics show something more useful than "OSStatus -25291". The list
/// is intentionally short — only codes we've seen in practice — because
/// SecCopyErrorMessageString is available for the rest.
private func secErrorMessage(_ status: OSStatus) -> String {
    if let cf = SecCopyErrorMessageString(status, nil) {
        return cf as String
    }
    switch status {
    case errSecItemNotFound: return "item not found"
    case errSecAuthFailed:   return "authentication failed"
    case errSecUserCanceled: return "user canceled"
    default: return "unknown"
    }
}
