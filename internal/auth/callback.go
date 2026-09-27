// Package auth orchestrates github/codex/claude logins inside a guest. On
// smol it is thin (browser-bridge.md §6): the machine's forward daemon
// bridges the host browser and OAuth loopback callbacks over its one agent
// exec. Remote backends keep a private agent session that does both itself.
package auth

// codexFixedPort is codex's well-known callback port. auth pre-binds it before
// `codex login` (no device auth), so the callback is bridged even if codex
// never hands its URL to the shim: on the private session with a listener
// of its own, on the thin path as an exact forward owned by its session. Callback-port parsing itself moved to the
// daemon (session.CallbackPort, roadmap step 7).
const codexFixedPort = 1455
