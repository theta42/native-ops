package server

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A minimal WebSocket (RFC 6455) server side, enough for GET /v1/events: text messages both ways, pings,
// and a clean close. The daemon pushes small JSON notes and the browser sends one or two, so there is no
// fragmentation of outgoing messages, no extensions (no compression) and no subprotocol, and an incoming
// message larger than wsMaxMessage ends the connection. No dependency for something this small.

const (
	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxMessage = 16 << 10

	wsText  = 0x1
	wsClose = 0x8
	wsPing  = 0x9
	wsPong  = 0xA
)

var errNotWebSocket = errors.New("not a websocket upgrade")

// wsConn is one upgraded connection. Writes are serialized; reads happen on one goroutine.
type wsConn struct {
	conn net.Conn
	rd   *bufio.Reader
	wmu  sync.Mutex
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// wsUpgrade answers a WebSocket handshake and takes the connection over. On a request that is not one it
// answers 400 itself and returns errNotWebSocket.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != http.MethodGet || !headerHas(r.Header, "Connection", "upgrade") || !headerHas(r.Header, "Upgrade", "websocket") ||
		r.Header.Get("Sec-WebSocket-Version") != "13" || key == "" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		writeError(w, http.StatusBadRequest, "bad_request", "this endpoint speaks WebSocket (version 13) only")
		return nil, errNotWebSocket
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not take the connection over")
		return nil, err
	}
	// The server's read and write timeouts were set for an ordinary request; this one lives on.
	_ = conn.SetDeadline(time.Time{})
	sum := sha1.Sum([]byte(key + wsGUID))
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, rd: rw.Reader}, nil
}

// writeFrame sends one unmasked, unfragmented frame.
func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write(append(hdr, payload...)); err != nil {
		return err
	}
	return nil
}

func (c *wsConn) writeText(b []byte) error { return c.writeFrame(wsText, b) }

// close sends a close frame (best effort) and closes the connection.
func (c *wsConn) close(code uint16, reason string) {
	payload := binary.BigEndian.AppendUint16(nil, code)
	if len(reason) > 120 {
		reason = reason[:120]
	}
	_ = c.writeFrame(wsClose, append(payload, reason...))
	_ = c.conn.Close()
}

// readMessage returns the next text message, answering pings on the way. A close, a binary message, an
// unmasked frame (a client must mask) or one too large is an error, and the caller closes.
func (c *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.rd, h[:]); err != nil {
			return nil, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0F
		if h[0]&0x70 != 0 {
			return nil, errors.New("websocket: reserved bits set")
		}
		if h[1]&0x80 == 0 {
			return nil, errors.New("websocket: unmasked client frame")
		}
		n := uint64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.rd, b[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.rd, b[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > wsMaxMessage || uint64(len(msg))+n > wsMaxMessage {
			return nil, errors.New("websocket: message too large")
		}
		var mask [4]byte
		if _, err := io.ReadFull(c.rd, mask[:]); err != nil {
			return nil, err
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.rd, payload); err != nil {
			return nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
		switch op {
		case wsPing:
			if err := c.writeFrame(wsPong, payload); err != nil {
				return nil, err
			}
			continue
		case wsPong:
			continue
		case wsClose:
			return nil, io.EOF
		case wsText, 0x0: // a text message, or its continuation
			if (op == wsText) != (msg == nil) {
				return nil, errors.New("websocket: a continuation without a message, or a message inside one")
			}
			msg = append(msg, payload...)
			if msg == nil {
				msg = []byte{}
			}
			if fin {
				return msg, nil
			}
		default:
			return nil, errors.New("websocket: unsupported frame")
		}
	}
}
