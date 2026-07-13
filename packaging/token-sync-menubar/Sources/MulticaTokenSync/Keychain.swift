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

    /// Write the entry, creating it when absent, updating in place when
    /// present. SecItemUpdate is preferred over delete+add so the ACL of an
    /// existing entry survives — if we recreated the entry every sync, the
    /// keychain would prompt the user on every rotation.
    func write(service: String, account: String, data: Data) throws {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
        let update: [String: Any] = [
            kSecValueData as String: data,
        ]
        let status = SecItemUpdate(query as CFDictionary, update as CFDictionary)
        if status == errSecItemNotFound {
            var add = query
            add[kSecValueData as String] = data
            let addStatus = SecItemAdd(add as CFDictionary, nil)
            guard addStatus == errSecSuccess else {
                throw KeychainError.write(addStatus)
            }
            return
        }
        guard status == errSecSuccess else {
            throw KeychainError.write(status)
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
