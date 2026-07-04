// Package auth orchestrates github/codex/claude logins inside a guest, bridging
// the host browser and OAuth loopback callbacks over the guest agent's channel.
package auth

import (
	"net/url"
	"strconv"
	"strings"
)

// codexFixedPort is codex's well-known callback port, handled up front by a
// dedicated forward rather than the dynamic callback bridge.
const codexFixedPort = 1455

// CallbackPort extracts the loopback callback port from an OAuth authorize URL:
// its redirect_uri query parameter must itself parse as a URL pointing at
// localhost/127.0.0.1:PORT. The URL crosses the guest->host boundary, so only
// the actual parameter is honored — a loopback address elsewhere in the URL
// must not make the host bind a port. Skips codex's fixed 1455 (handled
// separately). Ports maybe_bridge_callback.
func CallbackPort(raw string) (int, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	redirect := u.Query().Get("redirect_uri")
	if redirect == "" {
		return 0, false
	}
	r, err := url.Parse(redirect)
	if err != nil {
		return 0, false
	}
	host := strings.ToLower(r.Hostname())
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return 0, false
	}
	port, err := strconv.Atoi(r.Port())
	if err != nil || port < 1 || port > 65535 || port == codexFixedPort {
		return 0, false
	}
	return port, true
}
