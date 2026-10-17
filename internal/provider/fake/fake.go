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

	// Err, when set, is returned by every call: an API outage or a missing
	// token, for exercising callers' fallback paths.
	Err error
	// Calls counts calls by method name ("List", "Create", ...), so a test
	// can assert that a path did not dial the API.
	Calls map[string]int
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
		Calls:   map[string]int{},
	}
}

// Add seeds a server as if made elsewhere (e.g. an unmanaged one from the
// console), assigning an id if it has none. It returns the stored copy.
func (p *Provider) Add(s provider.Server) provider.Server {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.ID == 0 {
		p.nextID++
		s.ID = p.nextID
	}
	if s.State == "" {
		s.State = provider.StateRunning
	}
	s.Labels = copyLabels(s.Labels)
	p.servers[s.ID] = s
	return clone(s)
}

func (p *Provider) enter(method string) error {
	p.Calls[method]++
	return p.Err
}

func (p *Provider) List(ctx context.Context) ([]provider.Server, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter("List"); err != nil {
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
	if err := p.enter("Get"); err != nil {
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
	if err := p.enter("Create"); err != nil {
		return nil, err
	}
	for _, s := range p.servers {
		if s.Name == spec.Name {
			return nil, fmt.Errorf("fake: server name %q is already used", spec.Name)
		}
	}
	if spec.SSHKey != "" {
		if _, ok := p.keys[spec.SSHKey]; !ok {
			return nil, fmt.Errorf("fake: ssh key %q not found", spec.SSHKey)
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
	if spec.IPv4 {
		s.Address = fmt.Sprintf("192.0.2.%d", s.ID%250+1)
	} else {
		s.Address = fmt.Sprintf("2001:db8:%x::1", s.ID)
	}
	p.servers[s.ID] = s
	c := clone(s)
	return &c, nil
}

func (p *Provider) Delete(ctx context.Context, id int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter("Delete"); err != nil {
		return err
	}
	if _, ok := p.servers[id]; !ok {
		return fmt.Errorf("fake: server %d not found", id)
	}
	delete(p.servers, id)
	return nil
}

func (p *Provider) Power(ctx context.Context, id int64, on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.enter("Power"); err != nil {
		return err
	}
	s, ok := p.servers[id]
	if !ok {
		return fmt.Errorf("fake: server %d not found", id)
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
	if err := p.enter("EnsureSSHKey"); err != nil {
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
			return nil, fmt.Errorf("fake: ssh key with the same fingerprint already exists as %q", k.Name)
		}
	}
	p.nextID++
	k := provider.SSHKey{ID: p.nextID, Name: name, PublicKey: pubkey, Fingerprint: fmt.Sprintf("fake:%d", p.nextID)}
	p.keys[name] = k
	return &k, nil
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
