import AppKit
import ServiceManagement

/// The status item, its icon, the menu, and every action in it. Actions are
/// devvm invocations; the app decides nothing about machines itself.
final class MenuBar: NSObject, NSMenuDelegate {
    private let devvm: Devvm
    private let statusItem: NSStatusItem
    private let menu = NSMenu()
    private let dropView: DropTargetView
    var watcher: StatusWatcher?

    private var machines: [Machine] = []
    private var portsByMachine: [String: [Forward]] = [:]
    private var submenus: [String: NSMenu] = [:]

    /// AppKit tracks an open menu's item objects; tearing them down while the
    /// menu is displayed flickers, drops the highlight, and can crash. So a
    /// rebuild requested while open only refreshes existing rows in place and
    /// is replayed when the menu closes. Submenus are tracked the same way,
    /// so a closed submenu can still be refilled while the parent is open.
    private var openMenus = Set<ObjectIdentifier>()
    private var rebuildPending = false
    private var submenuRefillPending = Set<String>()
    private var headerItem: NSMenuItem?

    private let refresh = SingleFlight()
    private var portsLookups: [String: SingleFlight] = [:]
    private var copyOutPanels: [String: CopyOutPanel] = [:]

    private var cliVersion: String?
    private var updateAvailable: String? // latest tag when newer than the CLI
    private let defaults = UserDefaults.standard

    private enum Key {
        static let selected = "selectedMachine"
        static let lastUpdateCheck = "lastUpdateCheck"
        static func inbox(_ name: String) -> String { "inbox." + name }
    }

    init(devvm: Devvm) {
        self.devvm = devvm
        // Locals only until super.init(): Swift forbids reading self's
        // properties before then.
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        statusItem = item
        dropView = DropTargetView(frame: item.button?.bounds ?? .zero)
        super.init()

        statusItem.menu = menu
        menu.delegate = self
        if let button = statusItem.button {
            dropView.autoresizingMask = [.width, .height]
            button.addSubview(dropView)
            dropView.frame = button.bounds // the button may have been sized since init
        }
        dropView.canAccept = { [weak self] in self?.selectedMachine?.isLive ?? false }
        dropView.onDrop = { [weak self] urls in self?.copyIn(urls: urls) }
        rebuildMenu()
        updateIcon()
    }

    // MARK: Model

    var selectedName: String? {
        get { defaults.string(forKey: Key.selected) }
        set { defaults.set(newValue, forKey: Key.selected) }
    }

    var selectedMachine: Machine? {
        guard let name = selectedName else { return nil }
        return machines.first { $0.name == name }
    }

    func apply(_ machines: [Machine]) {
        self.machines = machines
        let names = Set(machines.map { $0.name })
        // A selected machine that left the registry clears the selection;
        // drops must never silently re-target another box.
        if let name = selectedName, !names.contains(name) {
            Log.status.notice("drop target \(name, privacy: .public) left the registry; selection cleared")
            selectedName = nil
        }
        // Auto-select only when nothing is chosen and exactly one machine is live.
        if selectedName == nil, machines.filter({ $0.isLive }).count == 1,
           let only = machines.first(where: { $0.isLive })?.name {
            Log.status.notice("auto-selected \(only, privacy: .public) as the drop target (only live machine)")
            selectedName = only
        }
        // Per-machine caches follow the registry.
        portsByMachine = portsByMachine.filter { names.contains($0.key) }
        portsLookups = portsLookups.filter { names.contains($0.key) }
        copyOutPanels = copyOutPanels.filter { names.contains($0.key) }
        updateIcon()
        rebuildMenu()
    }

    private func inbox(for name: String) -> String {
        return defaults.string(forKey: Key.inbox(name)) ?? "~"
    }

    // MARK: Icon

    private enum IconStyle { case filled, badged, outline }

    private func updateIcon() {
        let style: IconStyle
        if machines.contains(where: { $0.isReconnecting }) {
            style = .badged
        } else if let m = selectedMachine, m.isLive {
            style = .filled
        } else {
            style = .outline
        }
        statusItem.button?.image = MenuBar.icon(style)
        if let m = selectedMachine {
            statusItem.button?.toolTip = "devvm — drop target: \(m.name) (\(m.stateWords), \(m.forwardsWords))"
        } else {
            statusItem.button?.toolTip = "devvm — no drop target selected"
        }
    }

    private static var iconCache: [String: NSImage] = [:]

    /// Three static template forms; no animation, no text. Template images
    /// use alpha only, so the badge first knocks a hole in the box so the dot
    /// reads as a separate shape in the menu bar tint.
    private static func icon(_ style: IconStyle) -> NSImage {
        let key = "\(style)"
        if let cached = iconCache[key] { return cached }
        let size = NSSize(width: 18, height: 18)
        let name = style == .outline ? "shippingbox" : "shippingbox.fill"
        let symbol = NSImage(systemSymbolName: name, accessibilityDescription: "devvm")?
            .withSymbolConfiguration(NSImage.SymbolConfiguration(pointSize: 14, weight: .regular))
        let image = NSImage(size: size, flipped: false) { rect in
            if let symbol = symbol {
                let s = symbol.size
                let r = NSRect(x: (rect.width - s.width) / 2, y: (rect.height - s.height) / 2,
                               width: s.width, height: s.height)
                symbol.draw(in: r)
            }
            if style == .badged {
                let dot = NSRect(x: rect.maxX - 6.5, y: rect.maxY - 6.5, width: 5.5, height: 5.5)
                NSGraphicsContext.current?.compositingOperation = .destinationOut
                NSColor.black.setFill()
                NSBezierPath(ovalIn: dot.insetBy(dx: -1.5, dy: -1.5)).fill()
                NSGraphicsContext.current?.compositingOperation = .sourceOver
                NSBezierPath(ovalIn: dot).fill()
            }
            return true
        }
        image.isTemplate = true
        iconCache[key] = image
        return image
    }

    // MARK: Menu lifecycle

    private var mainMenuIsOpen: Bool { openMenus.contains(ObjectIdentifier(menu)) }

    func menuWillOpen(_ menu: NSMenu) {
        openMenus.insert(ObjectIdentifier(menu))
        guard menu === self.menu else { return }
        guard devvm.executable != nil else { return }
        // The watch stream only sees devvm-made changes; a VM stopped behind
        // devvm's back shows up here, on demand.
        Log.menu.notice("menu opened; refreshing status")
        refresh.run(devvm, ["status", "--plain"], terminatePrevious: true) { [weak self] r in
            guard let self = self, r.ok else { return }
            let machines = parseStatusBlock(r.stdout)
            Log.status.notice("menu-open refresh: \(StatusWatcher.summary(machines), privacy: .public)")
            self.apply(machines)
        }
        // Ports per live machine, for the "Open localhost:PORT" entries.
        for m in machines where m.isLive && m.forwardCount > 0 {
            let lookup = portsLookups[m.name] ?? SingleFlight()
            portsLookups[m.name] = lookup
            lookup.run(devvm, ["ports", "list", m.name]) { [weak self] r in
                guard let self = self, r.ok else { return }
                let forwards = parsePortsList(r.stdout)
                let summary = forwards.map { "\($0.host)->\($0.guest)\($0.pending ? "(pending)" : "")" }.joined(separator: " ")
                Log.status.notice("ports for \(m.name, privacy: .public): \(summary, privacy: .public)")
                self.portsByMachine[m.name] = forwards
                self.refillSubmenu(for: m.name)
            }
        }
        devvm.version { [weak self] v in
            guard let self = self, let v = v else { return }
            if let old = self.cliVersion, old != v {
                // The binary changed underneath the watch child; it keeps
                // running the old code until restarted.
                Log.status.notice("devvm changed \(old, privacy: .public) -> \(v, privacy: .public); restarting the watch child")
                self.watcher?.restart()
            }
            self.cliVersion = v
            self.rebuildMenu()
        }
        maybeCheckForUpdates()
    }

    func menuDidClose(_ menu: NSMenu) {
        openMenus.remove(ObjectIdentifier(menu))
        if menu === self.menu {
            if rebuildPending {
                rebuildPending = false
                rebuildMenu()
            }
            return
        }
        // A submenu closed; replay a refill that was deferred while it was open.
        if submenuRefillPending.remove(menu.title) != nil {
            refillSubmenu(for: menu.title)
        }
    }

    private func item(_ title: String, _ action: Selector?, _ represented: Any? = nil) -> NSMenuItem {
        let it = NSMenuItem(title: title, action: action, keyEquivalent: "")
        it.target = self
        it.representedObject = represented
        return it
    }

    private func rebuildMenu() {
        if mainMenuIsOpen {
            rebuildPending = true
            refreshRowsInPlace()
            return
        }
        menu.removeAllItems()
        submenus = [:]
        headerItem = nil
        guard devvm.executable != nil else {
            menu.addItem(item("devvm not found on PATH", nil))
            menu.addItem(item("Install it, then relaunch DevVM", nil))
            menu.addItem(.separator())
            menu.addItem(item("Quit", #selector(quit)))
            return
        }

        if let mismatch = appVersionMismatch() {
            menu.addItem(item("Reinstall menu bar app (\(mismatch))", #selector(reinstallApp)))
            menu.addItem(.separator())
        }
        if let latest = updateAvailable {
            menu.addItem(item("Update devvm to \(latest)…", #selector(updateNow)))
            menu.addItem(.separator())
        }

        let header = item(headerTitle(), nil)
        headerItem = header
        menu.addItem(header)
        menu.addItem(.separator())

        if machines.isEmpty {
            menu.addItem(item("No machines registered", nil))
        }
        for m in machines {
            // No action on the row itself: AppKit routes a click on an item
            // with a submenu to opening it, never to the action. Selection
            // lives inside the submenu ("Use as drop target").
            let it = item(m.rowTitle, nil, m.name)
            it.state = (m.name == selectedName) ? .on : .off
            let sub = NSMenu(title: m.name)
            sub.delegate = self // so open/close of the submenu is tracked
            fillSubmenu(sub, for: m)
            submenus[m.name] = sub
            it.submenu = sub
            menu.addItem(it)
        }

        menu.addItem(.separator())
        menu.addItem(item("Check for updates…", #selector(checkForUpdates)))
        let login = item("Launch at login", #selector(toggleLaunchAtLogin))
        login.state = SMAppService.mainApp.status == .enabled ? .on : .off
        menu.addItem(login)
        // A nil action leaves the version line disabled (autoenablesItems).
        menu.addItem(item(cliVersion.map { "devvm \($0) · app \(MenuBar.appVersion)" } ?? "app \(MenuBar.appVersion)", nil))
        menu.addItem(.separator())
        menu.addItem(item("Quit", #selector(quit)))
    }

    private func headerTitle() -> String {
        if let m = selectedMachine {
            return "Drop target: \(m.name)  (inbox \(inbox(for: m.name)))"
        }
        return "No drop target — pick a running machine"
    }

    /// The in-place half of a rebuild: titles, checkmarks, and the header of
    /// rows that already exist. Rows for machines that appeared or vanished
    /// wait for the deferred full rebuild.
    private func refreshRowsInPlace() {
        headerItem?.title = headerTitle()
        for it in menu.items {
            guard let name = it.representedObject as? String, it.submenu != nil,
                  let m = machines.first(where: { $0.name == name }) else { continue }
            it.title = m.rowTitle
            it.state = (m.name == selectedName) ? .on : .off
        }
    }

    /// Refills a machine's submenu unless that submenu is the one being
    /// displayed, in which case the refill waits for it to close.
    private func refillSubmenu(for name: String) {
        guard let sub = submenus[name], let m = machines.first(where: { $0.name == name }) else { return }
        if openMenus.contains(ObjectIdentifier(sub)) {
            submenuRefillPending.insert(name)
            return
        }
        fillSubmenu(sub, for: m)
    }

    private func fillSubmenu(_ sub: NSMenu, for m: Machine) {
        sub.removeAllItems()
        if m.isLive {
            sub.addItem(item("Use as drop target", #selector(selectMachine(_:)), m.name))
            sub.addItem(.separator())
        }
        switch m.state {
        case "running":
            sub.addItem(item("Stop", #selector(stopMachine(_:)), m.name))
        case "stopped":
            sub.addItem(item("Start", #selector(startMachine(_:)), m.name))
        default:
            break
        }
        if m.isLive {
            if m.forwards != "-" {
                sub.addItem(item("Ports up", #selector(portsUp(_:)), m.name))
                sub.addItem(item("Ports down", #selector(portsDown(_:)), m.name))
            }
            for f in portsByMachine[m.name] ?? [] {
                let title = f.pending ? "localhost:\(f.host) → guest \(f.guest) (pending)"
                                      : "Open localhost:\(f.host) → guest \(f.guest)"
                let it = item(title, f.pending ? nil : #selector(openPort(_:)), f.host)
                sub.addItem(it)
            }
            sub.addItem(.separator())
            sub.addItem(item("Copy in…", #selector(copyInPanel(_:)), m.name))
            sub.addItem(item("Copy out…", #selector(copyOut(_:)), m.name))
            sub.addItem(item("Set inbox folder… (\(inbox(for: m.name)))", #selector(setInbox(_:)), m.name))
        }
        if sub.items.isEmpty {
            sub.addItem(item(m.state == "dormant" ? "Dormant: provision it from the terminal" : "Nothing to do here", nil))
        }
    }

    // MARK: Actions

    @objc private func quit() {
        Log.menu.notice("Quit chosen")
        NSApp.terminate(nil)
    }

    @objc private func selectMachine(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String,
              let m = machines.first(where: { $0.name == name }) else { return }
        guard m.isLive else {
            Log.menu.notice("select \(name, privacy: .public) refused: not live (\(m.state, privacy: .public))")
            Notifications.shared.info(name, "Not running; start it first.")
            return
        }
        Log.menu.notice("drop target set to \(name, privacy: .public)")
        selectedName = name
        updateIcon()
        rebuildMenu()
    }

    @objc private func startMachine(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        Log.menu.notice("Start chosen for \(name, privacy: .public)")
        runReporting(["start", name], title: "Started \(name)")
    }

    @objc private func stopMachine(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        if let m = machines.first(where: { $0.name == name }), m.forwardsUp {
            NSApp.activate(ignoringOtherApps: true)
            let alert = NSAlert()
            alert.messageText = "Stop \(name)?"
            alert.informativeText = "\(m.forwardsWords). Stopping drops them."
            alert.addButton(withTitle: "Stop")
            alert.addButton(withTitle: "Cancel")
            guard alert.runModal() == .alertFirstButtonReturn else {
                Log.menu.notice("Stop of \(name, privacy: .public) cancelled at the forwards prompt")
                return
            }
        }
        Log.menu.notice("Stop chosen for \(name, privacy: .public)")
        runReporting(["stop", name], title: "Stopped \(name)")
    }

    @objc private func portsUp(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        Log.menu.notice("Ports up chosen for \(name, privacy: .public)")
        runReporting(["ports", "up", name], title: "\(name): forwards up")
    }

    @objc private func portsDown(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        Log.menu.notice("Ports down chosen for \(name, privacy: .public)")
        runReporting(["ports", "down", name], title: "\(name): forwards down")
    }

    @objc private func openPort(_ sender: NSMenuItem) {
        guard let port = sender.representedObject as? Int,
              let url = URL(string: "http://localhost:\(port)/") else { return }
        Log.menu.notice("opening \(url.absoluteString, privacy: .public)")
        NSWorkspace.shared.open(url)
    }

    private func runReporting(_ args: [String], title: String) {
        let argv = args.joined(separator: " ")
        devvm.run(args) { r in
            if r.ok {
                Log.menu.notice("devvm \(argv, privacy: .public) succeeded")
                Notifications.shared.info(title, r.stdout.trimmingCharacters(in: .whitespacesAndNewlines))
            } else {
                Log.menu.error("devvm \(argv, privacy: .public) failed: \(r.lastStderrLine, privacy: .public)")
                Notifications.shared.error("devvm \(argv) failed", r.lastStderrLine)
            }
        }
    }

    // MARK: Copy in

    private func copyIn(urls: [URL]) {
        guard let m = selectedMachine else {
            Log.drop.error("dropped \(urls.count, privacy: .public) file(s) but no drop target is selected; ignored")
            return
        }
        copyIn(urls: urls, into: m.name)
    }

    @objc private func copyInPanel(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        Log.menu.notice("Copy in… chosen for \(name, privacy: .public)")
        NSApp.activate(ignoringOtherApps: true)
        let panel = NSOpenPanel()
        panel.title = "Copy into \(name)"
        panel.prompt = "Copy"
        panel.canChooseFiles = true
        panel.canChooseDirectories = true
        panel.allowsMultipleSelection = true
        panel.begin { [weak self] response in
            guard response == .OK else {
                Log.menu.notice("Copy in… cancelled")
                return
            }
            self?.copyIn(urls: panel.urls, into: name)
        }
    }

    /// One `cp-in -t INBOX` per drop: one invocation, one result, and the
    /// CLI's all-or-nothing overwrite check applies to the whole batch.
    private func copyIn(urls: [URL], into name: String) {
        let paths = urls.map { $0.path }
        guard !paths.isEmpty else { return }
        var args = ["cp-in"]
        let anyDir = urls.contains { url in
            (try? url.resourceValues(forKeys: [.isDirectoryKey]))?.isDirectory == true
        }
        if anyDir { args.append("-r") }
        let dest = inbox(for: name)
        // `--` after NAME: a dropped path starting with `-` is a path, not a flag.
        args += ["-t", dest, name, "--"] + paths
        let what = paths.count == 1 ? (urls[0].lastPathComponent) : "\(paths.count) items"
        Log.drop.notice("copy in: \(paths.count, privacy: .public) item(s)\(anyDir ? " (with -r)" : "", privacy: .public) -> \(name, privacy: .public):\(dest, privacy: .public)")
        devvm.run(args) { r in
            if r.ok {
                Log.drop.notice("copied \(what, privacy: .public) -> \(name, privacy: .public):\(dest, privacy: .public)")
                Notifications.shared.info("Copied \(what)", "→ \(name):\(dest)")
            } else if r.isOverwriteRefusal {
                Log.drop.notice("copy of \(what, privacy: .public) refused: \(r.lastStderrLine, privacy: .public)")
                Notifications.shared.conflict("\(name): already exists", r.lastStderrLine, retryWith: args)
            } else {
                Log.drop.error("copy of \(what, privacy: .public) to \(name, privacy: .public) failed: \(r.lastStderrLine, privacy: .public)")
                Notifications.shared.error("Copy to \(name) failed", r.lastStderrLine)
            }
        }
    }

    @objc private func setInbox(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        NSApp.activate(ignoringOtherApps: true)
        let alert = NSAlert()
        alert.messageText = "Inbox folder on \(name)"
        alert.informativeText = "Dropped files land here. A guest path, relative to the home directory unless absolute; created if missing."
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 260, height: 24))
        field.stringValue = inbox(for: name)
        alert.accessoryView = field
        alert.addButton(withTitle: "Save")
        alert.addButton(withTitle: "Cancel")
        alert.window.initialFirstResponder = field
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        let value = field.stringValue.trimmingCharacters(in: .whitespaces)
        let inbox = value.isEmpty ? "~" : value
        Log.menu.notice("inbox for \(name, privacy: .public) set to \(inbox, privacy: .public)")
        defaults.set(inbox, forKey: Key.inbox(name))
        rebuildMenu()
        updateIcon()
    }

    // MARK: Copy out

    @objc private func copyOut(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        Log.menu.notice("Copy out… chosen for \(name, privacy: .public)")
        let panel = copyOutPanels[name] ?? CopyOutPanel(devvm: devvm, machine: name)
        copyOutPanels[name] = panel
        panel.show()
    }

    // MARK: Updates

    static var appVersion: String {
        return (Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String) ?? "0.0.0"
    }

    /// The version stamped in the bundle currently on disk, read fresh
    /// (Bundle.main caches the plist from launch), so a bundle that devvm
    /// replaced underneath this process can be told apart from the one it
    /// started from.
    private static var installedBundleVersion: String? {
        let plist = (Bundle.main.bundlePath as NSString).appendingPathComponent("Contents/Info.plist")
        return NSDictionary(contentsOfFile: plist)?["CFBundleShortVersionString"] as? String
    }

    /// The app is built per release and installed by `devvm menubar` at the
    /// CLI's version; a mismatch means one of them moved. This is distinct
    /// from a newer *release* existing (updateAvailable).
    private func appVersionMismatch() -> String? {
        guard let cli = cliVersion else { return nil }
        let a = MenuBar.appVersion.hasPrefix("v") ? String(MenuBar.appVersion.dropFirst()) : MenuBar.appVersion
        let c = cli.hasPrefix("v") ? String(cli.dropFirst()) : cli
        if a == c || c == "dev" || c.contains("-") { return nil } // dev/dirty CLI builds never match
        return "app \(a), devvm \(c)"
    }

    @objc private func reinstallApp() {
        Log.update.notice("Reinstall menu bar app chosen (app \(MenuBar.appVersion, privacy: .public), devvm \(self.cliVersion ?? "?", privacy: .public))")
        devvm.run(["menubar"]) { r in
            if !r.ok {
                Log.update.error("devvm menubar failed: \(r.lastStderrLine, privacy: .public)")
                Notifications.shared.error("devvm menubar failed", r.lastStderrLine)
            } else {
                Log.update.notice("devvm menubar succeeded; expecting to be relaunched by devvm")
            }
            // On success devvm relaunches the new bundle; this process is
            // simply superseded.
        }
    }

    /// Passive check at most every six hours, and only when the menu is
    /// opened: never a timer, never network in the background. The window
    /// is stamped before the check, so an offline failure also waits it out.
    private func maybeCheckForUpdates() {
        let last = defaults.object(forKey: Key.lastUpdateCheck) as? Date ?? .distantPast
        guard Date().timeIntervalSince(last) > 6 * 3600 else {
            Log.update.debug("passive update check skipped; last was \(Int(Date().timeIntervalSince(last) / 60), privacy: .public) min ago")
            return
        }
        Log.update.notice("passive update check (6h window elapsed)")
        defaults.set(Date(), forKey: Key.lastUpdateCheck)
        devvm.run(["update", "--check", "--plain"]) { [weak self] r in
            guard let self = self, r.ok else { return }
            let f = r.stdout.trimmingCharacters(in: .whitespacesAndNewlines).split(separator: "\t").map { String($0) }
            Log.update.notice("update check: \(r.stdout.trimmingCharacters(in: .whitespacesAndNewlines), privacy: .public)")
            self.updateAvailable = (f.count >= 3 && f[2] == "true") ? f[1] : nil
            self.rebuildMenu()
        }
    }

    @objc private func checkForUpdates() {
        Log.update.notice("Check for updates chosen")
        devvm.run(["update", "--check", "--plain"]) { [weak self] r in
            guard let self = self else { return }
            self.defaults.set(Date(), forKey: Key.lastUpdateCheck)
            guard r.ok else {
                Log.update.error("update check failed: \(r.lastStderrLine, privacy: .public)")
                Notifications.shared.error("Update check failed", r.lastStderrLine)
                return
            }
            let f = r.stdout.trimmingCharacters(in: .whitespacesAndNewlines).split(separator: "\t").map { String($0) }
            Log.update.notice("update check: \(r.stdout.trimmingCharacters(in: .whitespacesAndNewlines), privacy: .public)")
            guard f.count >= 3 else {
                Log.update.error("update check output not understood")
                return
            }
            self.updateAvailable = f[2] == "true" ? f[1] : nil
            self.rebuildMenu()
            NSApp.activate(ignoringOtherApps: true)
            let alert = NSAlert()
            if f[2] == "true" {
                alert.messageText = "devvm \(f[1]) is available"
                alert.informativeText = "Installed: \(f[0]). Updating replaces the CLI, restarts its forward daemons, and relaunches this app."
                alert.addButton(withTitle: "Update now")
                alert.addButton(withTitle: "Later")
                if alert.runModal() == .alertFirstButtonReturn { self.updateNow() }
            } else {
                alert.messageText = "devvm is up to date"
                alert.informativeText = "Installed: \(f[0]). Latest: \(f[1])."
                alert.addButton(withTitle: "OK")
                alert.runModal()
            }
        }
    }

    @objc private func updateNow() {
        Log.update.notice("Update now chosen; running devvm update")
        Notifications.shared.info("Updating devvm…", "The app relaunches when it finishes.")
        devvm.run(["update"]) { r in
            guard r.ok else {
                Log.update.error("devvm update failed: \(r.lastStderrLine, privacy: .public)")
                Notifications.shared.error("devvm update failed", r.lastStderrLine)
                return
            }
            Log.update.notice("devvm update succeeded")
            MenuBar.relaunch()
        }
    }

    /// After `devvm update`: if devvm already replaced and relaunched this
    /// bundle (the version on disk no longer matches the one this process
    /// started from), the new instance is up and this one just exits.
    /// Otherwise start a fresh instance of the bundle and exit only once
    /// that launch is confirmed, so a failed launch never leaves no app.
    static func relaunch() {
        if let onDisk = installedBundleVersion, onDisk != appVersion {
            Log.update.notice("bundle on disk is \(onDisk, privacy: .public), this process is \(appVersion, privacy: .public): devvm already relaunched the app; exiting")
            NSApp.terminate(nil)
            return
        }
        Log.update.notice("relaunching \(Bundle.main.bundlePath, privacy: .public) via NSWorkspace")
        let config = NSWorkspace.OpenConfiguration()
        config.createsNewApplicationInstance = true
        let url = URL(fileURLWithPath: Bundle.main.bundlePath)
        NSWorkspace.shared.openApplication(at: url, configuration: config) { _, error in
            DispatchQueue.main.async {
                if let error = error {
                    Log.update.error("relaunch failed: \(error.localizedDescription, privacy: .public)")
                    Notifications.shared.error("Relaunch failed", error.localizedDescription)
                } else {
                    Log.update.notice("new instance launched; exiting this one")
                    NSApp.terminate(nil)
                }
            }
        }
    }

    // MARK: Launch at login

    @objc private func toggleLaunchAtLogin() {
        let service = SMAppService.mainApp
        do {
            if service.status == .enabled {
                try service.unregister()
                Log.menu.notice("launch at login disabled")
            } else {
                try service.register()
                Log.menu.notice("launch at login enabled (status now \(service.status.rawValue, privacy: .public))")
            }
        } catch {
            Log.menu.error("launch at login change failed: \(error.localizedDescription, privacy: .public)")
            NSApp.activate(ignoringOtherApps: true)
            let alert = NSAlert()
            alert.messageText = "Could not change launch at login"
            alert.informativeText = "\(error.localizedDescription)\n\nAs a fallback, add DevVM.app under System Settings › General › Login Items."
            alert.runModal()
        }
        rebuildMenu()
    }
}
