package agentrpc

import (
	"fmt"
	"time"
)

// Event is a guest->host push over an "event" stream. Today the only kind is
// open-url (a URL the guest browser shim received). The host answers it
// (browser-bridge.md §2): after the event the agent reads one OpenReply line
// from the same stream and hands it to the shim, which prints what the host
// did. A host older than replies closes the stream instead; the agent then
// has nothing to report, and the shim says only that the URL was sent.
type Event struct {
	Type string `json:"type"`
	URL  string `json:"url,omitempty"`
}

// Event kinds.
const EventOpenURL = "open-url"

// OpenReply is the host's answer to an open-url event: what the subscriber
// that got the URL reported, relayed verbatim by every daemon on the way
// (so on a hub machine it names the laptop's port), or the daemon's own
// refusal when no subscriber got that far.
type OpenReply struct {
	// Opened: the host's browser opener started with URL. Not "the page
	// loaded": `open` and `xdg-open` hand off and exit.
	Opened bool `json:"opened"`
	// URL is what the host opened: the guest's URL, or a direct loopback
	// URL rewritten for the host port it was forwarded to.
	URL string `json:"url,omitempty"`
	// Guest and HostPort name the forward a loopback URL got: the guest
	// port and the host port the browser reaches it on.
	Guest    int `json:"guest,omitempty"`
	HostPort int `json:"host_port,omitempty"`
	// Err is why it was not opened, for the guest to read.
	Err string `json:"err,omitempty"`
	// Printed: the host printed the URL on its terminal for pasting.
	Printed bool `json:"printed,omitempty"`
}

// Message is the line the shim prints for the reply to url (the URL the
// guest asked for).
func (r OpenReply) Message(url string) string {
	if r.Opened {
		opened := r.URL
		if opened == "" {
			opened = url
		}
		msg := "devvm: opened on host -> " + opened
		if r.Guest > 0 && r.HostPort > 0 && r.HostPort != r.Guest {
			msg += fmt.Sprintf("   (guest %d, host port bumped to %d)", r.Guest, r.HostPort)
		}
		return msg
	}
	why := r.Err
	if why == "" {
		why = "no reason given"
	}
	if r.Printed {
		return fmt.Sprintf("devvm: not opened on host (%s); URL printed there", why)
	}
	return fmt.Sprintf("devvm: not opened on host (%s); open it yourself: %s", why, url)
}

// Reply waits, guest side. The daemon bounds its wait on the subscriber
// (15s) and binds before that, so the agent waits longer than both, and the
// shim longer than the agent: whichever end gives up first is the one that
// knows why.
const (
	AgentReplyWait = 60 * time.Second
	ShimReplyWait  = 75 * time.Second
)
