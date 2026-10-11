package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

// wsClient is the browser's side of a WebSocket, just enough to test /v1/events.
type wsClient struct {
	conn net.Conn
	rd   *bufio.Reader
	code int    // the handshake's status
	body string // the body of a refused handshake
}

func wsDial(t *testing.T, base string, headers map[string]string) *wsClient {
	t.Helper()
	host := strings.TrimPrefix(base, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	req := "GET /v1/events HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	for k, v := range headers {
		req += k + ": " + v + "\r\n"
	}
	if _, err := conn.Write([]byte(req + "\r\n")); err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(conn)
	res, err := http.ReadResponse(rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &wsClient{conn: conn, rd: rd, code: res.StatusCode}
	if res.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(res.Body)
		c.body = string(b)
		return c
	}
	if got := res.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" { // RFC 6455's example
		t.Fatalf("accept key: %q", got)
	}
	return c
}

// send writes one masked frame, as a browser must.
func (c *wsClient) send(op byte, payload []byte, masked bool) {
	hdr := []byte{0x80 | op}
	n := len(payload)
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	switch {
	case n < 126:
		hdr = append(hdr, maskBit|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, maskBit|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, maskBit|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	body := append([]byte(nil), payload...)
	if masked {
		var mask [4]byte
		_, _ = rand.Read(mask[:])
		hdr = append(hdr, mask[:]...)
		for i := range body {
			body[i] ^= mask[i%4]
		}
	}
	_, _ = c.conn.Write(append(hdr, body...))
}

// next reads frames until a text message, a close (op 8, with its code) or the deadline.
func (c *wsClient) next(t *testing.T, within time.Duration) (op byte, payload []byte) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(within))
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.rd, h[:]); err != nil {
			return 0, nil
		}
		if h[1]&0x80 != 0 {
			t.Fatal("the server must not mask its frames")
		}
		n := uint64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			_, _ = io.ReadFull(c.rd, b[:])
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			_, _ = io.ReadFull(c.rd, b[:])
			n = binary.BigEndian.Uint64(b[:])
		}
		payload = make([]byte, n)
		if _, err := io.ReadFull(c.rd, payload); err != nil {
			return 0, nil
		}
		switch op = h[0] & 0x0F; op {
		case wsPing, wsPong:
			if op == wsPing {
				c.send(wsPong, payload, true)
			}
			continue
		}
		return op, payload
	}
}

// expect reads messages until one of type typ that contains want, or fails.
func (c *wsClient) expect(t *testing.T, typ, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, b := c.next(t, time.Until(deadline))
		if op != wsText {
			t.Fatalf("waiting for %s %q: got op %d %q", typ, want, op, b)
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil && m["type"] == typ && strings.Contains(string(b), want) {
			return m
		}
	}
	t.Fatalf("no %s message with %q", typ, want)
	return nil
}

// closeCode reads until the server closes, and returns the close code.
func (c *wsClient) closeCode(t *testing.T, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		op, b := c.next(t, time.Until(deadline))
		if op == wsClose && len(b) >= 2 {
			return int(binary.BigEndian.Uint16(b))
		}
		if op == 0 {
			return -1 // the connection ended without a close frame
		}
	}
	return 0
}

type eventRig struct {
	url                      string
	srv                      *Server
	tokens                   *TokenStore
	users                    *UserStore
	jobs                     *Jobs
	viewer, scoped, deployer string
	cookie                   string
	host                     atomic.Value // what the status reports as the host
}

func newEventRig(t *testing.T) *eventRig {
	t.Helper()
	flush, recheck, every := eventFlush, eventRecheck, statusEvery
	eventFlush, eventRecheck, statusEvery = 10*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { eventFlush, eventRecheck, statusEvery = flush, recheck, every })

	dir := t.TempDir()
	r := &eventRig{}
	r.host.Store("h1")
	r.tokens, _ = OpenTokenStore(filepath.Join(dir, "tokens.json"))
	r.users, _ = OpenUserStore(filepath.Join(dir, "users.json"))
	r.viewer, _, _ = r.tokens.Create("dash", RoleViewer)
	r.deployer, _, _ = r.tokens.Create("ci", RoleDeployer)
	r.scoped, _, _ = r.tokens.CreateScoped("fleet", RoleDeployer, Scope{Names: []string{"demo-*"}, Images: []string{"x:*"}})
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sess, _ := NewSessions(key, time.Hour, false)
	if _, err := r.users.CreateLocal("sam", "Sam", RoleViewer, "a long enough password"); err != nil {
		t.Fatal(err)
	}
	r.cookie = SessionCookie + "=" + sess.Issue("sam", RoleViewer)
	audit, _ := OpenAudit(filepath.Join(dir, "audit.log"))
	t.Cleanup(func() { audit.Close() })
	r.jobs, _ = OpenJobs(filepath.Join(dir, "jobs"))
	s, err := New(Options{Tokens: r.tokens, Audit: audit, Version: "test", Users: r.users, Sessions: sess, Jobs: r.jobs,
		ImagePrune: func(context.Context, bool, func(string, ...any)) error { return nil },
		Status: func(context.Context) (*status.Snapshot, error) {
			return &status.Snapshot{Host: r.host.Load().(string), Time: time.Now()}, nil
		}, StatusTTL: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r.srv = s
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(s.events.closeAll) // a hijacked connection outlives ts.Close
	r.url = ts.URL
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.runStatusEvents(ctx)
	return r
}

func TestABrowserSessionHearsJobChangesAndStatus(t *testing.T) {
	r := newEventRig(t)
	host := strings.TrimPrefix(r.url, "http://")
	c := wsDial(t, r.url, map[string]string{"Cookie": r.cookie, "Origin": "http://" + host})
	if c.code != http.StatusSwitchingProtocols {
		t.Fatalf("handshake: %d %s", c.code, c.body)
	}
	c.expect(t, "hello", `"name":"sam"`)

	// The status comes with the note, once it is read, and again only when the host changed (not its time).
	first := c.expect(t, "change", `"status"`)
	if first["status"].(map[string]any)["host"] != "h1" {
		t.Fatalf("status note: %v", first)
	}
	r.host.Store("h2")
	c.expect(t, "change", `"host":"h2"`)

	job, _ := r.jobs.CreateKind("image:prune", "ci", "", "", "")
	c.expect(t, "change", `"job:`+string(job.ID)+`"`)
	r.jobs.Finish(job.ID, nil)
	c.expect(t, "change", `"jobs"`)
}

func TestTheStatusIsPushedOnlyWhenItChanges(t *testing.T) {
	h := newEventHub()
	a := []byte(`{"host":"h1","time":"2026-10-11T10:00:00Z"}`)
	b := []byte(`{"host":"h1","time":"2026-10-11T10:00:10Z"}`)
	c := []byte(`{"host":"h2","time":"2026-10-11T10:00:20Z"}`)
	if !h.setStatus(a, comparableStatus(a)) || h.setStatus(b, comparableStatus(b)) || !h.setStatus(c, comparableStatus(c)) {
		t.Fatal("the first status, and each change but not a new time, is news")
	}
}

func TestEventsRefuseOtherOriginsScopedTokensAndStrangers(t *testing.T) {
	r := newEventRig(t)
	// Another page holding the browser's cookie (a sibling subdomain, any site): browsers send the
	// cookie with a WebSocket handshake and apply no CORS to it, so the Origin is what stops it.
	if c := wsDial(t, r.url, map[string]string{"Cookie": r.cookie, "Origin": "https://tenant.example.com"}); c.code != http.StatusForbidden {
		t.Fatalf("a cookie from another origin: %d", c.code)
	}
	for name, h := range map[string]map[string]string{
		"scoped token":  {"Authorization": "Bearer " + r.scoped},
		"invalid token": {"Authorization": "Bearer nops_" + strings.Repeat("0", 64)},
	} {
		if c := wsDial(t, r.url, h); c.code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, c.code, c.body)
		}
	}
	// A pasted token comes as the first message: a scoped one is refused there too, and a connection
	// that never signs in is closed.
	c := wsDial(t, r.url, nil)
	c.send(wsText, []byte(`{"token":"`+r.scoped+`"}`), true)
	if code := c.closeCode(t, 3*time.Second); code != 4003 {
		t.Fatalf("a scoped token as the first message: close %d", code)
	}
	c = wsDial(t, r.url, nil)
	c.send(wsText, []byte(`{"hello":1}`), true)
	if code := c.closeCode(t, 3*time.Second); code != 4001 {
		t.Fatalf("no token: close %d", code)
	}
	// Not a WebSocket at all.
	res, err := http.Get(r.url + "/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a plain GET: %d", res.StatusCode)
	}
}

func TestAPastedTokenSignsInAndARevokedOneIsDropped(t *testing.T) {
	r := newEventRig(t)
	// More pasted-token browsers than may wait to sign in at once: each frees its place once signed in.
	for i := 0; i < cap(r.srv.eventPending)+2; i++ {
		other := wsDial(t, r.url, nil)
		if other.code != http.StatusSwitchingProtocols {
			t.Fatalf("browser %d: %d %s", i, other.code, other.body)
		}
		other.send(wsText, []byte(`{"token":"`+r.deployer+`"}`), true)
		other.expect(t, "hello", `"name":"ci"`)
	}
	c := wsDial(t, r.url, nil)
	c.send(wsText, []byte(`{"token":"`+r.viewer+`"}`), true)
	c.expect(t, "hello", `"role":"viewer"`)

	list, _ := r.tokens.List()
	for _, tok := range list {
		if tok.Name == "dash" {
			if err := r.tokens.Revoke(tok.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if code := c.closeCode(t, 3*time.Second); code != 4001 {
		t.Fatalf("a revoked token's connection must end: close %d", code)
	}

	// A disabled user's session ends the same way.
	s := wsDial(t, r.url, map[string]string{"Cookie": r.cookie})
	s.expect(t, "hello", `"name":"sam"`)
	if err := r.users.SetDisabled("sam", true); err != nil {
		t.Fatal(err)
	}
	if code := s.closeCode(t, 3*time.Second); code != 4001 {
		t.Fatalf("a disabled user's connection must end: close %d", code)
	}
}

// The browser must mask its frames, and may not send more than a small message.
func TestEventsRefuseUnmaskedAndOversizedFrames(t *testing.T) {
	r := newEventRig(t)
	c := wsDial(t, r.url, map[string]string{"Authorization": "Bearer " + r.deployer})
	c.expect(t, "hello", `"name":"ci"`)
	c.send(wsText, []byte(`{}`), false)
	if code := c.closeCode(t, 3*time.Second); code != -1 {
		t.Fatalf("an unmasked frame ends the connection: %d", code)
	}
	c = wsDial(t, r.url, map[string]string{"Authorization": "Bearer " + r.deployer})
	c.expect(t, "hello", `"name":"ci"`)
	c.send(wsText, make([]byte, wsMaxMessage+1), true)
	if code := c.closeCode(t, 3*time.Second); code != -1 {
		t.Fatalf("an oversized message ends the connection: %d", code)
	}
}

func TestStoppingTheDaemonClosesTheEventStreams(t *testing.T) {
	r := newEventRig(t)
	c := wsDial(t, r.url, map[string]string{"Authorization": "Bearer " + r.deployer})
	c.expect(t, "hello", "ci")
	r.srv.events.closeAll()
	if code := c.closeCode(t, 3*time.Second); code != 1001 {
		t.Fatalf("a stopping daemon says going away: %d", code)
	}
	if d := wsDial(t, r.url, map[string]string{"Authorization": "Bearer " + r.deployer}); d.code == http.StatusSwitchingProtocols {
		if code := d.closeCode(t, 3*time.Second); code != 1013 {
			t.Fatalf("after closing, a new connection is turned away: %d", code)
		}
	}
}

// Every store tells the UI when it changes.
func TestStoresPublishTheirChanges(t *testing.T) {
	r := newEventRig(t)
	c := wsDial(t, r.url, map[string]string{"Authorization": "Bearer " + r.deployer})
	c.expect(t, "hello", "ci")
	if _, _, err := r.tokens.Create("new", RoleViewer); err != nil {
		t.Fatal(err)
	}
	c.expect(t, "change", `"tokens"`)
}
