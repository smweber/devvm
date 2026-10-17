package hetzner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
	"github.com/smweber/devvm/internal/provider"
)

// TestRecord re-records the fixtures against the REAL Hetzner API, using the
// token Token() resolves. It creates two cx23 servers (one managed, one
// unlabelled and v6-only, so the fixtures cover pagination, the label
// selector, Managed both ways and the v6 address), a throwaway ssh key,
// exercises every endpoint, and deletes everything it made, also on
// failure. It bills a few minutes of two cx23s.
//
//	DEVVM_HETZNER_RECORD=1 go test ./internal/provider/hetzner -run TestRecord -v -timeout 30m
func TestRecord(t *testing.T) {
	if os.Getenv("DEVVM_HETZNER_RECORD") != "1" {
		t.Skip("set DEVVM_HETZNER_RECORD=1 to record against the real API")
	}
	token, err := Token(config.DefaultPaths())
	if err != nil {
		t.Fatal(err)
	}
	suffix := randHex(t, 3)
	m := recordMeta{
		Server:   "devvm-rec-" + suffix,
		ServerV6: "devvm-rec-" + suffix + "-b",
		Missing:  "devvm-rec-" + suffix + "-absent",
		Key:      "devvm-rec-" + suffix,
		KeyDup:   "devvm-rec-" + suffix + "-dup",
		PubKey:   genPubKey(t, "devvm-rec-"+suffix),
		Selector: "devvm=true,devvm-template=record",
		PerPage:  1,
	}
	t.Logf("recording with server %s, %s and key %s", m.Server, m.ServerV6, m.Key)

	rec := &recorder{token: token, base: http.DefaultTransport, out: map[string][]exchange{}}
	hc := &http.Client{Timeout: 30 * time.Second, Transport: rec}
	c := New(token, WithHTTPClient(hc))
	paged := New(token, WithHTTPClient(hc), WithPerPage(m.PerPage))

	// Cleanup runs on every exit, with a fresh context so a cancelled or
	// timed-out recording still deletes what it made. It sweeps by name
	// rather than trusting the ids it saw: a create can fail after the
	// server exists.
	defer func() {
		rec.use("")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cl := New(token, WithPollInterval(2*time.Second))
		for _, name := range []string{m.Server, m.ServerV6} {
			s, err := cl.Get(ctx, name)
			if err != nil {
				t.Errorf("cleanup: get %s: %v", name, err)
				continue
			}
			if s != nil {
				if err := cl.Delete(ctx, s.ID); err != nil {
					t.Errorf("cleanup: delete server %s: %v", name, err)
				} else {
					t.Logf("cleanup: deleted server %s (%d)", name, s.ID)
				}
			}
		}
		for _, name := range []string{m.Key, m.KeyDup} {
			k, err := cl.findSSHKey(ctx, name)
			if err != nil {
				t.Errorf("cleanup: find key %s: %v", name, err)
				continue
			}
			if k != nil {
				if err := cl.deleteSSHKey(ctx, k.ID); err != nil {
					t.Errorf("cleanup: delete key %s: %v", name, err)
				} else {
					t.Logf("cleanup: deleted key %s (%d)", name, k.ID)
				}
			}
		}
		left, err := cl.List(ctx)
		if err != nil {
			t.Errorf("cleanup: final list: %v", err)
		}
		for _, s := range left {
			if strings.HasPrefix(s.Name, "devvm-rec-") {
				t.Errorf("cleanup: server %s (%d) still exists", s.Name, s.ID)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	rec.use("unauthorized")
	if _, err := New("invalid-token", WithHTTPClient(hc)).List(ctx); err == nil {
		t.Fatal("list with a bad token succeeded")
	}

	rec.use("ssh_key_upload")
	if _, err := c.EnsureSSHKey(ctx, m.Key, m.PubKey); err != nil {
		t.Fatal(err)
	}
	rec.use("ssh_key_find")
	if _, err := c.EnsureSSHKey(ctx, m.Key, m.PubKey); err != nil {
		t.Fatal(err)
	}
	rec.use("ssh_key_conflict")
	if _, err := c.EnsureSSHKey(ctx, m.KeyDup, m.PubKey); err == nil {
		t.Fatal("uploading the same key under a second name succeeded")
	}

	spec := provider.Spec{
		Name: m.Server, Type: "cx23", Image: "ubuntu-24.04", Location: "nbg1", SSHKey: m.Key, IPv4: true,
		Labels: map[string]string{provider.LabelManaged: provider.LabelManagedValue, provider.LabelTemplate: "record"},
	}
	rec.use("create")
	srv, err := c.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	rec.use("create_v6")
	if _, err := c.Create(ctx, provider.Spec{
		Name: m.ServerV6, Type: "cx23", Image: "ubuntu-24.04", Location: "nbg1", SSHKey: m.Key,
	}); err != nil {
		t.Fatal(err)
	}
	rec.use("create_conflict")
	if _, err := c.Create(ctx, spec); err == nil {
		t.Fatal("creating a duplicate name succeeded")
	}

	rec.use("get_hit")
	if s, err := c.Get(ctx, m.Server); err != nil || s == nil {
		t.Fatalf("get: %v, %v", s, err)
	}
	rec.use("get_miss")
	if s, err := c.Get(ctx, m.Missing); err != nil || s != nil {
		t.Fatalf("get missing: %v, %v", s, err)
	}
	rec.use("list_pages")
	if _, err := paged.List(ctx); err != nil {
		t.Fatal(err)
	}
	rec.use("list_selector")
	if _, err := c.ListSelector(ctx, m.Selector); err != nil {
		t.Fatal(err)
	}

	rec.use("power_off")
	if err := c.Power(ctx, srv.ID, false); err != nil {
		t.Fatal(err)
	}
	rec.use("get_off")
	if _, err := c.Get(ctx, m.Server); err != nil {
		t.Fatal(err)
	}
	rec.use("power_on")
	if err := c.Power(ctx, srv.ID, true); err != nil {
		t.Fatal(err)
	}
	rec.use("delete")
	if err := c.Delete(ctx, srv.ID); err != nil {
		t.Fatal(err)
	}
	rec.use("get_after_delete")
	if s, err := c.Get(ctx, m.Server); err != nil || s != nil {
		t.Fatalf("get after delete: %v, %v", s, err)
	}
	rec.use("")

	rec.write(t, m)
}

// recorder is an http.RoundTripper that keeps each exchange under the
// current scenario. It records no headers, and write refuses to save a
// fixture that contains the token anywhere.
type recorder struct {
	token    string
	base     http.RoundTripper
	mu       sync.Mutex
	scenario string
	out      map[string][]exchange
}

func (r *recorder) use(s string) { r.mu.Lock(); r.scenario = s; r.mu.Unlock() }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	resp, err := r.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scenario == "" {
		return resp, nil
	}
	path := strings.TrimPrefix(req.URL.Path, "/v1")
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}
	r.out[r.scenario] = append(r.out[r.scenario], exchange{
		Method:   req.Method,
		Path:     path,
		Request:  rawOrNil(reqBody),
		Status:   resp.StatusCode,
		Response: rawOrNil(scrub(respBody)),
	})
	return resp, nil
}

// scrub removes the one secret Hetzner can return in a body: a create
// without an ssh key gets a root_password (ours always has a key, so it is
// null, but never trust that). Nothing else is normalised.
func scrub(b []byte) []byte {
	var v map[string]any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	if p, ok := v["root_password"]; ok && p != nil {
		v["root_password"] = "REDACTED"
		out, _ := json.Marshal(v)
		return out
	}
	return b
}

func rawOrNil(b []byte) json.RawMessage {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

func (r *recorder) write(t *testing.T, m recordMeta) {
	t.Helper()
	save := func(name string, v any) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if r.token != "" && bytes.Contains(b, []byte(r.token)) {
			t.Fatalf("%s: fixture contains the token; not writing it", name)
		}
		if err := os.WriteFile(filepath.Join("testdata", name+".json"), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, ex := range r.out {
		save(name, ex)
	}
	save("record", m)
}

func randHex(t *testing.T, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// genPubKey makes a throwaway ed25519 authorized_keys line. The private half
// is discarded: nobody logs in to a recording server.
func genPubKey(t *testing.T, comment string) string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, f := range [][]byte{[]byte("ssh-ed25519"), pub} {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(f)))
		b.Write(f)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(b.Bytes()) + " " + comment
}
