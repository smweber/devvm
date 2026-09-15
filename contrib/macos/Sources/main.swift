// DevVM menu bar app entry point. A bare `main.swift` (no @main) keeps the
// bootstrap explicit and lets build.sh compile with plain swiftc, no Xcode.
import AppKit

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
// Accessory: menu bar only, no Dock icon, no app menu. Info.plist also sets
// LSUIElement so the policy holds from the first frame.
app.setActivationPolicy(.accessory)

// `devvm menubar` (and `devvm update`) stop a running copy with SIGTERM when
// the AppleEvent quit is refused. A signal's default action skips
// applicationWillTerminate, which would orphan the `status --watch` child;
// route it through NSApp.terminate instead. The source must stay referenced
// (a global) or it is cancelled immediately.
signal(SIGTERM, SIG_IGN)
let sigterm = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
sigterm.setEventHandler {
    Log.app.notice("SIGTERM received; terminating cleanly")
    NSApp.terminate(nil)
}
sigterm.resume()

app.run()
