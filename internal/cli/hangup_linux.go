package cli

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// hangupPollMillis is how long one poll waits before rechecking ctx, so the
// watcher ends within that bound once its caller is done.
const hangupPollMillis = 250

// watchHangup returns a context cancelled when the reader of w goes away:
// w is a pipe or socket (*os.File) whose peer closed, which poll(2) reports
// as POLLHUP or POLLERR without anything being written. A hub-side command
// that does all its work before its first write (cp-out --to-tar) needs it:
// its laptop's ^C closes the ssh channel, and without the watch the hub
// would only notice at that first write, after downloading everything.
// Its stdin cannot tell (the laptop sends an empty one). For anything else
// (a buffer, a terminal, a regular file) it watches nothing. Linux only
// (hubs are Linux; hangup_other.go): poll's report of a closed pipe reader
// is not one to rely on elsewhere.
func watchHangup(ctx context.Context, w io.Writer) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	f, ok := w.(*os.File)
	if !ok {
		return ctx, cancel
	}
	// Not f.Fd(): that would switch stdout to blocking mode. The fd stays
	// valid for the watch: stdout is never closed while the command runs.
	rc, err := f.SyscallConn()
	if err != nil {
		return ctx, cancel
	}
	fd := -1
	if rc.Control(func(u uintptr) { fd = int(u) }) != nil || fd < 0 {
		return ctx, cancel
	}
	go func() {
		fds := []unix.PollFd{{Fd: int32(fd)}} // no events: HUP/ERR/NVAL are always reported
		for ctx.Err() == nil {
			n, err := unix.Poll(fds, hangupPollMillis)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return
			}
			if n == 0 {
				continue
			}
			if fds[0].Revents&unix.POLLNVAL != 0 {
				return // not a pollable fd: nothing to watch
			}
			if fds[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
				cancel()
				return
			}
			// Anything else (an fd that is always ready) can never hang up
			// in a way poll would show: stop rather than spin.
			return
		}
	}()
	return ctx, cancel
}
