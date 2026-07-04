package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// Bootstrap-hook kinds.
const (
	KindNone = "none"
	KindURL  = "url"
	KindCmd  = "cmd"
)

// Spec is a parsed bootstrap-hook value: a kind plus a target and args.
type Spec struct {
	Kind   string
	Target string // URL or command path
	Args   []string
}

// ParseSpec parses a machine's bootstrap-hook string:
//
//	url:<URL> [args...]   curl the URL and run it with args (default: bootstrap.sh)
//	cmd:<path> [args...]  run a guest command
//	none                  skip the hook
//
// An empty spec means the default url hook.
func ParseSpec(s string) (Spec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = config.DefaultBootstrapHook
	}
	if s == KindNone {
		return Spec{Kind: KindNone}, nil
	}
	kind, rest, ok := strings.Cut(s, ":")
	if !ok {
		return Spec{}, fmt.Errorf("invalid bootstrap-hook spec %q (want url:/cmd:/none)", s)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return Spec{}, fmt.Errorf("bootstrap-hook %q needs a %s value", s, kind)
	}
	switch kind {
	case KindURL, KindCmd:
		return Spec{Kind: kind, Target: fields[0], Args: fields[1:]}, nil
	default:
		return Spec{}, fmt.Errorf("unknown bootstrap-hook kind %q", kind)
	}
}

// RunHook executes the machine's bootstrap-hook as the dev user (login shell),
// after the caller has already run Prereqs. Reproduces bootstrap_machine's
// curl|bash for the default url spec, but the URL/cmd is now configurable.
func RunHook(ctx context.Context, b backend.Backend, m *config.Machine) error {
	spec, err := ParseSpec(m.BootstrapHook)
	if err != nil {
		return err
	}
	opts := backend.ExecOpts{Login: true, Stream: true}
	switch spec.Kind {
	case KindNone:
		return nil
	case KindURL:
		// argv: bash -lc 'curl -fsSL "$1" | bash -s -- "${@:2}"' _ URL ARGS...
		argv := []string{"bash", "-lc",
			`curl -fsSL "$1" | bash -s -- "${@:2}"`, "_", spec.Target}
		argv = append(argv, spec.Args...)
		return b.Run(ctx, opts, argv...)
	case KindCmd:
		argv := append([]string{spec.Target}, spec.Args...)
		return b.Run(ctx, opts, argv...)
	default:
		return fmt.Errorf("unknown bootstrap-hook kind %q", spec.Kind)
	}
}
