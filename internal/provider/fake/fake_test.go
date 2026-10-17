package fake

import (
	"context"
	"errors"
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
	if _, err := p.EnsureSSHKey(ctx, "k2", "ssh-ed25519 AAAA"); err == nil {
		t.Fatal("same key under a second name accepted")
	}

	spec := provider.Spec{Name: "web", Type: "cx23", SSHKey: "k", IPv4: true,
		Labels: map[string]string{provider.LabelManaged: provider.LabelManagedValue}}
	s, err := p.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !provider.Managed(*s) || s.State != provider.StateRunning || s.Address == "" {
		t.Fatalf("created %+v", s)
	}
	if _, err := p.Create(ctx, spec); err == nil {
		t.Fatal("duplicate name accepted")
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
	p.Add(provider.Server{Name: "prod"})
	all, _ := p.List(ctx)
	if len(all) != 2 || provider.Managed(all[1]) {
		t.Fatalf("list = %+v", all)
	}
	if err := p.Delete(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Get(ctx, "web"); got != nil || err != nil {
		t.Fatalf("after delete: %v, %v", got, err)
	}
	p.Err = errors.New("down")
	if _, err := p.List(ctx); err == nil {
		t.Fatal("Err not returned")
	}
	if p.Calls["Get"] != 3 {
		t.Fatalf("Calls[Get] = %d", p.Calls["Get"])
	}
}
