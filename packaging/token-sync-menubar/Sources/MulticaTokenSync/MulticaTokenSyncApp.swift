import SwiftUI
import AppKit

/// MulticaTokenSyncApp is the SwiftUI entry point. The app is
/// AppKit-driven now: NSStatusItem, NSMenu, and the diagnostics window
/// all live in AppDelegate (see AppDelegate.swift for the reason). The
/// SwiftUI App type still exists because Swift Package Manager @main
/// scaffolding needs at least one Scene — a Settings scene with an empty
/// body is invisible at runtime and satisfies the type system.
@main
struct MulticaTokenSyncApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate

    var body: some Scene {
        // Placeholder scene: LSUIElement apps have no dock icon and the
        // menu App > Settings shortcut isn't visible, so this Settings
        // scene never renders. Any other scene type would cost a hidden
        // window at launch that would immediately need dismissing.
        Settings {
            EmptyView()
        }
    }
}

/// SyncLoopRunner owns the background sync work. It has three drivers
/// with different failure modes and cadences — the app stays in sync as
/// long as ANY of them is working:
///
///   1. **k8s watch** (primary) — a long-lived streaming GET against the
///      broker Secret. Events arrive within milliseconds of a broker
///      write, so a rotation is applied to the keychain before the CLI
///      has a chance to hit a dead access_token. When the watch drops
///      (server-side timeout every ~10min, control-plane restart, socket
///      death after Mac sleep), the loop reconnects immediately.
///
///   2. **Safety poll** (fallback) — a DispatchSourceTimer that fires an
///      unconditional syncOnce every 10 minutes. Catches degraded-watch
///      states where the connection looks alive but events aren't
///      arriving (silent apiserver bug, stalled proxy, etc). Uses
///      dispatch (not Task.sleep) so it survives Mac sleep correctly.
///
///   3. **Sleep/wake trigger** — NSWorkspace.didWakeNotification fires
///      an immediate syncOnce and reconnects the watch. On wake, our
///      watch socket is almost certainly dead (kube-apiserver dropped it
///      when TCP keepalives stopped acking during sleep), and we can't
///      afford to wait for TCP to notice — the CLI is about to hit the
///      keychain and would fail with a stale token.
///
/// App Nap suppression via ProcessInfo.beginActivity(.background) keeps
/// macOS from suspending the whole process when the user hasn't
/// interacted with the menubar in a while.
actor SyncLoopRunner {
    private var watchTask: Task<Void, Never>?
    private var pollSource: DispatchSourceTimer?
    private var wakeObserver: Any?
    private var triggerContinuation: CheckedContinuation<Void, Never>?
    private var engine: SyncEngine?
    private var kube: KubeClient?
    // Retained ProcessInfo activity token — releasing it re-enables App
    // Nap on this process, so hold it for the lifetime of the loop.
    private var napToken: NSObjectProtocol?
    // Latest resourceVersion the watch has observed. Used to resume the
    // watch after a disconnect without replaying the full history —
    // k8s' resourceVersion is monotonically increasing per resource, so
    // passing it back with resourceVersion=X gives us "events strictly
    // newer than X".
    private var latestResourceVersion: String?

    func startIfNeeded(model: AppModel) {
        if watchTask != nil { return }

        // Kick off notification permission early — first health
        // transition is the moment we'd need it, and we don't want that
        // transition to race a permission prompt.
        Task { await Notifier.requestAuthorization() }

        // App Nap suppression: a background-priority activity is enough
        // to keep our socket + timer alive without asking the system to
        // prioritize us like a foreground app would. .idleSystemSleepDisabled
        // is deliberately NOT included — we don't want to prevent the
        // Mac from sleeping, we just want to run correctly when it's
        // awake, and the wake handler catches us up on resume.
        let reason = "Multica Token Sync: keychain reconciler must run to keep the local Claude Code login valid."
        napToken = ProcessInfo.processInfo.beginActivity(
            options: [.userInitiatedAllowingIdleSystemSleep],
            reason: reason
        )

        do {
            let cfg = try KubeConfig.load()
            let kc = try KubeClient(config: cfg)
            self.kube = kc
            self.engine = SyncEngine(kube: kc, keychain: KeychainStore())
        } catch {
            Task { @MainActor in model.setupError = error.localizedDescription }
            return
        }

        installWakeHandler(model: model)
        installSafetyPoll(model: model)

        self.watchTask = Task { [weak self] in
            await self?.watchLoop(model: model)
        }
    }

    /// triggerNow wakes the loop from ANY of its three drivers — the
    /// "Sync now" menu action calls this and so does the wake handler.
    /// A spurious call with nothing to do is harmless: it just runs one
    /// syncOnce.
    func triggerNow() {
        Task { [weak self] in
            guard let self, let engine = await self.engine else { return }
            await self.runOne(engine: engine, model: await self.currentModel)
        }
    }

    // We stash the model on first startIfNeeded so triggerNow (called
    // from the delegate's wake handler and the menu) doesn't need the
    // caller to thread it through again.
    private var storedModel: AppModel?
    private var currentModel: AppModel! { storedModel }

    // MARK: - Drivers

    /// watchLoop is the primary path. Each iteration:
    ///  1. seed by reading the current Secret (also captures RV)
    ///  2. reconcile once (bring keychain in line)
    ///  3. open the watch from that RV
    ///  4. apply each event via syncOnce (event-driven; near-real-time)
    ///  5. on error/close, backoff and reconnect
    private func watchLoop(model: AppModel) async {
        guard let engine, let kube else { return }
        self.storedModel = model

        var backoff: TimeInterval = 1
        while !Task.isCancelled {
            do {
                // Fresh read anchors us to a known RV and applies the
                // current state to the keychain before we start watching.
                let (_, rv) = try await kube.readBrokerStateWithVersion(
                    namespace: model.config.namespace,
                    name: model.config.secretName
                )
                self.latestResourceVersion = rv
                await runOne(engine: engine, model: model)

                // Success — reset backoff for the next reconnect.
                backoff = 1

                let stream = await kube.watchBrokerState(
                    namespace: model.config.namespace,
                    name: model.config.secretName,
                    sinceResourceVersion: latestResourceVersion
                )
                for try await event in stream {
                    if let rv = event.object.metadata?.resourceVersion, !rv.isEmpty {
                        self.latestResourceVersion = rv
                    }
                    switch event.type {
                    case "ADDED", "MODIFIED":
                        // A real state change on the broker's side — run
                        // the reconciler so it lands in the keychain.
                        await runOne(engine: engine, model: model)
                    case "BOOKMARK":
                        // No state change; just a checkpoint of the RV.
                        // Recording it above is all we need to do.
                        break
                    case "DELETED":
                        // Someone deleted the Secret out from under us.
                        // We can't recover — surface via setupError and
                        // let the safety poll keep trying so a re-created
                        // Secret eventually reconnects us.
                        await MainActor.run { model.setupError = "broker Secret was deleted" }
                    case "ERROR":
                        // Server-side error, e.g. "resource version too
                        // old". Break to the outer loop which resets RV
                        // by re-reading and reconnecting.
                        self.latestResourceVersion = nil
                        throw KubeError.parse("watch error event; will reseed")
                    default:
                        break
                    }
                }
                // Natural end of stream: k8s hit its server-side
                // timeout. Reconnect immediately (backoff already 1s).
            } catch is CancellationError {
                return
            } catch {
                // Any error is either "watch died" (network, TLS, RV
                // stale) or "reseed read failed". Backoff and retry —
                // the safety poll will keep syncing meanwhile.
                await runOne(engine: engine, model: model, forceError: error)
                try? await Task.sleep(nanoseconds: UInt64(backoff * 1_000_000_000))
                backoff = min(backoff * 2, 30) // cap at 30s so a long
                                                // outage doesn't leave
                                                // us minutes-behind.
            }
        }
    }

    /// installSafetyPoll uses DispatchSourceTimer, not Task.sleep. The
    /// dispatch timer fires against a real system clock and remains
    /// correct across sleep/wake; Task.sleep pauses in place during
    /// sleep and is unreliable on resume, which is what left us blind
    /// for 7+ hours after Mac sleep in the pre-refactor version.
    private func installSafetyPoll(model: AppModel) {
        let queue = DispatchQueue(label: "com.multica.token-sync.safety-poll", qos: .utility)
        let source = DispatchSource.makeTimerSource(queue: queue)
        // 10min interval, 60s leeway. Leeway lets the scheduler
        // coalesce with other wakeups for battery efficiency; timing
        // precision isn't material here — the safety poll is a backstop
        // to the (near-realtime) watch.
        source.schedule(deadline: .now() + 600, repeating: 600, leeway: .seconds(60))
        source.setEventHandler { [weak self] in
            Task { [weak self] in
                guard let self,
                      let engine = await self.engine else { return }
                await self.runOne(engine: engine, model: model)
            }
        }
        source.resume()
        self.pollSource = source
    }

    /// installWakeHandler subscribes to the workspace's wake
    /// notification. When it fires we drop the (probably-dead) watch
    /// socket by restarting the outer loop, and immediately run a sync
    /// so the keychain catches up before the CLI reads it.
    private func installWakeHandler(model: AppModel) {
        let center = NSWorkspace.shared.notificationCenter
        let name = NSWorkspace.didWakeNotification
        wakeObserver = center.addObserver(forName: name, object: nil, queue: nil) { [weak self] _ in
            Task { [weak self] in
                await self?.onWake(model: model)
            }
        }
    }

    private func onWake(model: AppModel) async {
        guard let engine else { return }
        // First: force a sync so the CLI has fresh credentials.
        await runOne(engine: engine, model: model)
        // Then: cancel the current watch (its socket is almost certainly
        // dead) so the outer loop reconnects with a fresh TCP session.
        watchTask?.cancel()
        watchTask = Task { [weak self] in
            await self?.watchLoop(model: model)
        }
    }

    // MARK: - runOne

    /// runOne executes one reconcile cycle and pushes the result through
    /// the model. `forceError` is used by the watch loop to record a
    /// visible failure without re-running the engine — an underlying
    /// error already tells us what to say, and running the engine again
    /// against the same broken state would just double-log.
    private func runOne(engine: SyncEngine, model: AppModel, forceError: Error? = nil) async {
        await MainActor.run { model.running = true }
        defer { Task { @MainActor in model.running = false } }

        let cfg = await MainActor.run { model.config }
        let outcome: SyncOutcome
        if let forceError {
            outcome = SyncOutcome(
                at: Date(),
                direction: .noop,
                wrote: false,
                brokerExpiresAt: nil,
                keychainExpiresAt: nil,
                errorMessage: describe(forceError)
            )
        } else {
            do {
                outcome = try await engine.syncOnce(cfg)
            } catch {
                outcome = SyncOutcome(
                    at: Date(),
                    direction: .noop,
                    wrote: false,
                    brokerExpiresAt: nil,
                    keychainExpiresAt: nil,
                    errorMessage: describe(error)
                )
            }
        }
        appendLog(outcome: outcome)
        let transition = await MainActor.run { model.record(outcome) }
        if transition.from != .failing && transition.to == .failing {
            Notifier.notifyFailing(cause: outcome.errorMessage)
        } else if transition.from == .failing && transition.to == .healthy {
            Notifier.notifyRecovered()
        }
    }

    /// describe renders errors preferring their debugDescription when
    /// available. AsyncHTTPClient's errors implement CustomStringConvertible
    /// with useful case names (e.g. "readTimeout", "cancelled") but their
    /// localizedDescription is a generic "The operation couldn't be
    /// completed. (AsyncHTTPClient.HTTPClientError error 1.)" that hides
    /// which case actually fired.
    private func describe(_ error: Error) -> String {
        // Try our own LocalizedError-conforming types first — their
        // errorDescription is already tuned to be useful.
        if let localized = (error as? LocalizedError)?.errorDescription {
            return localized
        }
        return "\(error)"
    }

    /// appendLog writes one line to the sync log file so operators
    /// grepping old paths still see fresh syncs after the launchctl unit
    /// is swapped out. Silent on I/O failures — logging is best-effort,
    /// and a menubar app with a broken $HOME/Library/Logs is already in
    /// trouble.
    private func appendLog(outcome: SyncOutcome) {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        let ts = f.string(from: outcome.at)
        let msg: String
        if let err = outcome.errorMessage {
            msg = "\(ts) level=ERROR msg=\"sync failed\" error=\"\(err)\"\n"
        } else {
            let wrote = outcome.wrote ? "wrote=true" : "wrote=false"
            msg = "\(ts) level=INFO msg=\"sync ok\" direction=\(outcome.direction.rawValue) \(wrote)\n"
        }
        let url = logFileURL()
        do {
            try FileManager.default.createDirectory(
                at: url.deletingLastPathComponent(),
                withIntermediateDirectories: true
            )
            if !FileManager.default.fileExists(atPath: url.path) {
                FileManager.default.createFile(atPath: url.path, contents: nil)
            }
            let handle = try FileHandle(forWritingTo: url)
            defer { try? handle.close() }
            try handle.seekToEnd()
            handle.write(Data(msg.utf8))
        } catch {
            // best-effort
        }
    }
}
