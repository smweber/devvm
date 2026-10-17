package cli

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/smweber/devvm/internal/backend"
	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/reach"
	"github.com/spf13/cobra"
)

// Remote reach (providers.md §5). A remote box's `status` state is its
// reach cache (package reach), refreshed only when a human looks: a
// one-shot `status` (which the menu bar app runs when its menu opens), a
// refresh request to `status --watch` (`__refresh`), and the commands that
// have just talked to the box. There is no background polling: nothing in
// devvm needs a remote box's state except someone looking at it, and a
// poll would turn every idle watcher into a stream of connects to boxes
// that may be asleep.

// reachTiming bounds one round. deadline is the whole round as the caller
// sees it, the hub listing's (hubTimes.deadline) so a one-shot `status`
// that runs both in parallel is bounded by one of them, not their sum;
// parallel caps the probes in flight, each a couple of local ssh
// processes and one socket. A box's dial is bounded by its connect_timeout
// (backend.probeDialTimeout, capped at backend.ProbeBudget, which is this
// deadline), so a slow box's connect still ends inside the round; one that
// times out reads `?`, not `unreachable`. A var so tests run with short
// values.
type reachTiming struct {
	deadline time.Duration
	parallel int
}

var reachTimes = reachTiming{deadline: 5 * time.Second, parallel: 16}

// remoteConfs is every probeable `[machines.*]` entry (Machine.Probeable),
// for a round. A file that does not parse has none.
func (a *App) remoteConfs() []*config.Machine {
	f, err := config.LoadFile(a.Paths)
	if err != nil {
		return nil
	}
	var out []*config.Machine
	for _, m := range f.Machines() {
		if m.Probeable() {
			out = append(out, m)
		}
	}
	return out
}

// reachRound probes every box in ms in parallel under one deadline and
// leaves the answers in the reach cache, where the snapshot that follows
// reads them. A box whose probe never got a slot before the deadline keeps
// its cached entry: nothing was tested, so nothing is claimed.
func (a *App) reachRound(ctx context.Context, ms []*config.Machine) {
	if len(ms) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, reachTimes.deadline)
	defer cancel()
	sem := make(chan struct{}, max(reachTimes.parallel, 1))
	var wg sync.WaitGroup
	for _, m := range ms {
		b, err := backend.For(m, a.Paths)
		if err != nil {
			continue
		}
		p, ok := b.(backend.Prober)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			p.Probe(ctx)
		}()
	}
	wg.Wait()
}

// noteContact records that a command has just talked to m: reachable now,
// established by how (the command's name), then wakes `status --watch`.
// err is the command's result: success, or an ssh exit status other than
// 255 (ssh's own failure; anything else is the remote command's, so ssh
// got there). mosh reports no such split, so only its success counts. A
// failure writes nothing: an auth or host-key refusal is not
// `unreachable`, and the next probe will say what it is.
func (a *App) noteContact(m *config.Machine, how, transport string, err error) {
	if m == nil || !m.Probeable() {
		return
	}
	if err != nil {
		var ee *exec.ExitError
		if transport == config.TransportMosh || !errors.As(err, &ee) || ee.ExitCode() == 255 || ee.ExitCode() < 0 {
			return
		}
	}
	reach.Record(a.Paths, m.Name, how)
	config.TouchChanged(a.Paths)
}

// refreshCmd is the hidden `__refresh`: ask every `status --watch` on this
// host for one live round (RequestRefresh's marker). It returns at once;
// the watcher emits a block when the round ends, if anything changed. It is
// for scripts and other consumers of the watch stream: the menu bar app
// does not need it, since the one-shot `status --plain` it runs when its
// menu opens probes the remotes itself and applies that output directly.
func (a *App) refreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__refresh",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.RequestRefresh(a.Paths)
		},
	}
}
