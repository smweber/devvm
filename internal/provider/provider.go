// Package provider is the seam between devvm and a cloud API that creates and
// destroys VMs (docs/proposals/providers.md). A provider VM has no
// [machines.*] entry: it is discovered from the API, the way smol VMs are
// discovered from `smolvm machine ls`, and the label devvm=true is its
// managed marker. The interface is written from Hetzner alone (§7); a second
// provider is allowed to change it when one is actually used.
package provider

import (
	"context"
	"path/filepath"
	"time"

	"github.com/smweber/devvm/internal/config"
)

// Server is one VM as the provider reports it. State is already mapped to
// the --plain vocabulary (State* below) so callers never learn a provider's
// own status words; Status keeps the provider's word for `status -v` and for
// explaining a "?".
type Server struct {
	ID       int64
	Name     string
	State    string
	Status   string
	Address  string // public IPv4 if the server has one, else its public IPv6 address
	Type     string
	Location string
	Labels   map[string]string
	Created  time.Time
}

// Spec is what Create needs. SSHKey is the name of a key already in the
// project (EnsureSSHKey puts it there). IPv4 false makes a v6-only server,
// which Hetzner bills less for.
type Spec struct {
	Name     string
	Type     string
	Image    string
	Location string
	SSHKey   string
	Labels   map[string]string
	IPv4     bool
}

// SSHKey is a public key stored in the provider project, referenced by name
// at create time.
type SSHKey struct {
	ID          int64
	Name        string
	Fingerprint string
	PublicKey   string
}

// Provider is the lifecycle surface devvm uses. There is deliberately no
// method that changes labels on an existing server (§2): Create sets them at
// birth and nothing in devvm writes them afterwards, so a server only becomes
// managed by the user labelling it in another tool. Every mutating call waits
// for the provider's action to finish, so a returned nil means done.
type Provider interface {
	// List returns every server in the project, managed or not: status is
	// the forgotten-VM guard, so unmanaged servers are listed too.
	List(ctx context.Context) ([]Server, error)
	// Get returns the server with this exact name, or nil, nil when absent.
	Get(ctx context.Context, name string) (*Server, error)
	// Create makes the server and waits for it to be created and started.
	Create(ctx context.Context, s Spec) (*Server, error)
	Delete(ctx context.Context, id int64) error
	Power(ctx context.Context, id int64, on bool) error
}

// KeyEnsurer is the create-time helper beside Provider: make sure a key with
// this name and this public key is in the project, uploading it if missing.
type KeyEnsurer interface {
	EnsureSSHKey(ctx context.Context, name, pubkey string) (*SSHKey, error)
}

// Labels devvm sets at create. LabelManaged=LabelManagedValue is the managed
// marker; LabelTemplate is informational only (nothing reads it).
const (
	LabelManaged      = "devvm"
	LabelManagedValue = "true"
	LabelTemplate     = "devvm-template"
)

// Managed reports whether the server carries devvm's managed label.
func Managed(s Server) bool {
	return s.Labels[LabelManaged] == LabelManagedValue
}

// The --plain state tokens a provider row may carry (providers.md §5).
const (
	StateRunning  = "running"
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateStopping = "stopping"
	StateUnknown  = "?"
)

// TokenPath is where a provider's API token lives: per-host credential state,
// never in devvm.toml or dotfiles (§1 "The token").
func TokenPath(p config.Paths, provider string) string {
	return filepath.Join(p.State, "providers", provider+".token")
}
