package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/status"
)

// InstanceOps is what the instance endpoints do on the host (engine.Instances). Every change is one
// idempotent call that reports progress to logf.
type InstanceOps interface {
	Template(ctx context.Context, name string) (template string, exists bool, err error)
	Launch(ctx context.Context, name string, spec engine.InstanceSpec, logf func(string, ...any)) (string, error)
	Update(ctx context.Context, name string, req engine.UpdateRequest, logf func(string, ...any)) error
	Resize(ctx context.Context, name string, req engine.ResizeRequest, logf func(string, ...any)) error
	Suspend(ctx context.Context, name string, req engine.SuspendRequest, logf func(string, ...any)) error
	Destroy(ctx context.Context, name string, purge bool, logf func(string, ...any)) error
	SetLabels(ctx context.Context, name string, set map[string]string, remove []string, logf func(string, ...any)) error
}

const maxInstanceBody = 64 << 10

// The tenant-instance endpoints exist so a system that manages tenants (the fleet manager) can create,
// change and remove them without holding a key to the host. What stops it doing more than that:
//
//   - a token with a scope (names, images, domains) may only call these endpoints, and only for
//     instances, images and domains that match its scope;
//   - a spec is validated against a short list of shapes (engine.InstanceSpec.Validate) and the
//     operator's policy, so it cannot reach a privileged container, a host path, someone else's
//     volume, or a Caddy directive the operator did not allow;
//   - only an instance launched from a template can be changed or removed here: gitea, plane and the
//     rest carry none and are refused;
//   - each change is a job (one at a time on the host, shared with apply) with a record on disk.

func (s *Server) actorScope(r *http.Request) (name string, scope *Scope) {
	name = "-"
	if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
		name, scope = h.name, h.scope
	}
	return
}

// checkScope reports why a scoped token may not use this instance name, image or domain ("" is fine).
func checkScope(scope *Scope, name, image, domain string) string {
	if scope == nil {
		return ""
	}
	switch {
	case !scope.AllowsName(name):
		return "this token may not manage an instance called " + name
	case image != "" && !scope.AllowsImage(image):
		return "this token may not use the image " + image
	case domain != "" && !scope.AllowsDomain(domain):
		return "this token may not publish the domain " + domain
	}
	return ""
}

// parseLabelFilter reads ?label=k=v (repeatable): an instance must carry every one.
func parseLabelFilter(raw []string) (map[string]string, error) {
	var out map[string]string
	for _, f := range raw {
		k, v, ok := strings.Cut(f, "=")
		if !ok || !engine.ValidLabelKey(k) || v == "" {
			return nil, fmt.Errorf("label must look like environment=production, not %q", f)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out, nil
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// checkLabelScope reports why a token with a label scope may not make this change to the instance called
// name ("" is fine). It matters only for a token whose scope names labels. The instance's labels *now* must
// be in scope (so it cannot touch another environment's instance), and so must the labels the change would
// leave it with: desired, when the caller supplies a whole set (hasDesired), which a new instance must.
// code is the HTTP status to answer with when msg is not empty.
func (s *Server) checkLabelScope(ctx context.Context, scope *Scope, name string, desired map[string]string, hasDesired bool) (code int, msg string) {
	if scope == nil || len(scope.Labels) == 0 {
		return 0, ""
	}
	snap, err := s.snapshot(ctx)
	if err != nil {
		log.Printf("instance labels scope %s: %v", name, err)
		return http.StatusBadGateway, "could not read the host's state to check the token's labels"
	}
	exists := false
	for _, i := range snap.Instances {
		if i.Name != name {
			continue
		}
		exists = true
		if !scope.AllowsLabels(i.Labels) {
			// The same answer as for an instance that is not the caller's to see.
			return http.StatusForbidden, "this token may not manage an instance called " + name
		}
	}
	if hasDesired && !scope.AllowsLabels(desired) {
		return http.StatusForbidden, "this token may only manage instances labelled " + scopeLabels(scope) + ", and the change would leave this one outside that"
	}
	if !exists && !hasDesired {
		return http.StatusForbidden, "a token limited to labels (" + scopeLabels(scope) + ") must give the labels of an instance it creates"
	}
	return 0, ""
}

func scopeLabels(scope *Scope) string {
	parts := []string{}
	for k, v := range scope.Labels {
		parts = append(parts, k+"="+strings.Join(v, "|"))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	return readJSONLimit(w, r, v, maxInstanceBody)
}

// readJSONLimit is readJSON with a body limit of its own.
func readJSONLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	body := http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be JSON of the documented shape: "+trim(err.Error()))
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "the body must be one JSON object")
		return false
	}
	return true
}

func trim(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// startInstanceJob takes the host, records a job, and runs work as it. It answers the request itself.
func (s *Server) startInstanceJob(w http.ResponseWriter, r *http.Request, op, name string, work func(ctx context.Context, logf func(string, ...any)) error) {
	if !s.applyMu.TryLock() {
		msg := "another change is already running on this host"
		if j, ok := s.opts.Jobs.Running(); ok {
			msg += " (" + string(j.ID) + ")"
		}
		auditDetail(r, "instance %s %s rejected: busy", op, name)
		writeError(w, http.StatusConflict, "busy", msg)
		return
	}
	actor, _ := s.actorScope(r)
	job, err := s.opts.Jobs.CreateKind("instance:"+op, actor, "", name, "")
	if err != nil {
		s.applyMu.Unlock()
		log.Printf("instance %s: create job: %v", op, err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record the job, so nothing was changed")
		return
	}
	auditDetail(r, "instance %s %s job=%s", op, name, job.ID)
	s.startJob(job.ID, actor, "instance "+op+" "+name, "instance "+op+" "+name, nil, work)
	w.Header().Set("Location", "/v1/jobs/"+string(job.ID))
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

// handleInstancePut is PUT /v1/instances/{name}: make the instance exist as the spec says (create it, or
// resume and converge the one an earlier call created).
func (s *Server) handleInstancePut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var spec engine.InstanceSpec
	if !incus.ValidName(name) || !readJSON(w, r, &spec) {
		if !incus.ValidName(name) {
			writeError(w, http.StatusBadRequest, "bad_request", "not a valid instance name")
		}
		return
	}
	_, scope := s.actorScope(r)
	if msg := checkScope(scope, name, spec.Image, spec.Domain); msg != "" {
		auditDetail(r, "instance put %s refused: outside the token's scope", name)
		writeError(w, http.StatusForbidden, "out_of_scope", msg)
		return
	}
	if err := spec.Validate(name, s.opts.InstancePolicy); err != nil {
		auditDetail(r, "instance put %s refused: invalid spec", name)
		writeError(w, http.StatusBadRequest, "invalid_spec", err.Error())
		return
	}
	if code, msg := s.checkLabelScope(r.Context(), scope, name, spec.Labels, spec.Labels != nil); msg != "" {
		auditDetail(r, "instance put %s refused: outside the token's labels", name)
		writeError(w, code, "out_of_scope", msg)
		return
	}
	tpl, exists, err := s.opts.Instances.Template(r.Context(), name)
	if err != nil {
		log.Printf("instance put %s: %v", name, err)
		writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
		return
	}
	if exists && tpl != spec.Template {
		auditDetail(r, "instance put %s refused: exists, not from this template", name)
		code, msg := http.StatusConflict, "an instance with that name exists and was not launched from this template"
		if tpl == "" {
			code, msg = http.StatusForbidden, "that is not a tenant instance: it was not launched from a template, so it cannot be managed here"
		}
		writeError(w, code, "not_managed", msg)
		return
	}
	s.startInstanceJob(w, r, "put", name, func(ctx context.Context, logf func(string, ...any)) error {
		_, err := s.opts.Instances.Launch(ctx, name, spec, logf)
		return err
	})
}

// handleInstanceUpdate is POST /v1/instances/{name}/update: move a tenant to another image.
func (s *Server) handleInstanceUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req engine.UpdateRequest
	if !incus.ValidName(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "not a valid instance name")
		return
	}
	if !readJSON(w, r, &req) {
		return
	}
	_, scope := s.actorScope(r)
	if msg := checkScope(scope, name, req.Image, ""); msg != "" {
		auditDetail(r, "instance update %s refused: outside the token's scope", name)
		writeError(w, http.StatusForbidden, "out_of_scope", msg)
		return
	}
	if code, msg := s.checkLabelScope(r.Context(), scope, name, nil, false); msg != "" {
		auditDetail(r, "instance update %s refused: outside the token's labels", name)
		writeError(w, code, "out_of_scope", msg)
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_spec", err.Error())
		return
	}
	if !s.requireManaged(w, r, name, "update") {
		return
	}
	s.startInstanceJob(w, r, "update", name, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.Instances.Update(ctx, name, req, logf)
	})
}

// handleInstanceResize is POST /v1/instances/{name}/resize: a live CPU/memory change, no restart.
func (s *Server) handleInstanceResize(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req engine.ResizeRequest
	if !incus.ValidName(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "not a valid instance name")
		return
	}
	if !readJSON(w, r, &req) {
		return
	}
	_, scope := s.actorScope(r)
	if msg := checkScope(scope, name, "", ""); msg != "" {
		auditDetail(r, "instance resize %s refused: outside the token's scope", name)
		writeError(w, http.StatusForbidden, "out_of_scope", msg)
		return
	}
	if code, msg := s.checkLabelScope(r.Context(), scope, name, nil, false); msg != "" {
		auditDetail(r, "instance resize %s refused: outside the token's labels", name)
		writeError(w, code, "out_of_scope", msg)
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_spec", err.Error())
		return
	}
	if !s.requireManaged(w, r, name, "resize") {
		return
	}
	s.startInstanceJob(w, r, "resize", name, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.Instances.Resize(ctx, name, req, logf)
	})
}

// handleInstanceSuspend is POST /v1/instances/{name}/suspend: replaces the published route with a
// static 503 carrying a reason, without touching the instance. Undone by asking for the instance
// again (PUT /v1/instances/{name}): every launch republishes the normal route unconditionally.
func (s *Server) handleInstanceSuspend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req engine.SuspendRequest
	if !incus.ValidName(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "not a valid instance name")
		return
	}
	if !readJSON(w, r, &req) {
		return
	}
	_, scope := s.actorScope(r)
	if msg := checkScope(scope, name, "", req.Domain); msg != "" {
		auditDetail(r, "instance suspend %s refused: outside the token's scope", name)
		writeError(w, http.StatusForbidden, "out_of_scope", msg)
		return
	}
	if code, msg := s.checkLabelScope(r.Context(), scope, name, nil, false); msg != "" {
		auditDetail(r, "instance suspend %s refused: outside the token's labels", name)
		writeError(w, code, "out_of_scope", msg)
		return
	}
	if err := req.Validate(s.opts.InstancePolicy); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_spec", err.Error())
		return
	}
	if !s.requireManaged(w, r, name, "suspend") {
		return
	}
	s.startInstanceJob(w, r, "suspend", name, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.Instances.Suspend(ctx, name, req, logf)
	})
}

// handleInstanceDelete is DELETE /v1/instances/{name}?purge_volumes=true.
func (s *Server) handleInstanceDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !incus.ValidName(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "not a valid instance name")
		return
	}
	_, scope := s.actorScope(r)
	if msg := checkScope(scope, name, "", ""); msg != "" {
		auditDetail(r, "instance delete %s refused: outside the token's scope", name)
		writeError(w, http.StatusForbidden, "out_of_scope", msg)
		return
	}
	if code, msg := s.checkLabelScope(r.Context(), scope, name, nil, false); msg != "" {
		auditDetail(r, "instance delete %s refused: outside the token's labels", name)
		writeError(w, code, "out_of_scope", msg)
		return
	}
	purge := r.URL.Query().Get("purge_volumes") == "true"
	if !s.requireManaged(w, r, name, "delete") {
		return
	}
	s.startInstanceJob(w, r, "delete", name, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.Instances.Destroy(ctx, name, purge, logf)
	})
}

// requireManaged answers the request unless name is an instance launched from a template.
func (s *Server) requireManaged(w http.ResponseWriter, r *http.Request, name, op string) bool {
	tpl, exists, err := s.opts.Instances.Template(r.Context(), name)
	switch {
	case err != nil:
		log.Printf("instance %s %s: %v", op, name, err)
		writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
	case !exists:
		writeError(w, http.StatusNotFound, "not_found", "no such instance")
	case tpl == "":
		auditDetail(r, "instance %s %s refused: not a tenant instance", op, name)
		writeError(w, http.StatusForbidden, "not_managed", "that is not a tenant instance: it was not launched from a template, so it cannot be managed here")
	default:
		return true
	}
	return false
}

// tenantView is a tenant instance as the API shows it.
type tenantView struct {
	Name     string            `json:"name"`
	Status   string            `json:"status"`
	Template string            `json:"template"`
	Image    string            `json:"image,omitempty"`
	IPv4     []string          `json:"ipv4,omitempty"`
	Created  string            `json:"created_at,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

func viewOfInstance(i status.Instance) tenantView {
	return tenantView{Name: i.Name, Status: i.Status, Template: i.Recorded["template"], Image: i.Recorded["image"], IPv4: i.IPv4, Created: i.CreatedAt, Labels: i.Labels}
}

// handleInstanceList is GET /v1/instances: the tenant instances the caller may see.
func (s *Server) handleInstanceList(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
	if err != nil {
		log.Printf("instances: %v", err)
		writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
		return
	}
	_, scope := s.actorScope(r)
	want, err := parseLabelFilter(r.URL.Query()["label"])
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	out := []tenantView{}
	for _, i := range snap.Instances {
		if i.Recorded["template"] == "" || (scope != nil && (!scope.AllowsName(i.Name) || !scope.AllowsLabels(i.Labels))) {
			continue
		}
		if !hasLabels(i.Labels, want) {
			continue
		}
		out = append(out, viewOfInstance(i))
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": out})
}

// handleInstanceGet is GET /v1/instances/{name}.
func (s *Server) handleInstanceGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	snap, err := s.snapshot(r.Context())
	if err != nil {
		log.Printf("instance: %v", err)
		writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
		return
	}
	_, scope := s.actorScope(r)
	for _, i := range snap.Instances {
		if i.Name == name && i.Recorded["template"] != "" && (scope == nil || (scope.AllowsName(name) && scope.AllowsLabels(i.Labels))) {
			writeJSON(w, http.StatusOK, viewOfInstance(i))
			return
		}
	}
	// The same answer whether it does not exist or is not the caller's to see.
	writeError(w, http.StatusNotFound, "not_found", "no such instance")
}

// handleInstanceLabels is PATCH /v1/instances/{name}/labels {"set": {...}, "remove": [...]}: change a tenant
// instance's labels live, with no restart. A token limited to labels may only leave the instance inside its
// own scope, so it cannot relabel an instance into (or out of) another environment.
func (s *Server) handleInstanceLabels(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !incus.ValidName(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "not a valid instance name")
		return
	}
	var req struct {
		Set    map[string]string `json:"set"`
		Remove []string          `json:"remove"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Set) == 0 && len(req.Remove) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "give labels to set or remove")
		return
	}
	for _, k := range req.Remove {
		if !engine.ValidLabelKey(k) {
			writeError(w, http.StatusBadRequest, "invalid_spec", fmt.Sprintf("label name %q is not allowed", k))
			return
		}
	}
	if err := engine.ValidateLabels(req.Set); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_spec", err.Error())
		return
	}
	_, scope := s.actorScope(r)
	if msg := checkScope(scope, name, "", ""); msg != "" {
		auditDetail(r, "instance labels %s refused: outside the token's scope", name)
		writeError(w, http.StatusForbidden, "out_of_scope", msg)
		return
	}
	if scope != nil && len(scope.Labels) > 0 {
		// What the instance would carry afterwards: what it has now, with the change applied.
		snap, err := s.snapshot(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
			return
		}
		after := map[string]string{}
		for _, i := range snap.Instances {
			if i.Name == name {
				for k, v := range i.Labels {
					after[k] = v
				}
			}
		}
		for _, k := range req.Remove {
			delete(after, k)
		}
		for k, v := range req.Set {
			after[k] = v
		}
		if code, msg := s.checkLabelScope(r.Context(), scope, name, after, true); msg != "" {
			auditDetail(r, "instance labels %s refused: outside the token's labels", name)
			writeError(w, code, "out_of_scope", msg)
			return
		}
	}
	tpl, exists, err := s.opts.Instances.Template(r.Context(), name)
	switch {
	case err != nil:
		log.Printf("instance labels %s: %v", name, err)
		writeError(w, http.StatusBadGateway, "host_unavailable", "could not read the host's state")
		return
	case !exists:
		writeError(w, http.StatusNotFound, "not_found", "no such instance")
		return
	case tpl == "":
		writeError(w, http.StatusForbidden, "not_managed", "that is not a tenant instance: it was not launched from a template, so it cannot be managed here")
		return
	}
	auditDetail(r, "instance labels %s set=%s remove=%s", name, engine.FormatLabels(req.Set), strings.Join(req.Remove, ","))
	s.startInstanceJob(w, r, "labels", name, func(ctx context.Context, logf func(string, ...any)) error {
		return s.opts.Instances.SetLabels(ctx, name, req.Set, req.Remove, logf)
	})
}
