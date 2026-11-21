package cli

// hubMinVersion is the oldest devvm a hub may run. The hub surface is a set of
// CLI contracts (`status --plain --local`, the cp tar flags, the proxied
// verbs), so it has a floor, never an exact match: `devvm update` runs on
// one side first all the time. devvm never reads a hub's version: the
// floor is only named, in the one-line refusal for a hub that answers a
// proxied create/setup without `setup` or --inline-template
// (runProxiedShaping).
//
// The floor is v0.2.0, the release that carries simplify.md: a hub
// machine is only ever the hub's labelled smol VM, a proxied
// `create`/`setup` sends the narrowed template flags (hook, memory, disk,
// repos) and nothing for ports, and the hub's listing speaks the label
// model. Laptop and hubs update together across it; nothing here shims an
// older hub (its own cobra error comes through as it said it).
// session.HubForwardsMinVersion and session.HubBridgeMinVersion are the
// same release, named where a relay open finds a hub daemon without
// relays or the bridge. A hub is not version-checked when it is
// registered (`config add` dials nothing). Bump this and
// TestHubMinVersionPinned together, so any change is deliberate.
const hubMinVersion = "v0.2.0"
