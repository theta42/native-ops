package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretStoreSyncPruneAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	s, err := OpenSecretStore(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, removed, err := s.Sync(map[string]string{"DO_API_TOKEN": "dop_1", "BACKUP_S3_ACCESS_KEY": "AK"}, false, "ci")
	if err != nil || strings.Join(changed, ",") != "BACKUP_S3_ACCESS_KEY,DO_API_TOKEN" || len(removed) != 0 {
		t.Fatalf("first sync: %v %v %v", changed, removed, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("the file must be 0600, is %o", fi.Mode().Perm())
	}
	// The same values again change nothing.
	if changed, _, _ := s.Sync(map[string]string{"DO_API_TOKEN": "dop_1"}, false, "ci"); len(changed) != 0 {
		t.Fatalf("an unchanged value is not a change: %v", changed)
	}
	// prune removes what the git server no longer has.
	changed, removed, err = s.Sync(map[string]string{"DO_API_TOKEN": "dop_2"}, true, "ci")
	if err != nil || strings.Join(changed, ",") != "DO_API_TOKEN" || strings.Join(removed, ",") != "BACKUP_S3_ACCESS_KEY" {
		t.Fatalf("prune: %v %v %v", changed, removed, err)
	}
	// It survives a restart, and the environment is only a fallback.
	s2, err := OpenSecretStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DO_API_TOKEN", "from-env")
	t.Setenv("ONLY_IN_ENV", "env-value")
	if s2.Lookup("DO_API_TOKEN") != "dop_2" || s2.Lookup("ONLY_IN_ENV") != "env-value" || s2.Lookup("NOWHERE") != "" {
		t.Fatal("lookup must prefer the store and fall back to the environment")
	}
	for _, bad := range []map[string]string{{"lower": "x"}, {"OK": ""}, {"OK": strings.Repeat("x", maxSecretBytes+1)}} {
		if _, _, err := s2.Sync(bad, false, "ci"); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSecretStore(path); err == nil {
		t.Fatal("a secret file readable by others must be refused")
	}
}

func TestSecretEndpointsAreAdminOnlyAndNeverReturnValues(t *testing.T) {
	rig := newAdminRig(t)
	store, err := OpenSecretStore(filepath.Join(t.TempDir(), "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	rig.srv.opts.Secrets = store
	rig.reload(t)

	deployer, _, _ := rig.tokens.Create("ci-deploy", RoleDeployer)
	body := map[string]any{"secrets": map[string]string{"DO_API_TOKEN": "dop_v1_supersecret"}}
	if code, _, _ := rig.call(t, "PUT", "/v1/secrets", deployer, body); code != http.StatusForbidden {
		t.Fatalf("a deployer must not set secrets: %d", code)
	}
	code, out, _ := rig.call(t, "PUT", "/v1/secrets", rig.admin, body)
	if code != 200 || strings.Contains(toJSON(out), "supersecret") || toJSON(out["changed"]) != `["DO_API_TOKEN"]` {
		t.Fatalf("sync: %d %v", code, out)
	}
	if v, _ := store.Get("DO_API_TOKEN"); v != "dop_v1_supersecret" {
		t.Fatal("the value was not stored")
	}
	code, out, _ = rig.call(t, "GET", "/v1/secrets", rig.admin, nil)
	if code != 200 || strings.Contains(toJSON(out), "supersecret") || !strings.Contains(toJSON(out), `"DO_API_TOKEN"`) {
		t.Fatalf("list must show names only: %d %v", code, out)
	}
	if code, _, _ := rig.call(t, "GET", "/v1/secrets", deployer, nil); code != http.StatusForbidden {
		t.Fatalf("a deployer must not list secrets: %d", code)
	}
	if code, _, _ := rig.call(t, "DELETE", "/v1/secrets/DO_API_TOKEN", rig.admin, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _, _ := rig.call(t, "DELETE", "/v1/secrets/DO_API_TOKEN", rig.admin, nil); code != 404 {
		t.Fatalf("deleting twice: %d", code)
	}
	audit, _ := os.ReadFile(rig.auditPath)
	if strings.Contains(string(audit), "supersecret") || !strings.Contains(string(audit), "secrets synced: changed=DO_API_TOKEN") {
		t.Fatalf("the audit log names what changed and never a value:\n%s", audit)
	}
}

// A secrets token (Scope.Secrets) is what a CI job that syncs a service's secrets holds instead of an admin
// token: it may set and prune the SERVICE_* names its scope matches, see only those, and do nothing else.
func TestASecretsTokenSyncsOnlyItsServiceSecretsAndNothingElse(t *testing.T) {
	rig := newAdminRig(t)
	store, err := OpenSecretStore(filepath.Join(t.TempDir(), "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	rig.srv.opts.Secrets = store
	rig.reload(t)
	if _, _, err := store.Sync(map[string]string{"DO_API_TOKEN": "dop_daemon", "SERVICE_OTHER_KEY": "other"}, false, "admin"); err != nil {
		t.Fatal(err)
	}

	code, out, _ := rig.call(t, "POST", "/v1/tokens", rig.admin, map[string]any{
		"name": "ci-crew-secrets", "role": "deployer", "scope": map[string]any{"secrets": []string{"SERVICE_CREW_*"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create a secrets token: %d %v", code, out)
	}
	tok := out["secret"].(string)

	code, out, _ = rig.call(t, "PUT", "/v1/secrets", tok, map[string]any{"secrets": map[string]string{"SERVICE_CREW_GOOGLE_KEY": "k1-supersecret"}})
	if code != 200 || toJSON(out["changed"]) != `["SERVICE_CREW_GOOGLE_KEY"]` || strings.Contains(toJSON(out), "supersecret") {
		t.Fatalf("sync its own secret: %d %v", code, out)
	}
	if listed := toJSON(out["secrets"]); strings.Contains(listed, "DO_API_TOKEN") || strings.Contains(listed, "SERVICE_OTHER_KEY") {
		t.Fatalf("a secrets token must see only its own names: %s", listed)
	}

	for _, name := range []string{"DO_API_TOKEN", "SERVICE_OTHER_KEY", "NATIVE_OPS_OIDC_CLIENT_SECRET"} {
		if code, _, _ := rig.call(t, "PUT", "/v1/secrets", tok, map[string]any{"secrets": map[string]string{name: "evil"}}); code != http.StatusForbidden {
			t.Errorf("a secrets token set %s: %d", name, code)
		}
	}
	if v, _ := store.Get("DO_API_TOKEN"); v != "dop_daemon" {
		t.Fatal("the daemon's own credential was changed")
	}

	// A prune removes only names within its scope.
	if _, _, err := store.Sync(map[string]string{"SERVICE_CREW_OLD": "old"}, false, "admin"); err != nil {
		t.Fatal(err)
	}
	code, out, _ = rig.call(t, "PUT", "/v1/secrets", tok, map[string]any{"secrets": map[string]string{"SERVICE_CREW_GOOGLE_KEY": "k1-supersecret"}, "prune": true})
	if code != 200 || toJSON(out["removed"]) != `["SERVICE_CREW_OLD"]` {
		t.Fatalf("prune within scope: %d %v", code, out)
	}
	for _, name := range []string{"DO_API_TOKEN", "SERVICE_OTHER_KEY"} {
		if _, ok := store.Get(name); !ok {
			t.Fatalf("a secrets token's prune removed %s, outside its scope", name)
		}
	}

	// Nothing else: not the list, not a delete, not plans, instances, tokens, whoami or MCP.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/secrets"}, {"DELETE", "/v1/secrets/SERVICE_CREW_GOOGLE_KEY"}, {"GET", "/v1/status"},
		{"GET", "/v1/whoami"}, {"GET", "/v1/tokens"}, {"POST", "/v1/tokens"},
	} {
		if code, _, _ := rig.call(t, c.method, c.path, tok, map[string]any{}); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("a secrets token reached %s %s: %d", c.method, c.path, code)
		}
	}
	h := &actorHolder{name: "ci-crew-secrets", role: RoleDeployer, scope: &Scope{Secrets: []string{"SERVICE_CREW_*"}}}
	for _, tool := range mcpTools {
		if rig.srv.mcpAllows(tool, h) {
			t.Errorf("a secrets token may use the MCP tool %s", tool.name)
		}
	}
}

func TestASecretsScopeNamesOnlyServiceSecretsAndOnlySecrets(t *testing.T) {
	for _, bad := range []Scope{
		{Secrets: []string{"DO_API_TOKEN"}},
		{Secrets: []string{"*"}},
		{Secrets: []string{"service_crew_*"}},
		{Secrets: []string{"SERVICE_CREW_*"}, Names: []string{"crew"}, Images: []string{"x:*"}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
	ok := Scope{Secrets: []string{"SERVICE_CREW_*", "SERVICE_GITEA_SMTP_PASSWORD"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if !ok.AllowsSecret("SERVICE_CREW_GOOGLE_KEY") || ok.AllowsSecret("SERVICE_GITEA_OTHER") || ok.AllowsSecret("DO_API_TOKEN") {
		t.Fatal("AllowsSecret must follow the patterns")
	}
	// A plain deployer still cannot sync anything.
	rig := newAdminRig(t)
	store, _ := OpenSecretStore(filepath.Join(t.TempDir(), "secrets.json"))
	rig.srv.opts.Secrets = store
	rig.reload(t)
	deployer, _, _ := rig.tokens.Create("ci-deploy", RoleDeployer)
	if code, _, _ := rig.call(t, "PUT", "/v1/secrets", deployer, map[string]any{"secrets": map[string]string{"SERVICE_CREW_X": "v"}}); code != http.StatusForbidden {
		t.Fatalf("an unscoped deployer must not sync secrets: %d", code)
	}
}
