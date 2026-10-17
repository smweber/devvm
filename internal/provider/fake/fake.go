// Package fake is an in-memory provider.Provider for other packages' tests,
// as the fake smolvm is for the smol backend. It keeps Hetzner's semantics
// where devvm relies on them: names are unique per project, labels are set
// only at create, power changes State, and actions finish synchronously.
package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/smweber/devvm/internal/provider"
)

// Provider is the fake. The zero value is not usable; call New.
type Provider struct {
	mu      sync.Mutex
	servers map[int64]provider.Server
	keys    map[string]provider.SSHKey
	nextID  int64

	calls map[string]int
	specs []provider.Spec

	// Err, when set, is returned by every call: an API outage or a missing
	// token, for exercising callers' fallback paths. Set it before sharing
	// the fake across goroutines.
	Err error
	// CreateErr, when set, is returned by Create after the server has been
	// stored and is returned with it: the real provider's "created, but the
	// start (or the read-back) failed" case, where the server exists and
	// bills though the call failed.
	CreateErr error
}

var (
	_ provider.Provider   = (*Provider)(nil)
	_ provider.KeyEnsurer = (*Provider)(nil)
)

// New returns an empty project.
func New() *Provider {
	return &Provider{
		servers: map[int64]provider.Server{},
		keys:    map[string]provider.SSHKey{},
		nextID:  1000,
		calls:   map[string]int{},
	}
}

// CallCount is how many times a method ("List", "Create", ...) was called,
// so a test can assert that a path did not dial the API.
func (p *Provider) CallCount(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[method]
}

// Add seeds a server as if made elsewhere (e.g. an unmanaged one from the
// console), assigning an id if it has none and a Status matching its State.
// A name already in the project is ErrConflict, as on the real API.
func (p *Provider) Add(s provider.Server) (provider.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, o := range p.servers {
		if o.Name == s.Name {
			return provider.Server{}, fmt.Errorf("fake: server name %q: %w", s.Name, provider.ErrConflict)
		}
	}
	if s.ID == 0 {
		p.nextID++
		s.ID = p.nextID
	}
	if s.State == "" {
		s.State = provider.StateRunning
	}
	if s.Status == "" {
		s.Status = statusFor[s.State]
	}
	s.Labels = copyLabels(s.Labels)
	p.servers[s.ID] = s
	return clone(s), nil
}

// statusFor is the Hetzner word for each state, so a seeded server reads
// like one the real client returned.
var statusFor = map[string]string{
	provider.StateRunning:  "running",
	provider.StateStopped:  "off",
	provider.StateStarting: "starting",
	provider.StateStopping: "stopping",
	provider.StateUnknown:  "unknown",
}

func (p *Provider) enter(ctx context.Context, method string) error {
	p.calls[method]++
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.Err
}

func (p *Provider) List(ctx context.Context) ([]provider.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter(ctx, "List"); err != nil {
		return nil, err
	}
	out := make([]provider.Server, 0, len(p.servers))
	for _, s := range p.servers {
		out = append(out, clone(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (p *Provider) Get(ctx context.Context, name string) (*provider.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter(ctx, "Get"); err != nil {
		return nil, err
	}
	for _, s := range p.servers {
		if s.Name == name {
			c := clone(s)
			return &c, nil
		}
	}
	return nil, nil
}

func (p *Provider) Create(ctx context.Context, spec provider.Spec) (*provider.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter(ctx, "Create"); err != nil {
		return nil, err
	}
	p.specs = append(p.specs, spec)
	for _, s := range p.servers {
		if s.Name == spec.Name {
			return nil, fmt.Errorf("fake: server name %q is already used: %w", spec.Name, provider.ErrConflict)
		}
	}
	if spec.SSHKey != "" {
		if _, ok := p.keys[spec.SSHKey]; !ok {
			return nil, fmt.Errorf("fake: ssh key %q: %w", spec.SSHKey, provider.ErrNotFound)
		}
	}
	p.nextID++
	s := provider.Server{
		ID:       p.nextID,
		Name:     spec.Name,
		State:    provider.StateRunning,
		Status:   "running",
		Type:     spec.Type,
		Location: spec.Location,
		Labels:   copyLabels(spec.Labels),
		Created:  time.Now(),
	}
	// Documentation ranges (RFC 5737 / RFC 3849), so a test can't dial them.
	if !spec.IPv6Only {
		s.Address = fmt.Sprintf("192.0.2.%d", s.ID%250+1)
	} else {
		s.Address = fmt.Sprintf("2001:db8:%x::1", s.ID)
	}
	p.servers[s.ID] = s
	c := clone(s)
	return &c, p.CreateErr
}

func (p *Provider) Delete(ctx context.Context, id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter(ctx, "Delete"); err != nil {
		return err
	}
	if _, ok := p.servers[id]; !ok {
		return fmt.Errorf("fake: server %d: %w", id, provider.ErrNotFound)
	}
	delete(p.servers, id)
	return nil
}

func (p *Provider) Power(ctx context.Context, id int64, on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter(ctx, "Power"); err != nil {
		return err
	}
	s, ok := p.servers[id]
	if !ok {
		return fmt.Errorf("fake: server %d: %w", id, provider.ErrNotFound)
	}
	if on {
		s.State, s.Status = provider.StateRunning, "running"
	} else {
		s.State, s.Status = provider.StateStopped, "off"
	}
	p.servers[id] = s
	return nil
}

func (p *Provider) EnsureSSHKey(ctx context.Context, name, pubkey string) (*provider.SSHKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter(ctx, "EnsureSSHKey"); err != nil {
		return nil, err
	}
	pubkey = strings.TrimSpace(pubkey)
	if k, ok := p.keys[name]; ok {
		if material(k.PublicKey) != material(pubkey) {
			return nil, fmt.Errorf("fake: ssh key %q in the project is a different key", name)
		}
		c := k
		return &c, nil
	}
	for _, k := range p.keys {
		if material(k.PublicKey) == material(pubkey) {
			// Same material under another name: use it, as the client does.
			c := k
			return &c, nil
		}
	}
	p.nextID++
	k := provider.SSHKey{ID: p.nextID, Name: name, PublicKey: pubkey, Fingerprint: fmt.Sprintf("fake:%d", p.nextID)}
	p.keys[name] = k
	return &k, nil
}

func (p *Provider) FindSSHKey(ctx context.Context, name string) (*provider.SSHKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter(ctx, "FindSSHKey"); err != nil {
		return nil, err
	}
	if k, ok := p.keys[name]; ok {
		c := k
		return &c, nil
	}
	return nil, nil
}

// AddKey seeds a project key as if uploaded elsewhere (the console, another
// host), for `ssh_key = NAME`.
func (p *Provider) AddKey(name, pubkey string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	p.keys[name] = provider.SSHKey{ID: p.nextID, Name: name, PublicKey: strings.TrimSpace(pubkey), Fingerprint: fmt.Sprintf("fake:%d", p.nextID)}
}

// Specs is every Spec Create was called with, in order, so a test can
// check the placement and labels a command asked for.
func (p *Provider) Specs() []provider.Spec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Spec(nil), p.specs...)
}

func material(line string) string {
	f := strings.Fields(line)
	if len(f) < 2 {
		return line
	}
	return f[0] + " " + f[1]
}

func copyLabels(l map[string]string) map[string]string {
	out := make(map[string]string, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

// clone hands out copies so a caller mutating a returned Server's labels
// can't relabel the stored one: the fake must not offer the label write the
// real interface deliberately lacks.
func clone(s provider.Server) provider.Server {
	s.Labels = copyLabels(s.Labels)
	return s
}
