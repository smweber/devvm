package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/agentrpc"
	"github.com/smweber/devvm/internal/hostbrowser"
)

// The browser bridge (browser-bridge.md §1–2, §4; roadmap step 7). A guest
// program hands the agent a URL (the BROWSER shim); the agent sends it up
// its one exec as an open-url event and waits for a reply; the transport
// hands it to this daemon's loop on events(); handleOpen classifies it,
// binds a forward if it names a loopback port, and delivers it to the most
// recent subscriber, whose answer is relayed back to the guest. The daemon
// never opens a browser itself and never rewrites a URL: the subscriber
// (an `attach`/`shell` on this host, OpenOnHost) does both, on its own host.

// Bridge event kinds.
const (
	KindExternal = "external" // nothing on loopback: opened as is
	KindDirect   = "direct"   // the URL itself is loopback (a dev server): connection-owned forward, may bump
	KindRedirect = "redirect" // a loopback redirect_uri (an OAuth callback): ttl-owned forward, exact port
)

// BridgeEvent is the Data of a session Event carrying an open-url to a
// subscriber. Guest is the loopback port a direct or redirect URL names;
// Bound is the host port this daemon forwarded it to, set only for a local
// subscriber (a relay's far daemon binds on its own transport, hub.md §8).
type BridgeEvent struct {
	Type  string `json:"type"` // agentrpc.EventOpenURL
	URL   string `json:"url"`
	Kind  string `json:"kind"`
	Guest int    `json:"guest,omitempty"`
	Bound int    `json:"bound,omitempty"`
}

// bridgeRequest is one open-url from the transport, with the one reply the
// guest is waiting for. reply must be called exactly once.
type bridgeRequest struct {
	url   string
	reply func(agentrpc.OpenReply)
}

// Bridge limits (browser-bridge.md §4, §7).
const (
	callbackTTL    = 10 * time.Minute // a redirect callback's wall-clock lifetime
	maxEphemeral   = 20               // live bridge forwards per machine
	openRateLimit  = 10               // opens per openRateWindow per machine
	openRateWindow = time.Minute
	minForwardPort = 1024 // privileged ports are never bound
	subscriberWait = 15 * time.Second
	// relaySubscriberWait is the wait on a relay subscriber (40s): its far
	// daemon first binds (a hub add, bounded by hubCallTimeout's 10s, then
	// the -L) and then waits subscriberWait on its own subscriber, plus
	// slack for the link. It stays below agentrpc.AgentReplyWait (pinned by
	// a test), so the guest still hears this daemon's answer, never the
	// agent's own timeout. Only step-8 laptops subscribe through a relay,
	// so no older peer depends on the shorter wait.
	relaySubscriberWait = subscriberWait + 10*time.Second + 15*time.Second
	reasonPending       = "forward pending (host is reconnecting); retry in a moment"
	reasonNoSession     = "no devvm session is attached to this machine"
	reasonNoReply       = "the attached devvm session did not answer in time"
	reasonGone          = "the attached devvm session went away"
	reasonRateLimit     = "too many opens (more than 10 a minute)"
	reasonEphemerals    = "too many browser-bridge forwards on this host (20)"
)

// classified is a URL as the bridge sees it: its kind, the guest loopback
// port it names (direct or redirect), and whether that address is a literal
// [::1], which names the family: a browser will not fall back to 127.0.0.1.
type classified struct {
	kind string
	port int
	v6   bool
}

// isLoopback reports whether a URL host is one of the three loopback
// spellings the bridge forwards.
func isLoopback(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// loopbackPort is the port of a loopback http(s) URL (the scheme's default
// when none is written: 80/443, refused later as privileged).
func loopbackPort(u *url.URL) (int, bool) {
	if !isLoopback(u.Hostname()) {
		return 0, false
	}
	p := u.Port()
	switch {
	case p != "":
	case u.Scheme == "http":
		p = "80"
	case u.Scheme == "https":
		p = "443"
	default:
		return 0, false
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// classify sorts a URL into direct, redirect or external (bridge §1). A URL
// whose own host is loopback is direct; otherwise one whose redirect_uri
// query parameter is a loopback URL with a port is redirect; anything else
// is external.
func classify(raw string) classified {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return classified{kind: KindExternal}
	}
	if port, ok := loopbackPort(u); ok {
		return classified{kind: KindDirect, port: port, v6: u.Hostname() == "::1"}
	}
	if port, ok := CallbackPort(raw); ok {
		r, _ := url.Parse(u.Query().Get("redirect_uri"))
		return classified{kind: KindRedirect, port: port, v6: r.Hostname() == "::1"}
	}
	return classified{kind: KindExternal}
}

// CallbackPort extracts the loopback callback port from an OAuth authorize
// URL: its redirect_uri query parameter must itself parse as a URL pointing
// at localhost/127.0.0.1/[::1]:PORT. The URL crosses the guest->host
// boundary, so only the actual parameter is honored: a loopback address
// elsewhere in the URL must not make the host bind a port. Moved here from
// internal/auth (roadmap step 7) without its codex 1455 exclusion: that port
// was skipped only because `auth` pre-bound it; the daemon binds codex's
// callback like any other (browser-bridge.md §1).
func CallbackPort(raw string) (int, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	redirect := u.Query().Get("redirect_uri")
	if redirect == "" {
		return 0, false
	}
	r, err := url.Parse(redirect)
	if err != nil || !isLoopback(r.Hostname()) {
		return 0, false
	}
	port, err := strconv.Atoi(r.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// handleOpen answers one open-url. It runs on a goroutine of its own, never
// on loop()'s (loop starts it and carries on): it binds, and it waits up to
// subscriberWait on a subscriber, and neither may stall reconnects, the
// retry ticker or the idle rule.
func (d *daemon) handleOpen(req bridgeRequest) {
	req.reply(d.bridgeOpen(req.url))
}

// bridgeOpen is the handler in bridge §1, returning the guest's reply.
func (d *daemon) bridgeOpen(raw string) agentrpc.OpenReply {
	shown := printable(raw)
	if _, ok := hostbrowser.Sanitize(raw); !ok {
		d.logf("%s: refused a non-http(s) URL from the guest: %s", d.name, shown)
		return agentrpc.OpenReply{Err: hostbrowser.ErrRefused.Error()}
	}
	if !d.allowOpen() {
		d.logf("%s: open refused, %s: %s", d.name, reasonRateLimit, shown)
		return agentrpc.OpenReply{Err: reasonRateLimit}
	}
	d.mu.Lock()
	var sub *sess
	if len(d.subs) > 0 {
		sub = d.subs[0]
	}
	state := d.state
	d.mu.Unlock()
	if sub == nil {
		// Logged, not opened: URLs open only while someone is in a devvm
		// session on this machine (bridge §7).
		d.logf("%s: no subscriber; not opened: %s", d.name, shown)
		return agentrpc.OpenReply{Err: reasonNoSession}
	}
	c := classify(raw)
	ev := BridgeEvent{Type: agentrpc.EventOpenURL, URL: raw, Kind: c.kind}
	if c.kind != KindExternal {
		ev.Guest = c.port
		if !sub.relay {
			// A local subscriber: the browser is on this host, so the
			// forward is bound here, before delivery. A relay subscriber
			// gets the event unbound; its daemon runs this same handler.
			if c.port < minForwardPort {
				return agentrpc.OpenReply{Err: fmt.Sprintf("refusing to forward privileged port %d", c.port), Guest: c.port}
			}
			if state != StateUp {
				return agentrpc.OpenReply{Err: reasonPending, Guest: c.port}
			}
			host, why := d.bridgeBind(c, sub)
			if why != "" {
				d.logf("%s: open refused (%s): %s", d.name, why, shown)
				return agentrpc.OpenReply{Err: why, Guest: c.port}
			}
			ev.Bound = host
		}
	}
	data, _ := json.Marshal(ev)
	wait := d.replyWait
	if sub.relay {
		wait = d.relayReplyWait
	}
	r, err := d.deliverTo(sub, data, wait)
	switch {
	case errors.Is(err, errReplyTimeout):
		return agentrpc.OpenReply{Err: reasonNoReply, Guest: ev.Guest, HostPort: ev.Bound}
	case err != nil:
		return agentrpc.OpenReply{Err: reasonGone, Guest: ev.Guest, HostPort: ev.Bound}
	}
	var rep agentrpc.OpenReply
	if json.Unmarshal(r, &rep) != nil {
		return agentrpc.OpenReply{Err: "the attached devvm session sent an unreadable reply", Guest: ev.Guest, HostPort: ev.Bound}
	}
	return rep
}

// bridgeBind forwards a loopback URL's guest port for a local subscriber:
// a direct open with bumping allowed, owned by the subscriber's connection
// (a dev-server tab lives as long as the session that opened it); a
// redirect exact, owned by ttl (the provider needs the port verbatim, so a
// busy one refuses the open). It returns the host port, or why not.
//
// The subscriber was chosen before this runs, outside the lock; addWith
// re-checks that its session is still registered where it records the
// owner, and applies the ephemeral cap in the critical section that would
// create the forward, so neither can be raced.
func (d *daemon) bridgeBind(c classified, sub *sess) (host int, why string) {
	own, info, exact := connOwner(sub.id), ownerInfo{}, false
	if c.kind == KindRedirect {
		own, info, exact = ttlOwner, ttlFor(d.now(), callbackTTL), true
	}
	r, err := d.addWith(c.port, c.port, exact, own, addOpts{info: info, admit: d.ephemeralRoomLocked})
	switch {
	case errors.Is(err, errSubscriberGone):
		return 0, reasonGone
	case errors.Is(err, errAdmitRefused):
		return 0, reasonEphemerals
	case errors.Is(err, errPortBusy) && exact:
		return 0, fmt.Sprintf("callback port %d is in use on the host", c.port)
	case err != nil:
		return 0, fmt.Sprintf("forward for guest port %d failed: %v", c.port, err)
	case r.pending:
		return 0, reasonPending
	}
	if c.v6 && !d.forwardV6(c.port) {
		// The URL names [::1] and this host's forward has no ::1 side: the
		// browser would not fall back to 127.0.0.1 (bridge §4). The owner
		// goes again only if this open added it: an earlier open's owner
		// on the same port (a ttl, a direct tab) is not this open's to take.
		if r.added {
			d.remove(c.port, own)
		}
		what := "address"
		if c.kind == KindRedirect {
			what = "callback address"
		}
		return 0, fmt.Sprintf("%s [::1]:%d is unavailable on the host", what, c.port)
	}
	return r.host, ""
}

// ephemeralRoomLocked is addWith's admit for the bridge: a new forward is
// refused once maxEphemeral live forwards have no conf owner and are held
// by the bridge (a ttl owner, or a local session's connection). A relay's
// forwards are another host's and do not count. d.mu must be held.
func (d *daemon) ephemeralRoomLocked() bool {
	n := 0
	for _, f := range d.forwards {
		if d.bridgeHeldLocked(f) {
			n++
		}
	}
	return n < maxEphemeral
}

// bridgeHeldLocked: f has no conf owner and a ttl or local-session owner.
func (d *daemon) bridgeHeldLocked(f *fwd) bool {
	held := false
	for o := range f.owners {
		switch o.kind {
		case OwnerConf:
			return false
		case OwnerTTL:
			held = true
		case OwnerConnection:
			if s := d.sessions[o.conn]; s != nil && !s.relay {
				held = true
			}
		}
	}
	return held
}

// forwardV6 reports whether guest's forward has its [::1] side bound.
func (d *daemon) forwardV6(guest int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.forwards[guest]
	return f != nil && f.v6
}

// allowOpen is the rate limit: at most openRateLimit opens in any
// openRateWindow per machine (bridge §7). A refused open does not count.
func (d *daemon) allowOpen() bool {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	keep := d.opens[:0]
	for _, t := range d.opens {
		if now.Sub(t) < openRateWindow {
			keep = append(keep, t)
		}
	}
	d.opens = keep
	if len(keep) >= openRateLimit {
		return false
	}
	d.opens = append(d.opens, now)
	return true
}

// printable makes a guest-supplied string safe to write to a terminal or a
// log: control characters (an escape sequence smuggled in a URL) become
// '?'.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}

// OpenOnHost is the subscriber's side of the bridge, for `attach` and
// `shell` (and thin `auth`, roadmap step 8): the session's OnEvent. It
// opens the URL with open (hostbrowser.Open) on this host, rewriting a
// bumped direct URL for the port this host actually forwarded (a
// redirect_uri is never rewritten), and answers with what happened. When
// the opener fails, the URL is printed on term so the user can paste it,
// and the guest is told so. Lines end in \r\n: an interactive session has
// the terminal in raw mode.
func OpenOnHost(term io.Writer, open func(string) error) func(json.RawMessage) json.RawMessage {
	return func(data json.RawMessage) json.RawMessage {
		rep := openOnHost(term, open, data)
		b, _ := json.Marshal(rep)
		return b
	}
}

func openOnHost(term io.Writer, open func(string) error, data json.RawMessage) agentrpc.OpenReply {
	var ev BridgeEvent
	if err := json.Unmarshal(data, &ev); err != nil || ev.Type != agentrpc.EventOpenURL {
		return agentrpc.OpenReply{Err: "this devvm does not know that event"}
	}
	target := ev.URL
	if ev.Kind == KindDirect && ev.Bound > 0 && ev.Bound != ev.Guest {
		target = withPort(ev.URL, ev.Bound)
	}
	rep := agentrpc.OpenReply{URL: target, Guest: ev.Guest, HostPort: ev.Bound}
	if err := open(target); err != nil {
		why := err.Error()
		if errors.Is(err, hostbrowser.ErrNoOpener) {
			why = "no browser opener"
		}
		rep.Err = why
		if !errors.Is(err, hostbrowser.ErrRefused) {
			fmt.Fprintf(term, "\r\ndevvm: could not open a browser (%s); open this URL on this host:\r\n  %s\r\n", why, printable(target))
			rep.Printed = true
		}
		return rep
	}
	rep.Opened = true
	return rep
}

// withPort is raw with its port replaced (a direct URL whose forward bumped).
func withPort(raw string, port int) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
	return u.String()
}
