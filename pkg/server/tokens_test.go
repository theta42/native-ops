package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) (*TokenStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.json")
	s, err := OpenTokenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func TestTokensAreStoredOnlyAsHashesInAPrivateFile(t *testing.T) {
	s, path := newStore(t)
	secret, tok, err := s.Create("ci", RoleDeployer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "nops_") || len(secret) != 5+64 {
		t.Fatalf("secret shape: %q", secret)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), secret[5:]) {
		t.Fatal("the secret must never be written to disk")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %o, want 600", fi.Mode().Perm())
	}
	got, ok := s.Verify(secret)
	if !ok || got.ID != tok.ID || got.Role != RoleDeployer || got.Name != "ci" {
		t.Fatalf("verify: %+v %v", got, ok)
	}
	for _, bad := range []string{"", "nope", secret + "x", secret[:len(secret)-1], "nops_" + strings.Repeat("0", 64)} {
		if _, ok := s.Verify(bad); ok {
			t.Errorf("accepted a wrong secret %q", bad)
		}
	}
}

func TestRevokeListAndReloadAcrossProcesses(t *testing.T) {
	s, path := newStore(t)
	secret, tok, _ := s.Create("a", RoleViewer)

	// `native-ops token create` in another process must reach a running daemon without a restart.
	other, _ := OpenTokenStore(path)
	secret2, _, err := other.Create("b", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(secret2); !ok {
		t.Fatal("a token created elsewhere must be honoured after the file changes")
	}

	ts, _ := s.List()
	if len(ts) != 2 || ts[0].Hash != "" || ts[1].Hash != "" {
		t.Fatalf("list must never hand a hash back out: %+v", ts)
	}
	if err := s.Revoke(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(secret); ok {
		t.Fatal("a revoked token must stop working")
	}
	if _, ok := other.Verify(secret); ok {
		t.Fatal("...in every process")
	}
	if err := s.Revoke("nope"); err == nil {
		t.Fatal("revoking an unknown id is an error")
	}
}

func TestBootstrapTokenFromTheEnvironment(t *testing.T) {
	s, path := newStore(t)
	secret := "nops_" + strings.Repeat("ab", 20)
	if err := s.SetBootstrap("short"); err == nil {
		t.Fatal("a weak bootstrap token must be refused")
	}
	if err := s.SetBootstrap(secret); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Verify(secret)
	if !ok || got.Role != RoleAdmin || got.Name != "bootstrap" {
		t.Fatalf("%+v %v", got, ok)
	}
	if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), "bootstrap") {
		t.Fatal("the bootstrap token lives in memory only")
	}
}

func TestOpenRefusesAWorldReadableTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte(`{"tokens":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTokenStore(path); err == nil || !strings.Contains(err.Error(), "must not be readable") {
		t.Fatalf("got %v", err)
	}
}

func TestCreateValidatesNameAndRole(t *testing.T) {
	s, _ := newStore(t)
	for _, tc := range []struct {
		name string
		role Role
	}{{"", RoleViewer}, {"x\ny", RoleViewer}, {"ok", "root"}, {strings.Repeat("n", 61), RoleViewer}} {
		if _, _, err := s.Create(tc.name, tc.role); err == nil {
			t.Errorf("Create(%q, %q) should fail", tc.name, tc.role)
		}
	}
}

func TestRoleOrdering(t *testing.T) {
	if !RoleAdmin.Allows(RoleViewer) || !RoleDeployer.Allows(RoleViewer) || RoleViewer.Allows(RoleDeployer) || RoleDeployer.Allows(RoleAdmin) {
		t.Fatal("viewer < deployer < admin")
	}
	if Role("").Allows(RoleViewer) || Role("root").Allows(RoleViewer) {
		t.Fatal("an unknown role has no access")
	}
}
