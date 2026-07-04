// Package agentbin embeds the cross-compiled devvm-agent guest binaries and
// installs the right one into a guest on demand. Baking the bytes into the host
// binary (rather than resolving a dist dir at runtime, as the old mux_dist_dir
// did) means `go install github.com/smweber/devvm/cmd/devvm@latest` yields a
// single self-contained artifact.
//
// The binaries under bin/ are produced by build.sh and committed. Regenerate
// them whenever cmd/devvm-agent changes.
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

// Install copies the correct-arch agent into the guest and returns its absolute
// path, idempotently: it skips when the guest already has this exact build
// (sha256 match). Managed boxes (smol, remote-managed) get a root-owned
// /usr/local/bin install; adopted remote-unmanaged hosts get a user-scoped
// ~/.local/bin install (no root, nothing system-level touched). approve, if
// non-nil, is called only when an install is actually needed — returning an
// error aborts, which is how the caller gates writes to an adopt host behind
// explicit consent.
func Install(ctx context.Context, b backend.Backend, m *config.Machine, approve func() error) (string, error) {
	var unameOut bytes.Buffer
	if err := b.Run(ctx, backend.ExecOpts{Stdout: &unameOut, Stderr: os.Stderr}, "uname", "-s", "-m"); err != nil {
		return "", fmt.Errorf("probe guest os/arch: %w", err)
	}
	data, arch, err := binFor(unameOut.String())
	if err != nil {
		return "", err
	}
	want := sha256hex(data)

	home, err := guestHome(ctx, b)
	if err != nil {
		return "", err
	}
	// Path + install strategy follow ownership. Staging sits beside the target so
	// the final mv is an atomic same-dir swap.
	guestPath := managedPath
	staging := home + "/.devvm-agent.new"
	if !m.Managed() {
		guestPath = home + "/.local/bin/devvm-agent"
		staging = home + "/.local/bin/.devvm-agent.new"
	}

	var guestSum bytes.Buffer
	// A missing file yields empty output; that's a cache miss, not an error.
	_ = b.Run(ctx, backend.ExecOpts{Stdout: &guestSum},
		"sh", "-c", "sha256sum "+guestPath+" 2>/dev/null | cut -d' ' -f1")
	if strings.TrimSpace(guestSum.String()) == want {
		return guestPath, nil
	}

	if approve != nil {
		if err := approve(); err != nil {
			return "", err
		}
	}

	fmt.Fprintf(os.Stderr, "devvm: installing devvm-agent (linux/%s) -> %s...\n", arch, guestPath)
	tmp, err := os.CreateTemp("", "devvm-agent-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()

	if m.Managed() {
		if err := b.Copy(tmp.Name(), staging); err != nil {
			return "", fmt.Errorf("copy agent to guest: %w", err)
		}
		// Install as root, then drop the staging copy.
		return guestPath, b.Run(ctx, backend.ExecOpts{User: "root"},
			"sh", "-c", fmt.Sprintf("install -m 0755 %s %s && rm -f %s", staging, guestPath, staging))
	}
	// Unmanaged: user-scoped, no root. Ensure the dir exists before staging into it.
	if err := b.Run(ctx, backend.ExecOpts{}, "sh", "-c", "mkdir -p "+path.Dir(guestPath)); err != nil {
		return "", err
	}
	if err := b.Copy(tmp.Name(), staging); err != nil {
		return "", fmt.Errorf("copy agent to guest: %w", err)
	}
	return guestPath, b.Run(ctx, backend.ExecOpts{},
		"sh", "-c", fmt.Sprintf("chmod 0755 %s && mv %s %s", staging, staging, guestPath))
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
