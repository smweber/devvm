package agentrpc

// Event is a guest->host push over an "event" stream. Today the only kind is
// open-url (a login URL the guest browser shim received); the host sanitizes and
// opens it, replacing the old ~/.devvm/urls file the host polled.
type Event struct {
	Type string `json:"type"`
	URL  string `json:"url,omitempty"`
}

// Event kinds.
const EventOpenURL = "open-url"
