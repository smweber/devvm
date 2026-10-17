package hetzner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
)

// TokenEnv overrides the token file, for scripts and for parity with the
// hcloud CLI, which reads the same variable (providers.md §1).
const TokenEnv = "HCLOUD_TOKEN"

// ErrNoToken means neither HCLOUD_TOKEN nor the token file holds a token.
var ErrNoToken = errors.New("hetzner: no API token (set " + TokenEnv + " or run: devvm providers token hetzner)")

// Warn is where Token's mode warning goes; a var so tests can capture it.
var Warn io.Writer = os.Stderr

// Token resolves this host's Hetzner token: HCLOUD_TOKEN first, else the
// one-line file <state>/providers/hetzner.token, trimmed. `providers token`
// writes the file 0600; one any other user can read gets a one-line warning
// on every read, as ssh warns about a private key, but is still used: the
// token is no less valid for it, and refusing would only break `status`.
func Token(p config.Paths) (string, error) {
	if t := strings.TrimSpace(os.Getenv(TokenEnv)); t != "" {
		return t, nil
	}
	path := provider.TokenPath(p, "hetzner")
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(Warn, "devvm: warning: %s is readable by others (mode %04o); run: chmod 600 %s\n", path, fi.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoToken
	}
	if err != nil {
		// The error names the path, never the content.
		return "", fmt.Errorf("hetzner: reading token: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", ErrNoToken
	}
	return t, nil
}
