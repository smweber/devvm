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
	"strings"

	"github.com/smweber/devvm/internal/backend"
)

//go:embed bin/devvm-agent-linux-amd64 bin/devvm-agent-linux-arm64
var binFS embed.FS

// GuestPath is where the agent is installed in the guest (root-owned, on PATH).
const GuestPath = "/usr/local/bin/devvm-agent"

// binFor returns the embedded agent bytes for a linux guest arch (uname -m).
func binFor(guestArch string) ([]byte, string, error) {
	var arch string
	switch strings.TrimSpace(guestArch) {
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

// Install copies the correct-arch agent into the guest, idempotently: it skips
// when the guest already has this exact build (sha256 match). Staging goes under
// the dev user's home rather than /tmp, which can be a fresh tmpfs across execs.
func Install(ctx context.Context, b backend.Backend) error {
	var archOut bytes.Buffer
	if err := b.Run(ctx, backend.ExecOpts{Stdout: &archOut, Stderr: os.Stderr}, "uname", "-m"); err != nil {
		return fmt.Errorf("probe guest arch: %w", err)
	}
	data, arch, err := binFor(archOut.String())
	if err != nil {
		return err
	}
	want := sha256hex(data)

	var guestSum bytes.Buffer
	// A missing file yields empty output; that's a cache miss, not an error.
	_ = b.Run(ctx, backend.ExecOpts{Stdout: &guestSum},
		"sh", "-c", "sha256sum "+GuestPath+" 2>/dev/null | cut -d' ' -f1")
	if strings.TrimSpace(guestSum.String()) == want {
		return nil
	}

	fmt.Fprintf(os.Stderr, "devvm: installing devvm-agent (linux/%s)...\n", arch)
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

	staging := "/home/" + backend.DefaultUser + "/.devvm-agent.new"
	if err := b.Copy(tmp.Name(), staging); err != nil {
		return fmt.Errorf("copy agent to guest: %w", err)
	}
	// Install as root, then drop the staging copy.
	return b.Run(ctx, backend.ExecOpts{User: "root"},
		"sh", "-c", fmt.Sprintf("install -m 0755 %s %s && rm -f %s", staging, GuestPath, staging))
}
