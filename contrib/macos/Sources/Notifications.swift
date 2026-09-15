import AppKit
import UserNotifications

/// User-facing results. Notifications are the default; when the notification
/// center is unavailable (authorization denied, or the app is somehow run
/// outside its bundle) a small floating panel that closes itself stands in,
/// so a result never blocks and never needs a click.
final class Notifications: NSObject, UNUserNotificationCenterDelegate {
    static let shared = Notifications()

    private static let conflictCategory = "devvm.conflict"
    private static let replaceAction = "devvm.replace"
    /// userInfo key carrying the argv to re-run with -f. It rides in the
    /// notification itself so "Replace" still works after the app relaunched
    /// (a process-local map would be empty by then).
    private static let retryKey = "devvm.retry"

    private var authorized = false
    private var usable = false

    func setup() {
        guard Bundle.main.bundleIdentifier != nil else {
            Log.notify.error("no bundle identifier (running outside the .app?); using toasts")
            return // UNUserNotificationCenter requires a bundle
        }
        usable = true
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        let replace = UNNotificationAction(identifier: Notifications.replaceAction, title: "Replace", options: [])
        let category = UNNotificationCategory(identifier: Notifications.conflictCategory, actions: [replace],
                                              intentIdentifiers: [], options: [])
        center.setNotificationCategories([category])
        // Seed from the stored decision so a result produced before the
        // (asynchronous, possibly user-gated) request below returns does not
        // needlessly fall back to a toast on an already-authorized install.
        center.getNotificationSettings { [weak self] settings in
            let granted = settings.authorizationStatus == .authorized || settings.authorizationStatus == .provisional
            Log.notify.notice("stored authorization status: \(settings.authorizationStatus.rawValue, privacy: .public) (granted=\(granted, privacy: .public))")
            DispatchQueue.main.async { if granted { self?.authorized = true } }
        }
        center.requestAuthorization(options: [.alert, .sound]) { [weak self] granted, err in
            if let err = err {
                Log.notify.error("authorization request failed: \(err.localizedDescription, privacy: .public); using toasts")
            } else {
                Log.notify.notice("authorization granted=\(granted, privacy: .public)\(granted ? "" : "; using toasts", privacy: .public)")
            }
            DispatchQueue.main.async { self?.authorized = granted }
        }
    }

    func info(_ title: String, _ body: String) {
        post(title: title, body: body, retry: nil)
    }

    func error(_ title: String, _ body: String) {
        post(title: title, body: body, retry: nil)
    }

    /// A copy was refused because it would overwrite; offer to redo it with -f.
    func conflict(_ title: String, _ body: String, retryWith args: [String]) {
        post(title: title, body: body, retry: args)
    }

    private func post(title: String, body: String, retry: [String]?) {
        Log.notify.notice("\(title, privacy: .public) — \(body, privacy: .public)")
        guard usable, authorized else {
            Log.notify.notice("notification center unavailable (usable=\(self.usable, privacy: .public), authorized=\(self.authorized, privacy: .public)); showing a toast")
            Toast.show(title: title, body: body, retry: retry)
            return
        }
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        if let retry = retry {
            content.categoryIdentifier = Notifications.conflictCategory
            content.userInfo = [Notifications.retryKey: retry]
        }
        let request = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
        UNUserNotificationCenter.current().add(request) { err in
            if let err = err {
                Log.notify.error("notification center refused: \(err.localizedDescription, privacy: .public); showing a toast")
                DispatchQueue.main.async { Toast.show(title: title, body: body, retry: retry) }
            }
        }
    }

    // Show banners even while the app is "active" (it never has a front window
    // but AppKit may still consider it foreground after a panel).
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound])
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                withCompletionHandler completionHandler: @escaping () -> Void) {
        if response.actionIdentifier == Notifications.replaceAction,
           let args = response.notification.request.content.userInfo[Notifications.retryKey] as? [String] {
            Log.notify.notice("Replace chosen from notification")
            DispatchQueue.main.async { Notifications.rerunWithForce(args) }
        }
        completionHandler()
    }

    static func rerunWithForce(_ args: [String]) {
        var forced = args
        if !forced.contains("-f") { forced.insert("-f", at: 1) } // after the verb
        Log.notify.notice("re-running with -f: devvm \(forced.joined(separator: " "), privacy: .public)")
        Devvm.shared.run(forced) { r in
            if r.ok {
                Notifications.shared.info("Replaced", forced.joined(separator: " "))
            } else {
                Notifications.shared.error("Copy failed", r.lastStderrLine)
            }
        }
    }
}

/// Fallback for when notifications are unavailable: a borderless floating
/// panel near the top-right that closes itself after a few seconds.
enum Toast {
    private static var panels: [NSPanel] = []
    private static var actions: [ToastAction] = []

    static func show(title: String, body: String, retry: [String]?) {
        let width: CGFloat = 340
        let panel = NSPanel(contentRect: NSRect(x: 0, y: 0, width: width, height: 70),
                            styleMask: [.borderless, .nonactivatingPanel], backing: .buffered, defer: false)
        panel.level = .floating
        panel.isOpaque = false
        panel.backgroundColor = .clear
        panel.hasShadow = true
        panel.ignoresMouseEvents = retry == nil

        let box = NSVisualEffectView(frame: NSRect(x: 0, y: 0, width: width, height: 70))
        box.material = .hudWindow
        box.state = .active
        box.wantsLayer = true
        box.layer?.cornerRadius = 10
        box.layer?.masksToBounds = true

        let titleLabel = NSTextField(labelWithString: title)
        titleLabel.font = .boldSystemFont(ofSize: 13)
        titleLabel.frame = NSRect(x: 12, y: 42, width: width - 24, height: 18)
        let bodyLabel = NSTextField(wrappingLabelWithString: body)
        bodyLabel.font = .systemFont(ofSize: 12)
        bodyLabel.frame = NSRect(x: 12, y: 6, width: width - (retry == nil ? 24 : 100), height: 34)
        box.addSubview(titleLabel)
        box.addSubview(bodyLabel)
        if let retry = retry {
            let button = NSButton(title: "Replace", target: nil, action: nil)
            button.bezelStyle = .rounded
            button.frame = NSRect(x: width - 84, y: 8, width: 72, height: 26)
            let handler = ToastAction(args: retry, panel: panel)
            actions.append(handler) // NSButton.target is weak; keep the handler alive
            button.target = handler
            button.action = #selector(ToastAction.fire)
            box.addSubview(button)
        }
        panel.contentView = box

        if let screen = NSScreen.main {
            let vf = screen.visibleFrame
            let y = vf.maxY - 80 - CGFloat(panels.count) * 80
            panel.setFrameOrigin(NSPoint(x: vf.maxX - width - 16, y: y))
        }
        panel.orderFrontRegardless()
        panels.append(panel)
        DispatchQueue.main.asyncAfter(deadline: .now() + (retry == nil ? 5 : 12)) {
            dismiss(panel)
        }
    }

    static func dismiss(_ panel: NSPanel) {
        panel.orderOut(nil)
        panels.removeAll { $0 === panel }
        actions.removeAll { $0.panel == nil || $0.panel === panel }
    }
}

private final class ToastAction: NSObject {
    let args: [String]
    weak var panel: NSPanel?
    init(args: [String], panel: NSPanel) {
        self.args = args
        self.panel = panel
    }
    @objc func fire() {
        Log.notify.notice("Replace chosen from toast")
        if let panel = panel { Toast.dismiss(panel) }
        Notifications.rerunWithForce(args)
    }
}
