package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/smweber/devvm/internal/agentrpc"
)

// clockThreshold is the drift the agent tolerates before it steps the guest
// clock to the host's. Below it is noise (the request's own latency, the
// whole-second resolution of `date -s`), and stepping a clock to fight
// noise would make it jump back and forth.
const clockThreshold = 2 * time.Second

// clockSetWait bounds one `sudo -n date`: -n never prompts, so anything
// slower is wedged.
const clockSetWait = 5 * time.Second

// clockSyncer is the agent's side of the time stream: a smol guest's clock
// does not resync after the host suspends (its init runs no time daemon), so
// the host daemon pushes its clock over the agent exec and this steps the
// guest's to it. At most one step per request; the host decides how often.
type clockSyncer struct {
	now     func() time.Time
	set     func(time.Time) error
	mu      sync.Mutex // one request at a time: two must never both step
	lastErr string     // the last setter error logged, so a refusal is logged once
}

func newClockSyncer() *clockSyncer {
	return &clockSyncer{now: time.Now, set: setClock}
}

// handle compares host with the guest's clock and steps the guest's when
// they are clockThreshold or more apart. The step lands at host plus the
// time since the request arrived; what sudo itself takes (milliseconds) is
// far below the threshold.
func (c *clockSyncer) handle(host time.Time) agentrpc.ClockReply {
	c.mu.Lock()
	defer c.mu.Unlock()
	guest := c.now()
	r := agentrpc.ClockReply{Guest: guest, Drift: host.Sub(guest), Result: agentrpc.ClockOK}
	if r.Drift < clockThreshold && r.Drift > -clockThreshold {
		return r
	}
	target := host.Add(c.now().Sub(guest))
	if err := c.set(target); err != nil {
		r.Result = "error: " + err.Error()
		if msg := err.Error(); msg != c.lastErr {
			c.lastErr = msg
			log.Printf("clock: %s off the host's; could not step it: %v", r.Drift, err)
		}
		return r
	}
	c.lastErr = ""
	r.Result = agentrpc.ClockStepped
	log.Printf("clock: stepped %s to the host's (%s)", r.Drift, target.UTC().Format(time.RFC3339))
	return r
}

// setClock sets the guest's wall clock. The serve agent runs as the managed
// user, not root, so it goes through `sudo -n date` (the NOPASSWD sudo that
// setup's EnsureUser grants; date -s exists in coreutils and busybox alike);
// as root it calls settimeofday itself. Where sudo is missing or wants a
// password, the error says so and the host logs it; nothing else breaks.
func setClock(t time.Time) error {
	if os.Geteuid() == 0 {
		tv := syscall.NsecToTimeval(t.UnixNano())
		return syscall.Settimeofday(&tv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), clockSetWait)
	defer cancel()
	// Whole seconds (busybox's @ takes no fraction), rounded.
	at := fmt.Sprintf("@%d", t.Add(500*time.Millisecond).Unix())
	out, err := exec.CommandContext(ctx, "sudo", "-n", "date", "-u", "-s", at).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("sudo -n date -u -s %s: %v: %s", at, err, msg)
		}
		return fmt.Errorf("sudo -n date -u -s %s: %v", at, err)
	}
	return nil
}
