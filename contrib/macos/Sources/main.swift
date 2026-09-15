// DevVM menu bar app entry point. A bare `main.swift` (no @main) keeps the
// bootstrap explicit and lets build.sh compile with plain swiftc, no Xcode.
import AppKit

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
// Accessory: menu bar only, no Dock icon, no app menu. Info.plist also sets
// LSUIElement so the policy holds from the first frame.
app.setActivationPolicy(.accessory)
app.run()
