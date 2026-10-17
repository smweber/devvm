package fake

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/smweber/devvm/internal/provider"
)

func TestFakeLifecycle(t *testing.T) {
	ctx := context.Background()
	p := New()
	if _, err := p.EnsureSSHKey(ctx, "k", "ssh-ed25519 AAAA one"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EnsureSSHKey(ctx, "k", "ssh-ed25519 AAAA other-comment"); err != nil {
		t.Fatalf("same key, new comment: %v", err)
	}
	if _, err := p.EnsureSSHKey(ctx, "k", "ssh-ed25519 BBBB"); err == nil {
		t.Fatal("different key under the same name accepted")
	}
	if k, err := p.EnsureSSHKey(ctx, "k2", "ssh-ed25519 AAAA"); err != nil || k.Name != "k" {
		t.Fatalf("same key under a second name: %+v, %v", k, err)
	}

	spec := provider.Spec{Name: "web", Type: "cx23", SSHKey: "k",
		Labels: map[string]string{provider.LabelManaged: provider.LabelManagedValue}}
	s, err := p.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !provider.Managed(*s) || s.State != provider.StateRunning || s.Address == "" {
		t.Fatalf("created %+v", s)
	}
	if _, err := p.Create(ctx, spec); !errors.Is(err, provider.ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	if strings.Contains(s.Address, ":") {
		t.Fatalf("default spec got a v6-only address %q", s.Address)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Create(cancelled, provider.Spec{Name: "other"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled create: %v", err)
	}
	if got, _ := p.Get(ctx, "other"); got != nil {
		t.Fatal("cancelled create made a server")
	}
	s.Labels[provider.LabelManaged] = "false" // must not reach the store
	got, _ := p.Get(ctx, "web")
	if got == nil || !provider.Managed(*got) {
		t.Fatalf("get = %+v", got)
	}
	if err := p.Power(ctx, s.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(ctx, "web"); got.State != provider.StateStopped {
		t.Fatalf("after poweroff state = %q", got.State)
	}
	prod, err := p.Add(provider.Server{Name: "prod", State: provider.StateStopped})
	if err != nil || prod.Status != "off" {
		t.Fatalf("add: %+v, %v", prod, err)
	}
	if _, err := p.Add(provider.Server{Name: "prod"}); !errors.Is(err, provider.ErrConflict) {
		t.Fatalf("duplicate add: %v", err)
	}
	all, _ := p.List(ctx)
	if len(all) != 2 || provider.Managed(all[1]) {
		t.Fatalf("list = %+v", all)
	}
	if err := p.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, s.ID); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if got, err := p.Get(ctx, "web"); got != nil || err != nil {
		t.Fatalf("after delete: %v, %v", got, err)
	}
	p.Err = errors.New("down")
	if _, err := p.List(ctx); err == nil {
		t.Fatal("Err not returned")
	}
	if n := p.CallCount("Get"); n != 4 {
		t.Fatalf("CallCount(Get) = %d", n)
	}
}

// CreateErr: the server is created and handed back with the error, and the
// project lists it (it exists and bills), so a caller can clean it up.
func TestFakeCreateErrKeepsServer(t *testing.T) {
	ctx := context.Background()
	p := New()
	startFailed := errors.New("start action failed")
	p.CreateErr = startFailed
	s, err := p.Create(ctx, provider.Spec{Name: "web", Type: "cx23"})
	if !errors.Is(err, startFailed) {
		t.Fatalf("err = %v, want the CreateErr", err)
	}
	if s == nil || s.Name != "web" || s.ID == 0 {
		t.Fatalf("server = %+v, want the created one with the error", s)
	}
	got, err := p.Get(ctx, "web")
	if err != nil || got == nil || got.ID != s.ID {
		t.Fatalf("Get after a failed Create = %+v, %v; the server must exist", got, err)
	}
	p.CreateErr = nil
	if err := p.Delete(ctx, s.ID); err != nil {
		t.Fatalf("deleting the half-created server: %v", err)
	}
	if got, _ := p.Get(ctx, "web"); got != nil {
		t.Errorf("server survived Delete: %+v", got)
	}
}
