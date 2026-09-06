package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/spf13/cobra"
)

func (a *App) completeCopyGuestPath(ctx context.Context, name, prefix string) ([]string, cobra.ShellCompDirective) {
	// Load registered machines directly: resolveLive's smol discovery probes
	// have no context and could block completion before the timed query starts.
	m, err := config.Load(a.ConfigDir, name)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	b, err := backend.For(m, a.ConfigDir)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return guestPathCompletions(ctx, b, prefix)
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
