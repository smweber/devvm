package hostbrowser

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://example.com/auth", "https://example.com/auth", true},
		{"http://localhost:8080/cb", "http://localhost:8080/cb", true},
		// No 1455 special case any more: the URL reaches the browser verbatim.
		{"http://localhost:1455/cb", "http://localhost:1455/cb", true},
		{"https://x?redirect_uri=http%3A%2F%2Flocalhost%3A1455%2F", "https://x?redirect_uri=http%3A%2F%2Flocalhost%3A1455%2F", true},
		{"file:///etc/passwd", "file:///etc/passwd", false},
		{"mailto:a@b.c", "mailto:a@b.c", false},
		{"-injection", "-injection", false},
	}
	for _, tt := range tests {
		got, ok := Sanitize(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("Sanitize(%q) = %q,%v want %q,%v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// openerName is the opener this OS looks up first.
func openerName() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}

// onlyPath makes dir the whole PATH.
func onlyPath(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir)
}

func TestOpenRefusesNonHTTP(t *testing.T) {
	if err := Open("file:///etc/passwd"); !errors.Is(err, ErrRefused) {
		t.Fatalf("Open(file://) = %v", err)
	}
}

func TestOpenNoOpener(t *testing.T) {
	onlyPath(t, t.TempDir())
	if err := Open("https://example.com/"); !errors.Is(err, ErrNoOpener) {
		t.Fatalf("Open with an empty PATH = %v, want ErrNoOpener", err)
	}
}

// An opener that is on PATH but cannot start (its interpreter is missing)
// is an error, not a success: the caller must print the URL.
func TestOpenOpenerFailsToStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, openerName()), []byte("#!/nonexistent/interpreter\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	onlyPath(t, dir)
	err := Open("https://example.com/")
	if err == nil || errors.Is(err, ErrNoOpener) || !strings.Contains(err.Error(), "failed to start") {
		t.Fatalf("Open with a broken opener = %v", err)
	}
}

// A working opener gets the URL verbatim as its one argument, and Open
// reports success once it started.
func TestOpenStartsOpener(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "opened")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + log + "\n"
	if err := os.WriteFile(filepath.Join(dir, openerName()), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// /bin/sh must still resolve for the script's shebang; PATH is only the
	// opener lookup.
	onlyPath(t, dir)
	url := "http://localhost:1455/cb?x=1"
	if err := Open(url); err != nil {
		t.Fatalf("Open = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(log); err == nil && len(b) > 0 {
			if got := strings.TrimSpace(string(b)); got != url {
				t.Fatalf("opener got %q, want %q", got, url)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the opener never ran")
}
