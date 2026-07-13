// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "MulticaTokenSync",
    platforms: [
        // macOS 14+ required for @Observable and the newer .environment /
        // .onChange overloads used by the app.
        .macOS(.v14)
    ],
    dependencies: [
        // Talos control plane requires mTLS with PEM client cert + key from
        // kubeconfig. AsyncHTTPClient + NIOSSL is the shortest path in pure
        // Swift: TLSConfiguration accepts PEM materials directly, no PKCS#12
        // dance and no temporary keychain to manage the SecIdentity.
        .package(url: "https://github.com/swift-server/async-http-client.git", from: "1.20.0"),
        .package(url: "https://github.com/apple/swift-nio.git", from: "2.65.0"),
        .package(url: "https://github.com/apple/swift-nio-ssl.git", from: "2.27.0"),
        // Yams for kubeconfig YAML. kubeconfig is trivially small so a full
        // YAML parser is overkill by weight but the correct dependency by
        // interface — anything homegrown will trip over corner cases the
        // kubeconfig writer emits (block scalars, anchors, quoting styles).
        .package(url: "https://github.com/jpsim/Yams.git", from: "5.1.0"),
    ],
    targets: [
        .executableTarget(
            name: "MulticaTokenSync",
            dependencies: [
                .product(name: "AsyncHTTPClient", package: "async-http-client"),
                .product(name: "NIOCore", package: "swift-nio"),
                // NIOPosix (not NIOTransportServices) forces AsyncHTTPClient
                // to run on the classic NIOSSL transport, which understands
                // TLSConfiguration with PEM materials directly. The default
                // Apple-platform transport is Network.framework, and its
                // NWProtocolTLSOptions bridge trips a precondition when
                // asked to consume custom PEM CA/certs — the exact crash we
                // hit before pinning the ELG explicitly.
                .product(name: "NIOPosix", package: "swift-nio"),
                .product(name: "NIOSSL", package: "swift-nio-ssl"),
                "Yams",
            ],
            path: "Sources/MulticaTokenSync"
        ),
    ]
)
