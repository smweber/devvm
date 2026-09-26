// Package auth orchestrates github/codex/claude logins inside a guest, bridging
// the host browser and OAuth loopback callbacks over the guest agent's channel.
package auth

// codexFixedPort is codex's well-known callback port. auth pre-binds it before
// `codex login` (no device auth), so the callback is bridged even if codex
// never hands its URL to the shim. Callback-port parsing itself moved to the
// daemon (session.CallbackPort, roadmap step 7).
const codexFixedPort = 1455
