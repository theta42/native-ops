package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

type rpcAnswer struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (r *applyRig) mcp(t *testing.T, token, body string, header ...string) (*http.Response, rpcAnswer) {
	t.Helper()
	req, _ := http.NewRequest("POST", r.srv.URL+"/mcp", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var a rpcAnswer
	_ = json.NewDecoder(res.Body).Decode(&a)
	return res, a
}

func (r *applyRig) mcpTools(t *testing.T, token string) []string {
	t.Helper()
	_, a := r.mcp(t, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var out struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(a.Result, &out); err != nil {
		t.Fatalf("tools/list: %+v", a)
	}
	var names []string
	for _, tool := range out.Tools {
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s: the input schema must be an object", tool.Name)
		}
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

type toolAnswer struct {
	IsError           bool           `json:"isError"`
	StructuredContent map[string]any `json:"structuredContent"`
	Content           []struct {
		Type, Text string
	} `json:"content"`
}

func (r *applyRig) callTool(t *testing.T, token, name, args string) (toolAnswer, *rpcError) {
	t.Helper()
	_, a := r.mcp(t, token, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`)
	var out toolAnswer
	if a.Error == nil {
		if err := json.Unmarshal(a.Result, &out); err != nil {
			t.Fatal(err)
		}
	}
	return out, a.Error
}

func TestMCPInitializesAndListsOnlyTheToolsATokenCanUse(t *testing.T) {
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "fleet.yml", body: "name: x\n"})}
	rig := newDeployRig(t, src)

	res, a := rig.mcp(t, rig.viewer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	var init struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      map[string]any `json:"serverInfo"`
		Instructions    string         `json:"instructions"`
	}
	_ = json.Unmarshal(a.Result, &init)
	if res.StatusCode != 200 || string(a.ID) != "1" || init.ProtocolVersion != "2025-03-26" || init.Capabilities["tools"] == nil ||
		init.ServerInfo["name"] != "native-ops" || !strings.Contains(init.Instructions, "plan_deploy") {
		t.Fatalf("initialize: %d %+v %+v", res.StatusCode, a, init)
	}
	if _, a := rig.mcp(t, rig.viewer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`); !strings.Contains(string(a.Result), mcpVersions[0]) {
		t.Fatalf("an unknown revision is answered with the newest: %s", a.Result)
	}
	if res, _ := rig.mcp(t, rig.viewer, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); res.StatusCode != http.StatusAccepted {
		t.Fatalf("a notification is accepted with no answer: %d", res.StatusCode)
	}

	has := func(tools []string, want ...string) string {
		set := map[string]bool{}
		for _, n := range tools {
			set[n] = true
		}
		out := ""
		for _, w := range want {
			name, absent := strings.CutPrefix(w, "!")
			if set[name] == absent {
				out += " " + w
			}
		}
		return out
	}
	if bad := has(rig.mcpTools(t, rig.viewer), "list_deploys", "get_job", "host_status", "!plan_deploy", "!deploy"); bad != "" {
		t.Errorf("a viewer reads, and cannot plan or deploy:%s", bad)
	}
	if bad := has(rig.mcpTools(t, rig.planner), "plan_deploy", "!deploy"); bad != "" {
		t.Errorf("a planner plans, and cannot deploy:%s", bad)
	}
	// Tools for endpoints this daemon has turned off are not listed.
	if bad := has(rig.mcpTools(t, rig.deployerToken), "deploy", "plan_deploy", "!list_instances", "!prune_images"); bad != "" {
		t.Errorf("a deployer:%s", bad)
	}
}

func TestMCPDeploysThroughTheDaemonsOwnEndpointsAndChecks(t *testing.T) {
	src := &fakeSource{protected: true, tree: tgz(t, entry{name: "fleet.yml", body: "name: x\n"})}
	rig := newDeployRig(t, src)

	plan, rerr := rig.callTool(t, rig.planner, "plan_deploy", `{"tag":"deploy-1"}`)
	if rerr != nil || plan.IsError || plan.StructuredContent["sha"] != deployCommit || len(plan.Content) != 2 || !strings.Contains(plan.Content[1].Text, "web") {
		t.Fatalf("plan_deploy: %+v %+v", plan, rerr)
	}
	if rig.applies.Load() != 0 {
		t.Fatal("planning applied something")
	}

	// A viewer neither sees deploy nor can call it.
	if _, rerr := rig.callTool(t, rig.viewer, "deploy", `{"tag":"deploy-1"}`); rerr == nil || rerr.Code != -32602 {
		t.Fatalf("a viewer calling deploy: %+v", rerr)
	}

	out, rerr := rig.callTool(t, rig.deployerToken, "deploy", `{"tag":"deploy-1","wait_seconds":10}`)
	if rerr != nil || out.IsError || out.StructuredContent["status"] != "succeeded" || out.StructuredContent["result"] != ResultApplied {
		t.Fatalf("deploy with a wait answers with the finished job: %+v %+v", out, rerr)
	}
	if rig.applies.Load() != 1 {
		t.Fatalf("applied %d times", rig.applies.Load())
	}
	deploys, _ := rig.callTool(t, rig.viewer, "list_deploys", `{}`)
	if cur, _ := deploys.StructuredContent["current"].(map[string]any); cur == nil || cur["tag"] != "deploy-1" {
		t.Fatalf("list_deploys: %+v", deploys.StructuredContent)
	}

	// The endpoint's refusals come back as tool errors, with what the endpoint said.
	src.protected = false
	out, _ = rig.callTool(t, rig.deployerToken, "deploy", `{"tag":"deploy-1","wait_seconds":10}`)
	if out.IsError || out.StructuredContent["status"] != "failed" || !strings.Contains(out.StructuredContent["error"].(string), "not protected") {
		t.Fatalf("a failed deploy is a failed job: %+v", out)
	}
	out, _ = rig.callTool(t, rig.planner, "plan_deploy", `{"tag":"nope"}`)
	if !out.IsError || !strings.HasPrefix(out.Content[0].Text, "HTTP 422") {
		t.Fatalf("a refusal is a tool error: %+v", out)
	}
	for _, args := range []string{`{}`, `{"tag":5}`, `{"tag":"deploy-1","wait_seconds":99}`} {
		if out, _ := rig.callTool(t, rig.deployerToken, "deploy", args); !out.IsError {
			t.Errorf("bad arguments %s: %+v", args, out)
		}
	}
	if out, _ := rig.callTool(t, rig.viewer, "get_job", `{"id":"../tokens"}`); !out.IsError {
		t.Errorf("a bad job id is refused before any request: %+v", out)
	}

	// Each tool call is in the audit log as the endpoint it called, by the token that called it.
	audit, _ := os.ReadFile(rig.audit)
	if !bytes.Contains(audit, []byte(`"path":"/v1/deploy"`)) || !bytes.Contains(audit, []byte(`mcp tools/call deploy`)) {
		t.Fatalf("audit:\n%s", audit)
	}
}

func TestMCPTakesATokenOnlyAndRefusesOtherOrigins(t *testing.T) {
	rig := newDeployRig(t, &fakeSource{protected: true})
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if res, _ := rig.mcp(t, "", ping); res.StatusCode != http.StatusUnauthorized || res.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("no token: %d", res.StatusCode)
	}
	if res, _ := rig.mcp(t, "garbage", ping); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a bad token: %d", res.StatusCode)
	}
	if res, _ := rig.mcp(t, rig.viewer, ping, "Origin", "https://evil.example"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("another origin: %d", res.StatusCode)
	}
	if res, a := rig.mcp(t, rig.viewer, ping); res.StatusCode != 200 || string(a.Result) != "{}" {
		t.Fatalf("ping: %d %+v", res.StatusCode, a)
	}
	for body, code := range map[string]int{`nope`: -32700, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`: -32600, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`: -32601} {
		if _, a := rig.mcp(t, rig.viewer, body); a.Error == nil || a.Error.Code != code {
			t.Errorf("%s: %+v, want error %d", body, a.Error, code)
		}
	}
	for _, m := range []string{"GET", "DELETE"} {
		if res, _ := rig.do(t, m, "/mcp", rig.viewer); res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp: %d", m, res.StatusCode)
		}
	}
}

func TestMCPGivesAScopedTokenOnlyItsInstanceTools(t *testing.T) {
	rig := newApplyRigWith(t, func(o *Options) {
		o.Instances, o.InstancePolicy = &fakeOps{existing: map[string]string{}}, testInstancePolicy
	})
	secret, _, err := rig.serverUnderTest.opts.Tokens.CreateScoped("fleet", RoleDeployer, Scope{Names: []string{"demo-*"}, Images: []string{"x:*"}})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rig.mcpTools(t, secret), ",")
	if got != "get_instance,get_job,list_instances,list_jobs,whoami" {
		t.Fatalf("a scoped token's tools: %s", got)
	}
	if out, rerr := rig.callTool(t, secret, "host_status", `{}`); rerr == nil {
		t.Fatalf("a scoped token must not read the host: %+v", out)
	}
}
