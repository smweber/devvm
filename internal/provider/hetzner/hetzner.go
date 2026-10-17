// Package hetzner is the Hetzner Cloud provider over plain REST. The handful
// of endpoints devvm uses are small, and the hcloud SDK would be the module's
// largest dependency (providers.md §7).
package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/smweber/devvm/internal/provider"
)

// DefaultBaseURL is the Hetzner Cloud API root.
const DefaultBaseURL = "https://api.hetzner.cloud/v1"

// Client implements provider.Provider and provider.KeyEnsurer. The token is
// held only here and sent only in the Authorization header; no error or
// output ever includes it.
type Client struct {
	token   string
	base    string
	http    *http.Client
	poll    time.Duration
	perPage int
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at another API root (tests, recording).
func WithBaseURL(u string) Option { return func(c *Client) { c.base = strings.TrimRight(u, "/") } }

// WithHTTPClient replaces the http.Client (tests, recording).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithPollInterval sets how often an action is polled while waiting.
func WithPollInterval(d time.Duration) Option { return func(c *Client) { c.poll = d } }

// WithPerPage sets the list page size (the API allows up to 50).
func WithPerPage(n int) Option { return func(c *Client) { c.perPage = n } }

var (
	_ provider.Provider   = (*Client)(nil)
	_ provider.KeyEnsurer = (*Client)(nil)
)

// New returns a client for the project the token belongs to. The default
// http.Client has a per-request timeout so a wedged API can't hang a
// command; waits on actions are bounded by the caller's context instead.
func New(token string, opts ...Option) *Client {
	c := &Client{
		token:   token,
		base:    DefaultBaseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
		poll:    time.Second,
		perPage: 50,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// APIError is a non-2xx answer, carrying Hetzner's error code and message
// verbatim with the HTTP status (§8). Rate limits (429, rate_limit_exceeded)
// arrive as one of these and are never retried: the loop that could hit them
// is `status --watch`, which caches instead.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	msg := e.Message
	if e.Code == "" && msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("hetzner: %s %s: %d %s: %s", e.Method, e.Path, e.Status, e.Code, msg)
	}
	return fmt.Sprintf("hetzner: %s %s: %d: %s", e.Method, e.Path, e.Status, msg)
}

// IsCode reports whether err is an APIError with this Hetzner error code
// (e.g. "uniqueness_error", "rate_limit_exceeded", "unauthorized").
func IsCode(err error, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

// ActionError is an action that finished with status "error".
type ActionError struct {
	ID      int64
	Command string
	Code    string
	Message string
}

func (e *ActionError) Error() string {
	return fmt.Sprintf("hetzner: action %s (%d) failed: %s: %s", e.Command, e.ID, e.Code, e.Message)
}

// do sends one request. path is relative to the base and may carry a query;
// it never contains the token, so it is safe in errors.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A *url.Error names the URL, which has no token in it; the header
		// is never part of a transport error.
		return fmt.Errorf("hetzner: %s %s: %w", method, path, unwrapURL(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("hetzner: %s %s: reading response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &APIError{Method: method, Path: path, Status: resp.StatusCode}
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil {
			ae.Code, ae.Message = e.Error.Code, e.Error.Message
		}
		return ae
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hetzner: %s %s: decoding response: %w", method, path, err)
	}
	return nil
}

// unwrapURL drops url.Error's "Get \"https://...\":" prefix, which would
// repeat the method and path we already print.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// Wire shapes: only the fields devvm reads.

type apiServer struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	Created    time.Time         `json:"created"`
	Labels     map[string]string `json:"labels"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
	// Servers now carry location at the top level; datacenter.location is
	// the older shape (the recorded fixtures have no datacenter at all), kept
	// as a fallback.
	Location struct {
		Name string `json:"name"`
	} `json:"location"`
	Datacenter struct {
		Location struct {
			Name string `json:"name"`
		} `json:"location"`
	} `json:"datacenter"`
	PublicNet struct {
		IPv4 *struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
		IPv6 *struct {
			IP string `json:"ip"`
		} `json:"ipv6"`
	} `json:"public_net"`
}

type apiAction struct {
	ID       int64  `json:"id"`
	Command  string `json:"command"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type apiPagination struct {
	Meta struct {
		Pagination struct {
			NextPage *int `json:"next_page"`
		} `json:"pagination"`
	} `json:"meta"`
}

type apiSSHKey struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}

func (s apiServer) server() provider.Server {
	out := provider.Server{
		ID:       s.ID,
		Name:     s.Name,
		State:    MapStatus(s.Status),
		Status:   s.Status,
		Type:     s.ServerType.Name,
		Location: s.Location.Name,
		Labels:   s.Labels,
		Created:  s.Created,
	}
	if out.Location == "" {
		out.Location = s.Datacenter.Location.Name
	}
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	switch {
	case s.PublicNet.IPv4 != nil && s.PublicNet.IPv4.IP != "":
		out.Address = s.PublicNet.IPv4.IP
	case s.PublicNet.IPv6 != nil && s.PublicNet.IPv6.IP != "":
		out.Address = v6Address(s.PublicNet.IPv6.IP)
	}
	return out
}

// v6Address turns the /64 Hetzner assigns ("2a01:4f8:c17:1234::/64") into
// the address the server answers on: Hetzner configures ::1 of the network
// on the primary interface, so that is what ssh must dial.
func v6Address(network string) string {
	ip, _, err := net.ParseCIDR(network)
	if err != nil {
		return network // already a bare address (or unparseable): pass it on
	}
	ip = ip.To16()
	ip[15] |= 1
	return ip.String()
}

// MapStatus maps Hetzner's server status to the --plain state tokens
// (providers.md §5). Anything unlisted, including a status Hetzner adds
// later, is "?" rather than a guess.
func MapStatus(status string) string {
	switch status {
	case "running":
		return provider.StateRunning
	case "off":
		return provider.StateStopped
	case "initializing", "starting":
		return provider.StateStarting
	case "stopping":
		return provider.StateStopping
	default: // deleting, rebuilding, migrating, unknown
		return provider.StateUnknown
	}
}

// List returns every server in the project.
func (c *Client) List(ctx context.Context) ([]provider.Server, error) {
	return c.ListSelector(ctx, "")
}

// ListSelector returns the servers matching a Hetzner label selector
// ("devvm=true", "devvm=true,devvm-template=web"); "" means all.
func (c *Client) ListSelector(ctx context.Context, selector string) ([]provider.Server, error) {
	var out []provider.Server
	for page := 1; ; {
		q := url.Values{}
		if selector != "" {
			q.Set("label_selector", selector)
		}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", strconv.Itoa(c.perPage))
		var resp struct {
			Servers []apiServer `json:"servers"`
			apiPagination
		}
		if err := c.do(ctx, http.MethodGet, "/servers?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		for _, s := range resp.Servers {
			out = append(out, s.server())
		}
		next := resp.Meta.Pagination.NextPage
		if next == nil || *next <= page {
			return out, nil
		}
		page = *next
	}
}

// Get looks the server up by exact name (Hetzner names are unique per
// project). nil, nil when there is none.
func (c *Client) Get(ctx context.Context, name string) (*provider.Server, error) {
	var resp struct {
		Servers []apiServer `json:"servers"`
	}
	if err := c.do(ctx, http.MethodGet, "/servers?name="+url.QueryEscape(name), nil, &resp); err != nil {
		return nil, err
	}
	for _, s := range resp.Servers {
		if s.Name == name {
			srv := s.server()
			return &srv, nil
		}
	}
	return nil, nil
}

func (c *Client) getByID(ctx context.Context, id int64) (*provider.Server, error) {
	var resp struct {
		Server apiServer `json:"server"`
	}
	if err := c.do(ctx, http.MethodGet, "/servers/"+strconv.FormatInt(id, 10), nil, &resp); err != nil {
		return nil, err
	}
	srv := resp.Server.server()
	return &srv, nil
}

// Create makes the server with the key and labels and no user_data:
// cloud-init would be a second shaping system beside `setup` (§3). It waits
// for the create action and any follow-up actions (the start), then re-reads
// the server so the returned State is the one it settled in.
func (c *Client) Create(ctx context.Context, s provider.Spec) (*provider.Server, error) {
	req := map[string]any{
		"name":               s.Name,
		"server_type":        s.Type,
		"image":              s.Image,
		"location":           s.Location,
		"start_after_create": true,
		"public_net": map[string]bool{
			"enable_ipv4": s.IPv4,
			"enable_ipv6": true,
		},
	}
	if s.SSHKey != "" {
		req["ssh_keys"] = []string{s.SSHKey}
	}
	if len(s.Labels) > 0 {
		req["labels"] = s.Labels
	}
	var resp struct {
		Server      apiServer   `json:"server"`
		Action      apiAction   `json:"action"`
		NextActions []apiAction `json:"next_actions"`
	}
	if err := c.do(ctx, http.MethodPost, "/servers", req, &resp); err != nil {
		return nil, err
	}
	for _, a := range append([]apiAction{resp.Action}, resp.NextActions...) {
		if err := c.wait(ctx, a); err != nil {
			srv := resp.Server.server()
			// The server exists: hand it back with the error so the caller
			// can name it (it bills) rather than lose track of it.
			return &srv, err
		}
	}
	return c.getByID(ctx, resp.Server.ID)
}

// Delete deletes the server and waits for the action.
func (c *Client) Delete(ctx context.Context, id int64) error {
	var resp struct {
		Action apiAction `json:"action"`
	}
	if err := c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(id, 10), nil, &resp); err != nil {
		return err
	}
	return c.wait(ctx, resp.Action)
}

// Power powers the server on or off (a hard poweroff, as `stop` on smol is)
// and waits for the action.
func (c *Client) Power(ctx context.Context, id int64, on bool) error {
	verb := "poweroff"
	if on {
		verb = "poweron"
	}
	var resp struct {
		Action apiAction `json:"action"`
	}
	if err := c.do(ctx, http.MethodPost, "/servers/"+strconv.FormatInt(id, 10)+"/actions/"+verb, nil, &resp); err != nil {
		return err
	}
	return c.wait(ctx, resp.Action)
}

// wait polls an action until it succeeds or fails. The first answer may
// already be final (the POST's own copy of the action), which costs no poll.
func (c *Client) wait(ctx context.Context, a apiAction) error {
	for {
		switch a.Status {
		case "success":
			return nil
		case "error":
			ae := &ActionError{ID: a.ID, Command: a.Command}
			if a.Error != nil {
				ae.Code, ae.Message = a.Error.Code, a.Error.Message
			}
			return ae
		}
		t := time.NewTimer(c.poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("hetzner: waiting for action %s (%d): %w", a.Command, a.ID, ctx.Err())
		case <-t.C:
		}
		var resp struct {
			Action apiAction `json:"action"`
		}
		if err := c.do(ctx, http.MethodGet, "/actions/"+strconv.FormatInt(a.ID, 10), nil, &resp); err != nil {
			return err
		}
		a = resp.Action
	}
}

// EnsureSSHKey finds the project key by name or uploads it. A key found
// under the name with different key material is an error rather than a
// silent substitute: the server would come up with a key this host can't
// use. Uploading a key whose fingerprint the project already holds under
// another name fails with Hetzner's uniqueness_error, surfaced verbatim.
func (c *Client) EnsureSSHKey(ctx context.Context, name, pubkey string) (*provider.SSHKey, error) {
	pubkey = strings.TrimSpace(pubkey)
	k, err := c.findSSHKey(ctx, name)
	if err != nil {
		return nil, err
	}
	if k != nil {
		if keyMaterial(k.PublicKey) != keyMaterial(pubkey) {
			return nil, fmt.Errorf("hetzner: ssh key %q in the project is a different key (%s)", name, k.Fingerprint)
		}
		return k, nil
	}
	var resp struct {
		SSHKey apiSSHKey `json:"ssh_key"`
	}
	if err := c.do(ctx, http.MethodPost, "/ssh_keys", map[string]string{"name": name, "public_key": pubkey}, &resp); err != nil {
		return nil, err
	}
	return resp.SSHKey.key(), nil
}

func (c *Client) findSSHKey(ctx context.Context, name string) (*provider.SSHKey, error) {
	var resp struct {
		SSHKeys []apiSSHKey `json:"ssh_keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/ssh_keys?name="+url.QueryEscape(name), nil, &resp); err != nil {
		return nil, err
	}
	for _, k := range resp.SSHKeys {
		if k.Name == name {
			return k.key(), nil
		}
	}
	return nil, nil
}

// deleteSSHKey exists for the recorder's cleanup only: devvm itself never
// removes a key from the project (another host may be using it).
func (c *Client) deleteSSHKey(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, "/ssh_keys/"+strconv.FormatInt(id, 10), nil, nil)
}

func (k apiSSHKey) key() *provider.SSHKey {
	return &provider.SSHKey{ID: k.ID, Name: k.Name, Fingerprint: k.Fingerprint, PublicKey: k.PublicKey}
}

// keyMaterial is the "type base64" part of an authorized_keys line, so a
// differing comment doesn't count as a different key.
func keyMaterial(line string) string {
	f := strings.Fields(line)
	if len(f) < 2 {
		return strings.TrimSpace(line)
	}
	return f[0] + " " + f[1]
}
