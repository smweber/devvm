package session

import (
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/smweber/devvm/internal/config"
)

// TestClientDaemonRoundTrip exercises the full control path — Client JSON over
// the unix socket, serveControl, handleConn, dispatch — against a daemon backed
// by the fake transport. This is the IPC that `port`/`tunnel` ride, minus a VM.
func TestClientDaemonRoundTrip(t *testing.T) {
	_, guest := echoServer(t)
	dir := shortTempDir(t)
	if err := os.MkdirAll(config.RuntimeDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &daemon{
		configDir: dir,
		name:      "t",
		tr:        &fakeTransport{dc: make(chan struct{})},
		forwards:  map[int]*fwd{},
		stop:      make(chan struct{}),
	}
	ln, _, err := listenControl(socketPath(dir, "t"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d.ln = ln
	go d.serveControl()

	cl := &Client{configDir: dir, name: "t"}
	if !cl.alive() {
		t.Fatal("ping failed")
	}

	pref := freePort(t)
	host, bumped, _, err := cl.Add(pref, guest)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if bumped || host != pref {
		t.Errorf("free pref %d should bind as-is, got host=%d bumped=%v", pref, host, bumped)
	}

	// Data round-trips through the daemon-managed forward.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", host))
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	io.WriteString(conn, "yo\n")
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "yo\n" {
		t.Fatalf("echo = %q err=%v", buf, err)
	}
	conn.Close()

	if fwds, err := cl.List(); err != nil || len(fwds) != 1 {
		t.Fatalf("List = %v err=%v", fwds, err)
	}
	if err := cl.Remove(guest); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if fwds, _ := cl.List(); len(fwds) != 0 {
		t.Fatalf("after Remove List = %v", fwds)
	}
}

func TestWaitGone(t *testing.T) {
	dir := shortTempDir(t)
	if err := os.MkdirAll(config.RuntimeDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	// Nothing there: gone at once.
	if !WaitGone(dir, "t", time.Second) {
		t.Fatal("WaitGone false with no socket")
	}
	// A live daemon: not gone until it shuts down.
	d := newDaemon(dir, "t", "test", newFakeTransport(), nil)
	d.logf = t.Logf
	ln, _, err := listenControl(socketPath(dir, "t"))
	if err != nil {
		t.Fatal(err)
	}
	d.ln = ln
	go d.serveControl()
	if WaitGone(dir, "t", 100*time.Millisecond) {
		t.Fatal("WaitGone true while the daemon answers")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		d.shutdown()
	}()
	if !WaitGone(dir, "t", 2*time.Second) {
		t.Fatal("WaitGone false after shutdown")
	}
	// A stale socket file nobody answers counts as present (not gone) until
	// removed: the caller must not spawn over a path a daemon may still own.
	sock := socketPath(dir, "t")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if WaitGone(dir, "t", 100*time.Millisecond) {
		t.Fatal("WaitGone true with a socket path still present")
	}
}
