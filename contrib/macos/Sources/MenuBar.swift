import AppKit
import ServiceManagement

/// The status item, its icon, the menu, and every action in it. Actions are
/// devvm invocations; the app decides nothing about machines itself.
final class MenuBar: NSObject, NSMenuDelegate {
    private let devvm: Devvm
    private let statusItem: NSStatusItem
    private let menu = NSMenu()
    private let shelf = DropShelf()
    var watcher: StatusWatcher?

    private var machines: [Machine] = []
    private var portsByMachine: [String: [Forward]] = [:]
    private var submenus: [String: NSMenu] = [:]
    /// The rows to retitle in place while the menu is open: machines by
    /// name, and each hub's section header by the hub's name.
    private var rowItems: [String: NSMenuItem] = [:]
    private var hubHeaderItems: [String: NSMenuItem] = [:]

    /// AppKit tracks an open menu's item objects; tearing them down while the
    /// menu is displayed flickers, drops the highlight, and can crash. So a
    /// rebuild requested while open only refreshes existing rows in place and
    /// is replayed when the menu closes. Submenus are tracked the same way,
    /// so a closed submenu can still be refilled while the parent is open.
    private var openMenus = Set<ObjectIdentifier>()
    private var rebuildPending = false
    private var submenuRefillPending = Set<String>()
    private var shelfItem: NSMenuItem?

    private let refresh = SingleFlight()
    private var lastRefresh = Date.distantPast
    private var portsLookups: [String: SingleFlight] = [:]
    private var copyOutPanels: [String: GuestPathPanel] = [:]
    private var inboxPanels: [String: GuestPathPanel] = [:]

    private var cliVersion: String?
    private var updateAvailable: String? // latest tag when newer than the CLI
    private let defaults = UserDefaults.standard

    private enum Key {
        static let lastUpdateCheck = "lastUpdateCheck"
        static func inbox(_ name: String) -> String { "inbox." + name }
    }

    init(devvm: Devvm) {
        self.devvm = devvm
        // Locals only until super.init(): Swift forbids reading self's
        // properties before then.
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        statusItem = item
        super.init()

        statusItem.menu = menu
        menu.delegate = self
        shelf.inbox = { [weak self] name in self?.inbox(for: name) ?? "~" }
        shelf.onDrop = { [weak self] name, urls, done in
            self?.copyIn(urls: urls, into: name, completion: done)
        }
        shelf.onClick = { [weak self] name in self?.showCopyOut(for: name) }
        shelf.contextMenu = { [weak self] name in self?.tileMenu(for: name) }
        // The shelf can sit open for days while the menu stays shut, so it
        // asks for the refresh a menu open gives; at most one per half
        // minute (each is a probe of every remote box and one provider call).
        shelf.onAttention = { [weak self] in self?.refreshStatus(why: "drop shelf", atMostEvery: 30) }
        rebuildMenu()
        updateIcon()
        if devvm.executable != nil && shelf.wasVisible { shelf.show() }
    }

    // MARK: Model

    func apply(_ machines: [Machine]) {
        self.machines = machines
        let names = Set(machines.map { $0.name })
        // Per-machine caches follow the registry.
        portsByMachine = portsByMachine.filter { names.contains($0.key) }
        portsLookups = portsLookups.filter { names.contains($0.key) }
        copyOutPanels = copyOutPanels.filter { names.contains($0.key) }
        inboxPanels = inboxPanels.filter { names.contains($0.key) }
        // The shelf's tiles follow the same snapshots as the menu.
        shelf.update(machines)
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
        let live = machines.filter { $0.isLive }
        let reconnecting = machines.contains(where: { $0.isReconnecting })
        if reconnecting {
            style = .badged
        } else if !live.isEmpty {
            style = .filled
        } else {
            style = .outline
        }
        statusItem.button?.image = MenuBar.icon(style)
        var tip = live.isEmpty ? "devvm — no running machines"
                               : "devvm — running: " + live.map { $0.name }.joined(separator: ", ")
        if reconnecting { tip += " (forwards reconnecting)" }
        statusItem.button?.toolTip = tip
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
        // The shelf's close button changes its state without telling us.
        shelfItem?.title = shelfToggleTitle
        guard devvm.executable != nil else { return }
        refreshStatus(why: "menu opened")
        // Ports per live machine, for the "Open localhost:PORT" entries.
        for m in machines { lookupPorts(for: m) }
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

    /// One `status --plain`, applied when it answers. The watch stream only
    /// sees devvm-made changes; a VM stopped behind devvm's back shows up
    /// here, on demand (a menu open, the shelf being looked at), never on a
    /// timer. The one-shot also probes the remote boxes and asks the
    /// providers, and leaves the answers in the caches the watch child
    /// reads. `atMostEvery` skips a refresh that follows another too soon.
    private func refreshStatus(why: String, atMostEvery interval: TimeInterval = 0) {
        guard devvm.executable != nil, Date().timeIntervalSince(lastRefresh) >= interval else { return }
        lastRefresh = Date()
        Log.status.notice("refreshing status (\(why, privacy: .public))")
        refresh.run(devvm, ["status", "--plain"], terminatePrevious: true) { [weak self] r in
            guard let self = self, r.ok else { return }
            let machines = parseStatusBlock(r.stdout)
            Log.status.notice("refresh (\(why, privacy: .public)): \(StatusWatcher.summary(machines), privacy: .public)")
            self.apply(machines)
        }
    }

    /// `ports list` for a live machine with forwards, into its submenu.
    private func lookupPorts(for m: Machine) {
        guard m.isLive && m.forwardCount > 0 else { return }
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
        rowItems = [:]
        hubHeaderItems = [:]
        shelfItem = nil
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

        let shelfToggle = item(shelfToggleTitle, #selector(toggleShelf))
        shelfItem = shelfToggle
        menu.addItem(shelfToggle)
        menu.addItem(.separator())

        let broken = machines.contains { $0.isBrokenConfig }
        if broken {
            menu.addItem(item("devvm.toml does not parse — open it…", #selector(openConfig)))
        }
        // One section per kind on this host (Local, Remote, a provider's),
        // then one per hub: the hub is a host, not a machine (nothing to
        // start, stop or drop on), so it is the header its machines sit
        // under. A lone kind needs no header.
        let sections = machineSections(machines)
        if sections.isEmpty && !broken {
            menu.addItem(item("No machines", nil))
        }
        let headed = sections.count > 1 || sections.first?.hubName != nil
        for section in sections {
            if headed {
                let header = NSMenuItem.sectionHeader(title: section.title)
                if let hub = section.hubName { hubHeaderItems[hub] = header }
                menu.addItem(header)
            }
            for m in section.members {
                menu.addItem(machineRow(m))
            }
            if section.members.isEmpty {
                menu.addItem(item("No machines", nil))
            }
        }

        menu.addItem(.separator())
        // The app's own housekeeping, out of the way of the machines. An
        // item with a submenu is enabled without an action.
        let app = item("DevVM", nil)
        let appMenu = NSMenu(title: "DevVM")
        let login = item("Launch at login", #selector(toggleLaunchAtLogin))
        login.state = SMAppService.mainApp.status == .enabled ? .on : .off
        appMenu.addItem(login)
        appMenu.addItem(item("Check for updates…", #selector(checkForUpdates)))
        appMenu.addItem(.separator())
        // A nil action leaves the version line disabled (autoenablesItems).
        appMenu.addItem(item(cliVersion.map { "devvm \($0) · app \(MenuBar.appVersion)" } ?? "app \(MenuBar.appVersion)", nil))
        app.submenu = appMenu
        menu.addItem(app)
        menu.addItem(item("Quit", #selector(quit)))
    }

    /// One machine's row with its submenu. No action on the row itself:
    /// AppKit routes a click on an item with a submenu to opening it, never
    /// to the action.
    private func machineRow(_ m: Machine) -> NSMenuItem {
        let it = item(m.rowTitle, nil)
        it.badge = m.rowDetail.map { NSMenuItemBadge(string: $0) }
        let sub = NSMenu(title: m.name)
        sub.delegate = self // so open/close of the submenu is tracked
        fillSubmenu(sub, for: m)
        submenus[m.name] = sub
        rowItems[m.name] = it
        it.submenu = sub
        return it
    }

    private var shelfToggleTitle: String {
        return shelf.isVisible ? "Hide Drop Shelf" : "Show Drop Shelf"
    }

    /// The in-place half of a rebuild: titles and badges of rows that
    /// already exist. Rows for machines that appeared or vanished wait for
    /// the deferred full rebuild.
    private func refreshRowsInPlace() {
        shelfItem?.title = shelfToggleTitle
        for m in machines {
            if m.isHub {
                hubHeaderItems[m.name]?.title = m.hubHeaderTitle
            } else if let it = rowItems[m.name] {
                it.title = m.rowTitle
                it.badge = m.rowDetail.map { NSMenuItemBadge(string: $0) }
            }
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

    /// A machine's actions: its submenu in the menu, and its tile's
    /// right-click menu on the shelf. Start and Stop follow the state alone
    /// (a remote box is never `running` or `stopped`; a provider's server
    /// is). Everything else needs a live machine, hub machines included:
    /// every devvm command takes `HUB/NAME` as a name, and its forwards are
    /// this Mac's own.
    private func fillSubmenu(_ sub: NSMenu, for m: Machine) {
        sub.removeAllItems()
        switch m.state {
        case "running":
            sub.addItem(item("Stop", #selector(stopMachine(_:)), m.name))
        case "stopped":
            sub.addItem(item("Start", #selector(startMachine(_:)), m.name))
        default:
            break
        }
        if m.isLive {
            // One ports item, the one that changes something: saved forwards
            // that are down go up, ones that are up go down, and a daemon
            // reconnecting can be nudged (`ports up` is its kick). Nothing
            // for a machine with no saved forwards (`-`).
            if m.isReconnecting {
                sub.addItem(item("Reconnect now", #selector(portsUp(_:)), m.name))
                sub.addItem(item("Ports down", #selector(portsDown(_:)), m.name))
            } else if m.forwardsUp {
                sub.addItem(item("Ports down", #selector(portsDown(_:)), m.name))
            } else if m.forwards == "down" {
                sub.addItem(item("Ports up", #selector(portsUp(_:)), m.name))
            }
            for f in portsByMachine[m.name] ?? [] {
                let title = f.pending ? "localhost:\(f.host) → guest \(f.guest) (pending)"
                                      : "Open localhost:\(f.host) → guest \(f.guest)"
                let it = item(title, f.pending ? nil : #selector(openPort(_:)), f.host)
                sub.addItem(it)
            }
            if !sub.items.isEmpty { sub.addItem(.separator()) }
            sub.addItem(item("Copy in…", #selector(copyInPanel(_:)), m.name))
            sub.addItem(item("Copy out…", #selector(copyOut(_:)), m.name))
            sub.addItem(item("Inbox folder… (\(inbox(for: m.name)))", #selector(setInbox(_:)), m.name))
        }
        if sub.items.isEmpty {
            let note: String
            if m.isUnreachable {
                note = m.hub == nil ? "Unreachable" : "Unreachable: its hub did not answer"
            } else {
                note = "Nothing to do here"
            }
            sub.addItem(item(note, nil))
        }
    }

    /// The same actions for a tile on the shelf. Built on demand and not
    /// tracked: it shows the ports last looked up, and asks again so the
    /// next one is current.
    private func tileMenu(for name: String) -> NSMenu? {
        guard let m = machines.first(where: { $0.name == name }) else { return nil }
        let menu = NSMenu(title: name)
        fillSubmenu(menu, for: m)
        lookupPorts(for: m)
        return menu
    }

    // MARK: Actions

    @objc private func quit() {
        Log.menu.notice("Quit chosen")
        NSApp.terminate(nil)
    }

    // MARK: Drop shelf

    @objc private func toggleShelf() {
        if shelf.isVisible {
            Log.menu.notice("Hide Drop Shelf chosen")
            shelf.hide()
        } else {
            Log.menu.notice("Show Drop Shelf chosen")
            shelf.show()
        }
    }

    @objc private func startMachine(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        Log.menu.notice("Start chosen for \(name, privacy: .public)")
        runReporting(["start", name], title: "Started \(name)")
    }

    @objc private func stopMachine(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        // Asked first only when stopping costs something that is not
        // obvious: forwards in use, or a bill that carries on.
        var notes: [String] = []
        if let m = machines.first(where: { $0.name == name }) {
            if m.forwardsUp { notes.append("\(m.forwardsWords). Stopping drops them.") }
            if m.isProvider { notes.append("A stopped server is still billed; only deleting it ends that.") }
        }
        if !notes.isEmpty {
            NSApp.activate(ignoringOtherApps: true)
            let alert = NSAlert()
            alert.messageText = "Stop \(name)?"
            alert.informativeText = notes.joined(separator: "\n\n")
            alert.addButton(withTitle: "Stop")
            alert.addButton(withTitle: "Cancel")
            guard alert.runModal() == .alertFirstButtonReturn else {
                Log.menu.notice("Stop of \(name, privacy: .public) cancelled at the prompt")
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
    /// `completion` (the shelf tile's progress) gets the result after the
    /// notification is posted.
    private func copyIn(urls: [URL], into name: String, completion: ((CommandResult) -> Void)? = nil) {
        let paths = urls.map { $0.path }
        guard !paths.isEmpty else {
            completion?(CommandResult(status: 1, stdout: "", stderr: "nothing to copy"))
            return
        }
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
            completion?(r)
        }
    }

    /// The inbox is a guest path like Copy out's, so it gets the same field
    /// and Tab completion. Empty means the home directory.
    @objc private func setInbox(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        Log.menu.notice("Inbox folder… chosen for \(name, privacy: .public)")
        let panel = inboxPanels[name] ?? GuestPathPanel(devvm: devvm, machine: name, config: GuestPathPanel.Config(
            title: "Inbox folder on \(name)",
            label: "Folder for dropped files (relative to home; created if missing):",
            placeholder: "~ (the home directory)",
            button: "Save",
            allowsEmpty: true)) { [weak self] _, path in
            self?.saveInbox(path, for: name)
        }
        inboxPanels[name] = panel
        let current = inbox(for: name)
        panel.show(text: current == "~" ? "" : current)
    }

    private func saveInbox(_ path: String, for name: String) {
        let inbox = path.isEmpty ? "~" : path
        Log.menu.notice("inbox for \(name, privacy: .public) set to \(inbox, privacy: .public)")
        defaults.set(inbox, forKey: Key.inbox(name))
        refillSubmenu(for: name)
        shelf.inboxChanged() // the tiles name the inbox
    }

    // MARK: Copy out

    @objc private func copyOut(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        Log.menu.notice("Copy out… chosen for \(name, privacy: .public)")
        showCopyOut(for: name)
    }

    /// From the menu, or a click on the machine's tile. One panel per
    /// machine, kept while the machine is listed, so the last path is still
    /// in the field next time.
    private func showCopyOut(for name: String) {
        let panel = copyOutPanels[name] ?? CopyOut.panel(devvm: devvm, machine: name)
        copyOutPanels[name] = panel
        panel.show()
    }

    // MARK: Config

    /// devvm.toml did not parse: open it in the default text editor (`open
    /// -t`: a .toml file often has no application of its own).
    @objc private func openConfig() {
        Log.menu.notice("open devvm.toml chosen")
        devvm.run(["config", "path"]) { r in
            let path = r.stdout.trimmingCharacters(in: .whitespacesAndNewlines)
            guard r.ok, !path.isEmpty else {
                Notifications.shared.error("devvm config path failed", r.lastStderrLine)
                return
            }
            let open = Process()
            open.executableURL = URL(fileURLWithPath: "/usr/bin/open")
            open.arguments = ["-t", path]
            do { try open.run() } catch {
                Log.menu.error("open -t \(path, privacy: .public) failed: \(error.localizedDescription, privacy: .public)")
                Notifications.shared.error("Could not open devvm.toml", path)
            }
        }
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

    /// After `devvm update` succeeded with this process still alive: start a
    /// fresh instance of the bundle (the new one, if devvm replaced it) and
    /// exit only once that launch is confirmed, so a failed launch never
    /// leaves no app. When devvm replaces the bundle it quits this app first
    /// and relaunches it itself, so this process never gets here; being here
    /// means nobody relaunched anything, whatever version is on disk. (It
    /// once took a newer bundle on disk as proof that devvm had relaunched
    /// the app and just exited; devvm had missed the running app, and the
    /// update ended with none.)
    static func relaunch() {
        if let onDisk = installedBundleVersion, onDisk != appVersion {
            Log.update.notice("bundle on disk is \(onDisk, privacy: .public), this process is \(appVersion, privacy: .public)")
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
