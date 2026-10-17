package cli

// hubMinVersion is the oldest devvm a hub may run. The hub surface is a set of
// CLI contracts (`status --plain --local`, the cp tar flags, the proxied
// verbs), so it is versioned like --plain already is: by a floor, not by
// equality — an exact-match rule would break hub access every time `devvm
// update` ran on one side first, which the menu bar app makes routine. Any
// other mismatch is a one-line warning, and `dev` on either side is never
// refused (the hubs run cross-builds while this lands).
//
// Hubs are usable only once the hub answers `status --plain --local` (the
// merged listing, roadmap step 3): an older hub would answer the listing with
// "unknown flag" and read as unreachable forever. So the first floor was the
// first tag that ships the listing, v0.1.13 (with step 4's cp flags). It was
// deliberately *not* raised for hub forwards: listing, cp and proxying kept
// working against a v0.1.13 hub, so forwards are checked per feature
// instead. `__session` and relay sessions need session.HubForwardsMinVersion
// (the first tag that ships roadmap step 6), and the laptop's hub transport
// says so when an older hub refuses `__session` or an older hub daemon
// answers a relay open without echoing `relay`.
//
// v0.1.16 is the first tag with `setup` (which replaced `bootstrap`) and
// the explicit template flags a proxied `create`/`setup HUB/NAME
// --template` sends (config.md §7): an older hub would reject both. The
// devvm.toml loader did not change the proxied argv, so it is no bump. A
// hub is no longer version-checked when it is registered (`config add`
// dials nothing): the proxied verbs that need the floor say so themselves.
// Bump this and TestHubMinVersionPinned together, so any change is
// deliberate.
const hubMinVersion = "v0.1.16"
