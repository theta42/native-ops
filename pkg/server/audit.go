package server

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// AuditEntry is one line of the append-only audit log (JSON lines).
type AuditEntry struct {
	Time   time.Time `json:"ts"`
	Actor  string    `json:"actor"` // token name, or "-" when unauthenticated
	Role   Role      `json:"role,omitempty"`
	Method string    `json:"method"`
	Path   string    `json:"path"` // never the query string: it could carry a secret
	Status int       `json:"status"`
	Remote string    `json:"remote,omitempty"`
	Millis int64     `json:"ms"`
}

// Audit appends entries to a file opened 0600. A nil *Audit discards entries.
type Audit struct {
	mu sync.Mutex
	f  *os.File
}

func OpenAudit(path string) (*Audit, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Audit{f: f}, nil
}

func (a *Audit) Log(e AuditEntry) {
	if a == nil {
		return
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.f.Write(append(b, '\n'))
}

func (a *Audit) Close() error {
	if a == nil {
		return nil
	}
	return a.f.Close()
}
