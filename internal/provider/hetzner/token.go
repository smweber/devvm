package hetzner

import (
	"errors"
	"fmt"
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

// Token resolves this host's Hetzner token: HCLOUD_TOKEN first, else the
// one-line file <state>/providers/hetzner.token, trimmed. The file's mode is
// not checked here; `providers token` (step 2) writes it 0600.
func Token(p config.Paths) (string, error) {
	if t := strings.TrimSpace(os.Getenv(TokenEnv)); t != "" {
		return t, nil
	}
	path := provider.TokenPath(p, "hetzner")
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
