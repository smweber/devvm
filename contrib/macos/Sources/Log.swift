import Foundation
import os

/// The app has no window, so it narrates to the unified log instead. One
/// subsystem, one category per concern, so a reader can filter to the part
/// that misbehaved:
///
///     log stream --predicate 'subsystem == "com.smweber.devvm.menubar"' --level info
///
/// Levels: `.notice` for normal events (persisted, shown by default),
/// `.error` for failures, `.debug` for the chatty per-keystroke completion
/// lookups (`--level debug` to see them). Every interpolated value is marked
/// `.public` on purpose: os_log redacts dynamic strings by default, which
/// would turn a path or machine name into `<private>` in the user's own log
/// and defeat the point. Never log a command's full stdout; stderr's last
/// line is enough and is what the notification shows anyway.
enum Log {
    private static let subsystem = Bundle.main.bundleIdentifier ?? "com.smweber.devvm.menubar"
    static let app = Logger(subsystem: subsystem, category: "app")
    static let devvm = Logger(subsystem: subsystem, category: "devvm")   // every CLI invocation
    static let drop = Logger(subsystem: subsystem, category: "drop")
    static let status = Logger(subsystem: subsystem, category: "status") // watch child + snapshots
    static let menu = Logger(subsystem: subsystem, category: "menu")     // user actions
    static let notify = Logger(subsystem: subsystem, category: "notify")
    static let update = Logger(subsystem: subsystem, category: "update")
}
