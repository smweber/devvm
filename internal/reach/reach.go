// Package reach is this host's last word on whether a remote box answers:
// one small cache file per box (<State>/cache/reach/NAME), written by a
// probe (backend's sshBackend.Probe) or by a command that has just talked
// to the box, and read by `status` on every snapshot. providers.md §5: a
// remote box's state is fetched when a human looks (a one-shot `status`, a
// refresh request) and never on the snapshot path, so a snapshot only ever
// costs this file read.
//
// The file is derived data, safe to lose (a missing one reads `?`), and
// hand-readable: `key=value` lines,
//
//	state=unreachable
//	verified=1760710000
//	since=1760709400
//	how=probe
//	reason=connect timed out
//
// state is reachable | unreachable | ?; verified is when (Unix seconds)
// the state was last established, and how (master, probe, or the command
// that talked to the box); since is when the state began, kept across
// writes of the same state, so `status -v` can say "unreachable since
// 14:02"; reason says why a probe failed or was not made.
package reach

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/config"
)

// The states, all existing --plain tokens (no new one for the Swift app).
const (
	Reachable   = "reachable"
	Unreachable = "unreachable"
	Unknown     = "?"
)

// How a state was established, besides a command's own name.
const (
	HowMaster = "master" // a live ControlMaster answered `ssh -O check`
	HowProbe  = "probe"  // a TCP connect to the resolved host:port
)

// Entry is one box's cache file.
type Entry struct {
	State    string
	Verified time.Time // zero: nothing was established (never probed, or not probeable)
	Since    time.Time // when State began; zero when unknown
	How      string
	Reason   string
}

// Dir is where the reach cache lives: under cache/, outside run/ and
// the config dir, so a probe's write never wakes the `status --watch` that ran it.
func Dir(p config.Paths) string { return filepath.Join(p.CacheDir(), "reach") }

// Path is one box's cache file.
func Path(p config.Paths, name string) string { return filepath.Join(Dir(p), name) }

// Read returns a box's cached entry. A missing, unreadable or garbled file
// is `?` (never probed): the cache is a hint, never a reason to fail.
func Read(p config.Paths, name string) Entry {
	data, err := os.ReadFile(Path(p, name))
	if err != nil {
		return Entry{State: Unknown}
	}
	return parse(string(data))
}

func parse(text string) Entry {
	e := Entry{State: Unknown}
	unix := func(v string) time.Time {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}
		}
		return time.Unix(n, 0)
	}
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "state":
			if v == Reachable || v == Unreachable || v == Unknown {
				e.State = v
			}
		case "verified":
			e.Verified = unix(v)
		case "since":
			e.Since = unix(v)
		case "how":
			e.How = v
		case "reason":
			e.Reason = v
		}
	}
	return e
}

func (e Entry) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "state=%s\n", e.State)
	if !e.Verified.IsZero() {
		fmt.Fprintf(&b, "verified=%d\n", e.Verified.Unix())
	}
	if !e.Since.IsZero() {
		fmt.Fprintf(&b, "since=%d\n", e.Since.Unix())
	}
	if e.How != "" {
		fmt.Fprintf(&b, "how=%s\n", oneLine(e.How))
	}
	if e.Reason != "" {
		fmt.Fprintf(&b, "reason=%s\n", oneLine(e.Reason))
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Write replaces a box's entry atomically (temp + rename, as the hub
// cache): a `status` reading it mid-write sees the old entry or the new
// one. Since carries over from the previous entry when the state did not
// change, so a box that keeps failing is "unreachable since" its first
// failure, not its latest.
func Write(p config.Paths, name string, e Entry) error {
	if e.Since.IsZero() {
		e.Since = e.Verified
		if prev := Read(p, name); prev.State == e.State && !prev.Since.IsZero() {
			e.Since = prev.Since
		}
	}
	dir := Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(e.text())
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), Path(p, name))
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}

// Record is what a command that has just talked to the box writes:
// reachable now, established by how (the command's name). Best-effort, as
// the cache is: a failed write is never a failed command.
func Record(p config.Paths, name, how string) {
	_ = Write(p, name, Entry{State: Reachable, Verified: time.Now(), How: how})
}

// Remove drops a box's entry (delete): the next machine of the name starts
// at `?`. A missing file is the goal.
func Remove(p config.Paths, name string) error {
	if err := os.Remove(Path(p, name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Plain is the --plain sixth field for a remote row: verified=<unix>:<how>,
// or verified=- when nothing was established.
func (e Entry) Plain() string {
	if e.Verified.IsZero() {
		return "verified=-"
	}
	how := e.How
	if how == "" {
		how = "?"
	}
	return fmt.Sprintf("verified=%d:%s", e.Verified.Unix(), how)
}

// Describe is the `status -v` line (and the menu's text under a row):
// "reachable, verified 12s ago (master)", "unreachable since 14:02
// (connect timed out)", "not probed (ProxyJump)".
func (e Entry) Describe(now time.Time) string {
	switch {
	case e.State == Reachable && !e.Verified.IsZero():
		return fmt.Sprintf("reachable, verified %s ago (%s)", ago(now.Sub(e.Verified)), e.How)
	case e.State == Reachable:
		return "reachable (unverified)" // a hand-edited or partial file: --plain says reachable too
	case e.State == Unreachable:
		since := e.Since
		if since.IsZero() {
			since = e.Verified
		}
		s := "unreachable"
		if !since.IsZero() {
			s += " since " + clock(since, now)
		}
		if e.Reason != "" {
			s += " (" + e.Reason + ")"
		}
		return s
	case e.Reason != "":
		return "not probed (" + e.Reason + ")"
	default:
		return "not probed yet"
	}
}

// ago is a coarse age: 12s, 4m, 3h, 2d.
func ago(d time.Duration) string {
	d = max(d, 0).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// clock is a wall-clock time, with the date once it is not today.
func clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.Format("2006-01-02") == now.Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}
