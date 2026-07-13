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

/// SyncLoopRunner owns the background sync task. Split off the App type
/// so its lifecycle (start, tick, error handling) is testable in
/// isolation and doesn't rely on SwiftUI environment plumbing.
actor SyncLoopRunner {
    private var task: Task<Void, Never>?
    private var triggerContinuation: CheckedContinuation<Void, Never>?
    private var engine: SyncEngine?
    private var kube: KubeClient?

    /// startIfNeeded bootstraps the sync engine and starts the loop. The
    /// AppDelegate calls this once on applicationDidFinishLaunching;
    /// redundant calls (should not happen, but harmless if the wiring
    /// changes) short-circuit because `task` is non-nil.
    func startIfNeeded(model: AppModel) {
        if task != nil { return }

        // Kick off notification permission early — first health
        // transition is the moment we'd need it, and we don't want that
        // transition to race a permission prompt.
        Task { await Notifier.requestAuthorization() }

        do {
            let cfg = try KubeConfig.load()
            let kc = try KubeClient(config: cfg)
            self.kube = kc
            self.engine = SyncEngine(kube: kc, keychain: KeychainStore())
        } catch {
            Task { @MainActor in
                model.setupError = error.localizedDescription
            }
            return
        }

        self.task = Task { [weak self] in
            await self?.loop(model: model)
        }
    }

    /// triggerNow is the "Sync now" menu-item hook. Wakes the loop from
    /// its sleep by resuming the pending continuation; a spurious wake
    /// with nothing to do is harmless.
    func triggerNow() {
        if let cont = triggerContinuation {
            triggerContinuation = nil
            cont.resume()
        }
    }

    private func loop(model: AppModel) async {
        guard let engine else { return }
        while !Task.isCancelled {
            await runOne(engine: engine, model: model)
            // Race sleep against the "Sync now" wake continuation.
            // Whichever wins terminates the wait; the loop then re-arms
            // both.
            let interval = await MainActor.run { model.config.interval }
            await withTaskGroup(of: Void.self) { group in
                group.addTask {
                    try? await Task.sleep(nanoseconds: UInt64(interval * 1_000_000_000))
                }
                group.addTask { [weak self] in
                    await self?.awaitTrigger()
                }
                await group.next()
                group.cancelAll()
            }
        }
    }

    private func awaitTrigger() async {
        await withCheckedContinuation { (cont: CheckedContinuation<Void, Never>) in
            triggerContinuation = cont
        }
    }

    private func runOne(engine: SyncEngine, model: AppModel) async {
        await MainActor.run { model.running = true }
        defer { Task { @MainActor in model.running = false } }

        let cfg = await MainActor.run { model.config }
        let outcome: SyncOutcome
        do {
            outcome = try await engine.syncOnce(cfg)
        } catch {
            outcome = SyncOutcome(
                at: Date(),
                direction: .noop,
                wrote: false,
                brokerExpiresAt: nil,
                keychainExpiresAt: nil,
                errorMessage: error.localizedDescription
            )
        }
        appendLog(outcome: outcome)
        let transition = await MainActor.run { model.record(outcome) }
        if transition.from != .failing && transition.to == .failing {
            Notifier.notifyFailing(cause: outcome.errorMessage)
        } else if transition.from == .failing && transition.to == .healthy {
            Notifier.notifyRecovered()
        }
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
