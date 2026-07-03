package agentrpc

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// tcpPipe returns two connected, buffered net.Conns (a real loopback socket
// pair). yamux deadlocks over the unbuffered, synchronous net.Pipe, whereas in
// production it rides a real exec stdio pipe — so tests use a real socket pair.
func tcpPipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		server, _ = ln.Accept()
		close(done)
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	<-done
	return client, server
}

// echoServer accepts one connection and echoes bytes back.
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(c, c); c.Close() }(c)
		}
	}()
	return ln
}

// TestForwardStream drives the full mux replacement in-process: a host yamux
// client opens a "forward" stream, the guest Serve loop dials the target and
// splices, and bytes round-trip. No VM required.
func TestForwardStream(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()

	hostConn, guestConn := tcpPipe(t)

	// Guest side: Serve over the pipe.
	go func() {
		_ = Serve(guestConn, guestConn, GuestHandlers{}, nil)
	}()

	// Host side: yamux client over the pipe. Close the underlying conns at the
	// end to force teardown (sess.Close alone can block on the recv loop).
	sess, err := yamux.Client(Stdio{In: hostConn, Out: hostConn}, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	// Stdio.Close is a no-op (the exec is torn down by killing the process, not
	// by closing the pipe), so closing the conns is what unblocks yamux here.
	defer func() { hostConn.Close(); guestConn.Close() }()

	stream, err := sess.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteHeader(stream, ForwardHeader(echo.Addr().String())); err != nil {
		t.Fatal(err)
	}

	msg := "hello-forward\n"
	if _, err := io.WriteString(stream, msg); err != nil {
		t.Fatal(err)
	}
	stream.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := bufio.NewReader(stream).ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if got != msg {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
	stream.Close()
}

// TestForwardStreamParallel opens several streams over the one session, the
// concurrency the single-exec design must support.
func TestForwardStreamParallel(t *testing.T) {
	echo := echoServer(t)
	defer echo.Close()

	hostConn, guestConn := tcpPipe(t)
	go func() { _ = Serve(guestConn, guestConn, GuestHandlers{}, nil) }()

	sess, err := yamux.Client(Stdio{In: hostConn, Out: hostConn}, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { hostConn.Close(); guestConn.Close() }()

	errc := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			stream, err := sess.Open()
			if err != nil {
				errc <- err
				return
			}
			defer stream.Close()
			if err := WriteHeader(stream, ForwardHeader(echo.Addr().String())); err != nil {
				errc <- err
				return
			}
			io.WriteString(stream, "ping\n")
			stream.SetReadDeadline(time.Now().Add(2 * time.Second))
			line, err := bufio.NewReader(stream).ReadString('\n')
			if err != nil {
				errc <- err
				return
			}
			if line != "ping\n" {
				errc <- io.ErrUnexpectedEOF
				return
			}
			errc <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("parallel stream %d: %v", i, err)
		}
	}
}

// TestEventPush validates the guest->host event channel: the agent opens an
// event stream (as the open-url shim triggers) and the host accepts + decodes.
func TestEventPush(t *testing.T) {
	hostConn, guestConn := tcpPipe(t)
	ready := make(chan *GuestSession, 1)
	go func() {
		_ = Serve(guestConn, guestConn, GuestHandlers{}, func(g *GuestSession) { ready <- g })
	}()

	sess, err := yamux.Client(Stdio{In: hostConn, Out: hostConn}, MuxConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { hostConn.Close(); guestConn.Close() }()

	g := <-ready
	stream, err := g.OpenEvent()
	if err != nil {
		t.Fatal(err)
	}
	json.NewEncoder(stream).Encode(Event{Type: EventOpenURL, URL: "https://example/auth"})

	hs, err := sess.Accept()
	if err != nil {
		t.Fatal(err)
	}
	hs.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(hs)
	header, err := ReadHeader(br)
	if err != nil || header != TypeEvent {
		t.Fatalf("header = %q err=%v", header, err)
	}
	var ev Event
	if err := json.NewDecoder(br).Decode(&ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if ev.Type != EventOpenURL || ev.URL != "https://example/auth" {
		t.Fatalf("event = %+v", ev)
	}
}
