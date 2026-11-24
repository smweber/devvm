import AppKit

/// A small browser for one machine's files: a guest path field with
/// shell-style Tab completion, and under it the list of what the field's
/// text matches. It picks what to copy out (CopyOut, below) and where drops
/// land (the inbox); what a confirmed choice means is the caller's
/// `onSubmit`.
///
/// Field and list are one thing seen twice. Both come from the CLI's own
/// completion command (one `__complete` per directory, so they show exactly
/// what the shell would), and the Tab logic lives in TabCompleter
/// (PathCompletion.swift). The list shows the candidates a Tab would work
/// on, so typing narrows it; its highlighted row is the one a Tab cycle
/// shows; a click on a row is a jump of that cycle (the field takes the
/// row's path); a double-click goes into a folder, or confirms a file. Up
/// and Down move through the rows as Tab and Shift-Tab do. Dotfiles appear
/// once a "." is typed, as in the shell.
///
/// The list never takes the keyboard: typing always reaches the field. It
/// stands in for AppKit's completion popup, which completes a "partial
/// word" it splits on `/`, `.` and `-`, takes over the arrow and Tab keys
/// while it is up, and has no notion of a common prefix or of cycling.
///
/// When `Config.multiple` is set, several rows can be selected (Cmd- or
/// Shift-click) and confirmed together, and rows can be dragged out to the
/// Finder or any app that takes files: the copy runs when they are dropped.
final class GuestPathPanel: NSObject, NSTextFieldDelegate, NSWindowDelegate,
                            NSTableViewDataSource, NSTableViewDelegate, NSFilePromiseProviderDelegate {
    struct Config {
        let title: String
        let label: String
        let placeholder: String
        let button: String
        /// Whether an empty field can be confirmed (the inbox: empty is home).
        let allowsEmpty: Bool
        /// Folders only, in the list and for Tab (the inbox).
        let foldersOnly: Bool
        /// Several rows at once, and dragging rows out (Copy out).
        let multiple: Bool
    }

    let machine: String
    private let devvm: Devvm
    private let config: Config
    /// The confirmed paths: the rows selected when there are several, else
    /// the field's text, trimmed. The panel is already hidden; `reopen()`
    /// brings it back with the text intact.
    private let onSubmit: (GuestPathPanel, [String]) -> Void
    private let panel: NSPanel
    private let field = NSTextField(frame: .zero)
    private let hint = NSTextField(labelWithString: "")
    private let table = ListTableView(frame: .zero)
    /// The rows: the candidates for the field's text, or a cycle's.
    private var listed: [String] = []
    /// Set while the list's selection is changed from here, so it is not
    /// taken for a click.
    private var selecting = false
    /// Dragged-out rows are copied one at a time: each is its own `cp-out`.
    private let promiseQueue: OperationQueue = {
        let q = OperationQueue()
        q.maxConcurrentOperationCount = 1
        return q
    }()

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
    /// The directory (ending in "/") the last completion wrote into the
    /// field, until the user edits it: a "/" typed right after it accepts
    /// it and goes in rather than doubling the slash. So a slash typed after
    /// a completed directory never doubles; that costs nothing, as "//"
    /// means the same as "/" on the guest.
    private var completedDir: String?
    /// The editing commands that accept the candidate a cycle shows.
    private static let acceptSelectors: Set<Selector> = [
        #selector(NSResponder.moveRight(_:)),
        #selector(NSResponder.moveToEndOfLine(_:)),
        #selector(NSResponder.moveToRightEndOfLine(_:)),
        #selector(NSResponder.moveToEndOfParagraph(_:)),
        #selector(NSResponder.moveToEndOfDocument(_:)),
    ]

    init(devvm: Devvm, machine: String, config: Config, onSubmit: @escaping (GuestPathPanel, [String]) -> Void) {
        self.devvm = devvm
        self.machine = machine
        self.config = config
        self.onSubmit = onSubmit
        panel = NSPanel(contentRect: NSRect(x: 0, y: 0, width: 440, height: 350),
                        styleMask: [.titled, .closable, .utilityWindow], backing: .buffered, defer: false)
        super.init()

        panel.title = config.title
        panel.level = .floating
        panel.isFloatingPanel = true
        panel.becomesKeyOnlyIfNeeded = false // typing must reach the field
        panel.hidesOnDeactivate = false
        panel.isReleasedWhenClosed = false
        panel.delegate = self

        let content = NSView(frame: panel.contentRect(forFrameRect: panel.frame))
        let label = NSTextField(labelWithString: config.label)
        label.frame = NSRect(x: 16, y: 318, width: 340, height: 18)
        label.lineBreakMode = .byTruncatingTail
        let up = NSButton(title: "Up", target: self, action: #selector(goUp))
        up.bezelStyle = .rounded
        up.controlSize = .small
        up.font = .systemFont(ofSize: NSFont.smallSystemFontSize)
        up.toolTip = "The folder above"
        up.frame = NSRect(x: 364, y: 314, width: 60, height: 24)
        field.frame = NSRect(x: 16, y: 288, width: 408, height: 24)
        field.placeholderString = config.placeholder
        field.delegate = self

        let column = NSTableColumn(identifier: NSUserInterfaceItemIdentifier("name"))
        column.resizingMask = .autoresizingMask
        table.addTableColumn(column)
        table.headerView = nil
        table.rowHeight = 20
        table.style = .plain
        table.allowsMultipleSelection = config.multiple
        table.allowsEmptySelection = true
        table.dataSource = self
        table.delegate = self
        table.target = self
        table.doubleAction = #selector(rowDoubleClicked)
        if config.multiple {
            table.setDraggingSourceOperationMask(.copy, forLocal: false)
        }
        let scroll = NSScrollView(frame: NSRect(x: 16, y: 62, width: 408, height: 218))
        scroll.documentView = table
        scroll.hasVerticalScroller = true
        scroll.autohidesScrollers = true
        scroll.borderType = .bezelBorder
        column.width = scroll.contentSize.width

        hint.frame = NSRect(x: 16, y: 40, width: 408, height: 16)
        hint.font = .systemFont(ofSize: 11)
        hint.textColor = .secondaryLabelColor
        hint.lineBreakMode = .byTruncatingTail
        hint.maximumNumberOfLines = 1

        let cancel = NSButton(title: "Cancel", target: self, action: #selector(cancel))
        cancel.bezelStyle = .rounded
        cancel.keyEquivalent = "\u{1b}"
        cancel.frame = NSRect(x: 254, y: 6, width: 80, height: 28)
        let copy = NSButton(title: config.button, target: self, action: #selector(submitTapped))
        copy.bezelStyle = .rounded
        copy.keyEquivalent = "\r"
        copy.frame = NSRect(x: 340, y: 6, width: 84, height: 28)

        let views: [NSView] = [label, up, field, scroll, hint, cancel, copy]
        views.forEach(content.addSubview)
        panel.contentView = content
        panel.initialFirstResponder = field
        panel.center()
    }

    /// Opens the panel, or brings it forward. `text` replaces what the field
    /// holds; without it the field keeps its last text.
    func show(text: String? = nil) {
        Log.menu.notice("path panel \"\(self.config.title, privacy: .public)\" opened")
        let opening = !panel.isVisible
        if let text = text, opening {
            field.stringValue = text
            completer = TabCompleter()
            completedDir = nil
        }
        NSApp.activate(ignoringOtherApps: true)
        panel.makeKeyAndOrderFront(nil)
        panel.makeFirstResponder(field)
        // List where the field is (the home directory, for an empty one)
        // without waiting for a key.
        if opening { lookup() }
    }

    @objc private func cancel() {
        Log.menu.notice("path panel \"\(self.config.title, privacy: .public)\" cancelled")
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
        completedDir = nil
        setHint("")
        show(list: [], selecting: nil)
    }

    // MARK: Completion

    func controlTextDidChange(_ obj: Notification) {
        guard !applying else { return }
        // A "/" typed after a directory a completion just wrote (the field
        // reads "src//"): accept that directory, drop the extra "/" and go
        // in, as a Tab on "src/" outside a cycle does. Without this, a
        // directory reached by cycling could only be entered by typing "/"
        // and deleting it again before the next Tab.
        //
        // Only for a real insertion: an undo of the collapse also reads
        // "src//", and collapsing it again would make the undo impossible
        // to get past (and edit the field from inside the undo manager).
        // The rewrite runs on the next turn of the run loop, after
        // insertText: has finished with the field editor (it may still set
        // the insertion point past the shortened text); the text is checked
        // again in case the user typed in between.
        let undo = field.currentEditor()?.undoManager
        let replaying = (undo?.isUndoing ?? false) || (undo?.isRedoing ?? false)
        if !replaying, let dir = completedDir, field.stringValue == dir + "/" {
            completedDir = nil
            completer.edited()
            pendingTab = false
            debounce?.invalidate()
            debounce = nil
            DispatchQueue.main.async { [weak self] in
                // Also not after the panel closed: tab() would start a lookup.
                guard let self = self, self.panel.isVisible,
                      self.field.stringValue == dir + "/" else { return }
                self.setText(dir)
                self.tab(backwards: false)
            }
            return
        }
        completedDir = nil
        // The user edited: any cycle ends, and a Tab still waiting for its
        // lookup is dropped (it was for text that no longer exists).
        completer.edited()
        pendingTab = false
        setHint("")
        // Narrow the list at once when the answer in hand covers the new
        // text; otherwise it keeps its rows until the lookup lands.
        if let narrowed = completer.answer?.candidates(for: field.stringValue) {
            show(list: narrowed, selecting: nil)
        }
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
                }
                if current == text {
                    self.setHint("Lookup failed — Tab to retry", detail: r.lastStderrLine)
                }
                return
            }
            var (found, directive) = GuestPathPanel.parseCompletions(r.stdout)
            if self.config.foldersOnly { found = found.filter { $0.hasSuffix("/") } }
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
                self.show(list: self.completer.answer?.candidates(for: current) ?? [], selecting: nil)
                self.setHint(self.listed.isEmpty ? "No matches" : "")
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
        // Down and Up walk the list, which is what a Tab cycle does.
        if commandSelector == #selector(NSResponder.moveDown(_:)) {
            tab(backwards: false)
            return true
        }
        if commandSelector == #selector(NSResponder.moveUp(_:)) {
            tab(backwards: true)
            return true
        }
        // Right arrow or Cmd-Right on a cycled candidate, caret at the end
        // (where the cycle put it), accepts it: the text stays, the cycle
        // ends, so the next Tab enters a directory instead of moving on.
        // The caret movement itself still happens.
        if GuestPathPanel.acceptSelectors.contains(commandSelector),
           let shown = completer.shown, shown == field.stringValue,
           textView.selectedRange().location == (shown as NSString).length {
            completer.edited()
            setHint("")
            // Fetch the directory's entries now, so the next Tab lists them
            // without waiting.
            if shown.hasSuffix("/") && inFlight != shown { lookup() }
        }
        return false
    }

    private func tab(backwards: Bool) {
        let text = field.stringValue
        switch completer.tab(text: text, backwards: backwards) {
        case .lookup:
            pendingTab = true
            pendingBackwards = backwards
            setHint("Looking up…")
            if inFlight != text { lookup() }
        case .noMatches:
            NSSound.beep()
            setHint("No matches")
            show(list: [], selecting: nil)
            // The next Tab asks the guest again rather than repeating this.
            completer.forget()
        case .unique(let path):
            setText(path)
            setHint("")
            show(list: [path], selecting: 0)
            // Fetch the directory's entries now, so the next Tab lists them
            // without waiting.
            if path.hasSuffix("/") && path != text { lookup() }
        case .extended(let prefix, let all):
            setText(prefix)
            setHint("")
            show(list: all, selecting: nil)
        case .cycled(let path, let index, let all):
            setText(path)
            setHint("\(index + 1) of \(all.count)")
            show(list: all, selecting: index)
        }
    }

    private func setHint(_ text: String, detail: String? = nil) {
        hint.stringValue = text
        hint.toolTip = detail
    }

    /// Replaces the field's text through the field editor, as a normal edit
    /// (so undo stays consistent) that is not treated as the user's, and
    /// puts the insertion point at the end.
    private func setText(_ s: String) {
        applying = true
        defer { applying = false }
        completedDir = s.hasSuffix("/") ? s : nil
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

    // MARK: List

    /// Shows `rows` with one of them highlighted, or none. Rows are only
    /// reloaded when they changed, so the scroll position survives a cycle.
    private func show(list rows: [String], selecting index: Int?) {
        selecting = true
        defer { selecting = false }
        if rows != listed {
            listed = rows
            table.reloadData()
        }
        if let i = index, i >= 0, i < listed.count {
            table.selectRowIndexes(IndexSet(integer: i), byExtendingSelection: false)
            table.scrollRowToVisible(i)
        } else {
            table.deselectAll(nil)
        }
    }

    func numberOfRows(in tableView: NSTableView) -> Int {
        return listed.count
    }

    func tableView(_ tableView: NSTableView, viewFor tableColumn: NSTableColumn?, row: Int) -> NSView? {
        guard row >= 0, row < listed.count else { return nil }
        let id = NSUserInterfaceItemIdentifier("entry")
        let cell: NSTableCellView
        if let reused = tableView.makeView(withIdentifier: id, owner: nil) as? NSTableCellView {
            cell = reused
        } else {
            cell = NSTableCellView(frame: NSRect(x: 0, y: 0, width: 380, height: 20))
            cell.identifier = id
            let icon = NSImageView(frame: NSRect(x: 2, y: 2, width: 16, height: 16))
            icon.imageScaling = .scaleProportionallyDown
            let name = NSTextField(labelWithString: "")
            name.frame = NSRect(x: 24, y: 2, width: 352, height: 16)
            name.autoresizingMask = [.width]
            name.lineBreakMode = .byTruncatingMiddle
            cell.addSubview(icon)
            cell.addSubview(name)
            cell.imageView = icon
            cell.textField = name
        }
        let path = listed[row]
        let isDir = path.hasSuffix("/")
        let leaf = TabCompleter.leafName(path)
        cell.textField?.stringValue = isDir ? String(leaf.dropLast()) : leaf
        cell.imageView?.image = NSImage(systemSymbolName: isDir ? "folder" : "doc", accessibilityDescription: nil)
        cell.imageView?.contentTintColor = isDir ? .controlAccentColor : .secondaryLabelColor
        return cell
    }

    /// A click. One row: the field takes its path, exactly as if a Tab
    /// cycle had reached it (so a folder is shown, not entered). Several:
    /// the field stays, and the selection is what Copy takes.
    func tableViewSelectionDidChange(_ notification: Notification) {
        guard !selecting else { return }
        let rows = table.selectedRowIndexes
        if rows.count == 1, let i = rows.first, i < listed.count {
            completer.select(i, in: listed)
            pendingTab = false
            debounce?.invalidate()
            debounce = nil
            setText(listed[i])
            setHint("\(i + 1) of \(listed.count)")
        } else if rows.count > 1 {
            setHint("\(rows.count) items selected")
        } else {
            setHint("")
        }
    }

    /// A double-click: into a folder, or straight on with a file.
    @objc private func rowDoubleClicked() {
        let row = table.clickedRow
        guard row >= 0, row < listed.count else { return }
        let path = listed[row]
        if path.hasSuffix("/") {
            enter(path)
        } else {
            setText(path)
            submitTapped()
        }
    }

    /// Makes `dir` (ending in "/", or "" for home) the directory the field
    /// is in, and lists it.
    private func enter(_ dir: String) {
        completer.edited()
        pendingTab = false
        setText(dir)
        setHint("")
        lookup()
    }

    @objc private func goUp() {
        // The directory being listed: the rows' own when there are rows (a
        // row shown in the field is in it, not below it), else the text's.
        let dir = listed.first.map { TabCompleter.directory(ofCandidate: $0) }
            ?? TabCompleter.directory(ofText: field.stringValue)
        guard let parent = TabCompleter.parent(of: dir) else {
            NSSound.beep()
            return
        }
        enter(parent)
    }

    // MARK: Drag out
    //
    // A dragged row is a file promise: nothing is copied until it is
    // dropped, and then one `cp-out` writes it where the drop landed (the
    // system chose a name that is free there, so nothing is overwritten).

    func tableView(_ tableView: NSTableView, pasteboardWriterForRow row: Int) -> NSPasteboardWriting? {
        guard config.multiple, row >= 0, row < listed.count else { return nil }
        let path = listed[row]
        let provider = NSFilePromiseProvider(fileType: path.hasSuffix("/") ? "public.folder" : "public.data",
                                             delegate: self)
        provider.userInfo = path
        return provider
    }

    func filePromiseProvider(_ filePromiseProvider: NSFilePromiseProvider, fileNameForType fileType: String) -> String {
        let path = filePromiseProvider.userInfo as? String ?? ""
        let leaf = TabCompleter.leafName(path)
        let name = leaf.hasSuffix("/") ? String(leaf.dropLast()) : leaf
        return name.isEmpty ? "untitled" : name
    }

    func operationQueue(for filePromiseProvider: NSFilePromiseProvider) -> OperationQueue {
        return promiseQueue
    }

    /// Runs on promiseQueue, one row at a time, and holds it until the copy
    /// is done: the next row's `cp-out` starts only then.
    func filePromiseProvider(_ filePromiseProvider: NSFilePromiseProvider, writePromiseTo url: URL,
                             completionHandler: @escaping (Error?) -> Void) {
        guard let path = filePromiseProvider.userInfo as? String else {
            completionHandler(CocoaError(.fileWriteUnknown))
            return
        }
        let machine = self.machine
        var args = ["cp-out"]
        if path.hasSuffix("/") { args.append("-r") }
        // `--` after NAME: a guest path starting with `-` is a path.
        args += [machine, "--", path, url.path]
        Log.drop.notice("drag out \(machine, privacy: .public):\(path, privacy: .public) -> \(url.path, privacy: .public)")
        let done = DispatchSemaphore(value: 0)
        var ok = false
        devvm.run(args) { r in
            ok = r.ok
            if r.ok {
                Notifications.shared.info("Copied \(path)", "from \(machine)")
            } else {
                Log.drop.error("drag out of \(path, privacy: .public) from \(machine, privacy: .public) failed: \(r.lastStderrLine, privacy: .public)")
                Notifications.shared.error("Copy from \(machine) failed", r.lastStderrLine)
            }
            done.signal()
        }
        done.wait()
        completionHandler(ok ? nil : CocoaError(.fileWriteUnknown))
    }

    // MARK: Submit

    @objc private func submitTapped() {
        let rows = table.selectedRowIndexes.filter { $0 < listed.count }
        let paths: [String]
        if config.multiple && rows.count > 1 {
            paths = rows.map { listed[$0] }
        } else {
            let path = field.stringValue.trimmingCharacters(in: .whitespaces)
            guard !path.isEmpty || config.allowsEmpty else { return }
            paths = [path]
        }
        // Hide the path window before the caller opens anything: it floats,
        // so a panel presented while it is up opens behind it.
        panel.orderOut(nil)
        stopCompletion()
        onSubmit(self, paths)
    }

    /// Back to the path window, text intact (a cancelled save panel).
    func reopen() {
        NSApp.activate(ignoringOtherApps: true)
        panel.makeKeyAndOrderFront(nil)
        panel.makeFirstResponder(field)
        lookup()
    }
}

/// The list never becomes first responder, so the keyboard stays in the
/// path field; clicks and drags still reach it.
private final class ListTableView: NSTableView {
    override var acceptsFirstResponder: Bool { return false }
}

/// "Copy out from NAME": the path panel, then a save panel (a file) or a
/// folder picker (a path ending in "/"). Cancelling that brings the path
/// back to be fixed; only a copy (or closing the path window) ends the flow.
enum CopyOut {
    static func panel(devvm: Devvm, machine: String) -> GuestPathPanel {
        let config = GuestPathPanel.Config(
            title: "Copy out from \(machine)",
            label: "Guest path (relative to home; Tab completes):",
            placeholder: "src/project/notes.txt or dir/",
            button: "Copy…",
            allowsEmpty: false,
            foldersOnly: false,
            multiple: true)
        return GuestPathPanel(devvm: devvm, machine: machine, config: config) { panel, paths in
            chooseDestination(devvm: devvm, machine: machine, paths: paths, panel: panel)
        }
    }

    private static func chooseDestination(devvm: Devvm, machine: String, paths: [String], panel: GuestPathPanel) {
        guard let path = paths.first else { return }
        // An accessory app is not frontmost by itself; without this the
        // save panel can open behind the app the user was just in.
        NSApp.activate(ignoringOtherApps: true)
        // `--` after NAME: a guest path starting with `-` must not be parsed
        // as a flag by the CLI.
        if paths.count > 1 || path.hasSuffix("/") {
            let what = paths.count > 1 ? "\(paths.count) items" : path
            let open = NSOpenPanel()
            open.title = "Copy \(machine):\(what) into…"
            open.prompt = "Copy here"
            open.canChooseFiles = false
            open.canChooseDirectories = true
            open.canCreateDirectories = true
            open.allowsMultipleSelection = false
            guard open.runModal() == .OK, let dir = open.url else {
                Log.menu.notice("copy out of \(what, privacy: .public): folder picker cancelled; back to the path")
                panel.reopen()
                return
            }
            // -r whenever a folder is among them; one invocation, so the
            // CLI's all-or-nothing overwrite check covers the whole batch.
            var args = ["cp-out"]
            if paths.contains(where: { $0.hasSuffix("/") }) { args.append("-r") }
            args += ["-t", dir.path, machine, "--"] + paths
            run(devvm, args, machine: machine, what: what)
        } else {
            let save = NSSavePanel()
            save.title = "Save \(machine):\(path) as…"
            save.nameFieldStringValue = (path as NSString).lastPathComponent
            save.canCreateDirectories = true
            guard save.runModal() == .OK, let url = save.url else {
                Log.menu.notice("copy out of \(path, privacy: .public): save panel cancelled; back to the path")
                panel.reopen()
                return
            }
            // The save panel already confirmed any overwrite, so -f is right here.
            run(devvm, ["cp-out", "-f", machine, "--", path, url.path], machine: machine, what: path)
        }
    }

    private static func run(_ devvm: Devvm, _ args: [String], machine: String, what: String) {
        Log.menu.notice("copy out \(machine, privacy: .public):\(what, privacy: .public)")
        devvm.run(args) { r in
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
