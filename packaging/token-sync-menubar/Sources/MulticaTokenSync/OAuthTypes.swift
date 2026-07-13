import Foundation

/// BrokerState mirrors the three keys the broker writes into its Secret. A
/// missing expiry (zero Date) is not the same as absent — the parser rejects
/// the Secret if either token is empty, so callers can trust these fields.
struct BrokerState: Equatable {
    let accessToken: String
    let refreshToken: String
    let expiresAt: Date
}

/// KeychainPayload mirrors Claude Code's on-disk credentials.json shape.
/// The property names match Claude Code's JSON keys byte-for-byte so a
/// round-trip through Codable produces the exact same file the CLI writes
/// itself. Any drift here silently breaks the Mac side of the sync.
struct KeychainPayload: Codable, Equatable {
    var claudeAiOauth: OAuthBlob

    struct OAuthBlob: Codable, Equatable {
        var accessToken: String
        var refreshToken: String
        var expiresAt: Int64 // millis since epoch
        var scopes: [String]
        var subscriptionType: String
    }
}

/// Scopes the Claude Code CLI uses; stable across rotations. Kept in one
/// place so a rotation that adds/removes a scope forces us to look at the
/// pull path, not silently overwrite a fresher scope set.
let defaultScopes: [String] = [
    "user:profile",
    "user:inference",
    "user:sessions:claude_code",
    "user:mcp_servers",
]

/// pushSkew tolerates minor timestamp drift so trivial round-trip differences
/// (RFC3339 rounds off sub-second precision; broker vs CLI stamp expires_at
/// slightly differently) don't cause a churn loop where each side keeps
/// "correcting" the other by a fraction of a second. Must stay well below the
/// broker's refreshPad so a genuine keychain rotation still triggers a push.
let pushSkew: TimeInterval = 30

enum SyncDirection: String, Codable {
    case noop, pull, push
}

/// SyncOutcome is what one `syncOnce` call returns to the UI layer. The
/// expiries are snapshotted at sync time so a noop still surfaces them for
/// the menu.
struct SyncOutcome: Equatable {
    let at: Date
    let direction: SyncDirection
    let wrote: Bool
    let brokerExpiresAt: Date?
    let keychainExpiresAt: Date?
    let errorMessage: String? // nil on success
}
