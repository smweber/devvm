import AppKit

// Wires the pieces together. Everything the app does is a `devvm` invocation;
// the app itself holds no state beyond what the CLI reports.
final class AppDelegate: NSObject, NSApplicationDelegate {
    private var menuBar: MenuBar?
    private var watcher: StatusWatcher?

    func applicationDidFinishLaunching(_ notification: Notification) {
        let devvm = Devvm.shared // resolves the login PATH and locates devvm
        Notifications.shared.setup()
        let menuBar = MenuBar(devvm: devvm)
        self.menuBar = menuBar

        guard devvm.executable != nil else {
            // The menu explains the problem; nothing else to start.
            return
        }
        let watcher = StatusWatcher(devvm: devvm)
        watcher.onSnapshot = { [weak menuBar] machines in
            menuBar?.apply(machines)
        }
        watcher.start()
        menuBar.watcher = watcher
        self.watcher = watcher
    }

    func applicationWillTerminate(_ notification: Notification) {
        watcher?.stop()
    }
}
