import Foundation
import Security

/// Errors surfaced by the keychain store. `.notFound` is expected on first
/// run when the CLI hasn't ever authenticated on this Mac; callers should
/// treat it as "keychain empty, pull from broker" rather than as a fatal.
/// `.interactionRequired` is our silent-deny: the item exists but the
/// current ACL doesn't trust this process, and we suppressed the password
/// prompt via kSecUseAuthenticationUIFail. Callers should treat it as
/// "must reset ACL by rewriting the item" — the write path's delete+add
/// re-establishes a permissive ACL.
enum KeychainError: Error, LocalizedError {
    case notFound(OSStatus)
    case interactionRequired(OSStatus)
    case unexpectedFormat
    case read(OSStatus)
    case write(OSStatus)
    case securityCLI(status: Int32, message: String)

    var errorDescription: String? {
        switch self {
        case .notFound(let s):
            return "keychain entry not found (OSStatus \(s))"
        case .interactionRequired(let s):
            return "keychain ACL denied read without prompt (OSStatus \(s)); next write will reset ACL"
        case .unexpectedFormat:
            return "keychain entry present but not readable as bytes"
        case .read(let s):
            return "keychain read failed (OSStatus \(s): \(secErrorMessage(s)))"
        case .write(let s):
            return "keychain write failed (OSStatus \(s): \(secErrorMessage(s)))"
        case .securityCLI(let status, let msg):
            return "/usr/bin/security exited \(status): \(msg)"
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
    /// Read the generic-password entry as raw bytes. Throws:
    ///  - `.notFound` if the item is absent (first run, or CLI never logged in).
    ///  - `.interactionRequired` if the ACL denies this process; the
    ///    `kSecUseAuthenticationUI: kSecUseAuthenticationUIFail` hint keeps
    ///    macOS from popping the "wants to access" password dialog on the
    ///    user's face, so the reconciler can decide programmatically what
    ///    to do (rewrite via delete+add to reset ACL, in our case).
    ///  - `.read` for anything else (system state we didn't anticipate).
    ///
    /// The prompt suppression is the whole point of `kSecUseAuthenticationUIFail`.
    /// Without it, every tick where CC's most recent write happened to leave
    /// us out of the trusted-app list (which is every tick, because CC
    /// created the item and macOS's `security add-generic-password -U -A`
    /// deliberately ignores `-A` on the update path — see the write() doc)
    /// interrupts the user with a login-keychain unlock dialog. On silent
    /// deny, our write path takes over and re-creates the item with a
    /// truly permissive ACL, which is the only way to reset it.
    func read(service: String, account: String) throws -> Data {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
            kSecUseAuthenticationUI as String: kSecUseAuthenticationUIFail,
        ]
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecItemNotFound {
            throw KeychainError.notFound(status)
        }
        // errSecInteractionNotAllowed (-25308) is what we get with
        // kSecUseAuthenticationUIFail when the ACL would have prompted.
        // errSecInteractionRequired (-25315) shows up on some macOS
        // versions for the same condition; treat both as the same case.
        if status == errSecInteractionNotAllowed || status == -25315 {
            throw KeychainError.interactionRequired(status)
        }
        guard status == errSecSuccess else {
            throw KeychainError.read(status)
        }
        guard let data = item as? Data else {
            throw KeychainError.unexpectedFormat
        }
        return data
    }

    /// Write the entry via /usr/bin/security as delete-then-add.
    ///
    /// The obvious-sounding path — `security add-generic-password -U -A`
    /// — looks like it updates the item in place AND resets the ACL to
    /// permissive. It does the first but not the second. Reading Apple's
    /// security_tool source (keychain_add.c) shows the update path calls
    /// `SecKeychainItemModifyAttributesAndData`, which ignores the
    /// access parameter entirely. `-A` is honored only on the create
    /// path. Which means every -U -A we've done in the past just
    /// overwrote data while leaving whatever ACL the original creator
    /// installed — for us, that's the CLI's own initial `claude login`,
    /// which scoped the ACL to just node/electron. Every one of our
    /// reads then trips the "wants to access" prompt on the user, and
    /// clicking Always Allow only sticks until CDHash changes on the
    /// next rebuild.
    ///
    /// Delete + add breaks this: the delete drops the CLI-scoped ACL,
    /// and the add re-creates the item with `-A`, which on the create
    /// path really does write a permissive access (nil trusted-app
    /// list). Every subsequent reader — us on the next tick, the CLI
    /// on its next refresh, the multica daemon spawning agents — reads
    /// without prompt. The tiny window where the item doesn't exist is
    /// on the order of tens of milliseconds; a concurrent CLI read
    /// there gets errSecItemNotFound and its normal retry logic kicks
    /// in on the next call.
    ///
    /// Password bytes travel on argv, not stdin, so we hit the code
    /// path in `security` that handles blobs larger than PASS_MAX
    /// (Claude's credentials blob comfortably exceeds it, and the stdin
    /// path silently truncates). macOS restricts argv visibility to the
    /// process owner by default, so this leaks nothing to any other
    /// user; the value is already in the user's keychain, which the
    /// same owner can read directly.
    func write(service: String, account: String, data: Data) throws {
        guard let value = String(data: data, encoding: .utf8) else {
            throw KeychainError.unexpectedFormat
        }

        // Delete first, best-effort. errSecItemNotFound (exit 44 from
        // the CLI) is expected on the very first write of a fresh Mac
        // where the CLI has never logged in; anything else we treat as
        // a soft warning and continue to the add.
        _ = runSecurity(args: [
            "delete-generic-password",
            "-s", service,
            "-a", account,
        ])

        // Fresh create with -A. This is the ONLY branch of security(1)
        // that actually installs a permissive ACL, so it has to be a
        // create (add without -U), never an update.
        let result = runSecurity(args: [
            "add-generic-password",
            "-s", service,
            "-a", account,
            "-A",
            "-w", value,
        ])
        if result.status != 0 {
            throw KeychainError.securityCLI(status: result.status, message: result.stderr)
        }
    }

    /// runSecurity is a small helper for the delete+add sequence. It
    /// returns exit status and captured stderr rather than throwing so
    /// the caller can make the delete step best-effort while insisting
    /// the add succeeds.
    private func runSecurity(args: [String]) -> (status: Int32, stderr: String) {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/security")
        process.arguments = args
        let errPipe = Pipe()
        process.standardError = errPipe
        process.standardOutput = Pipe()
        do {
            try process.run()
        } catch {
            return (Int32(errSecInternalError), "spawn failed: \(error)")
        }
        process.waitUntilExit()
        let errData = errPipe.fileHandleForReading.readDataToEndOfFile()
        let msg = (String(data: errData, encoding: .utf8) ?? "")
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return (process.terminationStatus, msg)
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
