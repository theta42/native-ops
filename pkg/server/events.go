package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// GET /v1/events is a WebSocket that tells the UI when what it shows has changed, so it never polls. It
// carries change notes, not data the caller might not be allowed to see: a page that hears its topic
// reads its data from the ordinary endpoint, with the caller's role. The one exception is the host
// status, which every caller of this endpoint may read (viewer): it comes in the note, and only when it
// differs from the last one.
//
// Topics: status, jobs, job:<id>, plans, recipes, tokens (tokens, the daemon's secrets and agent
// sign-ins). Notes are coalesced: at most one message every eventFlush, naming every topic that changed.
//
// Messages, as JSON text:
//
//	server: {"type":"hello","name":...,"role":...}            once signed in
//	server: {"type":"change","topics":[...],"status":{...}}   status only when the topics include it
//	client: {"token":"nops_..."}                              first, from a browser holding a pasted token
//
// A browser sends its session cookie with the handshake; a browser with a pasted token sends it as the
// first message (it cannot set a header, and a query string ends up in logs); any other client may send
// Authorization. Only the daemon's own pages may connect with a cookie (the Origin is checked, as a
// browser does not apply CORS to WebSockets). Tokens scoped to instances or secrets are refused: this is
// the UI's feed. Who is connected is checked again every eventRecheck, so a revoked token or a disabled
// user is dropped.

const (
	maxEventSubs = 64
	eventAuthIn  = 10 * time.Second
)

// How often the host status is read while anyone listens, how often a connection's caller is checked
// again (and pinged), and how long changes are gathered into one note (variables so tests can shorten
// them).
var (
	statusEvery  = 10 * time.Second
	eventRecheck = 30 * time.Second
	eventFlush   = 300 * time.Millisecond
)

// changeHook lets a store say it changed (what: an id, or empty). Stores embed it; the server sets it.
type changeHook struct{ fn atomic.Pointer[func(string)] }

// SetOnChange sets what runs after each change is saved.
func (c *changeHook) SetOnChange(f func(what string)) { c.fn.Store(&f) }

func (c *changeHook) changed(what string) {
	if f := c.fn.Load(); f != nil {
		(*f)(what)
	}
}

// eventHub fans change notes out to the connected UIs.
type eventHub struct {
	mu     sync.Mutex
	subs   map[*eventSub]struct{}
	status []byte // the last status pushed, as JSON
	closed bool
	wake   chan struct{} // the status reader's: a first subscriber arrived
	// The timings, fixed when the hub is made.
	flush, recheck, statusEvery time.Duration
}

type eventSub struct {
	mu      sync.Mutex
	pending map[string]bool
	wake    chan struct{}
	conn    *wsConn
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[*eventSub]struct{}{}, wake: make(chan struct{}, 1),
		flush: eventFlush, recheck: eventRecheck, statusEvery: statusEvery}
}

// publish notes that topic changed, for every subscriber.
func (h *eventHub) publish(topic string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.add(topic)
	}
}

func (s *eventSub) add(topic string) {
	s.mu.Lock()
	if len(s.pending) < 64 {
		s.pending[topic] = true
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *eventSub) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.pending))
	for t := range s.pending {
		out = append(out, t)
	}
	s.pending = map[string]bool{}
	sort.Strings(out)
	return out
}

func (h *eventHub) join(conn *wsConn) (*eventSub, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.subs) >= maxEventSubs {
		return nil, false
	}
	s := &eventSub{pending: map[string]bool{}, wake: make(chan struct{}, 1), conn: conn}
	h.subs[s] = struct{}{}
	select {
	case h.wake <- struct{}{}:
	default:
	}
	return s, true
}

func (h *eventHub) leave(s *eventSub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

func (h *eventHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// closeAll ends every connection: the daemon is stopping (http.Server.Shutdown does not close a hijacked
// connection).
func (h *eventHub) closeAll() {
	h.mu.Lock()
	h.closed = true
	subs := make([]*eventSub, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.conn.close(1001, "the daemon is stopping")
	}
}

// setStatus records a status snapshot and reports whether it differs from the last one, the time it was
// taken aside.
func (h *eventHub) setStatus(full, comparable []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.status
	h.status = full
	return prev == nil || !bytes.Equal(comparableStatus(prev), comparable)
}

func (h *eventHub) lastStatus() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// comparableStatus is a status snapshot without its time, so two reads of an unchanged host are equal.
func comparableStatus(b []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	delete(m, "time")
	out, _ := json.Marshal(m)
	return out
}

// runStatusEvents reads the host status every statusEvery while anyone listens on /v1/events, and
// publishes it when it changed. It returns when ctx ends.
func (s *Server) runStatusEvents(ctx context.Context) {
	h := s.events
	for {
		if h.count() == 0 {
			select {
			case <-ctx.Done():
				return
			case <-h.wake:
			}
		}
		if snap, err := s.snapshot(ctx); err == nil {
			if full, err := json.Marshal(snap); err == nil && h.setStatus(full, comparableStatus(full)) {
				h.publish("status")
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.statusEvery):
		}
	}
}

// eventCaller is who is connected, and how to tell they still may be.
type eventCaller struct {
	name  string
	role  Role
	valid func() bool
}

// eventCallerFrom identifies a handshake by its session cookie or Authorization header. ok is false when
// it carries neither (a pasted token comes as the first message); refused says why one it carries is not
// accepted.
func (s *Server) eventCallerFrom(r *http.Request) (c eventCaller, ok bool, refused string) {
	if s.opts.Sessions != nil {
		if username, role, signed := s.currentSession(r); signed {
			// A cookie is sent by the browser wherever the page is: only the daemon's own may use it.
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					return c, false, "a signed-in browser may connect only from this daemon's own pages"
				}
			}
			return eventCaller{name: username, role: role, valid: func() bool {
				_, now, still := s.currentSession(r)
				return still && now.Allows(RoleViewer)
			}}, true, ""
		}
	}
	if tok := bearer(r); tok != "" {
		return s.eventCallerByToken(tok)
	}
	return c, false, ""
}

func (s *Server) eventCallerByToken(secret string) (eventCaller, bool, string) {
	t, found := s.opts.Tokens.Verify(secret)
	switch {
	case !found:
		return eventCaller{}, false, "a valid API token is required"
	case t.Scoped():
		return eventCaller{}, false, "a token limited to instances or secrets cannot follow the UI's events"
	case !t.Role.Allows(RoleViewer):
		return eventCaller{}, false, "this token's role cannot do that"
	}
	return eventCaller{name: t.Name, role: t.Role, valid: func() bool {
		now, still := s.opts.Tokens.Verify(secret)
		return still && !now.Scoped() && now.Role.Allows(RoleViewer)
	}}, true, ""
}

// handleEvents is GET /v1/events (viewer; see the top of this file).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	caller, ok, refused := s.eventCallerFrom(r)
	if refused != "" {
		writeError(w, http.StatusForbidden, "forbidden", refused)
		return
	}
	if s.events.count() >= maxEventSubs {
		writeError(w, http.StatusServiceUnavailable, "busy", "too many connections; the page falls back to reloading")
		return
	}
	release := func() {}
	if !ok {
		// Not signed in yet: hold only a few such connections open at a time, until they send a token.
		select {
		case s.eventPending <- struct{}{}:
			var once sync.Once
			release = func() { once.Do(func() { <-s.eventPending }) }
			defer release()
		default:
			writeError(w, http.StatusServiceUnavailable, "busy", "too many connections are signing in; retry shortly")
			return
		}
	}
	conn, err := wsUpgrade(w, r)
	if err != nil {
		return
	}
	if !ok {
		// A browser holding a pasted token sends it first.
		_ = conn.conn.SetReadDeadline(time.Now().Add(eventAuthIn))
		msg, err := conn.readMessage()
		var first struct {
			Token string `json:"token"`
		}
		release()
		if err != nil || json.Unmarshal(msg, &first) != nil || first.Token == "" {
			conn.close(4001, "sign in first: send {\"token\": ...}")
			return
		}
		if caller, ok, refused = s.eventCallerByToken(first.Token); !ok {
			conn.close(4003, refused)
			return
		}
		_ = conn.conn.SetReadDeadline(time.Time{})
	}
	s.setActor(r, caller.name)
	if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
		h.role = caller.role
	}
	sub, joined := s.events.join(conn)
	if !joined {
		conn.close(1013, "too many connections")
		return
	}
	defer s.events.leave(sub)
	start := time.Now()
	defer func() { auditDetail(r, "events for %s", time.Since(start).Round(time.Second)) }()

	hello, _ := json.Marshal(map[string]any{"type": "hello", "name": caller.name, "role": caller.role})
	if conn.writeText(hello) != nil {
		conn.conn.Close()
		return
	}
	done := make(chan struct{})
	go func() { // the browser sends nothing we need; reading notices it leave, and answers its pings
		defer close(done)
		for {
			if _, err := conn.readMessage(); err != nil {
				return
			}
		}
	}()
	recheck := time.NewTicker(s.events.recheck)
	defer recheck.Stop()
	for {
		select {
		case <-done:
			conn.conn.Close()
			return
		case <-recheck.C:
			if !caller.valid() {
				conn.close(4001, "signed out")
				return
			}
			if conn.writeFrame(wsPing, nil) != nil {
				conn.conn.Close()
				return
			}
		case <-sub.wake:
			time.Sleep(s.events.flush) // let a burst of changes (a job's log) arrive as one note
			topics := sub.take()
			if len(topics) == 0 {
				continue
			}
			note := map[string]any{"type": "change", "topics": topics}
			for _, t := range topics {
				if t == "status" {
					note["status"] = json.RawMessage(s.events.lastStatus())
				}
			}
			b, _ := json.Marshal(note)
			if conn.writeText(b) != nil {
				conn.conn.Close()
				return
			}
		}
	}
}
