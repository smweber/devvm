package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/spf13/cobra"
)

func (a *App) completeCopyGuestPath(ctx context.Context, name, prefix string) ([]string, cobra.ShellCompDirective) {
	// A remote entry is built from devvm.toml directly: resolve has
	// no context and nothing for it to add. Anything else is a local smol VM,
	// resolved like cp itself (one `smolvm machine ls`, local and quick):
	// the record must have Managed settled, since the exec user depends on
	// it, and cp refuses a VM devvm did not create, so completion does too.
	var b backend.Backend
	if m, err := config.Load(a.Paths, name); err == nil && m.IsRemote() {
		if b, err = backend.For(m, a.Paths); err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	} else if _, b, err = a.resolve(name); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(ctx, completeTimeout())
	defer cancel()
	return guestPathCompletions(ctx, b, prefix)
}

// completeTimeout bounds one guest lookup. Shell completion wants a short cap
// (a stalled TAB is worse than no suggestions), but the macOS menu bar app,
// which drives the same lookup from a text field, pays a cold ssh
// ControlMaster handshake on the first keystroke that can exceed 2s, so it
// sets DEVVM_COMPLETE_TIMEOUT (a Go duration such as "10s").
func completeTimeout() time.Duration {
	if v := os.Getenv("DEVVM_COMPLETE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 2 * time.Second
}

func guestPathCompletions(ctx context.Context, b backend.Backend, prefix string) ([]string, cobra.ShellCompDirective) {
	directive := cobra.ShellCompDirectiveNoFileComp
	var out bytes.Buffer
	err := b.Run(ctx, backend.ExecOpts{
		BatchMode: true, Stdin: strings.NewReader(""), Stdout: &out, Stderr: io.Discard,
	}, "sh", "-c", guestPathCompletionScript, "devvm-complete", prefix)
	if err != nil {
		return nil, directive
	}
	var candidates []string
	for _, candidate := range strings.Split(out.String(), "\x00") {
		// Cobra's completion protocol uses newlines and tabs as separators.
		if candidate == "" || strings.ContainsAny(candidate, "\n\r\t") || !strings.HasPrefix(candidate, prefix) {
			continue
		}
		candidates = append(candidates, candidate)
		if strings.HasSuffix(candidate, "/") {
			directive |= cobra.ShellCompDirectiveNoSpace
		}
	}
	return candidates, directive
}

// Keep the typed prefix in suggestions, expanding ~ only for the lookup.
// No eval or unquoted user input: glob characters in a path are literal.
const guestPathCompletionScript = `set -eu
cd "$HOME"
input=$1
case "$input" in
  '~') printf '~/\000'; exit ;;
  */*) parent=${input%/*}/; leaf=${input##*/} ;;
  *) parent=; leaf=$input ;;
esac
lookup=$parent
case "$lookup" in '~/'*) lookup="$HOME/${lookup#\~/}" ;; esac
[ -n "$lookup" ] || lookup=./
for entry in "$lookup"* "$lookup".[!.]* "$lookup"..?*; do
  [ -e "$entry" ] || [ -L "$entry" ] || continue
  base=${entry##*/}
  case "$base" in "$leaf"*) ;; *) continue ;; esac
  case "$base:$leaf" in .*:) continue ;; esac
  suffix=
  [ ! -d "$entry" ] || suffix=/
  printf '%s\000' "$parent$base$suffix"
done
`
