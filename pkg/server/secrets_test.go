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
