package server

import (
	"context"
	"log"
	"net/http"

	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/incus"
)

// Moving a tenant instance between hosts (theta42/native-ops#87). Every daemon acts only on its own host
// and none calls another; an orchestrator (a fleet manager) holds a token on each and runs the sequence:
//
//  1. POST /v1/instances/{name}/migrate on the source: a job that copies the instance and its volumes to
//     the target (an Incus remote configured on the source), stops the source, starts the target and
//     health-checks it, rolling back on failure; on success it drops the route here and marks the copy
//     here as migrated;
//  2. GET /v1/instances/{name}/export-spec on the source: the spec, never any secret;
//  3. POST /v1/instances/{name}/adopt on the target, with that spec: a job that checks the instance there,
//     records the spec and publishes the route;
//  4. the orchestrator points DNS at the target;
//  5. POST /v1/instances/{name}/finalize-migration on the source: a job that deletes the copy there.

// authMigrate is the gate of the endpoints that move an instance between hosts: an admin, or a token
// scoped to the instance (the handler checks the scope). An unscoped deployer, the token a configuration
// repository's CI holds, may not: moving a tenant is the tenant manager's call, or an admin's.
func (s *Server) authMigrate(next http.HandlerFunc) http.Handler {
	return s.authScoped(RoleDeployer, func(w http.ResponseWriter, r *http.Request) {
		h, _ := r.Context().Value(actorKey{}).(*actorHolder)
		if h == nil || (h.scope == nil && !h.role.Allows(RoleAdmin)) {
			writeError(w, http.StatusForbidden, "forbidden", "moving an instance between hosts needs an admin, or a token scoped to the instance")
			return
		}
		next(w, r)
	})
}

// instanceInScope answers the request unless the caller's scope (names, then labels) allows the instance.
func (s *Server) instanceInScope(w http.ResponseWriter, r *http.Request, name, op, image, domain string, labels map[string]string, hasLabels bool) bool {
	if !incus.ValidName(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "not a valid instance name")
		return false
	}
	_, scope := s.actorScope(r)
	if msg := checkScope(scope, name, image, domain); msg != "" {
		auditDetail(r, "instance %s %s refused: outside the token's scope", op, name)
		writeError(w, http.StatusForbidden, "out_of_scope", msg)
		return false
	}
	if code, msg := s.checkLabelScope(r.Context(), scope, name, labels, hasLabels); msg != "" {
		auditDetail(r, "instance %s %s refused: outside the token's labels", op, name)
		writeError(w, code, "out_of_scope", msg)
		return false
	}
	return true
}

// migrateBody is the body of POST /v1/instances/{name}/migrate: where to (an Incus remote configured on
// this host, or a host of fleet.yml, whose incus_remote is that remote) and whether to resume.
type migrateBody struct {
	engine.MigrateRequest
	TargetHost string `json:"target_host,omitempty"`
}

// handleInstanceMigrate is POST /v1/instances/{name}/migrate
// {"target_remote": "...", "resume": false} or {"target_host": "...", ...}.
func (s *Server) handleInstanceMigrate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !incus.ValidName(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "not a valid instance name")
		return
	}
	var body migrateBody
	if !readJSON(w, r, &body) {
		return
	}
	req := body.MigrateRequest
	if body.TargetHost != "" {
		if req.TargetRemote != "" {
			writeError(w, http.StatusBadRequest, "bad_request", "give target_host or target_remote, not both")
			return
		}
		remote, code, err := s.targetRemoteOf(body.TargetHost)
		if err != nil {
			writeError(w, code, "bad_target_host", err.Error())
			return
		}
		req.TargetRemote = remote
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !s.instanceInScope(w, r, name, "migrate", "", "", nil, false) {
		return
	}
	rec, ok := s.managedRecord(w, r, name, "migrate", false)
	if !ok {
		return
	}
	if why := rec.MigrationRefusal(); why != "" {
		auditDetail(r, "instance migrate %s refused: %s", name, why)
		writeError(w, http.StatusConflict, "not_migratable", "cannot migrate "+name+": "+why)
		return
	}
	s.startInstanceJob(w, r, "migrate", name, func(ctx context.Context, logf func(string, ...any)) error {
		summary, err := s.opts.Instances.Migrate(ctx, name, req, logf)
		if err != nil {
			return err
		}
		logf("%s", summary)
		if id := jobIDOf(ctx); id != "" {
			s.opts.Jobs.update(id, func(j *Job) { j.Result = summary })
		}
		return nil
	})
}

// handleInstanceExportSpec is GET /v1/instances/{name}/export-spec: what a target host needs to adopt the
// instance (image, limits, volumes, health, domain, route directives, labels). It never includes the
// environment, which may hold secrets and moves with the instance in its environment file.
func (s *Server) handleInstanceExportSpec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	_, scope := s.actorScope(r)
	notFound := func() { writeError(w, http.StatusNotFound, "not_found", "no such instance") }
	if !incus.ValidName(name) || (scope != nil && !scope.AllowsName(name)) {
		notFound()
		return
	}
	rec, err := s.opts.Instances.Record(r.Context(), name)
	if err != nil {
		log.Printf("instance export-spec %s: %v", name, err)
		writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
		return
	}
	// The same answer as get for what does not exist, is not a tenant, or is not the caller's to see.
	if !rec.Exists || rec.Template == "" || (scope != nil && !scope.AllowsLabels(rec.Labels)) {
		notFound()
		return
	}
	if rec.Spec == nil {
		writeError(w, http.StatusConflict, "no_spec", "no spec is recorded on this instance (it was launched before specs were recorded): ask for it again with PUT and the same spec, which records one")
		return
	}
	spec := *rec.Spec
	spec.Env = nil
	out := map[string]any{"name": name, "spec": spec}
	if rec.MigratedTo != "" {
		out["migrated_to"] = rec.MigratedTo
	}
	writeJSON(w, http.StatusOK, out)
}

// handleInstanceAdopt is POST /v1/instances/{name}/adopt with an instance spec (export-spec's "spec"): take
// over an instance a migration put on this host. It is refused when no such instance is here.
func (s *Server) handleInstanceAdopt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !incus.ValidName(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "not a valid instance name")
		return
	}
	var spec engine.InstanceSpec
	if !readJSON(w, r, &spec) {
		return
	}
	if len(spec.Env) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_spec", "adopt takes no env: the instance's environment file moved with it (export-spec never includes one)")
		return
	}
	if !s.instanceInScope(w, r, name, "adopt", spec.Image, spec.Domain, spec.Labels, spec.Labels != nil) {
		return
	}
	if err := spec.Validate(name, s.opts.InstancePolicy); err != nil {
		auditDetail(r, "instance adopt %s refused: invalid spec", name)
		writeError(w, http.StatusBadRequest, "invalid_spec", err.Error())
		return
	}
	rec, err := s.opts.Instances.Record(r.Context(), name)
	switch {
	case err != nil:
		log.Printf("instance adopt %s: %v", name, err)
		writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
		return
	case !rec.Exists:
		auditDetail(r, "instance adopt %s refused: not on this host", name)
		writeError(w, http.StatusNotFound, "not_found", "no such instance on this host: migrate it here first")
		return
	case rec.Template == "":
		auditDetail(r, "instance adopt %s refused: not a tenant instance", name)
		writeError(w, http.StatusForbidden, "not_managed", "that is not a tenant instance: it was not launched from a template, so it cannot be managed here")
		return
	case rec.Template != spec.Template:
		auditDetail(r, "instance adopt %s refused: another template", name)
		writeError(w, http.StatusConflict, "not_managed", "the instance here was not launched from this template")
		return
	}
	s.startInstanceJob(w, r, "adopt", name, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.Instances.Adopt(ctx, name, spec, logf)
	})
}

// handleInstanceFinalizeMigration is POST /v1/instances/{name}/finalize-migration
// {"target_remote": "...", "purge_source_volumes": false}: delete this host's stopped copy of an instance
// it migrated away, once the target is verified.
func (s *Server) handleInstanceFinalizeMigration(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !incus.ValidName(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "not a valid instance name")
		return
	}
	var req engine.FinalizeMigrationRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.TargetRemote != "" {
		if err := (&engine.MigrateRequest{TargetRemote: req.TargetRemote}).Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}
	if !s.instanceInScope(w, r, name, "finalize-migration", "", "", nil, false) {
		return
	}
	rec, err := s.opts.Instances.Record(r.Context(), name)
	switch {
	case err != nil:
		log.Printf("instance finalize-migration %s: %v", name, err)
		writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
		return
	case !rec.Exists && req.TargetRemote == "":
		writeError(w, http.StatusNotFound, "not_found", "no such instance on this host (name target_remote to check that an earlier finalize completed)")
		return
	case rec.Exists && rec.Template == "":
		auditDetail(r, "instance finalize-migration %s refused: not a tenant instance", name)
		writeError(w, http.StatusForbidden, "not_managed", "that is not a tenant instance: it was not launched from a template, so it cannot be managed here")
		return
	case rec.Exists && rec.MigratedTo == "":
		auditDetail(r, "instance finalize-migration %s refused: not migrated", name)
		writeError(w, http.StatusConflict, "not_migrated", "this instance was not migrated away from this host; finalize-migration deletes only a migrated copy")
		return
	case rec.Exists && req.TargetRemote != "" && req.TargetRemote != rec.MigratedTo:
		writeError(w, http.StatusConflict, "not_migrated", "this instance was migrated to "+rec.MigratedTo+", not "+req.TargetRemote)
		return
	}
	s.startInstanceJob(w, r, "finalize-migration", name, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.Instances.FinalizeMigration(ctx, name, req, logf)
	})
}
