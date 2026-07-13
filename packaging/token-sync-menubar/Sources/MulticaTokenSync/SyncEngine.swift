import Foundation
import CryptoKit

/// SyncConfig is the tunable surface of one reconciler run. Values default
/// to what the operator wants today; a preferences UI can flip them later.
struct SyncConfig {
    var namespace: String = "multica"
    var secretName: String = "multica-claude-oauth-broker"
    var keychainService: String = "Claude Code-credentials"
    var keychainAccount: String = NSUserName()
    var dryRun: Bool = false
    var interval: TimeInterval = 300 // menubar tick cadence, seconds
}

/// SyncEngine is the Swift port of the Go reconciler. All decision logic
/// (which side is fresher, whether a write is warranted) is identical — the
/// tests below are the fixed points that guarantee bit-for-bit equivalent
/// behavior against a broker that expects a specific keychain payload
/// shape.
///
/// Direction rule: keychain-fresher-than-broker (by refresh_token AND by
/// expires_at + pushSkew) → push. Anything else → pull, which is a noop
/// when the two sides already match.
struct SyncEngine {
    let kube: KubeClient
    let keychain: KeychainStore

    func syncOnce(_ cfg: SyncConfig) async throws -> SyncOutcome {
        let broker = try await kube.readBrokerState(namespace: cfg.namespace, name: cfg.secretName)

        // Distinguish "ACL blocks us" from "item missing" from "read
        // broke". The interactionRequired branch is expected on any
        // Mac where the CLI created the keychain item before we did
        // (which is most of them); the write path's delete+add heals
        // it on the next pull. Treating it identically to notFound is
        // fine — the reconciler already handles nil existing by
        // falling through to pull.
        let existing: Data?
        let readError: Error?
        do {
            existing = try keychain.read(service: cfg.keychainService, account: cfg.keychainAccount)
            readError = nil
        } catch KeychainError.notFound {
            existing = nil
            readError = nil
        } catch let e as KeychainError where isInteractionRequired(e) {
            // Log the recovery-mode transition so diagnostics show
            // what's happening. The pull below will heal it.
            FileHandle.standardError.write(Data(
                "keychain read denied by ACL; pulling from broker to reset ACL via delete+add\n".utf8))
            existing = nil
            readError = e
        } catch {
            existing = nil
            readError = error
        }

        let parsed = parseKeychain(existing: existing)

        if shouldPush(kc: parsed, broker: broker) {
            return try await pushToBroker(cfg: cfg, kc: parsed, broker: broker)
        }
        var outcome = try pullToKeychain(cfg: cfg, broker: broker, existing: existing)
        // Surface the read error in the outcome so operators can see
        // the ACL-recovery path fired, without failing the sync (the
        // pull already succeeded and will unblock the next tick).
        if outcome.errorMessage == nil, let readError {
            outcome = SyncOutcome(
                at: outcome.at,
                direction: outcome.direction,
                wrote: outcome.wrote,
                brokerExpiresAt: outcome.brokerExpiresAt,
                keychainExpiresAt: outcome.keychainExpiresAt,
                errorMessage: "recovered from keychain ACL denial: \(readError.localizedDescription)"
            )
        }
        return outcome
    }

    private func isInteractionRequired(_ err: KeychainError) -> Bool {
        if case .interactionRequired = err { return true }
        return false
    }

    // MARK: - decision + write paths

    /// pushToBroker mirrors the Go path of the same name. The broker's
    /// observeAndCheckReseed detects the changed refresh_token, treats it as
    /// an operator reseed, and exchanges it against Anthropic — no explicit
    /// broker signaling needed from this side.
    private func pushToBroker(cfg: SyncConfig, kc: ParsedKeychain, broker: BrokerState) async throws -> SyncOutcome {
        let state = BrokerState(
            accessToken: kc.blob!.accessToken,
            refreshToken: kc.blob!.refreshToken,
            expiresAt: kc.expiresAt
        )
        if !cfg.dryRun {
            try await kube.writeBrokerState(namespace: cfg.namespace, name: cfg.secretName, state: state)
        }
        return SyncOutcome(
            at: Date(),
            direction: .push,
            wrote: !cfg.dryRun,
            brokerExpiresAt: broker.expiresAt,
            keychainExpiresAt: kc.expiresAt,
            errorMessage: nil
        )
    }

    /// pullToKeychain is the steady-state path when the broker is healthy.
    /// The noop test is deliberately field-level (OAuth token triple), NOT
    /// byte-level. Byte comparison sounds tighter but it repeatedly bites:
    /// the Claude Code CLI's own writes emit JSON with a different key
    /// order than Swift's JSONEncoder, so every steady-state tick would
    /// decide "bytes differ, rewrite," which then resets the keychain
    /// item's ACL and forces the user to re-approve every process that
    /// reads via `/usr/bin/security` on a five-minute cadence. Comparing
    /// (accessToken, refreshToken, expiresAt) treats the payload's
    /// container shape as immaterial — only a real rotation should cause
    /// us to write.
    private func pullToKeychain(cfg: SyncConfig, broker: BrokerState, existing: Data?) throws -> SyncOutcome {
        let brokerExpiresMs = Int64(broker.expiresAt.timeIntervalSince1970 * 1000)

        if let existing = existing,
           let e = try? JSONDecoder().decode(KeychainPayload.self, from: existing).claudeAiOauth,
           e.accessToken == broker.accessToken,
           e.refreshToken == broker.refreshToken,
           e.expiresAt == brokerExpiresMs {
            return SyncOutcome(
                at: Date(),
                direction: .noop,
                wrote: false,
                brokerExpiresAt: broker.expiresAt,
                keychainExpiresAt: Date(timeIntervalSince1970: TimeInterval(e.expiresAt) / 1000),
                errorMessage: nil
            )
        }

        // Real rotation. Rebuild the payload and write. Match Go's
        // encoding/json output: no key sorting, no pretty print.
        let payload = KeychainPayload(claudeAiOauth: .init(
            accessToken: broker.accessToken,
            refreshToken: broker.refreshToken,
            expiresAt: brokerExpiresMs,
            scopes: defaultScopes,
            subscriptionType: "max"
        ))
        let newBytes = try JSONEncoder().encode(payload)
        if !cfg.dryRun {
            try keychain.write(service: cfg.keychainService, account: cfg.keychainAccount, data: newBytes)
        }
        return SyncOutcome(
            at: Date(),
            direction: .pull,
            wrote: !cfg.dryRun,
            brokerExpiresAt: broker.expiresAt,
            keychainExpiresAt: Date(timeIntervalSince1970: TimeInterval(payload.claudeAiOauth.expiresAt) / 1000),
            errorMessage: nil
        )
    }
}

// MARK: - parse + push predicate (pure functions, unit-testable)

/// ParsedKeychain flattens the tri-state "read succeeded / entry missing /
/// entry corrupt" into a single value the predicate consumes. `parsed=false`
/// means the reconciler should treat the keychain as empty and pull the
/// broker's state — either because the entry is missing (first run) or
/// because it's unparseable (which the reader logs, but the reconciler still
/// heals by overwriting).
struct ParsedKeychain: Equatable {
    let blob: KeychainPayload.OAuthBlob?
    let expiresAt: Date // .distantPast when parsed=false
    let parsed: Bool

    static let empty = ParsedKeychain(blob: nil, expiresAt: .distantPast, parsed: false)
}

func parseKeychain(existing: Data?) -> ParsedKeychain {
    guard let data = existing, !data.isEmpty else {
        return .empty
    }
    do {
        let p = try JSONDecoder().decode(KeychainPayload.self, from: data)
        let expires = Date(timeIntervalSince1970: TimeInterval(p.claudeAiOauth.expiresAt) / 1000)
        return ParsedKeychain(blob: p.claudeAiOauth, expiresAt: expires, parsed: true)
    } catch {
        // A malformed keychain entry is safe to fall through to a pull —
        // the pull will overwrite it with a valid payload. Log noisily so
        // corruption doesn't hide behind the next sync's success line.
        FileHandle.standardError.write(Data("keychain payload unparseable; will overwrite from broker: \(error)\n".utf8))
        return .empty
    }
}

/// shouldPush is the exact Go predicate translated. All five conditions
/// must hold — dropping any of them re-introduces a failure mode we already
/// hit in production:
///  - unparseable keychain → fall back to pull
///  - keychain tokens empty → nothing to push
///  - refresh_token identical → nothing to reseed; push is a noop
///  - keychain not meaningfully ahead of k8s → push would just churn within
///    timestamp rounding noise
///  - keychain already expired → refuse to push a token we know is dead
func shouldPush(kc: ParsedKeychain, broker: BrokerState) -> Bool {
    guard kc.parsed, let blob = kc.blob else { return false }
    if blob.accessToken.isEmpty || blob.refreshToken.isEmpty { return false }
    if blob.refreshToken == broker.refreshToken { return false }
    if kc.expiresAt == .distantPast { return false }
    if kc.expiresAt <= broker.expiresAt.addingTimeInterval(pushSkew) { return false }
    if Date() > kc.expiresAt { return false }
    return true
}

/// keychainExpiresFromBytes reads back the expiry from an on-disk payload
/// without allocating the parse result. Only used on the noop path so the
/// menu can show the current keychain expiry alongside the broker's.
private func keychainExpiresFromBytes(_ data: Data) -> Date? {
    guard let p = try? JSONDecoder().decode(KeychainPayload.self, from: data),
          p.claudeAiOauth.expiresAt > 0 else { return nil }
    return Date(timeIntervalSince1970: TimeInterval(p.claudeAiOauth.expiresAt) / 1000)
}

// MARK: - fingerprint

/// sha256Hex fingerprints a payload so the pull path can noop when nothing
/// changed. Hex form (not raw) so a fingerprint can appear verbatim in log
/// output cross-referenced against the broker's log lines.
func sha256Hex(_ data: Data) -> String {
    let digest = SHA256.hash(data: data)
    return digest.map { String(format: "%02x", $0) }.joined()
}
