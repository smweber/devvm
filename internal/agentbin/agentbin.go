// Package agentbin embeds the cross-compiled devvm-agent guest binaries and
// installs the right one into a guest on demand. Baking the bytes into the host
// binary (rather than resolving a dist dir at runtime, as the old mux_dist_dir
// did) means `go install github.com/smweber/devvm/cmd/devvm@latest` yields a
// single self-contained artifact.
//
// The binaries under bin/ are produced by build.sh and committed. Regenerate
// them whenever cmd/devvm-agent changes. Install also writes the BROWSER
// shim (devvm-open-url) beside the agent: one operation, so wherever the
// agent is, the shim is (browser-bridge.md §5).
package agentbin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

//go:embed bin/devvm-agent-linux-amd64 bin/devvm-agent-linux-arm64
var binFS embed.FS

// managedPath is where the agent lives on a box devvm owns: root-owned, on PATH.
// Adopted (unmanaged) hosts instead get a user-scoped ~/.local/bin install.
const managedPath = "/usr/local/bin/devvm-agent"

// binFor returns the embedded agent bytes for a guest's `uname -s -m` (e.g.
// "Linux x86_64"). Only linux binaries are embedded, so a non-Linux remote
// (say a macOS adopt host) fails here with a clear message instead of after
// installing a binary that can't exec.
func binFor(unameSM string) ([]byte, string, error) {
	fields := strings.Fields(unameSM)
	if len(fields) != 2 {
		return nil, "", fmt.Errorf("unexpected guest uname output %q", unameSM)
	}
	osname, guestArch := fields[0], fields[1]
	if osname != "Linux" {
		return nil, "", fmt.Errorf(
			"guest OS is %s, but the devvm-agent only runs on Linux; 'auth' and smol forwards need it (everything else works agent-free)", osname)
	}
	var arch string
	switch guestArch {
	case "aarch64", "arm64":
		arch = "arm64"
	case "x86_64", "amd64":
		arch = "amd64"
	default:
		return nil, "", fmt.Errorf("unsupported guest arch %q for devvm-agent", guestArch)
	}
	name := "bin/devvm-agent-linux-" + arch
	data, err := binFS.ReadFile(name)
	if err != nil {
		return nil, "", fmt.Errorf("embedded agent %s missing: %w", name, err)
	}
	return data, arch, nil
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Paths is where Install put the agent and its BROWSER shim.
type Paths struct {
	Agent string // the devvm-agent binary
	Shim  string // devvm-open-url, beside it: `exec AGENT open-url "$@"`
}

// shimName is the BROWSER shim's file name, beside the agent.
const shimName = "devvm-open-url"

// shimScript is the shim's exact content for an agent at agentPath. The
// path is shell-quoted only when it needs it, so the usual shim is byte
// for byte what earlier installs wrote and its sha still matches.
func shimScript(agentPath string) string {
	return "#!/bin/sh\n" + shimExec(agentPath) + "\n"
}

// shimExec is the shim's one command line.
func shimExec(agentPath string) string {
	return "exec " + shq(agentPath) + ` open-url "$@"`
}

// shq quotes s for a POSIX shell: as is when it is only safe characters
// (every path devvm itself picks), single-quoted otherwise. Guest paths
// come from the guest's $HOME, so a space, a quote or a `%` must not break
// or redirect the install scripts.
func shq(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sumsScript prints one line per path: its sha256, or "-" when it cannot be
// read. Hashing stdin keeps the file name out of sha256sum's output, where
// a space or a backslash would change the line's shape.
func sumsScript(paths ...string) string {
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = shq(p)
	}
	return `for f in ` + strings.Join(q, " ") + `; do h=$( { sha256sum < "$f" | cut -d' ' -f1; } 2>/dev/null ); echo "${h:--}"; done`
}

// Install copies the correct-arch agent into the guest, and writes the
// BROWSER shim (devvm-open-url) beside it, as one idempotent operation
// (browser-bridge.md §5): each half is skipped when the guest already has
// that exact content (sha256 match), so a box whose daemon is up has both.
// Managed boxes (smol, remote-managed) get a root-owned /usr/local/bin
// install; adopted remote-unmanaged hosts get a user-scoped ~/.local/bin
// install (no root, nothing system-level touched). approve, if non-nil, is
// called once, only when something is actually written — returning an error
// aborts, which is how the caller gates writes to an adopt host behind
// explicit consent.
func Install(ctx context.Context, b backend.Backend, m *config.Machine, approve func() error) (Paths, error) {
	var unameOut bytes.Buffer
	if err := b.Run(ctx, backend.ExecOpts{Stdout: &unameOut, Stderr: os.Stderr}, "uname", "-s", "-m"); err != nil {
		return Paths{}, fmt.Errorf("probe guest os/arch: %w", err)
	}
	data, arch, err := binFor(unameOut.String())
	if err != nil {
		return Paths{}, err
	}
	home, err := guestHome(ctx, b)
	if err != nil {
		return Paths{}, err
	}
	// Path + install strategy follow ownership. Staging sits beside the target so
	// the final mv is an atomic same-dir swap.
	p := Paths{Agent: managedPath}
	staging := home + "/.devvm-agent.new"
	if !m.Managed() {
		p.Agent = home + "/.local/bin/devvm-agent"
		staging = home + "/.local/bin/.devvm-agent.new"
	}
	p.Shim = path.Join(path.Dir(p.Agent), shimName)
	shim := shimScript(p.Agent)

	// One probe for both halves. A missing file prints no line; that is a
	// cache miss, not an error (sha256sum's own exit status is ignored).
	var sums bytes.Buffer
	_ = b.Run(ctx, backend.ExecOpts{Stdout: &sums}, "sh", "-c", sumsScript(p.Agent, p.Shim))
	have := parseSums(sums.String())
	needAgent := len(have) < 1 || have[0] != sha256hex(data)
	needShim := len(have) < 2 || have[1] != sha256hex([]byte(shim))
	if !needAgent && !needShim {
		return p, nil
	}
	if approve != nil {
		if err := approve(); err != nil {
			return Paths{}, err
		}
	}
	if needAgent {
		if err := installAgent(ctx, b, m, data, arch, p.Agent, staging); err != nil {
			return Paths{}, err
		}
	}
	if needShim {
		if err := installShim(ctx, b, m, p); err != nil {
			return Paths{}, fmt.Errorf("install the browser shim: %w", err)
		}
	}
	return p, nil
}

// parseSums reads sumsScript's output: one hash (or "-") per line, in
// the order the paths were given.
func parseSums(out string) []string {
	var sums []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			sums = append(sums, line)
		}
	}
	return sums
}

func installAgent(ctx context.Context, b backend.Backend, m *config.Machine, data []byte, arch, guestPath, staging string) error {
	fmt.Fprintf(os.Stderr, "devvm: installing devvm-agent (linux/%s) -> %s...\n", arch, guestPath)
	tmp, err := os.CreateTemp("", "devvm-agent-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	if m.Managed() {
		if err := b.Copy(tmp.Name(), staging); err != nil {
			return fmt.Errorf("copy agent to guest: %w", err)
		}
		// Install as root, then drop the staging copy.
		return b.Run(ctx, backend.ExecOpts{User: "root"},
			"sh", "-c", fmt.Sprintf("install -m 0755 %s %s && rm -f %s", shq(staging), shq(guestPath), shq(staging)))
	}
	// Unmanaged: user-scoped, no root. Ensure the dir exists before staging into it.
	if err := b.Run(ctx, backend.ExecOpts{}, "sh", "-c", "mkdir -p "+shq(path.Dir(guestPath))); err != nil {
		return err
	}
	if err := b.Copy(tmp.Name(), staging); err != nil {
		return fmt.Errorf("copy agent to guest: %w", err)
	}
	return b.Run(ctx, backend.ExecOpts{},
		"sh", "-c", fmt.Sprintf("chmod 0755 %s && mv %s %s", shq(staging), shq(staging), shq(guestPath)))
}

// installShim writes the BROWSER shim beside the agent, following its
// ownership: root-owned beside a managed /usr/local/bin agent, user-owned in
// ~/.local/bin on an adopt host. Staged and renamed, so a login tool never
// runs a half-written shim. printf with the lines as arguments, never as
// its format (a `%` in a path would be a conversion), and not stdin: a
// one-shot smol exec has none. The bytes written equal shimScript, so the
// sha check stays idempotent.
func installShim(ctx context.Context, b backend.Backend, m *config.Machine, p Paths) error {
	tmp := p.Shim + ".new"
	script := fmt.Sprintf(`mkdir -p %s && printf '%%s\n' '#!/bin/sh' %s > %s && chmod 0755 %s && mv %s %s`,
		shq(path.Dir(p.Shim)), shq(shimExec(p.Agent)), shq(tmp), shq(tmp), shq(tmp), shq(p.Shim))
	user := ""
	if m.Managed() {
		user = "root"
	}
	return b.Run(ctx, backend.ExecOpts{User: user}, "sh", "-c", script)
}

// guestHome resolves the connecting user's home directory in the guest, so the
// agent path isn't hardcoded to a "dev" user (adopt hosts have arbitrary users).
func guestHome(ctx context.Context, b backend.Backend) (string, error) {
	var buf bytes.Buffer
	if err := b.Run(ctx, backend.ExecOpts{Stdout: &buf}, "sh", "-c", "echo $HOME"); err != nil {
		return "", fmt.Errorf("probe guest home: %w", err)
	}
	home := strings.TrimSpace(buf.String())
	if home == "" {
		return "", fmt.Errorf("guest $HOME is empty")
	}
	return home, nil
}
