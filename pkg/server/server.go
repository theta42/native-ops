// Package server is native-ops' optional remote daemon: an authenticated HTTP
// API (and a small embedded UI) that runs ON the host it manages. It exists so a
// deployment can be driven from CI over HTTPS with an API token — nobody has to
// install anything locally or SSH anywhere. The CLI stays the source of truth
// and works without the daemon; the daemon adds what the CLI lacks: API tokens,
// an audit log, and a place for job history and locking to live.
package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/theta42/native-ops/pkg/status"
)

//go:embed ui/index.html ui/app.js ui/style.css
var uiFS embed.FS

// Options configures a Server.
type Options struct {
	Addr      string
	Tokens    *TokenStore
	Audit     *Audit
	Status    func(ctx context.Context) (*status.Snapshot, error)
	Version   string
	StatusTTL time.Duration // how long a status snapshot is reused (default 5s)
}

type Server struct {
	opts Options

	cmu      sync.Mutex
	cache    *status.Snapshot
	cachedAt time.Time
}

func New(opts Options) (*Server, error) {
	if opts.Tokens == nil || opts.Status == nil {
		return nil, errors.New("server needs a token store and a status source")
	}
	if opts.StatusTTL <= 0 {
		opts.StatusTTL = 5 * time.Second
	}
	return &Server{opts: opts}, nil
}

type actorHolder struct {
	name string
	role Role
}

type actorKey struct{}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]string{"error": msg, "code": kind})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (s *Server) auth(min Role, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := s.opts.Tokens.Verify(bearer(r))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="native-ops"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API token is required")
			return
		}
		if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
			h.name, h.role = t.Name, t.Role
		}
		if !t.Role.Allows(min) {
			writeError(w, http.StatusForbidden, "forbidden", "this token's role cannot do that")
			return
		}
		next(w, r)
	})
}

// audited wraps every request: it records who did what and how it ended.
func (s *Server) audited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h := &actorHolder{name: "-"}
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), actorKey{}, h)))
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		s.opts.Audit.Log(AuditEntry{
			Time: start.UTC(), Actor: h.name, Role: h.role, Method: r.Method, Path: r.URL.Path,
			Status: sw.code, Remote: remote, Millis: time.Since(start).Milliseconds(),
		})
	})
}

func (s *Server) snapshot(ctx context.Context) (*status.Snapshot, error) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if s.cache != nil && time.Since(s.cachedAt) < s.opts.StatusTTL {
		return s.cache, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	snap, err := s.opts.Status(ctx)
	if err != nil {
		return nil, err
	}
	s.cache, s.cachedAt = snap, time.Now()
	return snap, nil
}

// uiFiles maps a request path to an embedded file. Only these are ever served.
var uiFiles = map[string]string{"/": "ui/index.html", "/index.html": "ui/index.html", "/app.js": "ui/app.js", "/style.css": "ui/style.css"}

var uiStarted = time.Now()

func (s *Server) uiHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := uiFiles[r.URL.Path]
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
			return
		}
		data, err := uiFS.ReadFile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "ui asset missing")
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		// ServeContent (unlike FileServer) never redirects and sets Content-Type from the name.
		http.ServeContent(w, r, name, uiStarted, bytes.NewReader(data))
	})
}

// Handler returns the daemon's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.opts.Version})
	})
	mux.Handle("GET /v1/whoami", s.auth(RoleViewer, func(w http.ResponseWriter, r *http.Request) {
		h, _ := r.Context().Value(actorKey{}).(*actorHolder)
		writeJSON(w, http.StatusOK, map[string]any{"name": h.name, "role": h.role, "version": s.opts.Version})
	}))
	mux.Handle("GET /v1/status", s.auth(RoleViewer, func(w http.ResponseWriter, r *http.Request) {
		snap, err := s.snapshot(r.Context())
		if err != nil {
			log.Printf("status: %v", err)
			writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
			return
		}
		writeJSON(w, http.StatusOK, snap)
	}))
	ui := s.uiHandler()
	mux.Handle("GET /{$}", ui)
	mux.Handle("GET /index.html", ui)
	mux.Handle("GET /app.js", ui)
	mux.Handle("GET /style.css", ui)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			// Known prefix, unknown route or method: still requires a token to find out which.
			s.auth(RoleViewer, func(w http.ResponseWriter, r *http.Request) {
				writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
			}).ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.audited(mux)
}

// ListenAndServe serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
