package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Session is the client side of a long-lived session connection
// (browser-bridge.md §3): the one client `attach`, `shell`, `auth` and the
// hub transport share. It owns dial (spawning the daemon if needed), the
// `session` open, `subscribe` with its ack, and reconnect: when the
// connection drops (the daemon was cycled by `update` or `stop`, or died) it
// redials with backoff, opens a new session and re-subscribes, so a held
// `attach` regains the bridge without being reattached.
//
// Forwards added on a session are owned by that connection and go with it;
// they are not re-added after a reconnect (they come back on the next open
// that needs them, bridge §4).
type Session struct {
	name string
	// dial connects to the daemon, spawning it if needed; it must give up
	// when cancel closes (Close), or Close waits out a daemon's come-up.
	dial func(cancel <-chan struct{}) (net.Conn, error)
	opts SessionOptions

	minBackoff, maxBackoff time.Duration
	callTimeout            time.Duration

	// opMu serializes (re)connecting with subscribe/unsubscribe, so a
	// subscribe racing a reconnect is either re-sent by it or sent on the
	// connection it installs, never lost in between.
	opMu sync.Mutex
	mu   sync.Mutex
	cur  *sessConn // nil while disconnected
	// connecting is the connection a (re)connect is still setting up (the
	// session open, the re-subscribe), so Close can cut it off instead of
	// waiting out callTimeout against a wedged daemon.
	connecting net.Conn
	subscribed bool
	closed     chan struct{}
	closeOnce  sync.Once
	done       chan struct{} // the supervisor has exited
}

// SessionOptions are the caller's hooks. Both are optional.
type SessionOptions struct {
	// OnEvent answers an event from the daemon; the returned payload is the
	// reply. It runs on its own goroutine per event, so it may block (on a
	// browser, or on an Add over this same session) without stalling the
	// reader. Without it every event is answered with an empty reply.
	OnEvent func(data json.RawMessage) json.RawMessage
	// OnReconnect runs after a dropped connection is replaced, with the
	// subscription (if any) already acknowledged on the new one.
	OnReconnect func()
	// Logf reports reconnect attempts; nil discards them.
	Logf func(format string, args ...any)
}

// Session reconnect backoff. The first retry comes quickly: the common case
// is `update` or `ports down`/`up` cycling the daemon under a held attach.
const (
	minSessionBackoff = 500 * time.Millisecond
	maxSessionBackoff = 30 * time.Second
	sessionCallWait   = 30 * time.Second
)

// ErrSessionDown is returned by a call made while the session is between
// connections; it is retried by nothing, since the caller's forward would
// belong to a connection that is already gone.
var ErrSessionDown = errors.New("session is reconnecting")

// OpenSession dials the machine's daemon (spawning it if needed), opens a
// session, and keeps it open, reconnecting, until Close. The first
// connection is made before it returns, so its error is the caller's.
func OpenSession(configDir, name string, opts SessionOptions) (*Session, error) {
	dial := func(cancel <-chan struct{}) (net.Conn, error) {
		if _, err := dialCancel(configDir, name, cancel); err != nil {
			return nil, err
		}
		return net.Dial("unix", socketPath(configDir, name))
	}
	return openSession(name, dial, opts, minSessionBackoff, maxSessionBackoff)
}

func openSession(name string, dial func(cancel <-chan struct{}) (net.Conn, error), opts SessionOptions, minB, maxB time.Duration) (*Session, error) {
	s := &Session{
		name: name, dial: dial, opts: opts,
		minBackoff: minB, maxBackoff: maxB, callTimeout: sessionCallWait,
		closed: make(chan struct{}), done: make(chan struct{}),
	}
	if err := s.connect(); err != nil {
		return nil, err
	}
	go s.supervise()
	return s, nil
}

func (s *Session) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// connect dials, opens the session, re-subscribes if subscribed, and only
// then installs the connection.
func (s *Session) connect() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	c, err := s.dial(s.closed)
	if err != nil {
		return err
	}
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		c.Close()
		return errors.New("session closed")
	default:
	}
	s.connecting = c
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.connecting = nil
		s.mu.Unlock()
	}()
	sc, err := openSessConn(c, s.callTimeout)
	if err != nil {
		c.Close()
		return err
	}
	go sc.read(s.opts.OnEvent)
	s.mu.Lock()
	sub := s.subscribed
	s.mu.Unlock()
	if sub {
		if _, err := sc.call(Request{Op: OpSubscribe}); err != nil {
			sc.close()
			return fmt.Errorf("re-subscribe: %w", err)
		}
	}
	s.mu.Lock()
	select {
	case <-s.closed: // Close ran while we were connecting
		s.mu.Unlock()
		sc.close()
		return errors.New("session closed")
	default:
	}
	s.cur = sc
	s.mu.Unlock()
	return nil
}

// supervise waits for the connection to drop and replaces it, with backoff,
// until Close.
func (s *Session) supervise() {
	defer close(s.done)
	for {
		s.mu.Lock()
		sc := s.cur
		s.mu.Unlock()
		select {
		case <-s.closed:
			return
		case <-sc.gone:
		}
		s.mu.Lock()
		s.cur = nil
		s.mu.Unlock()
		backoff := s.minBackoff
		for attempt := 1; ; attempt++ {
			t := time.NewTimer(backoff)
			select {
			case <-s.closed:
				t.Stop()
				return
			case <-t.C:
			}
			err := s.connect()
			if err == nil {
				break
			}
			backoff = min(backoff*2, s.maxBackoff)
			s.logf("devvm: %s: session reconnect attempt %d failed: %v (retrying in %s)", s.name, attempt, err, backoff)
		}
		if s.opts.OnReconnect != nil {
			s.opts.OnReconnect()
		}
	}
}

func (s *Session) current() *sessConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Subscribe makes this session the daemon's most recent subscriber and
// returns once the daemon has acknowledged it: every event after that
// reaches this session. It is remembered and re-sent on every reconnect;
// sent again, it moves this session back to the front.
func (s *Session) Subscribe() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	s.subscribed = true
	s.mu.Unlock()
	sc := s.current()
	if sc == nil {
		return ErrSessionDown // re-sent, and acknowledged, by the reconnect
	}
	_, err := sc.call(Request{Op: OpSubscribe})
	return err
}

// Unsubscribe stops events reaching this session (they fall through to the
// next most recent subscriber) and stops re-subscribing on reconnect.
func (s *Session) Unsubscribe() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	s.subscribed = false
	s.mu.Unlock()
	sc := s.current()
	if sc == nil {
		return nil // the next connection starts unsubscribed
	}
	_, err := sc.call(Request{Op: OpUnsubscribe})
	return err
}

// Add brings up a forward owned by this session's connection: it lives
// until Remove or until the connection closes. exact refuses a busy port
// instead of bumping (and keeps it from bumping on any later restore).
func (s *Session) Add(pref, guest int, exact bool) (host int, bumped, pending bool, err error) {
	sc := s.current()
	if sc == nil {
		return 0, false, false, ErrSessionDown
	}
	resp, err := sc.call(Request{Op: OpAdd, Host: pref, Guest: guest, Exact: exact})
	if err != nil {
		return 0, false, false, err
	}
	return resp.Host, resp.Bumped, resp.Pending, nil
}

// Remove drops this connection's ownership of a guest port's forward.
func (s *Session) Remove(guest int) error {
	sc := s.current()
	if sc == nil {
		return ErrSessionDown
	}
	_, err := sc.call(Request{Op: OpRemove, Guest: guest})
	return err
}

// Close ends the session for good: no reconnect, and the daemon drops the
// connection's forwards and subscription.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		close(s.closed)
		sc, pending := s.cur, s.connecting
		s.mu.Unlock()
		if sc != nil {
			sc.close()
		}
		// A reconnect past its dial (opening the session, re-subscribing)
		// fails at once on a closed conn instead of waiting out its reply.
		if pending != nil {
			pending.Close()
		}
	})
	<-s.done
	return nil
}

// sessConn is one connection of a Session: one reader for its life, writes
// serialized, replies matched to calls by id.
type sessConn struct {
	c       net.Conn
	br      *bufio.Reader // holds anything read past the session reply
	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan Response
	gone    chan struct{} // closed when the reader ends
	timeout time.Duration
}

// openSessConn sends `session` and reads its reply synchronously, before the
// reader starts: a daemon older than sessions answers without an id
// ("unknown op"), which no id-matching reader could route.
func openSessConn(c net.Conn, timeout time.Duration) (*sessConn, error) {
	sc := &sessConn{c: c, nextID: 1, pending: map[int64]chan Response{}, gone: make(chan struct{}), timeout: timeout}
	if err := sc.writeLine(Request{ID: 1, Op: OpSession}); err != nil {
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	var resp Response
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	if !resp.OK {
		if strings.HasPrefix(resp.Err, "unknown op") {
			return nil, fmt.Errorf("forward daemon (%s) predates sessions; restart it with 'devvm ports down' then 'ports up'", resp.Version)
		}
		return nil, fmt.Errorf("open session: %s", resp.Err)
	}
	sc.br = br
	return sc, nil
}

func (sc *sessConn) writeLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	sc.wmu.Lock()
	defer sc.wmu.Unlock()
	_ = sc.c.SetWriteDeadline(time.Now().Add(sessionWriteTimeout))
	_, err = sc.c.Write(append(b, '\n'))
	return err
}

// read is the connection's only reader: replies go to their calls by id (an
// unknown id is dropped), events to onEvent on a goroutine of their own.
func (sc *sessConn) read(onEvent func(json.RawMessage) json.RawMessage) {
	defer func() {
		sc.mu.Lock()
		close(sc.gone)
		sc.mu.Unlock()
		sc.c.Close()
	}()
	for {
		line, err := sc.br.ReadString('\n')
		if err != nil {
			return
		}
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			continue
		}
		if ev := resp.Event; ev != nil {
			go func() {
				var data json.RawMessage
				if onEvent != nil {
					data = onEvent(ev.Data)
				}
				_ = sc.writeLine(replyLine{Reply: &EventReply{ID: ev.ID, Data: data}})
			}()
			continue
		}
		sc.mu.Lock()
		ch, ok := sc.pending[resp.ID]
		delete(sc.pending, resp.ID)
		sc.mu.Unlock()
		if ok {
			ch <- resp // buffered 1
		}
	}
}

// replyLine is the client-to-daemon line answering an event: `{"reply":{…}}`.
type replyLine struct {
	Reply *EventReply `json:"reply"`
}

// call sends one request and waits for the reply with its id.
func (sc *sessConn) call(req Request) (Response, error) {
	ch := make(chan Response, 1)
	sc.mu.Lock()
	select {
	case <-sc.gone:
		sc.mu.Unlock()
		return Response{}, ErrSessionDown
	default:
	}
	sc.nextID++
	req.ID = sc.nextID
	sc.pending[req.ID] = ch
	sc.mu.Unlock()
	drop := func() {
		sc.mu.Lock()
		delete(sc.pending, req.ID)
		sc.mu.Unlock()
	}
	if err := sc.writeLine(req); err != nil {
		drop()
		return Response{}, err
	}
	t := time.NewTimer(sc.timeout)
	defer t.Stop()
	select {
	case resp := <-ch:
		if !resp.OK {
			return resp, errors.New(resp.Err)
		}
		return resp, nil
	case <-sc.gone:
		drop()
		return Response{}, ErrSessionDown
	case <-t.C:
		drop()
		return Response{}, fmt.Errorf("%s: no reply from the forward daemon", req.Op)
	}
}

func (sc *sessConn) close() { sc.c.Close() }
