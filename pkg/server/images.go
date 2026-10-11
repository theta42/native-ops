package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
)

// ImageBuildFunc builds app@ref from an unpacked configuration directory (the same shape POST
// /v1/plan and /v1/apply take), reporting progress to logf.
type ImageBuildFunc func(ctx context.Context, configDir, app, ref string, logf func(string, ...any)) error

// handleImageBuild is POST /v1/images/build?app=<name>&ref=<git-ref>. The body is a gzip-compressed
// tar of the native-ops-conf tree, exactly like /v1/plan and /v1/apply: the daemon needs no git
// access or credentials of its own, and every caller supplies the recipe it wants built rather than
// the daemon trusting a path on its own disk. A token with a scope may only build an image that
// matches its own Images glob, checked against the reference the build produces
// (<ImagePrefix><app>:<ref>, always with a tag: the recipe only omits one when no ref is given, and
// this endpoint always gives one), the same shape a scope is written in, e.g. "acme-platform:*".
func (s *Server) handleImageBuild(w http.ResponseWriter, r *http.Request) {
	app, ref := r.URL.Query().Get("app"), r.URL.Query().Get("ref")
	if !engine.ValidImageApp(app) {
		writeError(w, http.StatusBadRequest, "bad_request", "app must be a short lowercase label (a directory under images/ in the config repo)")
		return
	}
	if !engine.ValidImageRef(ref) {
		writeError(w, http.StatusBadRequest, "bad_request", "ref must be a plausible git branch or tag name")
		return
	}
	built := s.opts.ImagePrefix + app + ":" + ref
	actor, scope := s.actorScope(r)
	if scope != nil && !scope.AllowsImage(built) {
		auditDetail(r, "image build %s@%s refused: outside the token's scope", app, ref)
		writeError(w, http.StatusForbidden, "out_of_scope", "this token may not build "+built)
		return
	}
	u, ok := s.receiveTree(w, r, "image build")
	if !ok {
		return
	}
	// The build runs the upload's scripts on the host: only an approved recipe may (see recipes.go).
	digest, err := engine.RecipeDigest(u.root)
	if err != nil {
		u.discard()
		writeError(w, http.StatusBadRequest, "bad_config", trim(err.Error()))
		return
	}
	approved, err := s.opts.Recipes.Seen(digest, actor, app, u.sha)
	if err != nil {
		u.discard()
		writeError(w, http.StatusInternalServerError, "internal", "could not record the recipe, so nothing was built")
		return
	}
	if !approved {
		u.discard()
		auditDetail(r, "image build %s@%s refused: recipe %s not approved", app, ref, digest[:12])
		writeJSON(w, http.StatusForbidden, map[string]any{
			"code":   "recipe_not_approved",
			"digest": digest,
			"error": "this image recipe (scripts/ and images/ in the upload) has not been approved, and a build runs it on the host. " +
				"Merge the change and deploy a protected deploy tag at that commit: the tag approves its recipe; then run the build again. " +
				"(An admin can also approve it by hand: POST /v1/images/recipes/" + digest + "/approve or `native-ops remote recipe-approve " + digest + "`.) " +
				"Builds of other refs from the same recipe need no new approval.",
		})
		return
	}
	// The build waits for the host if another change runs (queue.go); the upload waits with it.
	job, err := s.queueJob(jobSpec{kind: "image:build", actor: actor, sha: u.sha, subject: app + "@" + ref,
		what: "build " + app + "@" + ref, audit: "image build " + app + "@" + ref, cleanup: u.discard,
		work: func(ctx context.Context, logf func(string, ...any)) error {
			return s.opts.ImageBuild(ctx, u.root, app, ref, logf)
		}})
	if err != nil {
		s.answerNotStarted(w, r, "image build "+app+"@"+ref, err)
		return
	}
	auditDetail(r, "image build %s@%s job=%s", app, ref, job.ID)
	answerJob(w, job, nil)
}

// ImagePruneFunc applies image retention on the host (see engine.ImageRetention); with dryRun it only
// reports what it would delete.
type ImagePruneFunc func(ctx context.Context, dryRun bool, logf func(string, ...any)) error

// handleImagePrune is POST /v1/images/prune[?dry_run=1] (deployer, not a scoped token: it acts on every
// app's images). It deletes, as a job, the images no instance runs that retention does not keep: orphans
// left by rebuilds, and tags older than the newest few per app. Builds already clean up after
// themselves; this is for a scheduled maintenance job and for images left before that.
func (s *Server) handleImagePrune(w http.ResponseWriter, r *http.Request) {
	dry := r.URL.Query().Get("dry_run") == "1" || r.URL.Query().Get("dry_run") == "true"
	actor, _ := s.actorScope(r)
	what := "image prune"
	if dry {
		what += " (dry run)"
	}
	job, err := s.queueJob(jobSpec{kind: "image:prune", actor: actor, what: what, audit: what,
		work: func(ctx context.Context, logf func(string, ...any)) error {
			return s.opts.ImagePrune(ctx, dry, logf)
		}})
	if err != nil {
		s.answerNotStarted(w, r, what, err)
		return
	}
	auditDetail(r, "%s job=%s", what, job.ID)
	answerJob(w, job, map[string]any{"dry_run": dry})
}

// RunImageRetention applies image retention on its own, every `every`, until ctx ends (0 turns it off). Nothing
// else would: a build deletes the image it displaces only if no instance runs it at that moment, which is the
// usual case (build now, deploy later), so that image becomes an orphan only when the deploy replaces the
// instance, and the retention rule runs only when something calls it. Left to a person or a pipeline to
// remember, the host fills up; it is the daemon's own housekeeping. It is the same rule as POST
// /v1/images/prune (an image an instance runs is never deleted) and is a recorded job like any other change.
// It does not queue: another change running or waiting means skipping this round, and the next one runs.
func (s *Server) RunImageRetention(ctx context.Context, every time.Duration) {
	if s.opts.ImagePrune == nil || s.opts.Jobs == nil || every <= 0 {
		return
	}
	first := every
	if first > 5*time.Minute {
		first = 5 * time.Minute // soon after a start, so a daemon restarted often still gets there
	}
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.pruneImagesNow()
		t.Reset(every)
	}
}

const scheduledActor = "native-ops (scheduled)"

// pruneImagesNow runs one retention job unless the host is busy.
func (s *Server) pruneImagesNow() {
	_, err := s.runJobNow(jobSpec{kind: "image:prune", actor: scheduledActor, what: "image prune (scheduled)", audit: "image prune",
		work: func(ctx context.Context, logf func(string, ...any)) error {
			return s.opts.ImagePrune(ctx, false, logf)
		}})
	var busy errBusy
	if err != nil && !errors.As(err, &busy) {
		log.Printf("scheduled image retention: could not record the job: %v", err)
	}
}
