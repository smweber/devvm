package hetzner

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// exchange is one recorded request and its answer. Headers are not
// recorded at all, so the Authorization header (the token) can't leak into
// a fixture; Path is relative to the API root and carries the query.
type exchange struct {
	Note     string          `json:"note,omitempty"`
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Request  json.RawMessage `json:"request,omitempty"`
	Status   int             `json:"status"`
	Response json.RawMessage `json:"response,omitempty"`
}

// recordMeta names what the recording created, so the replay tests can
// assert on the same names and ids without hardcoding a random suffix.
type recordMeta struct {
	Server   string `json:"server"`
	ServerV6 string `json:"server_v6"`
	Missing  string `json:"missing"`
	Key      string `json:"key"`
	KeyDup   string `json:"key_dup"`
	PubKey   string `json:"pubkey"`
	Selector string `json:"selector"`
	PerPage  int    `json:"per_page"`
}

func loadMeta(t *testing.T) recordMeta {
	t.Helper()
	var m recordMeta
	b, err := os.ReadFile(filepath.Join("testdata", "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func loadFixture(t *testing.T, name string) []exchange {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex []exchange
	if err := json.Unmarshal(b, &ex); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return ex
}

// replay serves a fixture strictly in order: each request must be the next
// recorded one (method, path+query, and a JSON-equal body), so the test
// proves the client makes the calls the real API saw, not just that it
// parses answers. Polling is deterministic under replay because the client
// decides to poll again only from the answers.
func replay(t *testing.T, name string, opts ...Option) *Client {
	t.Helper()
	ex := loadFixture(t, name)
	var mu sync.Mutex
	next := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if next >= len(ex) {
			t.Errorf("%s: unexpected extra request %s %s", name, r.Method, r.URL.RequestURI())
			http.Error(w, "no more fixtures", http.StatusTeapot)
			return
		}
		e := ex[next]
		next++
		if r.Method != e.Method || r.URL.RequestURI() != e.Path {
			t.Errorf("%s[%d]: got %s %s, want %s %s", name, next-1, r.Method, r.URL.RequestURI(), e.Method, e.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !jsonEqual(body, e.Request) {
			t.Errorf("%s[%d]: request body %s, want %s", name, next-1, body, e.Request)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(e.Status)
		if len(e.Response) > 0 && string(e.Response) != "null" {
			_, _ = w.Write(e.Response)
		}
	}))
	t.Cleanup(func() {
		srv.Close()
		mu.Lock()
		defer mu.Unlock()
		if next != len(ex) {
			t.Errorf("%s: %d of %d recorded requests were made", name, next, len(ex))
		}
	})
	opts = append([]Option{WithBaseURL(srv.URL), WithPollInterval(time.Millisecond)}, opts...)
	return New("test-token", opts...)
}

func jsonEqual(a, b []byte) bool {
	a, b = bytes.TrimSpace(a), bytes.TrimSpace(b)
	if len(a) == 0 || len(b) == 0 || string(b) == "null" {
		return len(a) == 0 && (len(b) == 0 || string(b) == "null")
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
