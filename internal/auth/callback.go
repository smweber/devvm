// Package auth orchestrates github/codex/claude logins inside a guest, bridging
// the host browser and OAuth loopback callbacks over the guest agent's channel.
package auth

import (
	"regexp"
	"strconv"
	"strings"
)

// codexFixedPort is codex's well-known callback port, handled up front by a
// dedicated forward rather than the dynamic callback bridge.
const codexFixedPort = 1455

var loopbackPortRe = regexp.MustCompile(`(?i)(localhost|127\.0\.0\.1)(%3a|:)([0-9]{2,5})`)

// CallbackPort extracts the loopback callback port from an OAuth authorize URL,
// if it has a redirect_uri pointing at localhost/127.0.0.1:PORT. It skips codex's
// fixed 1455 (handled separately). Ports maybe_bridge_callback.
func CallbackPort(url string) (int, bool) {
	if !strings.Contains(url, "redirect_uri=") {
		return 0, false
	}
	m := loopbackPortRe.FindStringSubmatch(url)
	if m == nil {
		return 0, false
	}
	port, err := strconv.Atoi(m[3])
	if err != nil || port == codexFixedPort {
		return 0, false
	}
	return port, true
}
