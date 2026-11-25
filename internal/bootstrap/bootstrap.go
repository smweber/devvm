package bootstrap

import (
	"bytes"
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
	// KindFile is a script on the host that runs setup, sent to the box
	// over the exec's stdin: the one hook that needs no hosting and nothing
	// in the box first. The cli reads it (it knows the config dir).
	KindFile = "file"
	// KindStarter is devvm's own starter script, the one embedded in the
	// cli that runs setup: a file: hook with nothing on disk. Opt-in like
	// any hook (the built-in is none).
	KindStarter = "starter"
)

// Spec is a parsed hook value: a kind plus a target and args.
type Spec struct {
	Kind   string
	Target string // URL or command path
	Args   []string
}

// ParseSpec parses a template's hook string:
//
//	url:<URL> [args...]   curl the URL and run it with args
//	cmd:<path> [args...]  run a guest command
//	file:<path> [args...] run a script from the host (beside devvm.toml
//	                      unless absolute or ~/), sent over stdin
//	starter               run devvm's starter script (the cli's embedded copy)
//	none                  skip the hook
//
// An empty spec means the built-in default, none.
func ParseSpec(s string) (Spec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		s = config.DefaultBootstrapHook
	}
	if s == KindNone || s == KindStarter {
		return Spec{Kind: s}, nil
	}
	kind, rest, ok := strings.Cut(s, ":")
	if !ok {
		return Spec{}, fmt.Errorf("invalid hook spec %q (want url:/cmd:/file:/starter/none)", s)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return Spec{}, fmt.Errorf("hook %q needs a %s value", s, kind)
	}
	switch kind {
	case KindURL, KindCmd, KindFile:
		return Spec{Kind: kind, Target: fields[0], Args: fields[1:]}, nil
	default:
		return Spec{}, fmt.Errorf("unknown hook kind %q", kind)
	}
}

// RunHook executes a hook spec as the box's user (login shell), after the
// caller has already run Prereqs. Reproduces bootstrap_machine's curl|bash
// for a url spec; the URL/cmd is the template's. script is a file: hook's
// content, read by the caller on the host; it is bash's program on stdin
// (never argv or a guest file), so the hook itself has no terminal input,
// like a url: hook's `curl | bash`.
func RunHook(ctx context.Context, b backend.Backend, hook string, script []byte) error {
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
	case KindFile, KindStarter:
		if len(script) == 0 {
			return fmt.Errorf("hook %q: no script was read", hook)
		}
		opts.Stdin = bytes.NewReader(script)
		argv := append([]string{"bash", "-s", "--"}, spec.Args...)
		return b.Run(ctx, opts, argv...)
	default:
		return fmt.Errorf("unknown hook kind %q", spec.Kind)
	}
}
