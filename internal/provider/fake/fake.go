// Package fake is an in-memory provider.Provider for other packages' tests,
// as the fake smolvm is for the smol backend. It keeps Hetzner's semantics
// where devvm relies on them: names are unique per project, labels are set
// only at create, power changes State, and actions finish synchronously.
package fake

import (
	"context"
	"errors"
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
	// CreateBlocks, when set, makes Create store the server and then wait
	// for its context, returning the server with the context's error: the
	// real client's "created, then the wait for the start ran out".
	CreateBlocks bool
	// Cat is what Catalog answers; nil answers ErrNoCatalog, so a test
	// that seeds none exercises the "catalogue unavailable" path (the
	// text inputs, no placement check). SampleCatalog is one to seed.
	Cat *provider.Catalog
	// CatalogErr, when set, is Catalog's answer: an API outage for the
	// catalogue alone.
	CatalogErr error
}

// ErrNoCatalog is Catalog's answer when no Cat was seeded.
var ErrNoCatalog = errors.New("fake: no catalog seeded")

var (
	_ provider.Provider   = (*Provider)(nil)
	_ provider.KeyEnsurer = (*Provider)(nil)
	_ provider.Cataloger  = (*Provider)(nil)
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

// Add seeds a server as if made elsewhere (e.g. one without devvm's label,
// from the console), assigning an id if it has none and a Status matching its State.
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
	// A seeded catalogue is the API's truth here: a type it lists as not
	// available in the location is refused, as Hetzner's 412 is.
	if p.Cat != nil {
		if t, ok := p.Cat.Type(spec.Type); ok && !t.AvailableIn(spec.Location) {
			return nil, fmt.Errorf("fake: server type %s in %s: %w", spec.Type, spec.Location, provider.ErrUnavailable)
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
	if p.CreateBlocks {
		p.mu.Unlock()
		<-ctx.Done()
		p.mu.Lock()
		return &c, fmt.Errorf("fake: waiting for server %d to start: %w", s.ID, ctx.Err())
	}
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

func (p *Provider) Catalog(ctx context.Context) (*provider.Catalog, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter(ctx, "Catalog"); err != nil {
		return nil, err
	}
	switch {
	case p.CatalogErr != nil:
		return nil, p.CatalogErr
	case p.Cat == nil:
		return nil, ErrNoCatalog
	}
	c := *p.Cat
	return &c, nil
}

// SampleCatalog is a small catalogue shaped like Hetzner's (2026-11): cx23
// sold out in fsn1 and not offered in ash, cx43 available in fsn1 only
// (sold out in nbg1, priced but not offered in hel1), an arm type, a
// dedicated one everywhere, a deprecated one, cx33 sold out everywhere,
// and IPv4 priced apart. The values the cli tests' templates use (cx23,
// cx43, fsn1, nbg1) are placements it accepts where the test means them
// to be.
func SampleCatalog() *provider.Catalog {
	eu := func(m, h float64) map[string]provider.Price {
		return map[string]provider.Price{"fsn1": {Monthly: m, Hourly: h}, "nbg1": {Monthly: m, Hourly: h}, "hel1": {Monthly: m, Hourly: h}}
	}
	ccx := eu(12.49, 0.02)
	ccx["ash"] = provider.Price{Monthly: 13.99, Hourly: 0.0224}
	return &provider.Catalog{
		Currency: "EUR",
		Locations: []provider.Location{
			{Name: "fsn1", City: "Falkenstein", Country: "DE"},
			{Name: "nbg1", City: "Nuremberg", Country: "DE"},
			{Name: "hel1", City: "Helsinki", Country: "FI"},
			{Name: "ash", City: "Ashburn, VA", Country: "US"},
		},
		Types: []provider.ServerType{
			{Name: "cx23", CPU: "shared", Arch: "x86", Cores: 2, MemoryGB: 4, DiskGB: 40, Available: []string{"nbg1", "hel1"}, SoldOut: []string{"fsn1"}, Prices: eu(3.49, 0.0056)},
			{Name: "cx43", CPU: "shared", Arch: "x86", Cores: 8, MemoryGB: 16, DiskGB: 160, Available: []string{"fsn1"}, SoldOut: []string{"nbg1"}, Prices: eu(9.49, 0.0152)},
			{Name: "cx33", CPU: "shared", Arch: "x86", Cores: 4, MemoryGB: 8, DiskGB: 80, SoldOut: []string{"fsn1", "nbg1", "hel1"}, Prices: eu(5.49, 0.0088)},
			{Name: "cax11", CPU: "shared", Arch: "arm", Cores: 2, MemoryGB: 4, DiskGB: 40, Available: []string{"fsn1", "nbg1"}, Prices: eu(3.79, 0.0061)},
			{Name: "ccx13", CPU: "dedicated", Arch: "x86", Cores: 2, MemoryGB: 8, DiskGB: 80, Available: []string{"fsn1", "nbg1", "hel1", "ash"}, Prices: ccx},
			{Name: "cx22", CPU: "shared", Arch: "x86", Cores: 2, MemoryGB: 4, DiskGB: 40, Deprecated: true, Available: []string{"nbg1"}, Prices: eu(3.29, 0.0053)},
		},
		IPv4: map[string]provider.Price{"fsn1": {Monthly: 0.5, Hourly: 0.0008}, "nbg1": {Monthly: 0.5, Hourly: 0.0008}, "hel1": {Monthly: 0.5, Hourly: 0.0008}, "ash": {Monthly: 0.5, Hourly: 0.0008}},
	}
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
