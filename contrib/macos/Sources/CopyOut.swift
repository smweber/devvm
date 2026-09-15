import AppKit

/// "Copy out from NAME": a guest path field that completes as you type, then
/// a save panel (file) or folder picker (directory). Completion candidates
/// come from the CLI's own completion command, so the field suggests exactly
/// what the shell would.
final class CopyOutPanel: NSObject, NSTextFieldDelegate {
    private let devvm: Devvm
    private let machine: String
    private let panel: NSPanel
    private let field = NSTextField(frame: .zero)
    private let hint = NSTextField(labelWithString: "")

    private let lookups = SingleFlight()
    private var debounce: Timer?
    private var candidates: [String] = []
    private var candidatesFor = ""

    init(devvm: Devvm, machine: String) {
        self.devvm = devvm
        self.machine = machine
        panel = NSPanel(contentRect: NSRect(x: 0, y: 0, width: 440, height: 118),
                        styleMask: [.titled, .closable, .utilityWindow], backing: .buffered, defer: false)
        super.init()

        panel.title = "Copy out from \(machine)"
        panel.level = .floating
        panel.isFloatingPanel = true
        panel.becomesKeyOnlyIfNeeded = false // typing must reach the field
        panel.hidesOnDeactivate = false
        panel.isReleasedWhenClosed = false

        let content = NSView(frame: panel.contentRect(forFrameRect: panel.frame))
        let label = NSTextField(labelWithString: "Guest path (relative to home unless absolute; Tab completes):")
        label.frame = NSRect(x: 16, y: 84, width: 408, height: 18)
        field.frame = NSRect(x: 16, y: 56, width: 408, height: 24)
        field.placeholderString = "src/project/notes.txt or dir/"
        field.delegate = self
        hint.frame = NSRect(x: 16, y: 36, width: 408, height: 16)
        hint.font = .systemFont(ofSize: 11)
        hint.textColor = .secondaryLabelColor

        let cancel = NSButton(title: "Cancel", target: self, action: #selector(cancel))
        cancel.bezelStyle = .rounded
        cancel.keyEquivalent = "\u{1b}"
        cancel.frame = NSRect(x: 254, y: 6, width: 80, height: 28)
        let copy = NSButton(title: "Copy…", target: self, action: #selector(copyTapped))
        copy.bezelStyle = .rounded
        copy.keyEquivalent = "\r"
        copy.frame = NSRect(x: 340, y: 6, width: 84, height: 28)

        let views: [NSView] = [label, field, hint, cancel, copy]
        views.forEach(content.addSubview)
        panel.contentView = content
        panel.initialFirstResponder = field
        panel.center()
    }

    func show() {
        Log.menu.notice("copy out panel opened for \(self.machine, privacy: .public)")
        NSApp.activate(ignoringOtherApps: true)
        panel.makeKeyAndOrderFront(nil)
        panel.makeFirstResponder(field)
    }

    @objc private func cancel() {
        // A pending debounce would otherwise fire after the panel is gone and
        // start a guest lookup (an ssh handshake) for nothing.
        debounce?.invalidate()
        debounce = nil
        lookups.cancel()
        hint.stringValue = ""
        panel.orderOut(nil)
    }

    // MARK: Completion

    func controlTextDidChange(_ obj: Notification) {
        debounce?.invalidate()
        let timer = Timer(timeInterval: 0.15, repeats: false) { [weak self] _ in
            self?.lookup()
        }
        RunLoop.main.add(timer, forMode: .common)
        debounce = timer
    }

    /// Only the latest `__complete` answer is used; an older one still in
    /// flight is left to finish (terminating devvm would orphan the ssh or
    /// smolvm child doing the actual lookup) and its result is dropped by the
    /// text-still-matches guard. The CLI's 2s cap is raised because a cold
    /// ssh handshake can take longer than that.
    private func lookup() {
        let text = field.stringValue
        Log.menu.debug("completion lookup on \(self.machine, privacy: .public) for \"\(text, privacy: .public)\"")
        lookups.run(devvm, ["__complete", "cp-out", machine, text],
                    extraEnv: ["DEVVM_COMPLETE_TIMEOUT": "10s"]) { [weak self] r in
            guard let self = self, self.field.stringValue == text else { return }
            var found: [String] = []
            var directive = ""
            for line in r.stdout.split(separator: "\n") {
                if line.hasPrefix(":") { directive = String(line); break } // cobra's directive line ends the list
                found.append(String(line))
            }
            Log.menu.debug("completion for \"\(text, privacy: .public)\": \(found.count, privacy: .public) candidate(s), directive \(directive, privacy: .public)")
            self.candidates = found
            self.candidatesFor = text
            self.hint.stringValue = found.isEmpty ? "" : "\(found.count) match\(found.count == 1 ? "" : "es") — press Tab"
            if !found.isEmpty, let editor = self.field.currentEditor() as? NSTextView {
                editor.complete(nil)
            }
        }
    }

    /// AppKit's completion popup asks for replacements of the "partial word"
    /// it found (it splits on `/`, `.`, `-`); candidates are whole paths that
    /// extend the typed text, so hand back the part past the word's start.
    func control(_ control: NSControl, textView: NSTextView, completions words: [String],
                 forPartialWordRange charRange: NSRange, indexOfSelectedItem index: UnsafeMutablePointer<Int>) -> [String] {
        let typed = textView.string as NSString
        guard candidatesFor == field.stringValue || typed.hasPrefix(candidatesFor) else { return [] }
        let start = charRange.location
        var out: [String] = []
        for c in candidates {
            let ns = c as NSString
            guard ns.hasPrefix(typed as String), ns.length > start else { continue }
            out.append(ns.substring(from: start))
        }
        index.pointee = -1
        return out
    }

    func control(_ control: NSControl, textView: NSTextView, doCommandBy commandSelector: Selector) -> Bool {
        if commandSelector == #selector(NSResponder.insertTab(_:)) {
            if candidatesFor == field.stringValue, !candidates.isEmpty {
                textView.complete(nil)
            } else {
                lookup()
            }
            return true
        }
        return false
    }

    // MARK: Copy

    @objc private func copyTapped() {
        let path = field.stringValue.trimmingCharacters(in: .whitespaces)
        guard !path.isEmpty else { return }
        debounce?.invalidate()
        debounce = nil
        lookups.cancel()
        // `--` after NAME: a guest path starting with `-` must not be parsed
        // as a flag by the CLI.
        if path.hasSuffix("/") {
            let open = NSOpenPanel()
            open.title = "Copy \(machine):\(path) into…"
            open.prompt = "Copy here"
            open.canChooseFiles = false
            open.canChooseDirectories = true
            open.canCreateDirectories = true
            open.allowsMultipleSelection = false
            open.begin { [weak self] response in
                guard let self = self, response == .OK, let dir = open.url else { return }
                self.run(["cp-out", "-r", "-t", dir.path, self.machine, "--", path], what: path)
            }
        } else {
            let save = NSSavePanel()
            save.title = "Save \(machine):\(path) as…"
            save.nameFieldStringValue = (path as NSString).lastPathComponent
            save.canCreateDirectories = true
            save.begin { [weak self] response in
                guard let self = self, response == .OK, let url = save.url else { return }
                // The save panel already confirmed any overwrite, so -f is right here.
                self.run(["cp-out", "-f", self.machine, "--", path, url.path], what: path)
            }
        }
    }

    private func run(_ args: [String], what: String) {
        panel.orderOut(nil)
        Log.menu.notice("copy out \(self.machine, privacy: .public):\(what, privacy: .public)")
        devvm.run(args) { [machine = self.machine] r in
            if r.ok {
                Log.menu.notice("copy out of \(what, privacy: .public) from \(machine, privacy: .public) succeeded")
                Notifications.shared.info("Copied \(what)", "from \(machine)")
            } else if r.isOverwriteRefusal {
                Log.menu.notice("copy out of \(what, privacy: .public) refused: would overwrite locally")
                Notifications.shared.conflict("Already exists locally", r.lastStderrLine, retryWith: args)
            } else {
                Log.menu.error("copy out of \(what, privacy: .public) from \(machine, privacy: .public) failed: \(r.lastStderrLine, privacy: .public)")
                Notifications.shared.error("Copy from \(machine) failed", r.lastStderrLine)
            }
        }
    }
}
