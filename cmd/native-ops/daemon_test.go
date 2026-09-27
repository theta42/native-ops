package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/remote"
	"github.com/theta42/native-ops/pkg/server"
)

func TestThePlanKeyIsCreatedOnceAndNeverTrustedWhenWeak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.key")
	k1, err := loadOrCreateKey(path)
	if err != nil || len(k1) != 32 {
		t.Fatalf("%v %d", err, len(k1))
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("the key file is 0600, got %v", st.Mode().Perm())
	}
	k2, err := loadOrCreateKey(path)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatal("the key must be the same after a restart, or every plan hash dies with it")
	}
	os.Chmod(path, 0o644)
	if _, err := loadOrCreateKey(path); err == nil {
		t.Error("a key others can read is refused")
	}
	os.Chmod(path, 0o600)
	os.WriteFile(path, []byte("abcd\n"), 0o600)
	if _, err := loadOrCreateKey(path); err == nil {
		t.Error("a short key is refused, not used")
	}
	os.WriteFile(path, []byte("not hex at all not hex at all not hex at all not hex at all!!\n"), 0o600)
	if _, err := loadOrCreateKey(path); err == nil {
		t.Error("a key that is not hex is refused")
	}
}

// fakeIncusHost is a stateful stand-in for a host with an edge and no `web` yet: launching web
// makes it exist. Every call is logged.
func fakeIncusHost(t *testing.T) (logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	launched := filepath.Join(dir, "launched")
	script := `#!/bin/bash
echo "$*" >> ` + logFile + `
case "$*" in
  "image alias list --format json"|"image list --format json") echo '[]' ;;
  "list edge --format json") echo '[{"name":"edge"}]' ;;
  "list web --format json")
    if [ -f ` + launched + ` ]; then echo '[{"name":"web","state":{"network":{"eth0":{"addresses":[{"family":"inet","address":"10.0.100.5","scope":"global"}]}}}}]'; else echo '[]'; fi ;;
  "config show web") printf 'config:\n  limits.cpu: "1"\n  user.native-ops.image: docker:nginx:1\n  volatile.base_image: ` + strings.Repeat("c", 64) + `\nprofiles:\n- default\n- base\n- service\ndevices:\n  data:\n    type: disk\n    pool: default\n    source: web-data\n    path: /data\n' ;;
  "storage volume show default web-data") if [ -f ` + launched + ` ]; then echo "name: web-data"; else echo "Error: Storage volume not found" >&2; exit 1; fi ;;
  "storage volume create"*) ;;
  "profile show "*) echo "name: profile" ;;
  "file pull edge/etc/caddy/Caddyfile -") echo 'import /etc/caddy/sites/*.caddy' ;;
  "file pull edge/etc/caddy/sites/web.caddy -") echo "Error: Path not found" >&2; exit 1 ;;
  launch*) touch ` + launched + ` ;;
  "config device add"*|"config set"*) ;;
  "file push"*) cat > /dev/null ;;
  "exec edge -- "*) ;;
  *) echo "fake incus: unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return logFile
}

func daemonFor(t *testing.T, enableApply bool) (ts *httptest.Server, deployer, viewer, stateDir string) {
	t.Helper()
	stateDir = t.TempDir()
	srv, closeFn, err := newDaemon(daemonConfig{StateDir: stateDir, Pool: "default", EnableApply: enableApply, Exec: remote.NewLocalExecutor()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.WaitForJobs(10 * time.Second); closeFn() })
	store, _ := server.OpenTokenStore(filepath.Join(stateDir, "tokens.json"))
	deployer, _, _ = store.Create("ci", server.RoleDeployer)
	viewer, _, _ = store.Create("dash", server.RoleViewer)
	ts = httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return
}

func call(t *testing.T, method, url, token string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/gzip")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestApplyIsNotServedUnlessTheDaemonWasStartedWithIt(t *testing.T) {
	fakeIncusHost(t)
	ts, deployer, _, stateDir := daemonFor(t, false)
	code, _ := call(t, "POST", ts.URL+"/v1/apply?expect="+strings.Repeat("a", 64), deployer, tarGz(t, confDir(t, freshService)))
	if code != 404 {
		t.Fatalf("without --enable-apply there is no apply endpoint, got %d", code)
	}
	if code, _ := call(t, "GET", ts.URL+"/v1/jobs", deployer, nil); code != 404 {
		t.Fatalf("nor jobs, got %d", code)
	}
	if code, _ := call(t, "POST", ts.URL+"/v1/plan", deployer, tarGz(t, confDir(t, freshService))); code != 200 {
		t.Fatalf("plan is always there, got %d", code)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "jobs")); !os.IsNotExist(err) {
		t.Fatal("no job store is created for a daemon that cannot apply")
	}
}

// The whole path against a fake host that behaves like one: plan, a wrong hash is refused with
// nothing touched, the right hash runs a job that really launches, and the same hash is no longer
// good once the host has changed.
func TestPlanThenApplyThroughTheDaemon(t *testing.T) {
	logFile := fakeIncusHost(t)
	ts, deployer, viewer, stateDir := daemonFor(t, true)
	tree := tarGz(t, confDir(t, freshService))

	code, plan := call(t, "POST", ts.URL+"/v1/plan?sha=abc1234", deployer, tree)
	hash, _ := plan["hash"].(string)
	if code != 200 || len(hash) != 64 || plan["exit"].(float64) != 2 {
		t.Fatalf("%d %v", code, plan)
	}
	mutating := func() string {
		calls, _ := os.ReadFile(logFile)
		var m []string
		for _, l := range strings.Split(string(calls), "\n") {
			for _, verb := range []string{"launch", "config device add", "config set", "file push", "storage volume create", "exec edge"} {
				if strings.HasPrefix(l, verb) {
					m = append(m, l)
				}
			}
		}
		return strings.Join(m, "\n")
	}
	if m := mutating(); m != "" {
		t.Fatalf("planning must change nothing:\n%s", m)
	}

	if code, out := call(t, "POST", ts.URL+"/v1/apply?expect="+strings.Repeat("0", 64), deployer, tree); code != 409 || out["code"] != "plan_changed" {
		t.Fatalf("a wrong hash: %d %v", code, out)
	}
	if code, _ := call(t, "POST", ts.URL+"/v1/apply?expect="+hash, viewer, tree); code != 403 {
		t.Fatalf("a viewer must not apply, got %d", code)
	}
	if m := mutating(); m != "" {
		t.Fatalf("a refused apply must change nothing:\n%s", m)
	}

	code, out := call(t, "POST", ts.URL+"/v1/apply?sha=abc1234&expect="+hash, deployer, tree)
	if code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	id := out["job"].(map[string]any)["id"].(string)
	var job map[string]any
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, job = call(t, "GET", ts.URL+"/v1/jobs/"+id, deployer, nil)
		if job["status"] != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if job["status"] != "succeeded" {
		t.Fatalf("the job: %v", job)
	}
	if log, _ := job["log"].(string); !strings.Contains(log, "web") {
		t.Fatalf("the job's log should say what was deployed: %q", log)
	}
	if m := mutating(); !strings.Contains(m, "launch") || !strings.Contains(m, "storage volume create") || !strings.Contains(m, "web.caddy") {
		t.Fatalf("the apply should really have launched web, made its volume and published its route:\n%s", m)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "jobs", id+".json")); err != nil {
		t.Fatalf("the job is recorded on disk: %v", err)
	}
	// The host changed, so the plan somebody approved is not the plan now.
	if code, out := call(t, "POST", ts.URL+"/v1/apply?expect="+hash, deployer, tree); code != 409 || out["code"] != "plan_changed" {
		t.Fatalf("after applying, the old hash must be refused: %d %v", code, out)
	}
}

// Two configurations that differ only in the value of a secret plan identically (values are never
// shown), but they are not the same apply, and the hash a deployer must present has to say so.
func TestTheDaemonsPlanHashCoversSecretValuesItDoesNotShow(t *testing.T) {
	fakeIncusHost(t)
	ts, deployer, _, _ := daemonFor(t, true)
	withToken := func(v string) []byte {
		return tarGz(t, confDir(t, freshService+"env: {API_TOKEN: "+v+"}\n"))
	}
	_, a := call(t, "POST", ts.URL+"/v1/plan", deployer, withToken("first-value-1"))
	_, b := call(t, "POST", ts.URL+"/v1/plan", deployer, withToken("second-value-2"))
	_, a2 := call(t, "POST", ts.URL+"/v1/plan", deployer, withToken("first-value-1"))
	if a["text"] != b["text"] || strings.Contains(a["text"].(string), "first-value") || strings.Contains(b["text"].(string), "second-value") {
		t.Fatalf("test setup: the plans should read the same and never show a value:\n%v\n%v", a["text"], b["text"])
	}
	if a["hash"] == b["hash"] {
		t.Fatal("a different secret is a different apply: the hash must differ")
	}
	if a["hash"] != a2["hash"] {
		t.Fatal("the same configuration must give the same hash (the key is stable)")
	}
	// Applying with the hash of the first while uploading the second is refused.
	if code, out := call(t, "POST", ts.URL+"/v1/apply?expect="+a["hash"].(string), deployer, withToken("second-value-2")); code != 409 || out["code"] != "plan_changed" {
		t.Fatalf("approved one secret, uploaded another: %d %v", code, out)
	}
}

func TestAPlanHashStillHoldsAfterTheDaemonRestarts(t *testing.T) {
	fakeIncusHost(t)
	stateDir := t.TempDir()
	tree := tarGz(t, confDir(t, freshService+"env: {API_TOKEN: x}\n"))
	hashFromFreshDaemon := func() string {
		srv, closeFn, err := newDaemon(daemonConfig{StateDir: stateDir, Pool: "default", Exec: remote.NewLocalExecutor()})
		if err != nil {
			t.Fatal(err)
		}
		defer closeFn()
		store, _ := server.OpenTokenStore(filepath.Join(stateDir, "tokens.json"))
		tok, _, _ := store.Create("ci"+time.Now().String(), server.RoleDeployer)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		_, out := call(t, "POST", ts.URL+"/v1/plan", tok, tree)
		h, _ := out["hash"].(string)
		return h
	}
	first, second := hashFromFreshDaemon(), hashFromFreshDaemon()
	if len(first) != 64 || first != second {
		t.Fatalf("a hash approved before a restart must still match after it: %q vs %q", first, second)
	}
}
