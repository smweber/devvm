import AppKit

/// A transparent view laid over the status item's button that accepts file
/// drags. The button itself can't be subclassed (NSStatusItem owns it) and a
/// view that returns nil from hitTest never receives drags (drag destinations
/// are found with hitTest too), so this view stays hittable and forwards
/// clicks to the button underneath, which opens the menu as usual.
final class DropTargetView: NSView {
    var canAccept: () -> Bool = { false }
    var onDrop: ([URL]) -> Void = { _ in }

    override init(frame: NSRect) {
        super.init(frame: frame)
        registerForDraggedTypes([.fileURL])
    }

    required init?(coder: NSCoder) { return nil }

    private var button: NSButton? { superview as? NSButton }

    /// The status bar button accepts the first click even when the app is
    /// not frontmost (it never is); the overlay must too, or that click is
    /// swallowed instead of opening the menu.
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }

    // MARK: Clicks pass through to the status bar button.

    override func mouseDown(with event: NSEvent) { button?.mouseDown(with: event) }
    override func mouseUp(with event: NSEvent) { button?.mouseUp(with: event) }
    override func rightMouseDown(with event: NSEvent) { button?.rightMouseDown(with: event) }
    override func rightMouseUp(with event: NSEvent) { button?.rightMouseUp(with: event) }

    // MARK: Drag destination
    //
    // NSView adopts NSDraggingDestination, so these are overrides. If a
    // toolchain reports "method does not override any method from its
    // superclass" here, delete the `override` keywords; the signatures are
    // the protocol's.

    override func draggingEntered(_ sender: NSDraggingInfo) -> NSDragOperation {
        guard canAccept(), hasFileURLs(sender) else { return [] }
        button?.highlight(true)
        return .copy
    }

    override func draggingUpdated(_ sender: NSDraggingInfo) -> NSDragOperation {
        return canAccept() && hasFileURLs(sender) ? .copy : []
    }

    override func draggingExited(_ sender: NSDraggingInfo?) {
        button?.highlight(false)
    }

    override func draggingEnded(_ sender: NSDraggingInfo) {
        button?.highlight(false)
    }

    override func performDragOperation(_ sender: NSDraggingInfo) -> Bool {
        button?.highlight(false)
        let urls = fileURLs(sender)
        guard !urls.isEmpty else { return false }
        onDrop(urls)
        return true
    }

    private func hasFileURLs(_ info: NSDraggingInfo) -> Bool {
        return !fileURLs(info).isEmpty
    }

    /// Real file URLs only: promised files (Mail, Photos) are not on disk yet
    /// and devvm cannot copy them.
    private func fileURLs(_ info: NSDraggingInfo) -> [URL] {
        let options: [NSPasteboard.ReadingOptionKey: Any] = [.urlReadingFileURLsOnly: true]
        let objects = info.draggingPasteboard.readObjects(forClasses: [NSURL.self], options: options) ?? []
        return objects.compactMap { $0 as? URL }
    }
}
