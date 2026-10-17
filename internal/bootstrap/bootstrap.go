package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
)

// Hook kinds.
const (
	KindNone = "none"
	KindURL  = "url"
	KindCmd  = "cmd"
)

// Spec is a parsed hook value: a kind plus a target and args.
type Spec struct {
	Kind   string
	Target string // URL or command path
	Args   []string
}

// ParseSpec parses a template's hook string (an old conf's bootstrap_hook):
//
//	url:<URL> [args...]   curl the URL and run it with args (default: bootstrap.sh)
//	cmd:<path> [args...]  run a guest command
//	none                  skip the hook
//
// An empty spec means the built-in default, none.
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
		return Spec{}, fmt.Errorf("invalid hook spec %q (want url:/cmd:/none)", s)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return Spec{}, fmt.Errorf("hook %q needs a %s value", s, kind)
	}
	switch kind {
	case KindURL, KindCmd:
		return Spec{Kind: kind, Target: fields[0], Args: fields[1:]}, nil
	default:
		return Spec{}, fmt.Errorf("unknown hook kind %q", kind)
	}
}

// RunHook executes a hook spec as the box's user (login shell), after the
// caller has already run Prereqs. Reproduces bootstrap_machine's curl|bash
// for a url spec; the URL/cmd is the template's.
func RunHook(ctx context.Context, b backend.Backend, hook string) error {
	spec, err := ParseSpec(hook)
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
		return fmt.Errorf("unknown hook kind %q", spec.Kind)
	}
}
