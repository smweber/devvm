package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/smweber/devvm/internal/config"
)

// Sessions (browser-bridge.md §3). A connection whose first line is
// `session` stays open: it holds the daemon (the idle rule counts it), it may
// add forwards it owns (dropped when it closes), and it may subscribe to
// events. One pipe carries requests, their replies, events and event
// replies, so:
//
//   - every request carries an id its reply echoes, and every event an id
//     the subscriber's reply names;
//   - each side runs one reader for the connection's life, which never
//     blocks waiting for a reply (a reply can queue behind an event);
//   - writes are serialized, one line at a time.
//
// Requests on a session run in order on one worker, so a reader busy with a
// slow add (an `ssh -O forward`) never stops event replies being routed.

// sessionWriteTimeout bounds one line written to a session. A client that
// stopped reading must not wedge the writer lock, and with it every event
// delivery to that session; the write fails and the session closes.
const sessionWriteTimeout = 10 * time.Second

// sessionQueue is how many requests a session may have queued behind the
// one running. The reader blocks beyond it, which only a client flooding
// requests without reading replies can reach.
const sessionQueue = 64

var (
	errNoSubscriber   = errors.New("no subscriber")
	errSubscriberGone = errors.New("subscriber went away")
	errReplyTimeout   = errors.New("subscriber did not reply in time")
)

// sess is one open session connection, as the daemon sees it.
type sess struct {
	id   uint64
	conn net.Conn
	wmu  sync.Mutex
	// pending maps an event id to the delivery waiting on its reply;
	// guarded by the daemon's mu.
	pending map[int64]chan json.RawMessage
	done    chan struct{} // closed when the reader ends
}

// write sends one line, serialized with every other write on this session.
// A failed write closes the connection, which ends the reader and with it
// the session.
func (s *sess) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(sessionWriteTimeout))
	if _, err := s.conn.Write(append(b, '\n')); err != nil {
		s.conn.Close()
		return err
	}
	return nil
}

// eventLine is the daemon-to-client line for an event: `{"event":{…}}` and
// nothing else, so a reader tells it from a reply by the key alone.
type eventLine struct {
	Event *Event `json:"event"`
}

// serveSession runs a session connection until it closes. open is the
// `session` request that opened it; the connection's buffered reader is
// passed on so nothing read past that line is lost.
func (d *daemon) serveSession(conn net.Conn, br *bufio.Reader, open Request) {
	_ = conn.SetReadDeadline(time.Time{})
	s := d.openSession(conn)
	state, _ := d.status()
	if s.write(Response{ID: open.ID, OK: true, State: state, Version: d.version}) != nil {
		d.closeSession(s)
		return
	}

	work := make(chan Request, sessionQueue)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for req := range work {
			select {
			case <-s.done:
				continue // the connection is gone: nothing to add for it, nobody to answer
			default:
			}
			resp := d.sessionDispatch(s, req)
			resp.ID = req.ID
			_ = s.write(resp)
		}
	}()

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = s.write(Response{Err: "bad request: " + err.Error()})
			continue
		}
		if req.Reply != nil {
			d.routeReply(s, *req.Reply)
			continue
		}
		work <- req
	}
	// Order matters: mark the session done so queued requests are skipped,
	// close the conn so a write in flight fails, and wait for the worker
	// before dropping the connection's owners, or an add that was binding
	// could adopt a forward for a connection that is already gone, and
	// nothing would ever drop that owner.
	close(s.done)
	d.unsubscribe(s) // later events fall through now, not after the worker drains
	conn.Close()
	close(work)
	<-workerDone
	d.closeSession(s)
}

// sessionDispatch runs one request from a session. add and remove are
// owned by the connection; subscribe and unsubscribe exist only here;
// everything else is the one-shot op.
func (d *daemon) sessionDispatch(s *sess, req Request) Response {
	switch req.Op {
	case OpAdd:
		host, bumped, pending, err := d.add(req.Host, req.Guest, req.Exact, connOwner(s.id))
		if err != nil {
			return Response{Err: err.Error()}
		}
		return Response{OK: true, Host: host, Bumped: bumped, Pending: pending}
	case OpRemove:
		// Only this connection's own ownership: a session never drops a
		// conf or ttl owner, or another session's.
		resp := Response{OK: true}
		if _, left := d.remove(req.Guest, connOwner(s.id)); left != nil {
			resp.Forwards = []Forward{*left}
		}
		return resp
	case OpSubscribe:
		d.subscribe(s)
		return Response{OK: true}
	case OpUnsubscribe:
		d.unsubscribe(s)
		return Response{OK: true}
	case OpSession:
		return Response{Err: "already a session"}
	default:
		return d.dispatch(req)
	}
}

func (d *daemon) openSession(conn net.Conn) *sess {
	d.mu.Lock()
	d.nextConn++
	s := &sess{id: d.nextConn, conn: conn, pending: map[int64]chan json.RawMessage{}, done: make(chan struct{})}
	d.sessions[s.id] = s
	d.mu.Unlock()
	config.TouchChanged(d.configDir)
	return s
}

// closeSession forgets a session: its subscription (events fall through to
// the next most recent subscriber), its undelivered replies (their waiters
// see s.done), and every forward owner it held.
//
// The owners go in the same critical section as the session: in between, a
// `down` would see no session but its forward still held and answer "stays
// up", and a list would show a forward nobody owns any more.
func (d *daemon) closeSession(s *sess) {
	own := connOwner(s.id)
	d.mu.Lock()
	delete(d.sessions, s.id)
	d.subs = removeSess(d.subs, s)
	s.pending = map[int64]chan json.RawMessage{}
	closers, _ := d.dropOwnersLocked(func(o owner) bool { return o == own })
	d.mu.Unlock()
	closeAll(closers)
	config.TouchChanged(d.configDir)
}

// closeAllSessions closes every session connection (shutdown). Each one's
// serveSession then unwinds through closeSession on its own goroutine.
func (d *daemon) closeAllSessions() {
	d.mu.Lock()
	conns := make([]net.Conn, 0, len(d.sessions))
	for _, s := range d.sessions {
		conns = append(conns, s.conn)
	}
	d.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// subscribe registers s as the most recent subscriber. Re-subscribing moves
// it to the front. The ack is sent by the caller only after this returns,
// so a client that waits for it knows every later event reaches it.
func (d *daemon) subscribe(s *sess) {
	d.mu.Lock()
	d.subs = append([]*sess{s}, removeSess(d.subs, s)...)
	d.mu.Unlock()
}

func (d *daemon) unsubscribe(s *sess) {
	d.mu.Lock()
	d.subs = removeSess(d.subs, s)
	d.mu.Unlock()
}

func removeSess(list []*sess, s *sess) []*sess {
	out := list[:0:0]
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// deliver hands one event to the most recent subscriber and waits for its
// reply. The subscriber disconnecting, or not answering within timeout, is
// an error (the bridge turns either into `opened: false`); the event is not
// re-sent to the next subscriber, which only gets later events. The payload
// is opaque here; roadmap step 7 defines it.
func (d *daemon) deliver(data json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	d.mu.Lock()
	if len(d.subs) == 0 {
		d.mu.Unlock()
		return nil, errNoSubscriber
	}
	s := d.subs[0]
	d.nextEvent++
	id := d.nextEvent
	ch := make(chan json.RawMessage, 1)
	s.pending[id] = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(s.pending, id)
		d.mu.Unlock()
	}()
	if err := s.write(eventLine{Event: &Event{ID: id, Data: data}}); err != nil {
		return nil, errSubscriberGone
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r, nil
	case <-s.done:
		return nil, errSubscriberGone
	case <-t.C:
		return nil, errReplyTimeout
	}
}

// routeReply hands a subscriber's reply to the delivery waiting on it. A
// reply for an id this session no longer has pending is late (timed out) or
// bogus, and is dropped.
func (d *daemon) routeReply(s *sess, r EventReply) {
	d.mu.Lock()
	ch, ok := s.pending[r.ID]
	delete(s.pending, r.ID)
	d.mu.Unlock()
	if !ok {
		d.logf("%s: dropped a late reply for event %d", d.name, r.ID)
		return
	}
	ch <- r.Data // buffered 1, and the id was removed: never blocks
}
