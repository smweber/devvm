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
    private var menuIsOpen = false

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
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
        dropView = DropTargetView(frame: statusItem.button?.bounds ?? .zero)
        super.init()

        statusItem.menu = menu
        menu.delegate = self
        if let button = statusItem.button {
            dropView.autoresizingMask = [.width, .height]
            button.addSubview(dropView)
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
        // Auto-select when there is exactly one live machine and nothing chosen.
        if selectedMachine == nil, machines.filter({ $0.isLive }).count == 1 {
            selectedName = machines.first { $0.isLive }?.name
        }
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

    // MARK: Menu

    func menuWillOpen(_ menu: NSMenu) {
        guard menu === self.menu else { return }
        menuIsOpen = true
        guard devvm.executable != nil else { return }
        // The watch stream only sees devvm-made changes; a VM stopped behind
        // devvm's back shows up here, on demand.
        refresh.run(devvm, ["status", "--plain"]) { [weak self] r in
            guard let self = self, r.ok else { return }
            self.apply(parseStatusBlock(r.stdout))
        }
        // Ports per live machine, for the "Open localhost:PORT" entries.
        for m in machines where m.isLive && m.forwardCount > 0 {
            let lookup = portsLookups[m.name] ?? SingleFlight()
            portsLookups[m.name] = lookup
            lookup.run(devvm, ["ports", "list", m.name]) { [weak self] r in
                guard let self = self, r.ok else { return }
                self.portsByMachine[m.name] = parsePortsList(r.stdout)
                if let sub = self.submenus[m.name] { self.fillSubmenu(sub, for: m) }
            }
        }
        devvm.version { [weak self] v in
            guard let self = self, let v = v else { return }
            if let old = self.cliVersion, old != v {
                // The binary changed underneath the watch child; it keeps
                // running the old code until restarted.
                self.watcher?.restart()
            }
            self.cliVersion = v
            self.rebuildMenu()
        }
        maybeCheckForUpdates()
    }

    func menuDidClose(_ menu: NSMenu) {
        if menu === self.menu { menuIsOpen = false }
    }

    private func item(_ title: String, _ action: Selector?, _ represented: Any? = nil) -> NSMenuItem {
        let it = NSMenuItem(title: title, action: action, keyEquivalent: "")
        it.target = self
        it.representedObject = represented
        return it
    }

    private func rebuildMenu() {
        menu.removeAllItems()
        submenus = [:]
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

        if let m = selectedMachine {
            menu.addItem(item("Drop target: \(m.name)  (inbox \(inbox(for: m.name)))", nil))
        } else {
            menu.addItem(item("No drop target — pick a running machine", nil))
        }
        menu.addItem(.separator())

        if machines.isEmpty {
            menu.addItem(item("No machines registered", nil))
        }
        for m in machines {
            let it = item("\(m.glyph) \(m.name) — \(m.stateWords), \(m.forwardsWords)", #selector(selectMachine(_:)), m.name)
            it.state = (m.name == selectedName) ? .on : .off
            let sub = NSMenu(title: m.name)
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
        let version = item(cliVersion.map { "devvm \($0) · app \(MenuBar.appVersion)" } ?? "app \(MenuBar.appVersion)", nil)
        version.isEnabled = false
        menu.addItem(version)
        menu.addItem(.separator())
        menu.addItem(item("Quit", #selector(quit)))
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

    @objc private func quit() { NSApp.terminate(nil) }

    @objc private func selectMachine(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String,
              let m = machines.first(where: { $0.name == name }) else { return }
        guard m.isLive else {
            Notifications.shared.info(name, "Not running; start it first.")
            return
        }
        selectedName = name
        updateIcon()
        rebuildMenu()
    }

    @objc private func startMachine(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
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
            guard alert.runModal() == .alertFirstButtonReturn else { return }
        }
        runReporting(["stop", name], title: "Stopped \(name)")
    }

    @objc private func portsUp(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        runReporting(["ports", "up", name], title: "\(name): forwards up")
    }

    @objc private func portsDown(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        runReporting(["ports", "down", name], title: "\(name): forwards down")
    }

    @objc private func openPort(_ sender: NSMenuItem) {
        guard let port = sender.representedObject as? Int,
              let url = URL(string: "http://localhost:\(port)/") else { return }
        NSWorkspace.shared.open(url)
    }

    private func runReporting(_ args: [String], title: String) {
        devvm.run(args) { r in
            if r.ok {
                Notifications.shared.info(title, r.stdout.trimmingCharacters(in: .whitespacesAndNewlines))
            } else {
                Notifications.shared.error("devvm \(args.joined(separator: " ")) failed", r.lastStderrLine)
            }
        }
    }

    // MARK: Copy in

    private func copyIn(urls: [URL]) {
        guard let m = selectedMachine else { return }
        copyIn(urls: urls, into: m.name)
    }

    @objc private func copyInPanel(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        NSApp.activate(ignoringOtherApps: true)
        let panel = NSOpenPanel()
        panel.title = "Copy into \(name)"
        panel.prompt = "Copy"
        panel.canChooseFiles = true
        panel.canChooseDirectories = true
        panel.allowsMultipleSelection = true
        panel.begin { [weak self] response in
            guard response == .OK else { return }
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
            (try? url.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) == true
        }
        if anyDir { args.append("-r") }
        let dest = inbox(for: name)
        args += ["-t", dest, name] + paths
        let what = paths.count == 1 ? (urls[0].lastPathComponent) : "\(paths.count) items"
        devvm.run(args) { r in
            if r.ok {
                Notifications.shared.info("Copied \(what)", "→ \(name):\(dest)")
            } else if r.isOverwriteRefusal {
                Notifications.shared.conflict("\(name): already exists", r.lastStderrLine, retryWith: args)
            } else {
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
        defaults.set(value.isEmpty ? "~" : value, forKey: Key.inbox(name))
        rebuildMenu()
        updateIcon()
    }

    // MARK: Copy out

    @objc private func copyOut(_ sender: NSMenuItem) {
        guard let name = sender.representedObject as? String else { return }
        let panel = copyOutPanels[name] ?? CopyOutPanel(devvm: devvm, machine: name)
        copyOutPanels[name] = panel
        panel.show()
    }

    // MARK: Updates

    static var appVersion: String {
        return (Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String) ?? "0.0.0"
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
        devvm.run(["menubar"]) { r in
            if !r.ok { Notifications.shared.error("devvm menubar failed", r.lastStderrLine) }
            // On success devvm relaunches the new bundle; this process is
            // simply superseded.
        }
    }

    /// Passive check at most every six hours, and only when the menu is
    /// opened: never a timer, never network in the background.
    private func maybeCheckForUpdates() {
        let last = defaults.object(forKey: Key.lastUpdateCheck) as? Date ?? .distantPast
        guard Date().timeIntervalSince(last) > 6 * 3600 else { return }
        defaults.set(Date(), forKey: Key.lastUpdateCheck)
        devvm.run(["update", "--check", "--plain"]) { [weak self] r in
            guard let self = self, r.ok else { return }
            let f = r.stdout.trimmingCharacters(in: .whitespacesAndNewlines).split(separator: "\t").map(String.init)
            self.updateAvailable = (f.count >= 3 && f[2] == "true") ? f[1] : nil
            self.rebuildMenu()
        }
    }

    @objc private func checkForUpdates() {
        devvm.run(["update", "--check", "--plain"]) { [weak self] r in
            guard let self = self else { return }
            self.defaults.set(Date(), forKey: Key.lastUpdateCheck)
            guard r.ok else {
                Notifications.shared.error("Update check failed", r.lastStderrLine)
                return
            }
            let f = r.stdout.trimmingCharacters(in: .whitespacesAndNewlines).split(separator: "\t").map(String.init)
            guard f.count >= 3 else { return }
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
        Notifications.shared.info("Updating devvm…", "The app relaunches when it finishes.")
        devvm.run(["update"]) { r in
            guard r.ok else {
                Notifications.shared.error("devvm update failed", r.lastStderrLine)
                return
            }
            MenuBar.relaunch()
        }
    }

    /// `open -n` starts a second instance of this bundle (the freshly updated
    /// one if devvm replaced it), then this one exits.
    static func relaunch() {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/open")
        p.arguments = ["-n", Bundle.main.bundlePath]
        try? p.run()
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) { NSApp.terminate(nil) }
    }

    // MARK: Launch at login

    @objc private func toggleLaunchAtLogin() {
        let service = SMAppService.mainApp
        do {
            if service.status == .enabled {
                try service.unregister()
            } else {
                try service.register()
            }
        } catch {
            NSApp.activate(ignoringOtherApps: true)
            let alert = NSAlert()
            alert.messageText = "Could not change launch at login"
            alert.informativeText = "\(error.localizedDescription)\n\nAs a fallback, add DevVM.app under System Settings › General › Login Items."
            alert.runModal()
        }
        rebuildMenu()
    }
}
