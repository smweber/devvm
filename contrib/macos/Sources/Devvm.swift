import Foundation

/// The outcome of one devvm invocation.
struct CommandResult {
    let status: Int32
    let stdout: String
    let stderr: String

    var ok: Bool { status == 0 }

    /// The last non-empty stderr line: devvm's own error is always last, after
    /// any transport chatter, and it is what a notification has room for.
    var lastStderrLine: String {
        let lines = stderr.split(separator: "\n").map { $0.trimmingCharacters(in: .whitespaces) }
        return lines.last(where: { !$0.isEmpty }) ?? (ok ? "" : "exit status \(status)")
    }

    var isOverwriteRefusal: Bool { stderr.contains("refusing to overwrite") }
}

/// Locates and runs the devvm CLI. All process work is off the main thread;
/// completions are delivered on it.
final class Devvm {
    static let shared = Devvm()

    /// Environment for every child: the user's login PATH, because a GUI app
    /// launched from Finder/login gets `/usr/bin:/bin:/usr/sbin:/sbin` and
    /// devvm's own children (smolvm, ssh, scp, gh) live in Homebrew or
    /// ~/.local/bin.
    let environment: [String: String]
    let executable: String?

    private init() {
        var env = ProcessInfo.processInfo.environment
        let path = Devvm.resolvePath(current: env["PATH"] ?? "")
        env["PATH"] = path
        environment = env
        executable = Devvm.locate("devvm", path: path)
        // Unified log (Console.app, or `log stream --process DevVM`): the one
        // place to see why a drop did nothing, since the app has no window.
        NSLog("devvm: PATH=%@", path)
        NSLog("devvm: executable=%@", executable ?? "(not found)")
    }

    // MARK: PATH

    private static func resolvePath(current: String) -> String {
        var entries: [String] = []
        func add(_ p: String) {
            let e = (p as NSString).expandingTildeInPath
            if !e.isEmpty, !entries.contains(e) { entries.append(e) }
        }
        loginPath().split(separator: ":").forEach { add(String($0)) }
        current.split(separator: ":").forEach { add(String($0)) }
        // Common install dirs, in case the login shell added nothing.
        ["~/.local/bin", "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"].forEach(add)
        return entries.joined(separator: ":")
    }

    /// Asks the login shell for its PATH by running `env` inside it, so the
    /// answer is colon-joined regardless of shell (fish included).
    ///
    /// This runs once, synchronously, at launch: nothing else can start until
    /// devvm is located. It is bounded to five seconds by waiting on a
    /// background reader rather than reading on this thread — a hung rc file,
    /// or one that backgrounds a process holding our pipe open (which would
    /// defeat a plain read-to-EOF forever), must not hang the app with no
    /// menu bar icon. On timeout the shell is killed and the empty answer
    /// falls back to the well-known directories.
    private static func loginPath() -> String {
        let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
        let p = Process()
        p.executableURL = URL(fileURLWithPath: shell)
        p.arguments = ["-l", "-c", "/usr/bin/env"]
        p.standardInput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        let out = Pipe()
        p.standardOutput = out
        do { try p.run() } catch { return "" }

        let box = DataBox()
        let done = DispatchSemaphore(value: 0)
        DispatchQueue.global().async {
            box.out = out.fileHandleForReading.readDataToEndOfFile()
            done.signal()
        }
        if done.wait(timeout: .now() + 5) == .timedOut {
            // Kill the shell (and its group, if it leads one) and force EOF on
            // our end; the reader thread is abandoned, not waited for.
            kill(p.processIdentifier, SIGKILL)
            kill(-p.processIdentifier, SIGKILL)
            try? out.fileHandleForWriting.close()
            return ""
        }
        p.waitUntilExit()
        guard p.terminationStatus == 0 else { return "" }
        let text = String(decoding: box.out, as: UTF8.self)
        for line in text.split(separator: "\n") where line.hasPrefix("PATH=") {
            return String(line.dropFirst("PATH=".count))
        }
        return ""
    }

    private static func locate(_ name: String, path: String) -> String? {
        let fm = FileManager.default
        for dir in path.split(separator: ":") {
            let candidate = (String(dir) as NSString).appendingPathComponent(name)
            if fm.isExecutableFile(atPath: candidate) { return candidate }
        }
        return nil
    }

    // MARK: Running

    /// Runs `devvm args...` and calls `completion` on the main queue. Returns
    /// the Process so a caller can terminate it (single-flight lookups).
    @discardableResult
    func run(_ args: [String], extraEnv: [String: String] = [:],
             completion: @escaping (CommandResult) -> Void) -> Process? {
        guard let exe = executable else {
            DispatchQueue.main.async {
                completion(CommandResult(status: 127, stdout: "", stderr: "devvm not found on PATH"))
            }
            return nil
        }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: exe)
        p.arguments = args
        var env = environment
        for (k, v) in extraEnv { env[k] = v }
        p.environment = env
        p.standardInput = FileHandle.nullDevice
        let out = Pipe(), err = Pipe()
        p.standardOutput = out
        p.standardError = err

        // Drain both pipes concurrently: a child that fills stderr while we
        // read stdout to EOF would otherwise deadlock. The results live in a
        // reference-typed box because dispatch closures are @Sendable and may
        // not mutate captured locals.
        let box = DataBox()
        let group = DispatchGroup()
        group.enter()
        DispatchQueue.global().async {
            box.out = out.fileHandleForReading.readDataToEndOfFile()
            group.leave()
        }
        group.enter()
        DispatchQueue.global().async {
            box.err = err.fileHandleForReading.readDataToEndOfFile()
            group.leave()
        }
        do {
            try p.run()
        } catch {
            // Release the readers: with the write ends closed they hit EOF.
            try? out.fileHandleForWriting.close()
            try? err.fileHandleForWriting.close()
            DispatchQueue.main.async {
                completion(CommandResult(status: 126, stdout: "", stderr: "cannot run devvm: \(error.localizedDescription)"))
            }
            return nil
        }
        NSLog("devvm: run %@", args.joined(separator: " "))
        DispatchQueue.global().async {
            p.waitUntilExit()
            group.wait()
            let result = CommandResult(status: p.terminationStatus,
                                       stdout: String(decoding: box.out, as: UTF8.self),
                                       stderr: String(decoding: box.err, as: UTF8.self))
            NSLog("devvm: exit %d for %@%@", result.status, args.first ?? "",
                  result.stderr.isEmpty ? "" : " — " + result.lastStderrLine)
            DispatchQueue.main.async { completion(result) }
        }
        return p
    }

    /// The CLI's version tag (`devvm version vX.Y.Z` -> `vX.Y.Z`), or nil.
    func version(completion: @escaping (String?) -> Void) {
        run(["--version"]) { r in
            guard r.ok else { completion(nil); return }
            let words = r.stdout.split(whereSeparator: { $0 == " " || $0 == "\n" })
            completion(words.last.map { String($0) })
        }
    }
}

/// Mutable pipe output shared between a dispatch closure and its waiter.
private final class DataBox {
    var out = Data()
    var err = Data()
}

/// At most one *counted* invocation: only the latest one's result is
/// delivered; earlier ones are dropped when they finish. Used for completion
/// lookups and the on-menu-open status refresh, where only the latest answer
/// matters.
///
/// By default a new run lets the previous process finish on its own rather
/// than terminating it: SIGTERM to devvm does not reach the ssh or smolvm
/// child it spawned, so killing a lookup mid-handshake only orphans the
/// transport. `terminatePrevious: true` is for cases where the old process
/// is known to be cheap and self-contained.
final class SingleFlight {
    private var current: Process?

    func run(_ devvm: Devvm, _ args: [String], extraEnv: [String: String] = [:],
             terminatePrevious: Bool = false,
             completion: @escaping (CommandResult) -> Void) {
        if terminatePrevious, let old = current, old.isRunning { old.terminate() }
        var proc: Process?
        proc = devvm.run(args, extraEnv: extraEnv) { [weak self] r in
            guard let self = self, self.current === proc else { return }
            self.current = nil
            completion(r)
        }
        current = proc
    }

    /// Drops the in-flight result and terminates the process if it is still
    /// running (terminating an already-reaped process is undefined).
    func cancel() {
        if let old = current, old.isRunning { old.terminate() }
        current = nil
    }
}
