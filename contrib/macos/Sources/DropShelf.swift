import AppKit

/// The drop shelf: a small floating panel, in the manner of Yoink, with one
/// tile per live machine. A tile is the machine's copy surface: dropping
/// files on it copies them into its inbox (`cp-in -t INBOX`, the same path
/// as Copy in…), a click opens Copy out…, and a right-click opens the
/// machine's menu.
///
/// It replaced a drop target laid over the status item, which was a poor
/// place to aim a drag: moving a drag to the top of the screen sets off
/// Mission Control. The shelf opens only from the menu (no drag
/// monitoring, no auto-show) and stays up until closed. It never takes
/// focus from the app a drag starts in: a non-activating panel that floats
/// on every Space, also beside full-screen apps, and remembers where it was
/// put, and whether it was open, across launches.
///
/// Tiles never move under the pointer. A snapshot can arrive at any time
/// (the watch child, a refresh the shelf itself asked for), and a tile
/// that vanished or appeared above the one being aimed at would put a drop
/// on the wrong machine. So while the pointer is over the shelf (a drag
/// included) the set and order of tiles is frozen: a machine that went away
/// keeps its tile, dimmed and refusing drops, and new ones wait. The freeze
/// is read from the pointer's position each time, never tracked from
/// enter/exit events, so a missed event cannot leave the shelf stuck.
final class DropShelf: NSObject, NSWindowDelegate {
    /// A drop on a tile: the machine, the files, and a completion the copy
    /// calls exactly once with its result (the tile shows it).
    var onDrop: (String, [URL], @escaping (CommandResult) -> Void) -> Void = { _, _, _ in }
    /// A click on a tile: Copy out… for that machine.
    var onClick: (String) -> Void = { _ in }
    /// A right-click on a tile: the machine's menu, or nil for none.
    var contextMenu: (String) -> NSMenu? = { _ in nil }
    /// A machine's inbox, shown on its tile.
    var inbox: (String) -> String = { _ in "~" }
    /// The shelf is being looked at (shown, or the pointer or a drag came
    /// over it): a good moment for a fresh status. The shelf can stay open
    /// for days without the menu, whose opening is the app's other refresh.
    /// The owner rate-limits it.
    var onAttention: () -> Void = {}

    private static let autosaveName = "DevVMDropShelf"
    private static let visibleKey = "shelfVisible"
    private static let width: CGFloat = 220
    private static let tileHeight: CGFloat = 52
    private static let gap: CGFloat = 8
    private static let pad: CGFloat = 10
    private static let headerHeight: CGFloat = 15

    private let panel: NSPanel
    private let container = ShelfContentView(frame: .zero)
    private let emptyLabel = NSTextField(labelWithString: "No running machines")
    /// Tiles by machine name, kept across updates: a tile is only created
    /// or removed when its machine appears or goes, so a snapshot never
    /// drops the hover state or a copy's progress.
    private var tiles: [String: ShelfTile] = [:]
    private var order: [String] = []
    /// The last snapshot, in the menu's section order.
    private var machines: [Machine] = []
    private var settleScheduled = false
    /// The menu's sections, for the small headers between groups of tiles:
    /// each machine's section title (kept for a tile that outlives its
    /// machine), and whether the menu shows headers at all (a lone kind
    /// has none). Labels are reused from one layout to the next.
    private var sectionTitle: [String: String] = [:]
    private var headed = false
    private var headers: [NSTextField] = []

    override init() {
        panel = NSPanel(contentRect: NSRect(x: 0, y: 0, width: DropShelf.width, height: 80),
                        styleMask: [.titled, .closable, .utilityWindow, .nonactivatingPanel],
                        backing: .buffered, defer: false)
        super.init()

        panel.title = "Drop Shelf"
        panel.level = .floating
        panel.isFloatingPanel = true
        panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        panel.hidesOnDeactivate = false
        panel.isMovableByWindowBackground = true
        panel.becomesKeyOnlyIfNeeded = true // nothing here takes typing
        panel.isReleasedWhenClosed = false
        panel.delegate = self

        emptyLabel.alignment = .center
        emptyLabel.font = .systemFont(ofSize: 12)
        emptyLabel.textColor = .secondaryLabelColor
        container.addSubview(emptyLabel)
        container.onPointer = { [weak self] inside in
            if inside { self?.onAttention() } else { self?.settleSoon() }
        }
        panel.contentView = container

        // Restore where the user last put it (force: the size too, so the
        // top edge lands where it was), then keep saving it.
        let restored = panel.setFrameUsingName(DropShelf.autosaveName, force: true)
        _ = panel.setFrameAutosaveName(DropShelf.autosaveName)
        if !restored || !NSScreen.screens.contains(where: { $0.visibleFrame.intersects(self.panel.frame) }) {
            placeTopRight()
        }
        layoutTiles()
    }

    var isVisible: Bool { panel.isVisible }

    /// Whether the shelf was open when the app last ran.
    var wasVisible: Bool { UserDefaults.standard.bool(forKey: DropShelf.visibleKey) }

    /// Shows the shelf without activating the app, so the app a drag starts
    /// in keeps focus.
    func show() {
        Log.drop.notice("drop shelf shown (\(self.order.count, privacy: .public) tile(s))")
        UserDefaults.standard.set(true, forKey: DropShelf.visibleKey)
        // A display may have gone while it was hidden.
        if !NSScreen.screens.contains(where: { $0.visibleFrame.intersects(self.panel.frame) }) {
            placeTopRight()
        }
        reconcile()
        panel.orderFrontRegardless()
        onAttention()
    }

    func hide() {
        Log.drop.notice("drop shelf hidden")
        UserDefaults.standard.set(false, forKey: DropShelf.visibleKey)
        panel.orderOut(nil)
    }

    /// The title bar's close button. Quitting the app closes no window, so
    /// a shelf open at quit stays remembered as open.
    func windowWillClose(_ notification: Notification) {
        Log.drop.notice("drop shelf closed")
        UserDefaults.standard.set(false, forKey: DropShelf.visibleKey)
    }

    /// Follows a status snapshot: one tile per live machine (hub machines
    /// included: cp-in takes `HUB/NAME` like any name), in the menu's order.
    func update(_ machines: [Machine]) {
        let sections = machineSections(machines)
        self.machines = sections.flatMap { $0.members }
        headed = sections.count > 1 || sections.first?.hubName != nil
        var titles = sectionTitle.filter { tiles[$0.key] != nil }
        for section in sections {
            for m in section.members { titles[m.name] = section.title }
        }
        sectionTitle = titles
        reconcile()
    }

    /// An inbox changed: the tiles name theirs.
    func inboxChanged() {
        reconcile()
    }

    /// The pointer (a drag's included) is over the shelf, title bar and all.
    private var frozen: Bool {
        return panel.isVisible && panel.frame.contains(NSEvent.mouseLocation)
    }

    /// Brings the tiles in line with the last snapshot. What a tile shows is
    /// always brought up to date; which tiles exist, and in what order, only
    /// while the pointer is elsewhere (see the type's comment). A tile whose
    /// machine went away also stays while it has a copy to finish reporting.
    private func reconcile() {
        let live = machines.filter { $0.isLive }
        let liveNames = Set(live.map { $0.name })
        let hold = frozen

        for m in live {
            tiles[m.name]?.update(machine: m, inbox: inbox(m.name))
        }
        for (name, tile) in tiles where !liveNames.contains(name) {
            if hold || tile.isBusy {
                // Still listed but not live (stopped, unreachable), or gone
                // from the listing altogether.
                let now = machines.first(where: { $0.name == name })
                tile.markGone(now?.stateWords ?? "gone")
            } else {
                tile.removeFromSuperview()
                tiles[name] = nil
            }
        }
        if hold {
            // New machines wait for the pointer to leave.
            if live.contains(where: { tiles[$0.name] == nil }) || tiles.values.contains(where: { $0.isGone }) {
                settleSoon()
            }
            return
        }
        for m in live where tiles[m.name] == nil {
            let tile = ShelfTile(name: m.name, frame: NSRect(x: 0, y: 0, width: DropShelf.width - 2 * DropShelf.pad,
                                                             height: DropShelf.tileHeight))
            tile.onDrop = { [weak self] t, urls in self?.dropped(urls, on: t) }
            tile.onClick = { [weak self] t in self?.onClick(t.name) }
            tile.menuProvider = { [weak self] t in self?.contextMenu(t.name) }
            tile.onDragEntered = { [weak self] in self?.onAttention() }
            tile.onDragLeft = { [weak self] in self?.settleSoon() }
            tile.onSettled = { [weak self] in self?.reconcile() }
            tile.update(machine: m, inbox: inbox(m.name))
            tiles[m.name] = tile
            container.addSubview(tile)
        }
        // Live machines in order, then tiles kept only for a copy's sake.
        let kept = order.filter { tiles[$0] != nil && !liveNames.contains($0) }
        order = live.map { $0.name } + kept
        layoutTiles()
    }

    /// Reconciles again once the pointer has had time to leave: the replay
    /// of whatever a freeze held back. Asked for when the pointer or a drag
    /// leaves, and by a reconcile that held something back, so it does not
    /// depend on the leave event arriving. While the pointer stays, it
    /// re-arms itself (one cheap check a second, only then).
    private func settleSoon() {
        guard !settleScheduled else { return }
        settleScheduled = true
        DispatchQueue.main.asyncAfter(deadline: .now() + (frozen ? 1.0 : 0.3)) { [weak self] in
            guard let self = self else { return }
            self.settleScheduled = false
            self.reconcile()
        }
    }

    private func dropped(_ urls: [URL], on tile: ShelfTile) {
        tile.began()
        onDrop(tile.name, urls, { [weak tile] r in
            tile?.finished(r)
        })
    }

    /// Stacks the tiles top-down under their sections' headers (the menu's:
    /// Local, Remote, a provider's, each hub's) and fits the panel to them,
    /// keeping its top edge where it is (the shelf grows and shrinks
    /// downwards). Only ever called with the pointer elsewhere.
    private func layoutTiles() {
        let pad = DropShelf.pad, h = DropShelf.tileHeight, gap = DropShelf.gap, w = DropShelf.width
        var y = pad
        var used = 0
        var lastTitle: String?
        for name in order {
            guard let tile = tiles[name] else { continue }
            let title = sectionTitle[name] ?? ""
            if headed && title != lastTitle {
                lastTitle = title
                let label = header(used)
                used += 1
                label.stringValue = title
                label.frame = NSRect(x: pad + 2, y: y, width: w - 2 * pad - 4, height: DropShelf.headerHeight)
                y += DropShelf.headerHeight + 4
            }
            tile.frame = NSRect(x: pad, y: y, width: w - 2 * pad, height: h)
            y += h + gap
        }
        for label in headers[used...] { label.isHidden = true }
        emptyLabel.isHidden = !order.isEmpty
        emptyLabel.frame = NSRect(x: pad, y: pad + (h - 18) / 2, width: w - 2 * pad, height: 18)

        let contentHeight = order.isEmpty ? 2 * pad + h : y - gap + pad
        let size = panel.frameRect(forContentRect: NSRect(x: 0, y: 0, width: w, height: contentHeight)).size
        let old = panel.frame
        if old.size != size {
            panel.setFrame(NSRect(x: old.minX, y: old.maxY - size.height, width: size.width, height: size.height),
                           display: true)
        }
    }

    /// The i-th header label, made on first use.
    private func header(_ i: Int) -> NSTextField {
        while headers.count <= i {
            let label = NSTextField(labelWithString: "")
            label.font = .systemFont(ofSize: 11, weight: .semibold)
            label.textColor = .secondaryLabelColor
            label.lineBreakMode = .byTruncatingTail
            container.addSubview(label)
            headers.append(label)
        }
        headers[i].isHidden = false
        return headers[i]
    }

    private func placeTopRight() {
        guard let screen = NSScreen.main ?? NSScreen.screens.first else { return }
        let vf = screen.visibleFrame
        let f = panel.frame
        panel.setFrameOrigin(NSPoint(x: vf.maxX - f.width - 20, y: vf.maxY - f.height - 20))
    }
}

/// The panel's content: top-down coordinates, so tiles stack from the top,
/// and one tracking area over all of it that says when the pointer comes
/// and goes (tile to tile is neither).
private final class ShelfContentView: NSView {
    var onPointer: ((Bool) -> Void)?
    private var tracking: NSTrackingArea?

    override var isFlipped: Bool { return true }

    override func updateTrackingAreas() {
        super.updateTrackingAreas()
        if let old = tracking { removeTrackingArea(old) }
        // activeAlways: the shelf never makes this app active.
        let area = NSTrackingArea(rect: .zero, options: [.mouseEnteredAndExited, .activeAlways, .inVisibleRect],
                                  owner: self, userInfo: nil)
        addTrackingArea(area)
        tracking = area
    }

    override func mouseEntered(with event: NSEvent) { onPointer?(true) }
    override func mouseExited(with event: NSEvent) { onPointer?(false) }
}

/// One machine on the shelf: its name and inbox, a highlight while a file
/// drag hovers, a copy's progress and result, and the click and right-click
/// that open Copy out… and the machine's menu.
private final class ShelfTile: NSView {
    let name: String
    var onDrop: ((ShelfTile, [URL]) -> Void)?
    var onClick: ((ShelfTile) -> Void)?
    var menuProvider: ((ShelfTile) -> NSMenu?)?
    var onDragEntered: (() -> Void)?
    var onDragLeft: (() -> Void)?
    /// Nothing left to report (the last copy's result has been shown): a
    /// tile kept past its machine can go.
    var onSettled: (() -> Void)?

    private let nameLabel: NSTextField
    private let detailLabel = NSTextField(labelWithString: "")
    private let spinner = NSProgressIndicator(frame: .zero)

    private var idleText = ""     // "→ ~/inbox", shown when idle
    private var goneText: String? // "stopped", "unreachable", "gone": not a target any more
    private var copies = 0        // copies in flight
    private var result: String?   // "✓ Copied" / "✗ …", shown briefly
    private var resultOK = false
    private var resultToken = 0
    private var dragOver = false { didSet { needsDisplay = true } }
    private var pointerInside = false
    private var tracking: NSTrackingArea?
    /// Where a press began, in screen coordinates, until it turns into a
    /// drag of the panel or is released as a click.
    private var pressOrigin: NSPoint?

    private static let hint = "Drop in · click to copy out"

    init(name: String, frame: NSRect) {
        self.name = name
        nameLabel = NSTextField(labelWithString: name)
        super.init(frame: frame)
        wantsLayer = true
        layerContentsRedrawPolicy = .onSetNeedsDisplay
        layer?.cornerRadius = 8

        let w = frame.width
        nameLabel.font = .boldSystemFont(ofSize: 13)
        nameLabel.lineBreakMode = .byTruncatingMiddle
        nameLabel.frame = NSRect(x: 10, y: 27, width: w - 42, height: 17)
        nameLabel.autoresizingMask = [.width]
        detailLabel.font = .systemFont(ofSize: 11)
        detailLabel.textColor = .secondaryLabelColor
        detailLabel.lineBreakMode = .byTruncatingMiddle
        detailLabel.frame = NSRect(x: 10, y: 9, width: w - 20, height: 15)
        detailLabel.autoresizingMask = [.width]
        spinner.style = .spinning
        spinner.controlSize = .small
        spinner.isDisplayedWhenStopped = false
        spinner.frame = NSRect(x: w - 26, y: 27, width: 16, height: 16)
        spinner.autoresizingMask = [.minXMargin]
        addSubview(nameLabel)
        addSubview(detailLabel)
        addSubview(spinner)

        registerForDraggedTypes([.fileURL])
    }

    required init?(coder: NSCoder) { return nil }

    // MARK: State

    /// A copy in flight or its result still on show.
    var isBusy: Bool { copies > 0 || result != nil }
    var isGone: Bool { goneText != nil }

    func update(machine m: Machine, inbox: String) {
        goneText = nil
        alphaValue = 1
        // A box never probed may be down; a drop then fails at ssh's timeout.
        idleText = (m.state == "?" ? "not checked · " : "") + "→ " + ShelfTile.display(inbox: inbox)
        refreshDetail()
    }

    /// The machine is no longer a target; the tile stays where it is until
    /// the shelf lets it go.
    func markGone(_ why: String) {
        goneText = why
        alphaValue = 0.5
        dragOver = false
        refreshDetail()
    }

    /// The inbox as a path a reader places at once: relative ones are under
    /// the guest's home.
    private static func display(inbox: String) -> String {
        if inbox.hasPrefix("/") || inbox.hasPrefix("~") { return inbox }
        return "~/" + inbox
    }

    func began() {
        copies += 1
        result = nil
        resultToken += 1
        spinner.startAnimation(nil)
        refreshDetail()
    }

    func finished(_ r: CommandResult) {
        copies = max(0, copies - 1)
        if copies == 0 { spinner.stopAnimation(nil) }
        resultOK = r.ok
        result = r.ok ? "✓ Copied" : (r.isOverwriteRefusal ? "✗ Already exists" : "✗ Failed")
        resultToken += 1
        let token = resultToken
        refreshDetail()
        DispatchQueue.main.asyncAfter(deadline: .now() + 3) { [weak self] in
            guard let self = self, self.resultToken == token else { return }
            self.result = nil
            self.refreshDetail()
            if !self.isBusy { self.onSettled?() }
        }
    }

    private func refreshDetail() {
        detailLabel.textColor = .secondaryLabelColor
        if copies > 0 {
            detailLabel.stringValue = "Copying…"
        } else if let text = result {
            detailLabel.stringValue = text
            detailLabel.textColor = resultOK ? .systemGreen : .systemRed
        } else if let gone = goneText {
            detailLabel.stringValue = gone
        } else if pointerInside && !dragOver {
            detailLabel.stringValue = ShelfTile.hint
        } else {
            detailLabel.stringValue = idleText
        }
    }

    // MARK: Appearance

    override var wantsUpdateLayer: Bool { return true }

    /// Colors are resolved here, where AppKit has set the view's appearance
    /// as current, so they follow light and dark mode.
    override func updateLayer() {
        layer?.backgroundColor = (dragOver ? NSColor.controlAccentColor.withAlphaComponent(0.25)
                                           : NSColor.controlBackgroundColor).cgColor
        layer?.borderColor = (dragOver ? NSColor.controlAccentColor : NSColor.separatorColor).cgColor
        layer?.borderWidth = dragOver ? 2 : 1
    }

    // MARK: Mouse
    //
    // The whole tile is one target: the labels never take the mouse (a drag
    // over them must reach the tile). A press that moves drags the panel, as
    // a press on its background does; one released in place is a click. The
    // tile tells the two apart itself (so it cannot also be a view whose
    // mouse-down moves the window: AppKit would keep the events).

    override func hitTest(_ point: NSPoint) -> NSView? {
        return frame.contains(point) ? self : nil
    }

    override var mouseDownCanMoveWindow: Bool { return false }

    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { return true }

    override func mouseDown(with event: NSEvent) {
        pressOrigin = NSEvent.mouseLocation
    }

    override func mouseDragged(with event: NSEvent) {
        guard let origin = pressOrigin else { return }
        let now = NSEvent.mouseLocation
        if abs(now.x - origin.x) > 3 || abs(now.y - origin.y) > 3 {
            pressOrigin = nil
            // Runs its own tracking loop until the button is released.
            window?.performDrag(with: event)
        }
    }

    override func mouseUp(with event: NSEvent) {
        guard pressOrigin != nil else { return }
        pressOrigin = nil
        guard !isGone, bounds.contains(convert(event.locationInWindow, from: nil)) else { return }
        Log.drop.notice("tile \(self.name, privacy: .public) clicked")
        onClick?(self)
    }

    override func menu(for event: NSEvent) -> NSMenu? {
        return isGone ? nil : menuProvider?(self)
    }

    /// The hover hint. Cosmetic only: nothing depends on the exit arriving.
    override func updateTrackingAreas() {
        super.updateTrackingAreas()
        if let old = tracking { removeTrackingArea(old) }
        let area = NSTrackingArea(rect: .zero, options: [.mouseEnteredAndExited, .activeAlways, .inVisibleRect],
                                  owner: self, userInfo: nil)
        addTrackingArea(area)
        tracking = area
    }

    override func mouseEntered(with event: NSEvent) {
        pointerInside = true
        refreshDetail()
    }

    override func mouseExited(with event: NSEvent) {
        pointerInside = false
        refreshDetail()
    }

    // MARK: Drag destination
    //
    // NSView adopts NSDraggingDestination, so these are overrides.

    override func draggingEntered(_ sender: NSDraggingInfo) -> NSDragOperation {
        onDragEntered?()
        guard !isGone else { return [] }
        let urls = fileURLs(sender)
        guard !urls.isEmpty else {
            Log.drop.notice("drag entered \(self.name, privacy: .public): refused (no file urls)")
            return []
        }
        Log.drop.notice("drag entered \(self.name, privacy: .public): \(urls.count, privacy: .public) file(s), accepting")
        dragOver = true
        return .copy
    }

    override func draggingUpdated(_ sender: NSDraggingInfo) -> NSDragOperation {
        // Asked again on every move: a tile that went away mid-hover stops
        // accepting at once.
        if isGone || fileURLs(sender).isEmpty {
            dragOver = false
            return []
        }
        return .copy
    }

    override func draggingExited(_ sender: NSDraggingInfo?) {
        dragOver = false
        onDragLeft?()
    }

    override func draggingEnded(_ sender: NSDraggingInfo) {
        dragOver = false
        onDragLeft?()
    }

    override func performDragOperation(_ sender: NSDraggingInfo) -> Bool {
        dragOver = false
        guard !isGone else { return false }
        let urls = fileURLs(sender)
        let names = urls.map { $0.lastPathComponent }.joined(separator: ", ")
        Log.drop.notice("dropped \(urls.count, privacy: .public) file(s) on \(self.name, privacy: .public): \(names, privacy: .public)")
        guard !urls.isEmpty, let onDrop = onDrop else { return false }
        onDrop(self, urls)
        return true
    }

    /// Real file URLs only: promised files (Mail, Photos) are not on disk yet
    /// and devvm cannot copy them.
    private func fileURLs(_ info: NSDraggingInfo) -> [URL] {
        let options: [NSPasteboard.ReadingOptionKey: Any] = [.urlReadingFileURLsOnly: true]
        let objects = info.draggingPasteboard.readObjects(forClasses: [NSURL.self], options: options) ?? []
        return objects.compactMap { $0 as? URL }
    }
}
