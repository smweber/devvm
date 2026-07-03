package session

import (
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/smweber/devvm/internal/agentrpc"
)

// fakeTransport binds real host listeners (so bind conflicts drive the bump)
// and forwards to a real guest port, standing in for smol/ssh in unit tests.
type fakeTransport struct{ dc chan struct{} }

func (f *fakeTransport) forward(hostPort, guestPort int) (io.Closer, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		return nil, errPortBusy
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				up, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", guestPort))
				if err != nil {
					c.Close()
					return
				}
				agentrpc.Splice(c, up)
			}(c)
		}
	}()
	return ln, nil
}

func (f *fakeTransport) dead() <-chan struct{} { return f.dc }
func (f *fakeTransport) Close() error          { return nil }

func newTestDaemon(t *testing.T) *daemon {
	t.Helper()
	return &daemon{
		configDir: t.TempDir(),
		name:      "t",
		tr:        &fakeTransport{dc: make(chan struct{})},
		forwards:  map[int]*fwd{},
		stop:      make(chan struct{}),
	}
}

// freePort returns a currently-free loopback port (bind :0, read it, release).
// add() needs a concrete preferred port to report back; 0 is not a valid pref.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

func echoServer(t *testing.T) (addr string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(c, c); c.Close() }(c)
		}
	}()
	return ln.Addr().String(), ln.Addr().(*net.TCPAddr).Port
}

func TestDaemonAddRemoveList(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)

	pref := freePort(t)
	host, bumped, err := d.add(pref, guest)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if bumped || host != pref {
		t.Errorf("free pref %d should bind as-is, got host=%d bumped=%v", pref, host, bumped)
	}

	// Data must round-trip through the forward.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", host))
	if err != nil {
		t.Fatalf("dial forward: %v", err)
	}
	io.WriteString(conn, "hi\n")
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hi\n" {
		t.Fatalf("echo = %q err=%v", buf, err)
	}
	conn.Close()

	if fs := d.list(); len(fs) != 1 || fs[0].Guest != guest {
		t.Fatalf("list = %v", fs)
	}

	// Adding the same guest again is idempotent (same host port).
	host2, _, _ := d.add(host, guest)
	if host2 != host {
		t.Errorf("re-add host = %d, want %d", host2, host)
	}

	d.remove(guest)
	if fs := d.list(); len(fs) != 0 {
		t.Fatalf("after remove list = %v", fs)
	}
}

func TestDaemonPortBump(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)

	// Occupy a host port to force a bump.
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()
	pref := occ.Addr().(*net.TCPAddr).Port

	host, bumped, err := d.add(pref, guest)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !bumped {
		t.Errorf("expected bump when preferred port %d is taken", pref)
	}
	if host == pref {
		t.Errorf("host %d should differ from taken pref %d", host, pref)
	}
	if host <= pref || host > pref+20 {
		t.Errorf("bumped host %d out of range (%d, %d]", host, pref, pref+20)
	}
}

func TestDaemonDispatch(t *testing.T) {
	_, guest := echoServer(t)
	d := newTestDaemon(t)

	if r := d.dispatch(Request{Op: OpPing}); !r.OK {
		t.Errorf("ping not ok: %+v", r)
	}
	if r := d.dispatch(Request{Op: OpAdd, Host: freePort(t), Guest: guest}); !r.OK {
		t.Errorf("add not ok: %+v", r)
	}
	if r := d.dispatch(Request{Op: OpList}); !r.OK || len(r.Forwards) != 1 {
		t.Errorf("list wrong: %+v", r)
	}
	if r := d.dispatch(Request{Op: OpRemove, Guest: guest}); !r.OK {
		t.Errorf("remove not ok: %+v", r)
	}
	if r := d.dispatch(Request{Op: "bogus"}); r.OK || r.Err == "" {
		t.Errorf("bogus op should error: %+v", r)
	}
}
