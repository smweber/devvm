package hetzner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
)

// The fixtures under testdata/ were recorded from the real API by TestRecord
// (testdata/record.json names what it made), except those whose exchanges
// carry a "note": those are hand-written in the API's documented shape for
// answers a recording can't provoke on demand (a rate limit, a failed action).

func managedSpec(m recordMeta) provider.Spec {
	return provider.Spec{
		Name: m.Server, Type: "cx23", Image: "ubuntu-24.04", Location: "nbg1", SSHKey: m.Key, IPv4: true,
		Labels: map[string]string{provider.LabelManaged: provider.LabelManagedValue, provider.LabelTemplate: "record"},
	}
}

func apiErr(t *testing.T, err error) *APIError {
	t.Helper()
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Fatalf("error leaks the token: %v", err)
	}
	return ae
}

func TestUnauthorized(t *testing.T) {
	_, err := replay(t, "unauthorized").List(context.Background())
	ae := apiErr(t, err)
	if ae.Status != http.StatusUnauthorized || ae.Code != "unauthorized" || ae.Message == "" {
		t.Fatalf("got %+v", ae)
	}
}

func TestRateLimitNotRetried(t *testing.T) {
	// replay fails the test on any request beyond the one recorded.
	_, err := replay(t, "rate_limit").List(context.Background())
	ae := apiErr(t, err)
	if ae.Status != http.StatusTooManyRequests || !IsCode(err, "rate_limit_exceeded") {
		t.Fatalf("got %+v", ae)
	}
	if !strings.Contains(err.Error(), "429 rate_limit_exceeded: "+ae.Message) {
		t.Fatalf("error text %q", err)
	}
}

func TestSSHKeyUploadFindConflict(t *testing.T) {
	m := loadMeta(t)
	ctx := context.Background()
	k, err := replay(t, "ssh_key_upload").EnsureSSHKey(ctx, m.Key, m.PubKey)
	if err != nil || k.Name != m.Key || k.ID == 0 || k.Fingerprint == "" {
		t.Fatalf("upload: %+v, %v", k, err)
	}
	found, err := replay(t, "ssh_key_find").EnsureSSHKey(ctx, m.Key, m.PubKey)
	if err != nil || found.ID != k.ID {
		t.Fatalf("find: %+v, %v", found, err)
	}
	_, err = replay(t, "ssh_key_conflict").EnsureSSHKey(ctx, m.KeyDup, m.PubKey)
	if ae := apiErr(t, err); ae.Status != http.StatusConflict || ae.Code != "uniqueness_error" {
		t.Fatalf("conflict: %+v", ae)
	}
}

func TestSSHKeyDifferentMaterial(t *testing.T) {
	m := loadMeta(t)
	// The find fixture answers with m.PubKey; asking for another key under
	// the same name must refuse rather than use the project's key.
	_, err := replay(t, "ssh_key_find").EnsureSSHKey(context.Background(), m.Key, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOther")
	if err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateWaitsForActions(t *testing.T) {
	m := loadMeta(t)
	s, err := replay(t, "create").Create(context.Background(), managedSpec(m))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != m.Server || s.State != provider.StateRunning || s.Status != "running" ||
		s.Type != "cx23" || s.Location != "nbg1" || !provider.Managed(*s) ||
		s.Labels[provider.LabelTemplate] != "record" || s.Created.IsZero() {
		t.Fatalf("created %+v", s)
	}
	if strings.Contains(s.Address, ":") || s.Address == "" {
		t.Fatalf("address %q, want the public IPv4", s.Address)
	}
}

func TestCreateV6Only(t *testing.T) {
	m := loadMeta(t)
	s, err := replay(t, "create_v6").Create(context.Background(), provider.Spec{
		Name: m.ServerV6, Type: "cx23", Image: "ubuntu-24.04", Location: "nbg1", SSHKey: m.Key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.Managed(*s) {
		t.Fatalf("unlabelled server reads managed: %+v", s.Labels)
	}
	if !strings.HasSuffix(s.Address, "::1") || strings.Contains(s.Address, "/") {
		t.Fatalf("address %q, want the ::1 of the /64", s.Address)
	}
}

func TestCreateConflict(t *testing.T) {
	m := loadMeta(t)
	s, err := replay(t, "create_conflict").Create(context.Background(), managedSpec(m))
	if s != nil {
		t.Fatalf("server returned with a rejected create: %+v", s)
	}
	if ae := apiErr(t, err); ae.Status != http.StatusConflict || ae.Code != "uniqueness_error" || ae.Message == "" {
		t.Fatalf("got %+v", ae)
	}
}

func TestGet(t *testing.T) {
	m := loadMeta(t)
	ctx := context.Background()
	s, err := replay(t, "get_hit").Get(ctx, m.Server)
	if err != nil || s == nil || s.Name != m.Server || s.State != provider.StateRunning {
		t.Fatalf("hit: %+v, %v", s, err)
	}
	if s, err := replay(t, "get_miss").Get(ctx, m.Missing); s != nil || err != nil {
		t.Fatalf("miss: %+v, %v", s, err)
	}
	if s, err := replay(t, "get_off").Get(ctx, m.Server); err != nil || s.State != provider.StateStopped || s.Status != "off" {
		t.Fatalf("off: %+v, %v", s, err)
	}
	if s, err := replay(t, "get_after_delete").Get(ctx, m.Server); s != nil || err != nil {
		t.Fatalf("after delete: %+v, %v", s, err)
	}
}

func TestListPaginates(t *testing.T) {
	m := loadMeta(t)
	all, err := replay(t, "list_pages", WithPerPage(m.PerPage)).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range all {
		names[s.Name] = provider.Managed(s)
	}
	if len(all) != 2 || names[m.Server] != true || names[m.ServerV6] != false || len(names) != 2 {
		t.Fatalf("list = %v", names)
	}
}

func TestListSelector(t *testing.T) {
	m := loadMeta(t)
	got, err := replay(t, "list_selector").ListSelector(context.Background(), m.Selector)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != m.Server || !provider.Managed(got[0]) {
		t.Fatalf("selector list = %+v", got)
	}
}

func TestPowerAndDelete(t *testing.T) {
	m := loadMeta(t)
	ctx := context.Background()
	id := serverID(t, m)
	if err := replay(t, "power_off").Power(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if err := replay(t, "power_on").Power(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err := replay(t, "delete").Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestActionError(t *testing.T) {
	err := replay(t, "action_error").Power(context.Background(), 42, true)
	var ae *ActionError
	if !errors.As(err, &ae) || ae.Code == "" || ae.Message == "" || ae.Command != "start_server" {
		t.Fatalf("err = %v", err)
	}
}

// serverID reads the managed server's id from the create fixture, so the
// power/delete paths are replayed against the id the API really used.
func serverID(t *testing.T, m recordMeta) int64 {
	t.Helper()
	s, err := replay(t, "get_hit").Get(context.Background(), m.Server)
	if err != nil || s == nil {
		t.Fatalf("get_hit: %v", err)
	}
	return s.ID
}

func TestWaitHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":{"id":7,"command":"start_server","status":"running"}}`))
	}))
	defer srv.Close()
	c := New("test-token", WithBaseURL(srv.URL), WithPollInterval(5*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Power(ctx, 1, true)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestMapStatus(t *testing.T) {
	for in, want := range map[string]string{
		"running": "running", "off": "stopped", "initializing": "starting", "starting": "starting",
		"stopping": "stopping", "deleting": "?", "rebuilding": "?", "migrating": "?", "unknown": "?", "": "?", "new": "?",
	} {
		if got := MapStatus(in); got != want {
			t.Errorf("MapStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestV6Address(t *testing.T) {
	for in, want := range map[string]string{
		"2a01:4f8:c17:1234::/64": "2a01:4f8:c17:1234::1",
		"2a01:4f8:c17:1234::1":   "2a01:4f8:c17:1234::1",
	} {
		if got := v6Address(in); got != want {
			t.Errorf("v6Address(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToken(t *testing.T) {
	dir := t.TempDir()
	p := config.Paths{Config: dir, State: dir}
	t.Setenv(TokenEnv, "")
	if _, err := Token(p); !errors.Is(err, ErrNoToken) {
		t.Fatalf("no token: %v", err)
	}
	path := provider.TokenPath(p, "hetzner")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Token(p); !errors.Is(err, ErrNoToken) {
		t.Fatalf("blank file: %v", err)
	}
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := Token(p); err != nil || tok != "from-file" {
		t.Fatalf("file: %q, %v", tok, err)
	}
	t.Setenv(TokenEnv, "from-env")
	if tok, err := Token(p); err != nil || tok != "from-env" {
		t.Fatalf("env: %q, %v", tok, err)
	}
}

func TestLocationFallsBackToDatacenter(t *testing.T) {
	var s apiServer
	if err := json.Unmarshal([]byte(`{"datacenter":{"location":{"name":"fsn1"}}}`), &s); err != nil {
		t.Fatal(err)
	}
	if got := s.server().Location; got != "fsn1" {
		t.Fatalf("Location = %q", got)
	}
}
