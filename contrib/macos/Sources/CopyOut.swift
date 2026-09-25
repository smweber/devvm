import AppKit

/// "Copy out from NAME": a guest path field with shell-style Tab completion,
/// then a save panel (file) or folder picker (directory). Completion
/// candidates come from the CLI's own completion command, so the field
/// suggests exactly what the shell would; the Tab logic itself lives in
/// TabCompleter (PathCompletion.swift).
///
/// Candidates show in a one-line label under the field rather than AppKit's
/// completion popup: the popup completes a "partial word" it splits on `/`,
/// `.` and `-`, takes over the arrow and Tab keys while it is up, and has no
/// notion of a common prefix or of cycling, which is what made Tab feel
/// finicky. A label never takes focus, so Tab stays in the field.
final class CopyOutPanel: NSObject, NSTextFieldDelegate, NSWindowDelegate {
    private let devvm: Devvm
    private let machine: String
    private let panel: NSPanel
    private let field = NSTextField(frame: .zero)
    private let hint = NSTextField(labelWithString: "")

    private let lookups = SingleFlight()
    private var debounce: Timer?
    /// The text of the lookup in flight, so a Tab pressed while it runs
    /// waits for it instead of starting a duplicate.
    private var inFlight: String?
    private var completer = TabCompleter()
    /// A Tab pressed before its lookup returned: applied when it lands.
    private var pendingTab = false
    private var pendingBackwards = false
    /// Set while a completion rewrites the field, so the change it causes
    /// is not mistaken for the user editing.
    private var applying = false

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
        panel.delegate = self

        let content = NSView(frame: panel.contentRect(forFrameRect: panel.frame))
        let label = NSTextField(labelWithString: "Guest path (relative to home unless absolute; Tab completes):")
        label.frame = NSRect(x: 16, y: 84, width: 408, height: 18)
        field.frame = NSRect(x: 16, y: 56, width: 408, height: 24)
        field.placeholderString = "src/project/notes.txt or dir/"
        field.delegate = self
        hint.frame = NSRect(x: 16, y: 36, width: 408, height: 16)
        hint.font = .systemFont(ofSize: 11)
        hint.textColor = .secondaryLabelColor
        hint.lineBreakMode = .byTruncatingTail
        hint.maximumNumberOfLines = 1

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
        Log.menu.notice("copy out panel for \(self.machine, privacy: .public) cancelled")
        panel.orderOut(nil)
        stopCompletion()
    }

    /// The title bar's close button.
    func windowWillClose(_ notification: Notification) {
        stopCompletion()
    }

    /// Ends every completion activity. A pending debounce would otherwise
    /// fire after the panel is gone and start a guest lookup (an ssh
    /// handshake) for nothing; the answer is dropped too, as the guest may
    /// have changed by the next time the panel opens.
    private func stopCompletion() {
        debounce?.invalidate()
        debounce = nil
        lookups.cancel()
        inFlight = nil
        pendingTab = false
        completer = TabCompleter()
        hint.stringValue = ""
        hint.toolTip = nil
    }

    // MARK: Completion

    func controlTextDidChange(_ obj: Notification) {
        guard !applying else { return }
        // The user edited: any cycle ends, and a Tab still waiting for its
        // lookup is dropped (it was for text that no longer exists).
        completer.edited()
        pendingTab = false
        hint.stringValue = ""
        hint.toolTip = nil
        debounce?.invalidate()
        let timer = Timer(timeInterval: 0.15, repeats: false) { [weak self] _ in
            self?.lookup()
        }
        RunLoop.main.add(timer, forMode: .common)
        debounce = timer
    }

    /// Only the latest `__complete` answer is used; an older one still in
    /// flight is left to finish (terminating devvm would orphan the ssh or
    /// smolvm child doing the actual lookup) and its result is dropped, by
    /// SingleFlight and by the text-still-matches guard. The CLI's 2s cap is
    /// raised because a cold ssh handshake can take longer than that.
    private func lookup() {
        debounce?.invalidate()
        debounce = nil
        let text = field.stringValue
        inFlight = text
        Log.menu.debug("completion lookup on \(self.machine, privacy: .public) for \"\(text, privacy: .public)\"")
        // `--` before the text: without it cobra completes flag names for a
        // path starting with `-`; after it, the text is the positional
        // being completed (cobra passes args [NAME] and the text through).
        lookups.run(devvm, ["__complete", "cp-out", machine, "--", text],
                    extraEnv: ["DEVVM_COMPLETE_TIMEOUT": "10s"]) { [weak self] r in
            guard let self = self else { return }
            self.inFlight = nil
            let current = self.field.stringValue
            guard r.ok else {
                // Never cache a failed run's (empty) answer.
                Log.menu.error("completion lookup for \"\(text, privacy: .public)\" failed: \(r.lastStderrLine, privacy: .public)")
                if self.pendingTab && current == text {
                    self.pendingTab = false
                    NSSound.beep()
                    self.hint.stringValue = "Lookup failed — Tab to retry"
                    self.hint.toolTip = r.lastStderrLine
                }
                return
            }
            let (found, directive) = CopyOutPanel.parseCompletions(r.stdout)
            Log.menu.debug("completion for \"\(text, privacy: .public)\": \(found.count, privacy: .public) candidate(s), directive \(directive, privacy: .public)")
            let answer = PathCompletion(query: text, candidates: found)
            // The user typed on while it ran: still usable when the new text
            // only narrows the old query exactly (see PathCompletion).
            guard current == text || answer.candidates(for: current) != nil else { return }
            self.completer.received(answer)
            if self.pendingTab {
                self.pendingTab = false
                self.tab(backwards: self.pendingBackwards)
            } else if !self.completer.isCycling {
                let n = self.completer.answer?.candidates(for: current)?.count ?? 0
                self.hint.stringValue = n == 0 ? "" : "\(n) match\(n == 1 ? "" : "es") — press Tab"
                self.hint.toolTip = nil
            }
        }
    }

    /// Splits `__complete` output into candidates and cobra's directive.
    /// The directive is the last line starting with ":" (a guest file may
    /// itself be named ":something", so an earlier one is a candidate).
    private static func parseCompletions(_ stdout: String) -> ([String], String) {
        var lines = stdout.split(separator: "\n").map { String($0) }
        var directive = ""
        if let i = lines.lastIndex(where: { $0.hasPrefix(":") }) {
            directive = lines[i]
            lines = Array(lines[..<i])
        }
        return (lines, directive)
    }

    /// Tab and Shift-Tab never leave the field: they complete.
    func control(_ control: NSControl, textView: NSTextView, doCommandBy commandSelector: Selector) -> Bool {
        if commandSelector == #selector(NSResponder.insertTab(_:)) {
            tab(backwards: false)
            return true
        }
        if commandSelector == #selector(NSResponder.insertBacktab(_:)) {
            tab(backwards: true)
            return true
        }
        return false
    }

    private func tab(backwards: Bool) {
        let text = field.stringValue
        switch completer.tab(text: text, backwards: backwards) {
        case .lookup:
            pendingTab = true
            pendingBackwards = backwards
            hint.stringValue = "Looking up…"
            hint.toolTip = nil
            if inFlight != text { lookup() }
        case .noMatches:
            NSSound.beep()
            hint.stringValue = "No matches"
            hint.toolTip = nil
            // The next Tab asks the guest again rather than repeating this.
            completer.forget()
        case .unique(let path):
            setText(path)
            hint.stringValue = ""
            hint.toolTip = nil
            // Fetch the directory's entries now, so the next Tab lists them
            // without waiting.
            if path.hasSuffix("/") && path != text { lookup() }
        case .extended(let prefix, let all):
            setText(prefix)
            showList(all, current: nil)
        case .cycled(let path, let index, let all):
            setText(path)
            showList(all, current: index)
        }
    }

    private func showList(_ all: [String], current: Int?) {
        hint.stringValue = TabCompleter.summary(all, current: current)
        hint.toolTip = all.map { TabCompleter.leafName($0) }.joined(separator: "\n")
    }

    /// Replaces the field's text through the field editor, as a normal edit
    /// (so undo stays consistent) that is not treated as the user's, and
    /// puts the insertion point at the end.
    private func setText(_ s: String) {
        applying = true
        defer { applying = false }
        guard let editor = field.currentEditor() as? NSTextView else {
            field.stringValue = s
            return
        }
        let all = NSRange(location: 0, length: (editor.string as NSString).length)
        if editor.shouldChangeText(in: all, replacementString: s) {
            editor.replaceCharacters(in: all, with: s)
            editor.didChangeText()
        }
        let end = NSRange(location: (s as NSString).length, length: 0)
        editor.setSelectedRange(end)
        editor.scrollRangeToVisible(end)
    }

    // MARK: Copy

    @objc private func copyTapped() {
        let path = field.stringValue.trimmingCharacters(in: .whitespaces)
        guard !path.isEmpty else { return }
        // Hide the path window before the save panel opens: it floats, so
        // a panel presented while it is up opens behind it. Cancelling the
        // save panel brings it back with the path intact, to be fixed; only
        // a copy (or closing the path window) ends the flow.
        panel.orderOut(nil)
        stopCompletion()
        // An accessory app is not frontmost by itself; without this the
        // save panel can open behind the app the user was just in.
        NSApp.activate(ignoringOtherApps: true)
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
            guard open.runModal() == .OK, let dir = open.url else {
                Log.menu.notice("copy out of \(path, privacy: .public): folder picker cancelled; back to the path")
                reopen()
                return
            }
            run(["cp-out", "-r", "-t", dir.path, machine, "--", path], what: path)
        } else {
            let save = NSSavePanel()
            save.title = "Save \(machine):\(path) as…"
            save.nameFieldStringValue = (path as NSString).lastPathComponent
            save.canCreateDirectories = true
            guard save.runModal() == .OK, let url = save.url else {
                Log.menu.notice("copy out of \(path, privacy: .public): save panel cancelled; back to the path")
                reopen()
                return
            }
            // The save panel already confirmed any overwrite, so -f is right here.
            run(["cp-out", "-f", machine, "--", path, url.path], what: path)
        }
    }

    /// Back to the path window after a cancelled save panel, text intact.
    private func reopen() {
        NSApp.activate(ignoringOtherApps: true)
        panel.makeKeyAndOrderFront(nil)
        panel.makeFirstResponder(field)
    }

    private func run(_ args: [String], what: String) {
        Log.menu.notice("copy out \(self.machine, privacy: .public):\(what, privacy: .public)")
        devvm.run(args) { [machine = self.machine] r in
            if r.ok {
                Log.menu.notice("copy out of \(what, privacy: .public) from \(self.machine, privacy: .public) succeeded")
                Notifications.shared.info("Copied \(what)", "from \(machine)")
            } else if r.isOverwriteRefusal {
                Log.menu.notice("copy out of \(what, privacy: .public) refused: would overwrite locally")
                Notifications.shared.conflict("Already exists locally", r.lastStderrLine, retryWith: args)
            } else {
                Log.menu.error("copy out of \(what, privacy: .public) from \(self.machine, privacy: .public) failed: \(r.lastStderrLine, privacy: .public)")
                Notifications.shared.error("Copy from \(machine) failed", r.lastStderrLine)
            }
        }
    }
}
