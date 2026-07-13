import Foundation
import Yams

/// KubeConfig is the minimum slice of ~/.kube/config the reconciler needs to
/// authenticate to the API server. Fields track the "current" context: we
/// deliberately do not model contexts as a switchable set — the menubar app
/// runs against exactly one cluster (the one the user is logged in to as
/// they installed the app) and adding context switching is a preferences-UI
/// concern that belongs later, if at all.
struct KubeConfig {
    let serverURL: URL
    let caCertificatePEM: Data      // trust roots for server cert validation
    let clientCertificatePEM: Data  // mTLS client cert (may be a chain)
    let clientKeyPEM: Data          // mTLS client private key
}

enum KubeConfigError: Error, LocalizedError {
    case fileNotFound(String)
    case parseFailure(String)
    case missingField(String)
    case decodeBase64(String)

    var errorDescription: String? {
        switch self {
        case .fileNotFound(let p):
            return "kubeconfig not found at \(p)"
        case .parseFailure(let m):
            return "kubeconfig parse failed: \(m)"
        case .missingField(let f):
            return "kubeconfig missing field: \(f)"
        case .decodeBase64(let f):
            return "kubeconfig field \(f) is not valid base64"
        }
    }
}

extension KubeConfig {
    /// Load resolves KUBECONFIG env → ~/.kube/config exactly like kubectl,
    /// then walks current-context → context.cluster → context.user and
    /// returns the four bytes-of-PEM the TLS layer needs.
    static func load() throws -> KubeConfig {
        let path = ProcessInfo.processInfo.environment["KUBECONFIG"]
            ?? "\(NSHomeDirectory())/.kube/config"

        guard FileManager.default.fileExists(atPath: path) else {
            throw KubeConfigError.fileNotFound(path)
        }
        let text: String
        do {
            text = try String(contentsOfFile: path, encoding: .utf8)
        } catch {
            throw KubeConfigError.fileNotFound("\(path): \(error.localizedDescription)")
        }

        let root: Any
        do {
            root = try Yams.load(yaml: text) as Any
        } catch {
            throw KubeConfigError.parseFailure(error.localizedDescription)
        }
        guard let dict = root as? [String: Any] else {
            throw KubeConfigError.parseFailure("root is not a mapping")
        }

        guard let current = dict["current-context"] as? String, !current.isEmpty else {
            throw KubeConfigError.missingField("current-context")
        }
        guard let contexts = dict["contexts"] as? [[String: Any]] else {
            throw KubeConfigError.missingField("contexts")
        }
        guard let ctxEntry = contexts.first(where: { ($0["name"] as? String) == current }),
              let ctx = ctxEntry["context"] as? [String: Any] else {
            throw KubeConfigError.missingField("contexts[name=\(current)]")
        }
        guard let clusterName = ctx["cluster"] as? String,
              let userName = ctx["user"] as? String else {
            throw KubeConfigError.missingField("context.cluster / context.user")
        }

        guard let clusters = dict["clusters"] as? [[String: Any]] else {
            throw KubeConfigError.missingField("clusters")
        }
        guard let clusterEntry = clusters.first(where: { ($0["name"] as? String) == clusterName }),
              let cluster = clusterEntry["cluster"] as? [String: Any] else {
            throw KubeConfigError.missingField("clusters[name=\(clusterName)]")
        }
        guard let server = cluster["server"] as? String, let serverURL = URL(string: server) else {
            throw KubeConfigError.missingField("clusters[\(clusterName)].server")
        }
        let caPEM: Data
        if let b64 = cluster["certificate-authority-data"] as? String {
            guard let decoded = Data(base64Encoded: sanitizeBase64(b64)) else {
                throw KubeConfigError.decodeBase64("certificate-authority-data")
            }
            caPEM = decoded
        } else if let path = cluster["certificate-authority"] as? String {
            caPEM = try readFile(path, field: "certificate-authority")
        } else {
            throw KubeConfigError.missingField("certificate-authority[-data]")
        }

        guard let users = dict["users"] as? [[String: Any]] else {
            throw KubeConfigError.missingField("users")
        }
        guard let userEntry = users.first(where: { ($0["name"] as? String) == userName }),
              let user = userEntry["user"] as? [String: Any] else {
            throw KubeConfigError.missingField("users[name=\(userName)]")
        }
        let certPEM: Data
        if let b64 = user["client-certificate-data"] as? String {
            guard let decoded = Data(base64Encoded: sanitizeBase64(b64)) else {
                throw KubeConfigError.decodeBase64("client-certificate-data")
            }
            certPEM = decoded
        } else if let path = user["client-certificate"] as? String {
            certPEM = try readFile(path, field: "client-certificate")
        } else {
            throw KubeConfigError.missingField("client-certificate[-data]")
        }
        let keyPEM: Data
        if let b64 = user["client-key-data"] as? String {
            guard let decoded = Data(base64Encoded: sanitizeBase64(b64)) else {
                throw KubeConfigError.decodeBase64("client-key-data")
            }
            keyPEM = decoded
        } else if let path = user["client-key"] as? String {
            keyPEM = try readFile(path, field: "client-key")
        } else {
            throw KubeConfigError.missingField("client-key[-data]")
        }

        return KubeConfig(
            serverURL: serverURL,
            caCertificatePEM: caPEM,
            clientCertificatePEM: certPEM,
            clientKeyPEM: keyPEM
        )
    }
}

/// Yams occasionally hands back base64 strings with embedded newlines / CR
/// depending on kubeconfig-writer style; Data(base64Encoded:) refuses those
/// unless the options include ignoreUnknownCharacters, and getting a nil
/// there is indistinguishable from truly-corrupt input. Strip whitespace up
/// front so a genuine decode failure means what it says.
private func sanitizeBase64(_ s: String) -> String {
    s.filter { !$0.isWhitespace }
}

private func readFile(_ path: String, field: String) throws -> Data {
    let expanded = (path as NSString).expandingTildeInPath
    guard let data = FileManager.default.contents(atPath: expanded) else {
        throw KubeConfigError.fileNotFound("\(field): \(expanded)")
    }
    return data
}
