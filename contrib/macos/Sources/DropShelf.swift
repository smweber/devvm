import AppKit

/// The drop shelf: a small floating panel, in the manner of Yoink, with one
/// tile per live machine. Dropping files on a tile copies them into that
/// machine's inbox (`cp-in -t INBOX`, the same path as Copy in…).
///
/// It replaced a drop target laid over the status item, which was a poor
/// place to aim a drag: moving a drag to the top of the screen sets off
/// Mission Control. The shelf opens only from the menu (no drag
/// monitoring, no auto-show) and stays up until closed. It never takes
/// focus from the app a drag starts in: a non-activating panel that floats
/// on every Space, also beside full-screen apps, and remembers where it was
/// put across launches.
final class DropShelf: NSObject {
    /// A drop on a tile: the machine, the files, and a completion the copy
    /// calls exactly once with its result (the tile shows it).
    var onDrop: (String, [URL], @escaping (CommandResult) -> Void) -> Void = { _, _, _ in }
    /// A machine's inbox, for the tile's tooltip.
    var inbox: (String) -> String = { _ in "~" }

    private static let autosaveName = "DevVMDropShelf"
    private static let width: CGFloat = 200
    private static let tileHeight: CGFloat = 52
    private static let gap: CGFloat = 8
    private static let pad: CGFloat = 10

    private let panel: NSPanel
    private let container = FlippedView(frame: .zero)
    private let emptyLabel = NSTextField(labelWithString: "No running machines")
    /// Tiles by machine name, kept across updates: a tile is only created
    /// or removed when its machine appears or goes, so a snapshot arriving
    /// mid-drag never drops the hover state or a copy's progress.
    private var tiles: [String: ShelfTile] = [:]
    private var order: [String] = []

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

        emptyLabel.alignment = .center
        emptyLabel.font = .systemFont(ofSize: 12)
        emptyLabel.textColor = .secondaryLabelColor
        container.addSubview(emptyLabel)
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

    /// Shows the shelf without activating the app, so the app a drag starts
    /// in keeps focus.
    func show() {
        Log.drop.notice("drop shelf shown (\(self.order.count, privacy: .public) tile(s))")
        panel.orderFrontRegardless()
    }

    func hide() {
        Log.drop.notice("drop shelf hidden")
        panel.orderOut(nil)
    }

    /// Flashes a machine's tile, so "Open Drop Shelf" from its submenu
    /// points at the right one.
    func highlight(_ name: String) {
        tiles[name]?.flash()
    }

    /// Follows a status snapshot: one tile per live machine (hub machines
    /// included: cp-in takes `HUB/NAME` like any name), in status order.
    func update(_ machines: [Machine]) {
        let live = machines.filter { $0.isLive }
        let names = live.map { $0.name }
        for name in Array(tiles.keys) where !names.contains(name) {
            tiles[name]?.removeFromSuperview()
            tiles[name] = nil
        }
        for m in live {
            let tile: ShelfTile
            if let existing = tiles[m.name] {
                tile = existing
            } else {
                tile = ShelfTile(name: m.name, frame: NSRect(x: 0, y: 0, width: DropShelf.width - 2 * DropShelf.pad,
                                                             height: DropShelf.tileHeight))
                tile.onDrop = { [weak self] t, urls in self?.dropped(urls, on: t) }
                tiles[m.name] = tile
                container.addSubview(tile)
            }
            tile.update(machine: m, inbox: inbox(m.name))
        }
        order = names
        layoutTiles()
    }

    private func dropped(_ urls: [URL], on tile: ShelfTile) {
        tile.began()
        onDrop(tile.name, urls, { [weak tile] r in
            tile?.finished(r)
        })
    }

    /// Stacks the tiles top-down and fits the panel to them, keeping its top
    /// edge where it is (the shelf grows and shrinks downwards).
    private func layoutTiles() {
        let pad = DropShelf.pad, h = DropShelf.tileHeight, gap = DropShelf.gap, w = DropShelf.width
        for (i, name) in order.enumerated() {
            tiles[name]?.frame = NSRect(x: pad, y: pad + CGFloat(i) * (h + gap), width: w - 2 * pad, height: h)
        }
        emptyLabel.isHidden = !order.isEmpty
        emptyLabel.frame = NSRect(x: pad, y: pad + (h - 18) / 2, width: w - 2 * pad, height: 18)

        let rows = CGFloat(max(order.count, 1))
        let contentHeight = 2 * pad + rows * h + (rows - 1) * gap
        let size = panel.frameRect(forContentRect: NSRect(x: 0, y: 0, width: w, height: contentHeight)).size
        let old = panel.frame
        if old.size != size {
            panel.setFrame(NSRect(x: old.minX, y: old.maxY - size.height, width: size.width, height: size.height),
                           display: true)
        }
    }

    private func placeTopRight() {
        guard let screen = NSScreen.main ?? NSScreen.screens.first else { return }
        let vf = screen.visibleFrame
        let f = panel.frame
        panel.setFrameOrigin(NSPoint(x: vf.maxX - f.width - 20, y: vf.maxY - f.height - 20))
    }
}

/// Top-down coordinates, so tiles stack from the top of the panel.
private final class FlippedView: NSView {
    override var isFlipped: Bool { return true }
}

/// One machine on the shelf: its name and state, a highlight while a file
/// drag hovers, and a copy's progress and result.
private final class ShelfTile: NSView {
    let name: String
    var onDrop: ((ShelfTile, [URL]) -> Void)?

    private let nameLabel: NSTextField
    private let detailLabel = NSTextField(labelWithString: "")
    private let spinner = NSProgressIndicator(frame: .zero)

    private var stateText = ""    // "running · 2 forwards up", shown when idle
    private var copies = 0        // copies in flight
    private var result: String?   // "✓ Copied" / "✗ …", shown briefly
    private var resultOK = false
    private var resultToken = 0
    private var flashToken = 0
    private var hovering = false { didSet { needsDisplay = true } }
    private var flashing = false { didSet { needsDisplay = true } }

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
        detailLabel.lineBreakMode = .byTruncatingTail
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

    // MARK: Appearance

    override var wantsUpdateLayer: Bool { return true }

    /// Colors are resolved here, where AppKit has set the view's appearance
    /// as current, so they follow light and dark mode.
    override func updateLayer() {
        let active = hovering || flashing
        layer?.backgroundColor = (active ? NSColor.controlAccentColor.withAlphaComponent(0.25)
                                         : NSColor.controlBackgroundColor).cgColor
        layer?.borderColor = (active ? NSColor.controlAccentColor : NSColor.separatorColor).cgColor
        layer?.borderWidth = active ? 2 : 1
    }

    func update(machine m: Machine, inbox: String) {
        stateText = "\(m.stateWords) · \(m.forwardsWords)"
        toolTip = "Drop files to copy them into \(name):\(inbox)"
        refreshDetail()
    }

    func flash() {
        flashToken += 1
        let token = flashToken
        flashing = true
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.2) { [weak self] in
            guard let self = self, self.flashToken == token else { return }
            self.flashing = false
        }
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
        }
    }

    private func refreshDetail() {
        if copies > 0 {
            detailLabel.stringValue = "Copying…"
            detailLabel.textColor = .secondaryLabelColor
        } else if let text = result {
            detailLabel.stringValue = text
            detailLabel.textColor = resultOK ? .systemGreen : .systemRed
        } else {
            detailLabel.stringValue = stateText
            detailLabel.textColor = .secondaryLabelColor
        }
    }

    // MARK: Mouse
    //
    // The whole tile is one target: the labels never take the mouse (a drag
    // over them must reach the tile), and a press on it moves the panel.

    override func hitTest(_ point: NSPoint) -> NSView? {
        return frame.contains(point) ? self : nil
    }

    override var mouseDownCanMoveWindow: Bool { return true }

    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { return true }

    // MARK: Drag destination
    //
    // NSView adopts NSDraggingDestination, so these are overrides (the same
    // signatures the status item's drop view used).

    override func draggingEntered(_ sender: NSDraggingInfo) -> NSDragOperation {
        let urls = fileURLs(sender)
        guard !urls.isEmpty else {
            Log.drop.notice("drag entered \(self.name, privacy: .public): refused (no file urls)")
            return []
        }
        Log.drop.notice("drag entered \(self.name, privacy: .public): \(urls.count, privacy: .public) file(s), accepting")
        hovering = true
        return .copy
    }

    override func draggingUpdated(_ sender: NSDraggingInfo) -> NSDragOperation {
        return fileURLs(sender).isEmpty ? [] : .copy
    }

    override func draggingExited(_ sender: NSDraggingInfo?) {
        hovering = false
    }

    override func draggingEnded(_ sender: NSDraggingInfo) {
        hovering = false
    }

    override func performDragOperation(_ sender: NSDraggingInfo) -> Bool {
        hovering = false
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
