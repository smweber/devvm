import Foundation

/// One row of `devvm status --plain`. Tokens are passed through as strings so
/// a newer CLI can add states without breaking the app; helpers below map the
/// documented ones and treat anything else as unknown.
///
/// Hubs (another host running devvm) add no column: a hub's own row has
/// backend `hub`, and a machine on it is named `HUB/NAME` with the state the
/// hub reported, or `unreachable` for every row of a hub that did not answer
/// (its last listing, so the machines stay visible). The forwards column of
/// a hub machine is this Mac's own.
struct Machine {
    let name: String
    let backend: String
    let state: String    // running | stopped | dormant | reachable | unreachable | broken conf | ?
    let forwards: String // up:N | reconnecting:N | down | -

    /// A hub's own row: a host machines live on, not a machine. Never a
    /// drop target, never started or stopped. The backend alone does not
    /// say: a `HUB/NAME` row also carries backend `hub` when the hub did not
    /// list the machine (or did not answer and only this Mac's tables name
    /// it), so a hub is a `hub`-backend row with no slash in its name.
    var isHub: Bool { backend == "hub" && hub == nil }
    /// The hub a `HUB/NAME` row lives on, or nil for a local machine.
    var hub: String? {
        guard let slash = name.firstIndex(of: "/") else { return nil }
        return String(name[..<slash])
    }
    /// What the row shows: a hub machine sits under its hub's header, so the
    /// part after the slash is enough. Actions still use the full `name`.
    var label: String {
        guard let slash = name.firstIndex(of: "/") else { return name }
        return String(name[name.index(after: slash)...])
    }
    var isUnreachable: Bool { state == "unreachable" }

    /// Running smol VMs and remote hosts (which always report `reachable`;
    /// devvm does not probe them, so this is "selectable", not "alive"). A
    /// hub itself is neither: nothing is copied to or started on it.
    var isLive: Bool { !isHub && (state == "running" || state == "reachable") }
    /// Ports work only on machines this Mac reaches directly until devvm's
    /// hub forwards (roadmap step 6) land. Everything else — start, stop,
    /// copy in/out, the drop target — goes through the hub by name (`h/web`
    /// is a machine name to every devvm command), so it is gated on isLive
    /// alone.
    var isDirect: Bool { hub == nil }
    var isReconnecting: Bool { forwards.hasPrefix("reconnecting") }
    var forwardsUp: Bool { forwards.hasPrefix("up:") && forwardCount > 0 }
    var forwardCount: Int {
        guard let colon = forwards.firstIndex(of: ":") else { return 0 }
        return Int(forwards[forwards.index(after: colon)...]) ?? 0
    }

    var stateWords: String {
        switch state {
        case "running", "stopped", "dormant", "reachable", "unreachable": return state
        case "broken conf": return "broken conf"
        default: return "unknown"
        }
    }

    var forwardsWords: String {
        let n = forwardCount
        if forwards.hasPrefix("up:") { return n == 1 ? "1 forward up" : "\(n) forwards up" }
        if forwards.hasPrefix("reconnecting:") { return "reconnecting (\(n) pending)" }
        if forwards == "down" { return "forwards down" }
        if forwards == "-" { return "no ports" }
        return "forwards unknown"
    }

    /// A glyph for the menu row. Filled = live, hollow = off, dotted = unknown
    /// (which `unreachable` is: the hub did not say).
    var glyph: String {
        if isLive { return "●" }
        if state == "stopped" || state == "dormant" { return "○" }
        return "◌"
    }

    /// The menu row title; also used to refresh rows in place while the menu
    /// is open.
    var rowTitle: String { "\(glyph) \(label) — \(stateWords), \(forwardsWords)" }

    /// The section header for a hub: its name and whether it answered.
    var hubHeaderTitle: String { "\(name) — hub, \(stateWords)" }
}

/// Parses one blank-line-separated block of tab-separated rows.
func parseStatusBlock(_ text: String) -> [Machine] {
    var out: [Machine] = []
    for line in text.split(separator: "\n") {
        let f = line.split(separator: "\t", omittingEmptySubsequences: false).map { String($0) }
        guard f.count >= 3, !f[0].isEmpty else { continue }
        out.append(Machine(name: f[0], backend: f[1], state: f[2], forwards: f.count > 3 ? f[3] : "-"))
    }
    return out
}

/// Owns the one long-lived `devvm status --plain --watch` child. devvm pushes
/// a fresh block whenever its own state changes, so the app never polls.
/// If the child dies it is restarted with backoff; `restart()` is used when
/// the CLI binary changed underneath it (a rename-over update leaves the old
/// process running old code).
final class StatusWatcher {
    var onSnapshot: (([Machine]) -> Void)?

    private let devvm: Devvm
    private var process: Process?
    /// The stdout read end, kept so its readabilityHandler can be cleared
    /// before the Process (and with it the Pipe) is released. Dropping a
    /// FileHandle while its handler is still armed is a known crash.
    private var readHandle: FileHandle?
    private var buffer = Data()
    private var backoff: TimeInterval = 1
    private var launchedAt = Date.distantPast
    private var restartTimer: Timer?
    private var stopped = false
    private var generation = 0

    init(devvm: Devvm) { self.devvm = devvm }

    func start() {
        stopped = false
        launch()
    }

    func stop() {
        stopped = true
        restartTimer?.invalidate()
        generation += 1 // orphan any late callbacks
        clearReader()
        if let p = process, p.isRunning {
            Log.status.notice("stopping watch child [\(p.processIdentifier, privacy: .public)]")
            p.terminate()
        }
        process = nil
    }

    func restart() {
        Log.status.notice("restarting the watch child (the devvm binary changed)")
        restartTimer?.invalidate()
        backoff = 1
        generation += 1
        clearReader()
        if let p = process, p.isRunning { p.terminate() }
        process = nil
        launch()
    }

    private func clearReader() {
        readHandle?.readabilityHandler = nil
        readHandle = nil
    }

    private func launch() {
        guard !stopped, let exe = devvm.executable else { return }
        generation += 1
        let gen = generation
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = ["status", "--plain", "--watch"]
        p.environment = devvm.environment
        p.standardInput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        let out = Pipe()
        p.standardOutput = out
        buffer = Data()
        let handle = out.fileHandleForReading
        handle.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            if data.isEmpty { // EOF
                handle.readabilityHandler = nil
                return
            }
            DispatchQueue.main.async { self?.consume(data, gen: gen) }
        }
        p.terminationHandler = { [weak self] _ in
            DispatchQueue.main.async { self?.exited(gen: gen) }
        }
        do {
            try p.run()
        } catch {
            Log.status.error("watch child did not start: \(error.localizedDescription, privacy: .public)")
            handle.readabilityHandler = nil
            scheduleRestart()
            return
        }
        Log.status.notice("watch child started [\(p.processIdentifier, privacy: .public)]: devvm status --plain --watch")
        readHandle = handle
        process = p
        launchedAt = Date()
    }

    private static let separator = Data("\n\n".utf8)

    private func consume(_ data: Data, gen: Int) {
        guard gen == generation else { return }
        buffer.append(data)
        while let range = buffer.range(of: StatusWatcher.separator) {
            let block = String(decoding: buffer.subdata(in: buffer.startIndex..<range.lowerBound), as: UTF8.self)
            buffer.removeSubrange(buffer.startIndex..<range.upperBound)
            let machines = parseStatusBlock(block)
            Log.status.notice("snapshot: \(StatusWatcher.summary(machines), privacy: .public)")
            onSnapshot?(machines)
        }
        // An empty registry is a lone blank line, never followed by a second.
        if buffer == Data("\n".utf8) {
            buffer = Data()
            Log.status.notice("snapshot: no machines registered")
            onSnapshot?([])
        }
    }

    /// One line per snapshot: `personal=running/up:2 work=stopped/-`.
    static func summary(_ machines: [Machine]) -> String {
        if machines.isEmpty { return "no machines registered" }
        return "\(machines.count) machine(s): " + machines.map { "\($0.name)=\($0.state)/\($0.forwards)" }.joined(separator: " ")
    }

    private func exited(gen: Int) {
        guard gen == generation, !stopped else { return }
        // A child that stayed up long enough to be useful earns a fresh
        // backoff; one that printed once and died must not be restarted
        // every second. Judged here, at exit, where the uptime is known (the
        // first block arrives immediately, so it says nothing about that).
        if Date().timeIntervalSince(launchedAt) > 10 { backoff = 1 }
        let status = process?.terminationStatus ?? -1
        Log.status.error("watch child exited with status \(status, privacy: .public) after \(Int(Date().timeIntervalSince(self.launchedAt)), privacy: .public)s")
        clearReader()
        process = nil
        scheduleRestart()
    }

    private func scheduleRestart() {
        guard !stopped else { return }
        let delay = backoff
        backoff = min(backoff * 2, 30)
        Log.status.notice("restarting the watch child in \(Int(delay), privacy: .public)s")
        restartTimer?.invalidate()
        let timer = Timer(timeInterval: delay, repeats: false) { [weak self] _ in
            self?.launch()
        }
        // Common modes, so the restart is not stalled while a menu is being
        // tracked or an alert is up.
        RunLoop.main.add(timer, forMode: .common)
        restartTimer = timer
    }
}

/// One live forward from `devvm ports list NAME`:
///     guest 8080  -> localhost:8080  (pending)
struct Forward {
    let guest: Int
    let host: Int
    let pending: Bool
}

func parsePortsList(_ text: String) -> [Forward] {
    var out: [Forward] = []
    for raw in text.split(separator: "\n") {
        let line = raw.trimmingCharacters(in: .whitespaces)
        guard line.hasPrefix("guest ") else { continue }
        let parts = line.split(whereSeparator: { $0 == " " || $0 == "\t" }).map { String($0) }
        // ["guest", "8080", "->", "localhost:8080", "(pending)"?]
        guard parts.count >= 4, let guest = Int(parts[1]), parts[2] == "->",
              parts[3].hasPrefix("localhost:"), let host = Int(parts[3].dropFirst("localhost:".count))
        else { continue }
        out.append(Forward(guest: guest, host: host, pending: parts.contains("(pending)")))
    }
    return out
}
