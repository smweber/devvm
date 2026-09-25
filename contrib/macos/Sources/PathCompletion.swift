import Foundation

/// The answer to one `devvm __complete cp-out NAME TEXT` lookup: the text it
/// was made for and the candidates the CLI printed. Each candidate is a
/// whole path that starts with that text (the CLI filters on it), a
/// directory ends in "/", and they come in the guest shell's glob order.
struct PathCompletion {
    let query: String
    let candidates: [String]

    init(query: String, candidates: [String]) {
        self.query = query
        var seen = Set<String>()
        self.candidates = candidates.filter { c in
            (query.isEmpty || c.hasPrefix(query)) && seen.insert(c).inserted
        }
    }

    /// The candidates for `text`: this answer's own when `text` is its
    /// query, else this answer narrowed locally when that is exactly what a
    /// fresh lookup would return, else nil (a lookup is needed).
    ///
    /// Narrowing is exact when `text` only extends the last path segment
    /// (no "/" added): the guest lists the same directory and keeps the
    /// entries whose name starts with the longer leaf. Two exceptions come
    /// from the CLI's script: dotfiles are left out while the leaf is empty,
    /// so an empty leaf cannot be narrowed to one starting with "."; and a
    /// bare "~" is special-cased to "~/".
    func candidates(for text: String) -> [String]? {
        if text == query { return candidates }
        guard query.isEmpty || text.hasPrefix(query) else { return nil }
        guard query != "~" else { return nil }
        let ext = String(text.dropFirst(query.count))
        if ext.contains("/") { return nil }
        let leafWasEmpty = query.isEmpty || query.hasSuffix("/")
        if leafWasEmpty && ext.hasPrefix(".") { return nil }
        return candidates.filter { $0.hasPrefix(text) }
    }

    static func longestCommonPrefix(_ strings: [String]) -> String {
        guard var prefix = strings.first else { return "" }
        for s in strings.dropFirst() {
            while !prefix.isEmpty && !s.hasPrefix(prefix) {
                prefix.removeLast()
            }
        }
        return prefix
    }
}

/// Shell-style Tab completion as pure value logic; CopyOutPanel owns the
/// AppKit side (the field, the lookups, the hint label).
///
/// One Tab, given the candidates for the current text:
/// - none: nothing happens (the panel beeps);
/// - one: the text becomes it (a directory keeps its "/", ready for the
///   next segment);
/// - several: the text grows to their longest common prefix when that is
///   longer than what is typed;
/// - several with nothing left to grow: each Tab cycles to the next
///   candidate, Shift-Tab to the previous, wrapping around.
/// Any edit by the user ends a cycle (`edited()`).
struct TabCompleter {
    enum Outcome {
        /// No answer covers the current text yet: look it up, and Tab
        /// again when the answer lands.
        case lookup
        case noMatches
        /// The single candidate (possibly equal to the text already).
        case unique(String)
        /// The longest common prefix, and all the candidates it covers.
        case extended(String, [String])
        /// The candidate now shown, its index, and all the candidates.
        case cycled(String, Int, [String])
    }

    private(set) var answer: PathCompletion?
    private var cycle: [String] = []
    private var cycleIndex = -1

    var isCycling: Bool { cycleIndex >= 0 && cycleIndex < cycle.count }

    /// A lookup landed. An in-progress cycle keeps its own candidates.
    mutating func received(_ answer: PathCompletion) {
        self.answer = answer
    }

    /// Drops the cached answer, so the next Tab looks up again. Used after
    /// "no matches": the CLI prints an empty list (exit 0) when the guest
    /// lookup fails or times out, so an empty answer must never stick.
    mutating func forget() {
        answer = nil
    }

    /// The user changed the text (typing, deleting, pasting).
    mutating func edited() {
        cycle = []
        cycleIndex = -1
    }

    mutating func tab(text: String, backwards: Bool) -> Outcome {
        // Still showing a cycled candidate: move to the next one.
        if isCycling && cycle[cycleIndex] == text {
            step(backwards: backwards)
            return .cycled(cycle[cycleIndex], cycleIndex, cycle)
        }
        edited()
        guard let a = answer, let found = a.candidates(for: text) else { return .lookup }
        // Only a lookup made for this very text may say "no matches"; an
        // empty narrowing may come from an answer that was empty in error.
        if found.isEmpty { return text == a.query ? .noMatches : .lookup }
        if found.count == 1 { return .unique(found[0]) }

        let common = PathCompletion.longestCommonPrefix(found)
        if common.count > text.count {
            // Every candidate starts with `common`, and a common prefix of
            // entries in one directory never adds a "/", so this is the
            // exact answer for the grown text: the next Tab cycles at once.
            answer = PathCompletion(query: common, candidates: found)
            return .extended(common, found)
        }
        cycle = found
        cycleIndex = backwards ? found.count - 1 : 0
        // A candidate equal to what is typed ("foo" among "foo", "foobar")
        // would make the first Tab look like it did nothing.
        if cycle[cycleIndex] == text { step(backwards: backwards) }
        return .cycled(cycle[cycleIndex], cycleIndex, cycle)
    }

    private mutating func step(backwards: Bool) {
        let n = cycle.count
        cycleIndex = (cycleIndex + (backwards ? n - 1 : 1)) % n
    }

    /// The last segment of a candidate, as a list shows it: "src/app/" is
    /// "app/", "notes.txt" stays "notes.txt".
    static func leafName(_ path: String) -> String {
        var p = Substring(path)
        let isDir = p.hasSuffix("/")
        if isDir { p = p.dropLast() }
        if let slash = p.lastIndex(of: "/") {
            p = p[p.index(after: slash)...]
        }
        return String(p) + (isDir ? "/" : "")
    }

    /// One line for the hint label under the field: "3 matches: a  b  c",
    /// or while cycling "2 of 5: a  [b]  c  d  e", starting just before the
    /// current one so it stays visible when the line is truncated.
    static func summary(_ candidates: [String], current: Int?) -> String {
        let names = candidates.map { TabCompleter.leafName($0) }
        guard let cur = current, cur >= 0, cur < names.count else {
            return "\(names.count) matches: " + names.joined(separator: "  ")
        }
        let start = max(0, cur - 2)
        let shown = names[start...].enumerated().map { pair -> String in
            pair.offset + start == cur ? "[\(pair.element)]" : pair.element
        }
        return "\(cur + 1) of \(names.count): " + (start > 0 ? "…  " : "") + shown.joined(separator: "  ")
    }
}
