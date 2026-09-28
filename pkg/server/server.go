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
	"io/fs"
	"log"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/status"
)

//go:embed ui
var uiFS embed.FS

// Options configures a Server.
type Options struct {
	Addr   string
	Tokens *TokenStore
	Audit  *Audit
	Status func(ctx context.Context) (*status.Snapshot, error)
	// Plan works out what apply would do for an unpacked configuration directory. Without it
	// POST /v1/plan is not served. It must not change the host.
	Plan PlanFunc
	// Apply and Jobs together enable POST /v1/apply and /v1/jobs. Apply changes the host, so it is
	// only ever called with a plan that passed the gate in handleApply.
	Apply ApplyFunc
	Jobs  *Jobs
	// Instances, with Jobs, enables the tenant-instance endpoints (PUT/DELETE /v1/instances/{name}, update,
	// list). InstancePolicy says what a spec may ask for.
	Instances      InstanceOps
	InstancePolicy engine.InstancePolicy
	// Plans records the plans made and the admin approvals that let an apply through. It is
	// required with Apply: an apply only ever runs a plan somebody approved.
	Plans *Plans
	// ImageBuild, with Jobs, enables POST /v1/images/build. A token with a scope may use it for an
	// image its scope allows (see InstanceOps); one without a scope may build anything.
	ImageBuild ImageBuildFunc
	// Users, with Sessions, enables local sign-in for the UI (POST /api/login, GET /api/session). OIDC,
	// with Sessions, adds a generic OpenID Connect sign-in. Either way a signed-in person may call the
	// API with a session cookie instead of a pasted token; API tokens keep working unchanged.
	Users    *UserStore
	Sessions *Sessions
	OIDC     *OIDCConfig
	// JobDrain is how long a shutdown waits for a running apply to finish (default 5 minutes).
	JobDrain  time.Duration
	Version   string
	StatusTTL time.Duration // how long a status snapshot is reused (default 5s)
}

type Server struct {
	opts Options

	cmu      sync.Mutex
	cache    *status.Snapshot
	cachedAt time.Time

	planSlots chan struct{}
	applyMu   sync.Mutex // one apply at a time on this host
	jobsWG    sync.WaitGroup
}

func New(opts Options) (*Server, error) {
	if opts.Tokens == nil || opts.Status == nil {
		return nil, errors.New("server needs a token store and a status source")
	}
	if (opts.Users != nil || opts.OIDC != nil) && opts.Sessions == nil {
		return nil, errors.New("local users and OIDC sign-in need a session signer")
	}
	if opts.OIDC != nil && opts.Users == nil {
		return nil, errors.New("OIDC sign-in needs a user store to record who signed in")
	}
	if (opts.Apply != nil || opts.Instances != nil || opts.ImageBuild != nil) && opts.Jobs == nil {
		return nil, errors.New("apply, instances and image builds need a job store: every change to the host is a job with a record")
	}
	if opts.Apply != nil && (opts.Plan == nil || opts.Plans == nil) {
		return nil, errors.New("apply needs a plan source and a plan store (approvals) to check the plan against")
	}
	if opts.Jobs != nil && opts.Apply == nil && opts.Instances == nil && opts.ImageBuild == nil {
		return nil, errors.New("a job store is only useful with apply, instances or image builds")
	}
	if opts.JobDrain <= 0 {
		opts.JobDrain = 5 * time.Minute
	}
	if opts.StatusTTL <= 0 {
		opts.StatusTTL = 5 * time.Second
	}
	return &Server{opts: opts, planSlots: make(chan struct{}, maxConcurrentPlans)}, nil
}

type actorHolder struct {
	name   string
	role   Role
	detail string
	scope  *Scope // set when the token is limited to tenant instances
}

// auditDetail attaches a note to the current request's audit entry.
func auditDetail(r *http.Request, format string, a ...any) {
	if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
		h.detail = fmt.Sprintf(format, a...)
	}
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

// auth requires a token of at least the given role. A token limited to a scope of tenant instances is
// refused here: it may only call the instance endpoints (authScoped).
func (s *Server) auth(min Role, next http.HandlerFunc) http.Handler {
	return s.authWith(min, false, next)
}

// authScoped is auth for the endpoints a scoped token may call. The handler enforces the scope.
func (s *Server) authScoped(min Role, next http.HandlerFunc) http.Handler {
	return s.authWith(min, true, next)
}

func (s *Server) authWith(min Role, allowScoped bool, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A person signed in through the UI first: the session cookie carries a username and role, and a
		// session is never scoped, so it may call any endpoint its role allows.
		if s.opts.Sessions != nil {
			if username, role, ok := s.opts.Sessions.FromRequest(r); ok {
				if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
					h.name, h.role = username, role
				}
				if !role.Allows(min) {
					writeError(w, http.StatusForbidden, "forbidden", "this account's role cannot do that")
					return
				}
				next(w, r)
				return
			}
		}
		t, ok := s.opts.Tokens.Verify(bearer(r))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="native-ops"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API token is required")
			return
		}
		if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
			h.name, h.role = t.Name, t.Role
			if t.Scoped() {
				h.scope = t.Scope
			}
		}
		if !t.Role.Allows(min) {
			writeError(w, http.StatusForbidden, "forbidden", "this token's role cannot do that")
			return
		}
		if t.Scoped() && !allowScoped {
			writeError(w, http.StatusForbidden, "forbidden", "this token is limited to managing its own instances")
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
			Status: sw.code, Remote: remote, Millis: time.Since(start).Milliseconds(), Detail: h.detail,
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

// uiFiles maps a request path to an embedded file: "/" is the page and every
// other file is served at its path under ui/ (/static/..., /static-modules/...).
// It is built from what is actually embedded, so only those files are ever served.
var uiFiles = func() map[string]string {
	m := map[string]string{"/": "ui/index.html"}
	err := fs.WalkDir(uiFS, "ui", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			m[strings.TrimPrefix(p, "ui")] = p
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	return m
}()

// uiTypes is explicit because the host's mime database may be missing or lack woff2,
// and the daemon sends nosniff.
var uiTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".woff2": "font/woff2",
}

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
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		if ct, ok := uiTypes[path.Ext(name)]; ok {
			h.Set("Content-Type", ct)
		}
		// ServeContent (unlike FileServer) never redirects.
		http.ServeContent(w, r, name, uiStarted, bytes.NewReader(data))
	})
}

// Handler returns the daemon's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.opts.Version})
	})
	if s.opts.Users != nil || s.opts.OIDC != nil {
		mux.HandleFunc("GET /api/session", s.handleSession)
		mux.HandleFunc("POST /api/login", s.handleLogin)
		mux.HandleFunc("POST /api/logout", s.handleLogout)
	}
	if s.opts.OIDC != nil {
		mux.HandleFunc("GET /auth/oidc", s.handleOIDCStart)
		mux.HandleFunc("GET /auth/oidc/callback", s.handleOIDCCallback)
	}
	mux.Handle("GET /v1/whoami", s.authScoped(RoleViewer, func(w http.ResponseWriter, r *http.Request) {
		h, _ := r.Context().Value(actorKey{}).(*actorHolder)
		out := map[string]any{"name": h.name, "role": h.role, "version": s.opts.Version}
		if h.scope != nil {
			out["scope"] = h.scope
		}
		writeJSON(w, http.StatusOK, out)
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
	if s.opts.Plan != nil {
		mux.Handle("POST /v1/plan", s.auth(RolePlanner, s.handlePlan))
	}
	if s.opts.Plans != nil {
		mux.Handle("GET /v1/plans", s.auth(RoleViewer, s.handlePlanList))
		mux.Handle("GET /v1/plans/{hash}", s.auth(RoleViewer, s.handlePlanGet))
		mux.Handle("POST /v1/plans/{hash}/approve", s.auth(RoleAdmin, s.handlePlanApprove))
		mux.Handle("DELETE /v1/plans/{hash}/approval", s.auth(RoleAdmin, s.handlePlanRevoke))
	}
	if s.opts.Instances != nil {
		mux.Handle("GET /v1/instances", s.authScoped(RoleViewer, s.handleInstanceList))
		mux.Handle("GET /v1/instances/{name}", s.authScoped(RoleViewer, s.handleInstanceGet))
		mux.Handle("PUT /v1/instances/{name}", s.authScoped(RoleDeployer, s.handleInstancePut))
		mux.Handle("POST /v1/instances/{name}/update", s.authScoped(RoleDeployer, s.handleInstanceUpdate))
		mux.Handle("POST /v1/instances/{name}/resize", s.authScoped(RoleDeployer, s.handleInstanceResize))
		mux.Handle("POST /v1/instances/{name}/suspend", s.authScoped(RoleDeployer, s.handleInstanceSuspend))
		mux.Handle("DELETE /v1/instances/{name}", s.authScoped(RoleDeployer, s.handleInstanceDelete))
	}
	if s.opts.Jobs != nil {
		mux.Handle("GET /v1/jobs", s.authScoped(RoleViewer, s.handleJobs))
		mux.Handle("GET /v1/jobs/{id}", s.authScoped(RoleViewer, s.handleJob))
	}
	if s.opts.Apply != nil {
		mux.Handle("POST /v1/apply", s.auth(RoleDeployer, s.handleApply))
	}
	if s.opts.ImageBuild != nil {
		mux.Handle("POST /v1/images/build", s.authScoped(RoleDeployer, s.handleImageBuild))
	}
	ui := s.uiHandler()
	mux.Handle("GET /{$}", ui)
	mux.Handle("GET /index.html", ui)
	mux.Handle("GET /static/", ui)
	mux.Handle("GET /static-modules/", ui)
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
		WriteTimeout:      120 * time.Second, // a plan upload is read (30s) and planned (60s) before the answer is written
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
		// A running apply is not cut off: wait for it, so the host is not left part-way.
		if !s.WaitForJobs(s.opts.JobDrain) {
			log.Printf("shutdown: an apply is still running after %s; it will be recorded as interrupted", s.opts.JobDrain)
		}
		return nil
	}
}
