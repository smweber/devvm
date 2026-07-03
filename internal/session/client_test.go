package session

import (
	"fmt"
	"io"
	"net"
	"os"
	"testing"

	"github.com/smweber/devvm/internal/config"
)

// TestClientDaemonRoundTrip exercises the full control path — Client JSON over
// the unix socket, serveControl, handleConn, dispatch — against a daemon backed
// by the fake transport. This is the IPC that `port`/`tunnel` ride, minus a VM.
func TestClientDaemonRoundTrip(t *testing.T) {
	_, guest := echoServer(t)
	dir := t.TempDir()
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
	ln, err := net.Listen("unix", socketPath(dir, "t"))
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
	host, bumped, err := cl.Add(pref, guest)
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
