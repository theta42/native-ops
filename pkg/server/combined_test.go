package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// send is a request with any method, for the tests that mix the endpoints.
func (f *fixture) send(t *testing.T, method, path, token, contentType string, body []byte) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

// With both apply and the instance endpoints on, they are one host: one change at a time, a rejected
// apply does not spend the admin's approval, and a token limited to its instances reaches nothing else,
// including the plans an admin approves.
func TestApplyAndInstancesShareTheHostAndABusyApplyKeepsItsApproval(t *testing.T) {
	ops := &fakeOps{existing: map[string]string{}, block: make(chan struct{}), started: make(chan struct{}, 4)}
	var scoped string
	rig := newApplyRigWith(t, func(o *Options) {
		o.Instances, o.InstancePolicy = ops, testInstancePolicy
		var err error
		scoped, _, err = o.Tokens.CreateScoped("fleet-manager", RoleDeployer, Scope{Names: []string{"demo-*"}, Images: []string{"opsavor-platform:*"}, Domains: []string{"*.opsavor.app"}})
		if err != nil {
			t.Fatal(err)
		}
	})
	hash := creatingPlan().Hash()
	query := "?expect=" + hash
	tree := goodTree(t)
	rig.approveFor(t, query, tree) // an admin approved this plan
	rig.autoApprove = false

	// An instance change is running on the host.
	spec, _ := json.Marshal(tenantSpecFor("demo-a"))
	res, body := rig.send(t, "PUT", "/v1/instances/demo-a", scoped, "application/json", spec)
	if res.StatusCode != 202 {
		t.Fatalf("instance create: %d %s", res.StatusCode, body)
	}
	<-ops.started

	// An approved apply is refused as busy, and its approval is still there.
	res, b := rig.apply(t, rig.deployerToken, query, tree)
	if res.StatusCode != 409 || !strings.Contains(b, `"code":"busy"`) {
		t.Fatalf("an apply while an instance change runs: %d %s", res.StatusCode, b)
	}
	if err := rig.plans.Check(hash); err != nil {
		t.Fatalf("a busy refusal must not use up the approval: %v", err)
	}
	close(ops.block)
	rig.waitJob(t, jobID(t, body), JobSucceeded)
	ops.block = nil

	// Free again: the same approval now runs the apply, once.
	res, b = rig.apply(t, rig.deployerToken, query, tree)
	if res.StatusCode != 202 {
		t.Fatalf("the approved apply once the host is free: %d %s", res.StatusCode, b)
	}
	rig.waitJob(t, jobID(t, b), JobSucceeded)
	if res, _ := rig.apply(t, rig.deployerToken, query, tree); res.StatusCode != 403 {
		t.Fatalf("the approval is single-use: %d", res.StatusCode)
	}

	// And the other way round: an apply in flight makes an instance change wait.
	rig.block, rig.started = make(chan struct{}), make(chan struct{}, 4)
	rig.autoApprove = true
	res, b = rig.apply(t, rig.deployerToken, query, tree)
	if res.StatusCode != 202 {
		t.Fatalf("second apply: %d %s", res.StatusCode, b)
	}
	<-rig.started
	spec, _ = json.Marshal(tenantSpecFor("demo-b"))
	if res, b := rig.send(t, "PUT", "/v1/instances/demo-b", scoped, "application/json", spec); res.StatusCode != 409 || !strings.Contains(b, `"code":"busy"`) {
		t.Fatalf("an instance change while an apply runs: %d %s", res.StatusCode, b)
	}
	close(rig.block)
	rig.waitJob(t, jobID(t, b), JobSucceeded)

	// A scoped token sees jobs (to follow its own changes) and instances, and nothing else: not the plans
	// (whose approval is an admin's), not apply, not the plan endpoint, not the status.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/plans"}, {"GET", "/v1/plans/" + hash}, {"POST", "/v1/plans/" + hash + "/approve"},
		{"DELETE", "/v1/plans/" + hash + "/approval"}, {"POST", "/v1/apply" + query}, {"POST", "/v1/plan"}, {"GET", "/v1/status"},
	} {
		if res, b := rig.send(t, c.method, c.path, scoped, "application/json", nil); res.StatusCode != 403 {
			t.Errorf("%s %s with a scoped token: %d %s", c.method, c.path, res.StatusCode, b)
		}
	}
	if res, b := rig.send(t, "GET", "/v1/jobs", scoped, "", nil); res.StatusCode != 200 {
		t.Errorf("a scoped token follows its jobs: %d %s", res.StatusCode, b)
	}
}
