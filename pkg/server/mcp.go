package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// POST /mcp is a Model Context Protocol server (the Streamable HTTP transport, stateless, answering in
// JSON), so an AI agent can see and drive deploys with an API token, the way CI does.
//
// It adds no power of its own. Every tool is one or two calls to the daemon's own endpoints, made with
// the caller's token, through the same handler (and audit log) as any other request: a tool can do
// exactly what the token could do with curl, and the role and scope checks are the endpoints' own.
// tools/list shows only the tools the token's role (and scope) can use and the daemon has enabled.
//
// The tools cover reading the host, deploys, jobs and plans. Approving plans and recipes, tokens, users,
// secrets, restores and daemon upgrades are deliberately not tools: those are the human gates.
//
// /mcp takes a bearer token only, never the UI's session cookie, and refuses a request from a browser
// page of another origin (the transport's DNS rebinding rule). The token is an API token, or, with OAuth
// on, an access token from a person's sign-in (oauth.go), which acts with that person's current role.

// mcpVersions are the protocol revisions spoken, newest first.
var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = `native-ops manages one host's Incus containers from a git configuration repository.

Deploys: a deploy applies the commit a protected deploy-* tag points at; the daemon reads it from the git server, so pushing the tag (in git) is the approval and nothing is uploaded. To ship a change: merge it, push a deploy tag in git, then deploy it.
- list_deploys: what the host runs now (current) and the history.
- plan_deploy: what a tag would change, without changing anything. Do this before deploy and show the plan to the person.
- deploy: run the deploy of a tag as a job; pass wait_seconds to wait for it, or follow it with get_job.
- A plan that is blocked never applies; a deploy with nothing to change succeeds without applying.
- To roll back, push a new deploy tag at the earlier commit and deploy that.

Jobs: every change to the host is a job (list_jobs, get_job). get_job with wait_seconds waits for it to end; the log is shown to deployers and admins.
Only one change runs at a time: a "busy" error means another job is running; wait for it and retry.`

type mcpTool struct {
	name, title, description string
	schema                   map[string]any
	min                      Role
	scoped                   bool // a token limited to tenant instances may use it
	readOnly, destructive    bool
	enabled                  func(o *Options) bool
	// call makes the tool's requests through do, which runs one daemon request with the caller's token.
	call func(ctx context.Context, args map[string]any, do mcpDo) (int, []byte, error)
}

type mcpDo func(ctx context.Context, method, path string, body any) (int, []byte)

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}

var waitProp = intProp("Seconds to wait for the job to end before answering (0 to 60). The answer is the job, finished or still running.", 0, 60)

// get is a tool that is one GET.
func get(path func(args map[string]any) (string, error)) func(context.Context, map[string]any, mcpDo) (int, []byte, error) {
	return func(ctx context.Context, args map[string]any, do mcpDo) (int, []byte, error) {
		p, err := path(args)
		if err != nil {
			return 0, nil, err
		}
		code, body := do(ctx, "GET", p, nil)
		return code, body, nil
	}
}

// startAndWait is a tool that starts a job with one request and, with wait_seconds, waits for it.
func startAndWait(ctx context.Context, args map[string]any, do mcpDo, method, path string, body any) (int, []byte, error) {
	wait, err := intArg(args, "wait_seconds", 0, 60)
	if err != nil {
		return 0, nil, err
	}
	code, out := do(ctx, method, path, body)
	if code != http.StatusAccepted || wait == 0 {
		return code, out, nil
	}
	var started struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if json.Unmarshal(out, &started) != nil || !validJobID(started.Job.ID) {
		return code, out, nil
	}
	code, out = do(ctx, "GET", "/v1/jobs/"+started.Job.ID+"?wait="+strconv.Itoa(wait), nil)
	return code, out, nil
}

func query(args map[string]any, names ...string) (string, error) {
	q := url.Values{}
	for _, n := range names {
		switch v := args[n].(type) {
		case nil:
		case string:
			if v != "" {
				q.Set(n, v)
			}
		case float64:
			q.Set(n, strconv.FormatInt(int64(v), 10))
		default:
			return "", fmt.Errorf("%s has the wrong type", n)
		}
	}
	if len(q) == 0 {
		return "", nil
	}
	return "?" + q.Encode(), nil
}

func strArg(args map[string]any, name string) (string, error) {
	v, ok := args[name].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("%s is required, as a string", name)
	}
	return v, nil
}

func intArg(args map[string]any, name string, min, max int) (int, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return 0, nil
	}
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) || int(f) < min || int(f) > max {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d", name, min, max)
	}
	return int(f), nil
}

func always(*Options) bool { return true }

var mcpTools = []mcpTool{
	{
		name: "whoami", title: "Who am I", description: "The token's name, role and scope, and the daemon's version.",
		schema: obj(map[string]any{}), min: RoleViewer, scoped: true, readOnly: true, enabled: always,
		call: get(func(map[string]any) (string, error) { return "/v1/whoami", nil }),
	},
	{
		name: "host_status", title: "Host status",
		description: "The host's instances, data volumes, images, metrics (load, memory, disk) and warnings, plus the edge's routes and certificates and DNS records when configured. Config values are never included, only key names.",
		schema:      obj(map[string]any{}), min: RoleViewer, readOnly: true, enabled: always,
		call: get(func(map[string]any) (string, error) { return "/v1/status", nil }),
	},
	{
		name: "list_deploys", title: "List deploys",
		description: "Deploys, newest first: current is the newest deploy that left the host matching its tag (what it runs now), running is the deploy in progress. Each names the tag, commit, outcome (result: applied, no_changes, daemon_upgraded), who ran it and the plan's counts.",
		schema:      obj(map[string]any{"limit": intProp("How many deploys to list (default 50).", 1, keepJobs)}),
		min:         RoleViewer, readOnly: true, enabled: func(o *Options) bool { return o.Deploy != nil && o.Apply != nil },
		call: get(func(a map[string]any) (string, error) { q, err := query(a, "limit"); return "/v1/deploys" + q, err }),
	},
	{
		name: "plan_deploy", title: "Plan a deploy",
		description: "What deploying a tag would change on the host, without changing anything: the daemon reads the tag's commit from the git server and plans it against the host now. Returns the plan (text is the readable form), exit (0 nothing to change, 1 blocked, 2 changes), whether the tag is deployable (protected), and whether the deploy would first upgrade the daemon.",
		schema:      obj(map[string]any{"tag": strProp("The deploy tag, e.g. deploy-2026.10.03.")}, "tag"),
		min:         RolePlanner, readOnly: true, enabled: func(o *Options) bool { return o.Deploy != nil && o.Apply != nil },
		call: func(ctx context.Context, a map[string]any, do mcpDo) (int, []byte, error) {
			tag, err := strArg(a, "tag")
			if err != nil {
				return 0, nil, err
			}
			code, out := do(ctx, "POST", "/v1/deploy/plan", map[string]string{"tag": tag})
			return code, out, nil
		},
	},
	{
		name: "deploy", title: "Deploy a tag",
		description: "Deploy the commit a protected deploy tag points at, as a job: plan it, refuse a blocked plan, apply it. The tag must already be pushed in git (the push is the approval). Changes the host: show the person plan_deploy's plan first. Returns the job; with wait_seconds, the job once it ends (or still running when the wait is over; follow it with get_job).",
		schema: obj(map[string]any{
			"tag":          strProp("The deploy tag, e.g. deploy-2026.10.03."),
			"wait_seconds": waitProp,
		}, "tag"),
		min: RoleDeployer, destructive: true, enabled: func(o *Options) bool { return o.Deploy != nil && o.Apply != nil },
		call: func(ctx context.Context, a map[string]any, do mcpDo) (int, []byte, error) {
			tag, err := strArg(a, "tag")
			if err != nil {
				return 0, nil, err
			}
			return startAndWait(ctx, a, do, "POST", "/v1/deploy", map[string]string{"tag": tag})
		},
	},
	{
		name: "list_jobs", title: "List jobs",
		description: "Jobs (every change to the host is one), newest first, without logs.",
		schema: obj(map[string]any{
			"kind":   strProp(`A job kind: deploy, apply, image:build, image:prune, edge:apply, backup, restore, dns:sync, daemon:upgrade; or a family ending in a colon, e.g. "instance:".`),
			"status": map[string]any{"type": "string", "enum": []string{"running", "succeeded", "failed", "interrupted"}},
			"limit":  intProp("How many jobs to list.", 1, keepJobs),
		}),
		min: RoleViewer, scoped: true, readOnly: true, enabled: func(o *Options) bool { return o.Jobs != nil },
		call: get(func(a map[string]any) (string, error) {
			q, err := query(a, "kind", "status", "limit")
			return "/v1/jobs" + q, err
		}),
	},
	{
		name: "get_job", title: "Get a job",
		description: "One job: its status (running, succeeded, failed, interrupted), error, result and, for deployers and admins, its log.",
		schema: obj(map[string]any{
			"id":           strProp("The job id, j-<seconds>-<hex>."),
			"wait_seconds": waitProp,
		}, "id"),
		min: RoleViewer, scoped: true, readOnly: true, enabled: func(o *Options) bool { return o.Jobs != nil },
		call: get(func(a map[string]any) (string, error) {
			id, err := strArg(a, "id")
			if err != nil {
				return "", err
			}
			if !validJobID(id) {
				return "", fmt.Errorf("%q is not a job id", id)
			}
			wait, err := intArg(a, "wait_seconds", 0, 60)
			return "/v1/jobs/" + id + "?wait=" + strconv.Itoa(wait), err
		}),
	},
	{
		name: "list_plans", title: "List plans",
		description: "The plans that were made (by CI, plan_deploy or a deploy) and where each stands: pending, approved, used, expired, stale or blocked.",
		schema:      obj(map[string]any{}), min: RoleViewer, readOnly: true, enabled: func(o *Options) bool { return o.Plans != nil },
		call: get(func(map[string]any) (string, error) { return "/v1/plans", nil }),
	},
	{
		name: "get_plan", title: "Get a plan", description: "One plan by its hash, with its text and state.",
		schema: obj(map[string]any{"hash": strProp("The plan's 64-character hash.")}, "hash"),
		min:    RoleViewer, readOnly: true, enabled: func(o *Options) bool { return o.Plans != nil },
		call: get(func(a map[string]any) (string, error) {
			h, err := strArg(a, "hash")
			if err != nil {
				return "", err
			}
			if !hashRe.MatchString(h) {
				return "", fmt.Errorf("hash must be 64 lowercase hex characters")
			}
			return "/v1/plans/" + h, nil
		}),
	},
	{
		name: "list_instances", title: "List tenant instances", description: "The tenant instances the token may see.",
		schema: obj(map[string]any{}), min: RoleViewer, scoped: true, readOnly: true, enabled: func(o *Options) bool { return o.Instances != nil },
		call: get(func(map[string]any) (string, error) { return "/v1/instances", nil }),
	},
	{
		name: "get_instance", title: "Get a tenant instance", description: "One tenant instance by name.",
		schema: obj(map[string]any{"name": strProp("The instance name.")}, "name"),
		min:    RoleViewer, scoped: true, readOnly: true, enabled: func(o *Options) bool { return o.Instances != nil },
		call: get(func(a map[string]any) (string, error) {
			n, err := strArg(a, "name")
			return "/v1/instances/" + url.PathEscape(n), err
		}),
	},
	{
		name: "prune_images", title: "Prune images",
		description: "Image retention, as a job: delete images no instance runs that are orphans left by rebuilds or tags older than the newest kept per app. dry_run (the default) only reports what would go, in the job's log.",
		schema: obj(map[string]any{
			"dry_run":      map[string]any{"type": "boolean", "description": "Only report (default true).", "default": true},
			"wait_seconds": waitProp,
		}),
		min: RoleDeployer, destructive: true, enabled: func(o *Options) bool { return o.ImagePrune != nil },
		call: func(ctx context.Context, a map[string]any, do mcpDo) (int, []byte, error) {
			dry := true
			if v, ok := a["dry_run"]; ok && v != nil {
				b, ok := v.(bool)
				if !ok {
					return 0, nil, fmt.Errorf("dry_run must be true or false")
				}
				dry = b
			}
			p := "/v1/images/prune"
			if dry {
				p += "?dry_run=1"
			}
			return startAndWait(ctx, a, do, "POST", p, nil)
		},
	},
}

// mcpHandler serves /mcp. inner is the daemon's whole handler, which the tools call.
func (s *Server) mcpHandler(inner func() http.Handler) http.Handler {
	serve := s.authScoped(RoleViewer, func(w http.ResponseWriter, r *http.Request) {
		s.serveMCP(w, r, inner())
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				writeError(w, http.StatusForbidden, "forbidden", "requests from another origin are refused")
				return
			}
		}
		tok := bearer(r)
		if tok == "" {
			w.Header().Set("WWW-Authenticate", s.mcpChallenge(r, ""))
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API token, or an OAuth sign-in, is required")
			return
		}
		r.Header.Del("Cookie") // a token, never a browser session
		if strings.HasPrefix(tok, oauthAccessPrefix) && s.opts.OAuth != nil {
			// An OAuth access token acts as its person, with the role they have now.
			g, ok := s.opts.OAuth.VerifyAccess(tok)
			var (
				username string
				role     Role
			)
			if ok {
				username, role, ok = s.oauthUser(g)
			}
			if !ok {
				w.Header().Set("WWW-Authenticate", s.mcpChallenge(r, "invalid_token"))
				writeError(w, http.StatusUnauthorized, "unauthorized", "the access token is invalid, expired or revoked, or its person may no longer sign in")
				return
			}
			if h, ok := r.Context().Value(actorKey{}).(*actorHolder); ok {
				h.name, h.role = username+" (via "+g.ClientName+")", role
			}
			s.serveMCP(w, r, inner())
			return
		}
		if _, ok := s.opts.Tokens.Verify(tok); !ok {
			w.Header().Set("WWW-Authenticate", s.mcpChallenge(r, "invalid_token"))
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API token is required")
			return
		}
		serve.ServeHTTP(w, r)
	})
}

// mcpCaller is the caller /mcp authenticated, handed to the requests its tools make (see authWith).
type mcpCaller struct {
	name  string
	role  Role
	scope *Scope
}

type mcpCallerKey struct{}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const maxMCPBody = 1 << 20

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request, inner http.Handler) {
	reply := func(id json.RawMessage, result any, rerr *rpcError) {
		if id == nil {
			id = json.RawMessage("null")
		}
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rerr != nil {
			msg["error"] = rerr
		} else {
			msg["result"] = result
		}
		writeJSON(w, http.StatusOK, msg)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMCPBody)).Decode(&raw); err != nil {
		reply(nil, nil, &rpcError{-32700, "the body is not JSON"})
		return
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '[' {
		reply(nil, nil, &rpcError{-32600, "batches are not supported: send one message per request"})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		reply(req.ID, nil, &rpcError{-32600, "not a JSON-RPC 2.0 request"})
		return
	}
	if req.ID == nil { // a notification (or a response): nothing to answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	h, _ := r.Context().Value(actorKey{}).(*actorHolder)
	auditDetail(r, "mcp %s", req.Method)
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := mcpVersions[0]
		for _, v := range mcpVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		reply(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "native-ops", "title": "native-ops", "version": s.opts.Version},
			"instructions":    mcpInstructions,
		}, nil)
	case "ping":
		reply(req.ID, map[string]any{}, nil)
	case "tools/list":
		tools := []map[string]any{}
		for _, t := range mcpTools {
			if s.mcpAllows(t, h) {
				tools = append(tools, map[string]any{
					"name": t.name, "title": t.title, "description": t.description, "inputSchema": t.schema,
					"annotations": map[string]any{"title": t.title, "readOnlyHint": t.readOnly, "destructiveHint": t.destructive, "openWorldHint": false},
				})
			}
		}
		reply(req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			reply(req.ID, nil, &rpcError{-32602, "params must be {name, arguments}"})
			return
		}
		var tool *mcpTool
		for i := range mcpTools {
			if mcpTools[i].name == p.Name && s.mcpAllows(mcpTools[i], h) {
				tool = &mcpTools[i]
			}
		}
		if tool == nil {
			reply(req.ID, nil, &rpcError{-32602, fmt.Sprintf("unknown tool %q (or not one this token can use)", p.Name)})
			return
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		auditDetail(r, "mcp tools/call %s", tool.name)
		code, body, err := tool.call(r.Context(), p.Arguments, s.mcpDo(r, inner))
		if err != nil {
			reply(req.ID, toolResult(http.StatusBadRequest, []byte(`{"error":`+strconv.Quote(err.Error())+`,"code":"bad_arguments"}`)), nil)
			return
		}
		reply(req.ID, toolResult(code, body), nil)
	default:
		reply(req.ID, nil, &rpcError{-32601, "no such method: " + req.Method})
	}
}

// mcpAllows reports whether a tool is on, and usable by the caller.
func (s *Server) mcpAllows(t mcpTool, h *actorHolder) bool {
	return h != nil && t.enabled(&s.opts) && h.role.Allows(t.min) && (h.scope == nil || t.scoped)
}

// mcpDo runs one request through the daemon's own handler, as the caller: the identity /mcp verified
// (an API token's, or an OAuth grant's person), and their address.
func (s *Server) mcpDo(outer *http.Request, inner http.Handler) mcpDo {
	h, _ := outer.Context().Value(actorKey{}).(*actorHolder)
	caller := mcpCaller{}
	if h != nil {
		caller = mcpCaller{name: h.name, role: h.role, scope: h.scope}
	}
	return func(ctx context.Context, method, path string, body any) (int, []byte) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req, err := http.NewRequestWithContext(context.WithValue(ctx, mcpCallerKey{}, caller), method, path, rd)
		if err != nil {
			return http.StatusBadRequest, []byte(`{"error":"bad request","code":"bad_request"}`)
		}
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = outer.RemoteAddr
		req.Host = outer.Host
		rec := &bufferedResponse{header: http.Header{}, code: http.StatusOK}
		inner.ServeHTTP(rec, req)
		return rec.code, rec.body.Bytes()
	}
}

// toolResult is a tools/call result: the endpoint's JSON answer as text, as structured content, and
// any plan text or job log in it once more as plain text, which reads better than escaped JSON.
func toolResult(code int, body []byte) map[string]any {
	var pretty bytes.Buffer
	if json.Indent(&pretty, body, "", "  ") != nil {
		pretty.Reset()
		pretty.Write(body)
	}
	content := []map[string]any{{"type": "text", "text": pretty.String()}}
	out := map[string]any{"isError": code >= 400}
	var v map[string]any
	if json.Unmarshal(body, &v) == nil {
		out["structuredContent"] = v
		for _, k := range []string{"text", "log"} {
			if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
				content = append(content, map[string]any{"type": "text", "text": s})
			}
		}
	}
	if code >= 400 {
		content[0]["text"] = fmt.Sprintf("HTTP %d %s\n%s", code, http.StatusText(code), content[0]["text"])
	}
	out["content"] = content
	return out
}

type bufferedResponse struct {
	header http.Header
	code   int
	wrote  bool
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(code int) {
	if !b.wrote {
		b.code, b.wrote = code, true
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.wrote = true
	return b.body.Write(p)
}
