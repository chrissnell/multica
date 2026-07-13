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
        // Connect timeout is bounded because a misbehaving control plane
        // would otherwise sit in TCP handshake for the kernel default
        // (~2m). Read timeout is DELIBERATELY unset: it's applied to
        // idle time between chunks on the response body, and our watch
        // connection is idle by design (k8s pushes events only on
        // change). All quick reads are bounded per-request by the
        // caller-supplied execute(timeout:), so removing the connection
        // read timeout doesn't leave any request unbounded.
        httpConfig.timeout.connect = .seconds(10)

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
        return try await readBrokerStateWithVersion(namespace: namespace, name: name).state
    }

    /// readBrokerStateWithVersion also returns the ResourceVersion so a
    /// caller starting a watch can send `resourceVersion=X` on the query
    /// and receive only events strictly newer than the state it just saw.
    /// Callers that don't need the version (the sync path) can use the
    /// state-only variant above.
    func readBrokerStateWithVersion(namespace: String, name: String) async throws -> (state: BrokerState, resourceVersion: String?) {
        let path = "/api/v1/namespaces/\(namespace)/secrets/\(name)"
        let body = try await get(path: path)
        let secret = try JSONDecoder().decode(K8sSecret.self, from: body)
        let state = try Self.brokerState(from: secret, namespace: namespace, name: name)
        return (state, secret.metadata?.resourceVersion)
    }

    /// brokerState decodes a K8sSecret DTO into our reconciler-facing
    /// BrokerState. Factored out so both the plain read and the watch
    /// event handler go through the exact same decoding path — any
    /// tolerance we grant one, we grant both.
    static func brokerState(from secret: K8sSecret, namespace: String, name: String) throws -> BrokerState {
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
            expiresAt = .distantPast
        }

        guard let atStr = String(data: accessToken, encoding: .utf8),
              let rtStr = String(data: refreshToken, encoding: .utf8),
              !atStr.isEmpty, !rtStr.isEmpty else {
            throw KubeError.parse("secret \(namespace)/\(name) missing access_token or refresh_token")
        }
        return BrokerState(accessToken: atStr, refreshToken: rtStr, expiresAt: expiresAt)
    }

    /// watchBrokerState opens a long-lived streaming GET against the k8s
    /// watch endpoint for the broker Secret. Each server-sent JSON event
    /// (ADDED / MODIFIED / DELETED / BOOKMARK / ERROR) is yielded through
    /// the returned AsyncThrowingStream. Consumers should also carry a
    /// safety-poll fallback because the watch can be silently degraded
    /// (control-plane restart, TCP RST after sleep, etc.) — the stream
    /// throws on those, but only when the next event would have shown up.
    ///
    /// `sinceResourceVersion` scopes the watch to events newer than the
    /// state the caller has already observed. Passing nil means "start
    /// from now, don't replay history" (implemented by omitting the query
    /// parameter, which k8s treats as "current").
    ///
    /// Timeouts: k8s randomly closes watches after ~5-10 minutes on its
    /// side to shed load; that's expected and consumers must reconnect.
    /// We set an HTTPClient read timeout of 15 minutes to be safely
    /// larger than the server-side ceiling — anything shorter would
    /// masquerade transient control-plane latency as a stream error.
    ///
    /// The returned stream is unbounded: it stays alive until the caller
    /// stops iterating, the request errors, or the server closes it.
    func watchBrokerState(namespace: String, name: String, sinceResourceVersion: String?) -> AsyncThrowingStream<K8sWatchEvent, Error> {
        let path = "/api/v1/namespaces/\(namespace)/secrets"
        var query = "?fieldSelector=metadata.name%3D\(name)&watch=true&allowWatchBookmarks=true&timeoutSeconds=600"
        if let rv = sinceResourceVersion, !rv.isEmpty {
            query += "&resourceVersion=\(rv)"
        }
        let url = baseURL + path + query
        let baseURLCapture = baseURL

        return AsyncThrowingStream { continuation in
            let task = Task { [client] in
                do {
                    var request = HTTPClientRequest(url: url)
                    request.method = .GET
                    // Ask for the JSON watch stream shape explicitly. The
                    // apiserver defaults to it for watch=true requests, but
                    // pinning the accept header avoids surprises if a proxy
                    // (Cloudflare, ingress) negotiates something else.
                    request.headers.add(name: "accept", value: "application/json")
                    _ = baseURLCapture
                    // 15 minutes is comfortably above k8s' server-side
                    // watch cap (~5-10min) so a natural server close
                    // never masquerades as a client-side stall.
                    let response = try await client.execute(request, timeout: .seconds(900))
                    guard (200...299).contains(Int(response.status.code)) else {
                        // Try to pull the body for an error snippet
                        let body = try await response.body.collect(upTo: 8192)
                        var data = Data()
                        data.append(contentsOf: body.readableBytesView)
                        let msg = String(data: data, encoding: .utf8) ?? "<binary>"
                        throw KubeError.badStatus(status: Int(response.status.code), path: path, body: msg)
                    }

                    // Line-buffered parse: k8s' streaming watch emits one
                    // compact JSON event per line. Accumulate bytes across
                    // HTTP chunks, split on \n, decode each complete line.
                    var buffer = Data()
                    for try await chunk in response.body {
                        buffer.append(contentsOf: chunk.readableBytesView)
                        while let nlIdx = buffer.firstIndex(of: 0x0A) {
                            let line = buffer.subdata(in: buffer.startIndex..<nlIdx)
                            buffer.removeSubrange(buffer.startIndex...nlIdx)
                            if line.isEmpty { continue }
                            do {
                                let event = try JSONDecoder().decode(K8sWatchEvent.self, from: line)
                                continuation.yield(event)
                            } catch {
                                // A malformed line is a bug in k8s or our
                                // decoder — surface as a hard error so the
                                // consumer restarts the watch rather than
                                // silently missing events.
                                throw KubeError.parse("watch decode: \(error)")
                            }
                        }
                    }
                    // Body ended normally (k8s hit its server-side timeout).
                    // Signal end so the consumer can reconnect.
                    continuation.finish()
                } catch is CancellationError {
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            continuation.onTermination = { _ in
                task.cancel()
            }
        }
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
/// carries resourceVersion, which the watch startup path uses to resume from
/// exactly the state we already know about.
struct K8sSecret: Decodable {
    let metadata: Metadata?
    let data: [String: String]?

    struct Metadata: Decodable {
        let resourceVersion: String?
    }
}

/// K8sWatchEvent is one line of the k8s watch stream. `object` is a
/// raw-value carrier: for MODIFIED/ADDED/DELETED it's a Secret, for
/// BOOKMARK it's a mostly-empty object carrying only the current
/// resourceVersion, for ERROR it's a Status. The consumer inspects
/// `type` to know which shape to expect.
struct K8sWatchEvent: Decodable {
    let type: String
    let object: K8sSecret
}

func decodeB64Field(_ value: String?, name: String) throws -> Data {
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
