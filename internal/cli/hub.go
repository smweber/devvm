package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

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
// "unknown flag" and read as unreachable forever. So this constant is the
// first tag that ships the listing, v0.1.13 (with step 4's cp flags). It is
// deliberately *not* raised for hub forwards: listing, cp and proxying keep
// working against a v0.1.13 hub, so forwards are checked per feature
// instead. `__session` and relay sessions need session.HubForwardsMinVersion
// (the first tag that ships roadmap step 6), and the laptop's hub transport
// says so when an older hub refuses `__session` or an older hub daemon
// answers a relay open without echoing `relay`. Bump this and
// TestHubMinVersionPinned together, so any change is deliberate.
const hubMinVersion = "v0.1.13"

// devvmVersionRe matches cobra's `devvm --version` line. Anchored per line
// (not to the whole output) because the login shell that runs it may print a
// banner first (hub.md §6 solves the same thing for tar streams with a marker).
var devvmVersionRe = regexp.MustCompile(`(?m)^devvm version (\S+)\s*$`)

// checkHubVersion runs `devvm --version` on the hub through its login shell
// and applies hubVersionCheck. It is part of `create --backend hub`, after
// the test connection and before the conf is saved, so a hub that cannot
// run devvm at all is refused up front rather than on the first proxied
// command.
func (a *App) checkHubVersion(ctx context.Context, b backend.Backend, hub *config.Machine) error {
	var out bytes.Buffer
	err := b.Run(ctx, backend.ExecOpts{
		BatchMode: true, Stdin: strings.NewReader(""), Stdout: &out, Stderr: io.Discard,
	}, backend.LoginShellArgv("devvm", "--version")...)
	if err != nil {
		return fmt.Errorf("cannot run devvm on %s (%s): %w — is it installed there and on the login shell's PATH?",
			hub.Name, hub.SSHHost, err)
	}
	warn, err := hubVersionCheck(out.String(), Version)
	if err != nil {
		return fmt.Errorf("hub %s: %w", hub.Name, err)
	}
	if warn != "" {
		fmt.Fprintf(a.Stderr, "devvm: hub %s: %s\n", hub.Name, warn)
	}
	return nil
}

// hubVersionCheck reads the hub's `devvm --version` output and decides:
// refused (err) below hubMinVersion; silent on an exact match with this
// build; otherwise a warning — a newer hub, an older-but-supported hub, a
// `dev` build on either side, or output with no recognizable version line.
func hubVersionCheck(output, local string) (warn string, err error) {
	m := devvmVersionRe.FindStringSubmatch(output)
	if m == nil {
		got := strings.TrimSpace(output)
		if len(got) > 60 {
			got = got[:60] + "…"
		}
		return fmt.Sprintf("could not read its devvm version (got %q); hub commands may fail if it predates %s", got, hubMinVersion), nil
	}
	hub := m[1]
	if !tagRe.MatchString(hub) {
		// "dev" (a plain `go build`, or the cross-builds the test hubs run) or
		// a bare git hash: not orderable, never refused.
		return fmt.Sprintf("its devvm reports %q, not a release; hub commands need %s or newer", hub, hubMinVersion), nil
	}
	if compareVersions(hub, hubMinVersion) < 0 {
		return "", fmt.Errorf("runs devvm %s; hubs need %s or newer (run 'devvm update' there)", hub, hubMinVersion)
	}
	switch {
	case hub == local:
		return "", nil
	case !tagRe.MatchString(local):
		return fmt.Sprintf("runs devvm %s; this host runs a local build (%s)", hub, local), nil
	case compareVersions(hub, local) > 0:
		return fmt.Sprintf("runs a newer devvm (%s) than this host (%s); run 'devvm update' here", hub, local), nil
	default:
		return fmt.Sprintf("runs an older devvm (%s) than this host (%s); run 'devvm update' there", hub, local), nil
	}
}
