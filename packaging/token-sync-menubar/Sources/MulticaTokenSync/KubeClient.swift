import Foundation
import AsyncHTTPClient
import NIOCore
import NIOPosix
import NIOSSL
import NIOHTTP1

/// KubeClient hits the Kubernetes API server directly over mTLS. Scope is
/// intentionally tiny: read one Secret and JSON-merge-patch it. We do not
/// wrap this in a generic client abstraction — one call site, one path,
/// mirrored exactly against how the Go reconciler talked to client-go.
///
/// One HTTPClient is created per process and shut down at exit. The client
/// itself is thread-safe.
actor KubeClient {
    private let client: HTTPClient
    private let baseURL: String

    init(config: KubeConfig) throws {
        // Trust roots + client identity are wired through NIOSSL.
        // TLSConfiguration.makeClientConfiguration() defaults to system trust
        // store, which we do NOT want for a homelab CA — replace it with the
        // kubeconfig-embedded CA. NIOSSL accepts PEM materials natively so
        // there's no need for a temporary keychain or PKCS#12 conversion.
        let caCerts = try NIOSSLCertificate.fromPEMBytes(Array(config.caCertificatePEM))
        let clientCerts = try NIOSSLCertificate.fromPEMBytes(Array(config.clientCertificatePEM))
        let privateKey = try NIOSSLPrivateKey(bytes: Array(config.clientKeyPEM), format: .pem)

        var tls = TLSConfiguration.makeClientConfiguration()
        tls.trustRoots = .certificates(caCerts)
        tls.certificateChain = clientCerts.map { .certificate($0) }
        tls.privateKey = .privateKey(privateKey)

        var httpConfig = HTTPClient.Configuration()
        httpConfig.tlsConfiguration = tls
        // A misbehaving control-plane socket would otherwise hang the sync
        // for kernel-default timeouts (~2m). Fifteen seconds is comfortably
        // more than a healthy control plane needs and still leaves plenty of
        // room before the 5-min sync interval.
        httpConfig.timeout.connect = .seconds(10)
        httpConfig.timeout.read = .seconds(15)

        // Pin to a MultiThreadedEventLoopGroup (NIOPosix) rather than the
        // AsyncHTTPClient.singleton, which on macOS selects the
        // NIOTransportServices/Network.framework transport. That transport
        // rejects PEM-based TLSConfiguration with a precondition trap; the
        // classic NIOSSL transport is the one that speaks PEM natively.
        let elg: EventLoopGroup = MultiThreadedEventLoopGroup.singleton
        self.client = HTTPClient(eventLoopGroupProvider: .shared(elg), configuration: httpConfig)
        self.baseURL = config.serverURL.absoluteString
            .trimmingCharacters(in: CharacterSet(charactersIn: "/"))
    }

    /// Shutdown the underlying HTTPClient. Call from an app-exit hook — not
    /// calling it produces a NIO precondition failure at process teardown.
    func shutdown() async {
        try? await client.shutdown()
    }

    /// Read the broker's state Secret and decode the three base64 keys the
    /// reconciler cares about. Mirrors ReadBrokerState in the Go original:
    /// missing access_token or refresh_token is an error (bootstrap not
    /// complete), so downstream code can trust that a successful return
    /// carries usable credentials.
    func readBrokerState(namespace: String, name: String) async throws -> BrokerState {
        let path = "/api/v1/namespaces/\(namespace)/secrets/\(name)"
        let body = try await get(path: path)
        let secret = try JSONDecoder().decode(K8sSecret.self, from: body)
        let data = secret.data ?? [:]

        let accessToken = try decodeB64Field(data["access_token"], name: "access_token")
        let refreshToken = try decodeB64Field(data["refresh_token"], name: "refresh_token")

        let expiresAt: Date
        if let expB64 = data["expires_at"], !expB64.isEmpty {
            let raw = try decodeB64Field(expB64, name: "expires_at")
            guard let s = String(data: raw, encoding: .utf8), let d = KubeClient.rfc3339.date(from: s) else {
                throw KubeError.parse("expires_at not RFC3339")
            }
            expiresAt = d
        } else {
            expiresAt = .distantPast // Go's zero-value equivalent
        }

        guard let atStr = String(data: accessToken, encoding: .utf8),
              let rtStr = String(data: refreshToken, encoding: .utf8),
              !atStr.isEmpty, !rtStr.isEmpty else {
            throw KubeError.parse("secret \(namespace)/\(name) missing access_token or refresh_token")
        }
        return BrokerState(accessToken: atStr, refreshToken: rtStr, expiresAt: expiresAt)
    }

    /// JSON-merge-patch the three data keys with base64-encoded values.
    /// Matches the Go binary's WriteBrokerState byte-for-byte so a broker
    /// tick sees identical Secret state whether the client was Go or Swift.
    func writeBrokerState(namespace: String, name: String, state: BrokerState) async throws {
        let path = "/api/v1/namespaces/\(namespace)/secrets/\(name)"
        let expiresRFC3339 = KubeClient.rfc3339.string(from: state.expiresAt)
        let dataDict: [String: String] = [
            "access_token": Data(state.accessToken.utf8).base64EncodedString(),
            "refresh_token": Data(state.refreshToken.utf8).base64EncodedString(),
            "expires_at": Data(expiresRFC3339.utf8).base64EncodedString(),
        ]
        let patchBody: [String: Any] = ["data": dataDict]
        let body = try JSONSerialization.data(withJSONObject: patchBody, options: [.sortedKeys])
        try await sendPatch(path: path, contentType: "application/merge-patch+json", body: body)
    }

    // MARK: - HTTP primitives

    private func get(path: String) async throws -> Data {
        var request = HTTPClientRequest(url: baseURL + path)
        request.method = .GET
        request.headers.add(name: "accept", value: "application/json")
        let response = try await client.execute(request, timeout: .seconds(15))
        return try await collectBody(response, path: path)
    }

    private func sendPatch(path: String, contentType: String, body: Data) async throws {
        var request = HTTPClientRequest(url: baseURL + path)
        request.method = .PATCH
        request.headers.add(name: "content-type", value: contentType)
        request.headers.add(name: "accept", value: "application/json")
        request.body = .bytes(ByteBuffer(bytes: body))
        let response = try await client.execute(request, timeout: .seconds(15))
        _ = try await collectBody(response, path: path)
    }

    private func collectBody(_ response: HTTPClientResponse, path: String) async throws -> Data {
        let buffer = try await response.body.collect(upTo: 4 * 1024 * 1024) // 4MB ceiling: Secrets are tiny
        var data = Data()
        data.reserveCapacity(buffer.readableBytes)
        data.append(contentsOf: buffer.readableBytesView)
        guard (200...299).contains(Int(response.status.code)) else {
            let snippet = String(data: data.prefix(400), encoding: .utf8) ?? "<binary>"
            throw KubeError.badStatus(status: Int(response.status.code), path: path, body: snippet)
        }
        return data
    }

    // Kubernetes emits RFC3339 timestamps in the exact form Go's
    // time.RFC3339 does (`2026-07-13T12:12:01Z`). ISO8601DateFormatter with
    // .withInternetDateTime handles this format; we cache the formatter as
    // its per-instance construction cost dominates the actual parse.
    private static let rfc3339: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()
}

// MARK: - k8s wire types

/// K8sSecret is the just-enough slice of the Kubernetes Secret shape the
/// reconciler needs. `.data` is a map of base64-encoded strings; `metadata`
/// carries resourceVersion which we accept for future optimistic-concurrency
/// use but don't act on today.
private struct K8sSecret: Decodable {
    let metadata: Metadata?
    let data: [String: String]?

    struct Metadata: Decodable {
        let resourceVersion: String?
    }
}

private func decodeB64Field(_ value: String?, name: String) throws -> Data {
    guard let v = value else {
        throw KubeError.parse("field \(name) missing")
    }
    guard let d = Data(base64Encoded: v) else {
        throw KubeError.parse("field \(name) not base64")
    }
    return d
}

enum KubeError: Error, LocalizedError {
    case badStatus(status: Int, path: String, body: String)
    case parse(String)
    case tls(String)

    var errorDescription: String? {
        switch self {
        case .badStatus(let s, let p, let b):
            return "HTTP \(s) on \(p): \(b)"
        case .parse(let m):
            return "parse error: \(m)"
        case .tls(let m):
            return "TLS error: \(m)"
        }
    }
}
