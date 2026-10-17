package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// interrupted is the error of a command that a signal stopped after its
// own cleanup ran. Execute prints it, then re-raises sig with the default
// disposition, so the parent sees a death by that signal (a shell script
// looping over copies stops on ^C) rather than an ordinary exit status;
// 128+sig is the status should the re-raise not end the process.
type interrupted struct {
	sig  syscall.Signal
	note string // what the interruption could not stop (afterUpload)
}

func (e *interrupted) Error() string {
	msg := "interrupted (" + e.sig.String() + ")"
	if e.note != "" {
		msg += " " + e.note
	}
	return msg
}

// afterUpload marks a hub cp-in's failure once ssh had read the whole
// archive: the hub's `cp-in --from-tar -` may then have the complete
// stream, and goes on to place it whatever happens here, so an
// interruption says so instead of implying nothing was copied. Without a
// signal it is just err.
type afterUpload struct{ err error }

func (e *afterUpload) Error() string { return e.err.Error() }
func (e *afterUpload) Unwrap() error { return e.err }

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
//
// Only signals the process does not ignore are caught: Notify would
// re-enable an ignored one, so `nohup devvm cp-in` would die on SIGHUP and
// a copy backgrounded with & (the shell ignores SIGINT for it) on the
// terminal's ^C. Execute's re-raise relies on this too: the default
// disposition it restores is the one the process started with.
func interruptible(parent context.Context, fn func(ctx context.Context) error) error {
	catch := caughtSignals()
	if len(catch) == 0 {
		return fn(parent)
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, catch...)
	defer signal.Stop(sigs)
	return runInterruptible(parent, sigs, fn)
}

// caughtSignals is interruptSignals minus those this process ignores.
func caughtSignals() []os.Signal {
	var catch []os.Signal
	for _, s := range interruptSignals {
		if !signal.Ignored(s) {
			catch = append(catch, s)
		}
	}
	return catch
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
	ie := &interrupted{sig: ss}
	var au *afterUpload
	if errors.As(err, &au) {
		ie.note = "after upload; the hub may still complete the copy"
	}
	return ie
}

// uploadedAll reports whether f, handed to a child as its stdin, was read
// to its end: the child shares f's file offset.
func uploadedAll(f *os.File) bool {
	off, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return false
	}
	info, err := f.Stat()
	return err == nil && off >= info.Size()
}
