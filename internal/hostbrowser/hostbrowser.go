// Package hostbrowser opens login URLs handed back from a guest in the host's
// browser. Because the URL crosses the guest->host trust boundary it is treated
// as untrusted: only plain http(s) is opened (blocking file://, mailto:, custom
// scheme handlers, and option injection via a leading '-'). Ports host_open_url.
package hostbrowser

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Sanitize rewrites the OAuth-common `localhost:1455` to `127.0.0.1:1455` (macOS
// resolves localhost to ::1, but the guest callback is IPv4 loopback) and
// returns ok=false for any non-http(s) URL.
func Sanitize(url string) (string, bool) {
	url = strings.ReplaceAll(url, "localhost:1455", "127.0.0.1:1455")
	url = strings.ReplaceAll(url, "localhost%3A1455", "127.0.0.1%3A1455")
	url = strings.ReplaceAll(url, "localhost%3a1455", "127.0.0.1%3A1455")
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url, true
	}
	return url, false
}

// Open sanitizes then opens the URL in the host's default browser, falling back
// to printing it if no opener is available or the URL is not http(s).
func Open(url string) {
	clean, ok := Sanitize(url)
	if !ok {
		fmt.Printf("devvm: refusing to open non-http(s) URL from guest: %s\n", clean)
		return
	}
	if opener, args := openerFor(clean); opener != "" {
		if exec.Command(opener, args...).Start() == nil {
			return
		}
	}
	fmt.Printf("devvm: open this URL in your browser: %s\n", clean)
}

func openerFor(url string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{url}
	default:
		for _, c := range []string{"xdg-open", "wslview"} {
			if _, err := exec.LookPath(c); err == nil {
				return c, []string{url}
			}
		}
	}
	return "", nil
}
