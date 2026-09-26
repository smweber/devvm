// Package hostbrowser opens URLs handed back from a guest in the host's
// browser. Because the URL crosses the guest->host trust boundary it is treated
// as untrusted: only plain http(s) is opened (blocking file://, mailto:, custom
// scheme handlers, and option injection via a leading '-'). Ports host_open_url.
package hostbrowser

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Why Open did not open a URL. Callers word the guest's reply from these
// (browser-bridge.md §1: "handed to a browser" means the opener started).
var (
	ErrRefused  = errors.New("host refused a non-http(s) URL")
	ErrNoOpener = errors.New("no browser opener")
)

// Sanitize returns ok=false for any non-http(s) URL. It no longer rewrites
// anything: codex's `localhost:1455` used to become `127.0.0.1:1455` because
// macOS resolves localhost to ::1 first and the callback listened on IPv4
// only, but every forward now binds ::1 too (browser-bridge.md §4), and a
// redirect_uri must reach the provider verbatim.
func Sanitize(url string) (string, bool) {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url, true
	}
	return url, false
}

// Open opens the URL in the host's default browser. It returns an error when
// the URL is refused (ErrRefused), when no opener is found on PATH
// (ErrNoOpener), or when the opener fails to start; nil means the opener
// process started, not that a page loaded (`open` and `xdg-open` hand the URL
// off and exit, so their status says nothing more). It prints nothing: the
// caller decides where a fallback URL goes.
func Open(url string) error {
	clean, ok := Sanitize(url)
	if !ok {
		return ErrRefused
	}
	opener := findOpener()
	if opener == "" {
		return ErrNoOpener
	}
	cmd := exec.Command(opener, clean)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("browser opener %s failed to start: %w", opener, err)
	}
	go cmd.Wait() // reap it; its exit status says nothing about the browser
	return nil
}

// openers are the candidates for this OS, looked up on PATH in order.
func openers() []string {
	if runtime.GOOS == "darwin" {
		return []string{"open"}
	}
	return []string{"xdg-open", "wslview"}
}

// lookPath is exec.LookPath (a var for tests).
var lookPath = exec.LookPath

func findOpener() string {
	for _, c := range openers() {
		if p, err := lookPath(c); err == nil {
			return p
		}
	}
	return ""
}
