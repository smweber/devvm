package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// interrupted is the error of a command that a signal stopped after its
// own cleanup ran. Execute prints it, then re-raises sig with the default
// disposition, so the parent sees a death by that signal (a shell script
// looping over copies stops on ^C) rather than an ordinary exit status;
// 128+sig is the status should the re-raise not end the process.
type interrupted struct{ sig syscall.Signal }

func (e *interrupted) Error() string { return "interrupted (" + e.sig.String() + ")" }

// interruptSignals are what interruptible catches: ^C, kill, and a hangup
// (a closed terminal, or an ssh -t session's end on a hub).
var interruptSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// interruptible runs fn with a context that a termination signal cancels,
// instead of letting the signal end the process on the spot. Without it a
// ^C mid-copy killed devvm before any deferred cleanup ran, leaving the
// laptop-side spool (os.MkdirTemp under $TMPDIR, the whole archive) behind.
// Cancelling the context kills the host command running under it (runHost's
// exec.CommandContext); on a terminal ^C that command has had the SIGINT
// too. fn then unwinds through its defers: the spool goes, and a guest
// staging directory is removed by copyArchive's own cleanup, which runs on
// a fresh context for exactly this reason.
//
// A signal that arrives once fn already succeeded changes nothing: the
// work is done, so the command exits 0.
func interruptible(parent context.Context, fn func(ctx context.Context) error) error {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, interruptSignals...)
	defer signal.Stop(sigs)
	return runInterruptible(parent, sigs, fn)
}

// runInterruptible is interruptible with the signal source given, for tests.
// Later signals while fn unwinds are absorbed: the cleanup is short and
// bounded by the host commands' own kill, and a second ^C must not cut it.
//
// A terminal ^C reaches the host command too, which may die and fail fn
// before the watcher below has taken the signal; so once fn returns, the
// watcher is stopped and sigs looked at once more, and that failure still
// reads as the interruption it was.
func runInterruptible(parent context.Context, sigs <-chan os.Signal, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var got os.Signal
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		select {
		case got = <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()
	err := fn(ctx)
	cancel()
	<-watched
	if got == nil {
		select {
		case got = <-sigs:
		default:
		}
	}
	if err == nil || got == nil {
		return err
	}
	ss, ok := got.(syscall.Signal)
	if !ok {
		ss = syscall.SIGINT
	}
	return &interrupted{sig: ss}
}
