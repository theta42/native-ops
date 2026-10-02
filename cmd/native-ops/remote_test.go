package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/theta42/native-ops/pkg/server"
)

func TestPackTreeIsAcceptedByTheDaemonsExtractor(t *testing.T) {
	src := t.TempDir()
	for name, body := range map[string]string{
		"fleet.yml":                       "name: prod\n",
		"services/web/service.yml":        "image: x\n",
		"edge/Caddyfile":                  "import /etc/caddy/sites/*.caddy\n",
		".git/config":                     "[core]\n",
		"templates/platform/template.yml": "name: platform\n",
	} {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := packTree(src)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := server.ExtractTarGz(bytes.NewReader(b), dest); err != nil {
		t.Fatalf("the daemon refuses what remote sends: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "services", "web", "service.yml")); string(got) != "image: x\n" {
		t.Fatalf("content lost: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git must not be sent")
	}
}

func TestPackTreeRefusesASymlink(t *testing.T) {
	src := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "passwd")); err != nil {
		t.Skip(err)
	}
	if _, err := packTree(src); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlink must be an error: %v", err)
	}
}

func TestWaitFollowsAJobToItsEnd(t *testing.T) {
	var polls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/v1/jobs/j-1-abcdef12" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if polls.Add(1) < 2 {
			_, _ = w.Write([]byte(`{"status":"running"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"failed","error":"boom","log":"step 1\n"}`))
	}))
	defer ts.Close()
	c := &remoteClient{base: ts.URL, token: "tok", http: ts.Client()}
	if code := c.wait(context.Background(), "j-1-abcdef12"); code != 1 {
		t.Fatalf("a failed job must exit 1, got %d", code)
	}
	if polls.Load() != 2 {
		t.Fatalf("polled %d times", polls.Load())
	}
}

func TestREADMECarriesTheUsage(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), usageText) {
		t.Fatal("README.md's CLI Usage block must be exactly usageText (copy `native-ops` with no arguments into it)")
	}
}

func TestDNSSyncWithNoRecordsNeedsNoProvider(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte("name: f\ndns_provider: digitalocean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DO_API_TOKEN", "")
	var logged []string
	if err := dnsSyncSource(func(string) string { return "" }, nil)(context.Background(), dir, func(f string, a ...any) { logged = append(logged, f) }); err != nil {
		t.Fatalf("nothing to sync must succeed without provider credentials: %v", err)
	}
	if len(logged) != 1 {
		t.Fatalf("it must say there was nothing to sync: %v", logged)
	}
}

func writeFleet(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func lookupOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDaemonBackupsGoOnlyToThePinnedDestination(t *testing.T) {
	dir := writeFleet(t, "name: f\nbackup:\n  endpoint: https://evil.example.net\n  region: x\n  bucket: loot\n")
	keys := map[string]string{"BACKUP_S3_ACCESS_KEY": "AK", "BACKUP_S3_SECRET_KEY": "SK"}
	if _, err := daemonBackupStore(dir, nil, lookupOf(keys)); err == nil || !strings.Contains(err.Error(), "no pinned backup destination") {
		t.Fatalf("without a pinned destination a backup must be refused: %v", err)
	}
	keys["NATIVE_OPS_BACKUP_ENDPOINT"], keys["NATIVE_OPS_BACKUP_BUCKET"] = "https://nyc3.digitaloceanspaces.com", "fleet-backups"
	if _, err := daemonBackupStore(dir, nil, lookupOf(keys)); err == nil || !strings.Contains(err.Error(), "not the one pinned") {
		t.Fatalf("an upload naming another destination must be refused: %v", err)
	}
	good := writeFleet(t, "name: f\nbackup:\n  endpoint: https://nyc3.digitaloceanspaces.com/\n  region: nyc3\n  bucket: fleet-backups\n")
	if _, err := daemonBackupStore(good, nil, lookupOf(keys)); err != nil {
		t.Fatalf("the pinned destination with synced keys must work: %v", err)
	}
	delete(keys, "BACKUP_S3_SECRET_KEY")
	if _, err := daemonBackupStore(good, nil, lookupOf(keys)); err == nil {
		t.Fatal("missing keys must be an error")
	}
}

func TestDaemonDNSSyncTouchesOnlyAllowedZonesAndNeedsAToken(t *testing.T) {
	dir := writeFleet(t, "name: f\ndns_provider: digitalocean\ndns_records:\n  - zone: victim.example\n    type: A\n    name: www\n    value: 203.0.113.66\n")
	logf := func(string, ...any) {}
	err := dnsSyncSource(lookupOf(map[string]string{"DO_API_TOKEN": "t"}), []string{"example.com"})(context.Background(), dir, logf)
	if err == nil || !strings.Contains(err.Error(), "may not change") {
		t.Fatalf("a zone the daemon was not given must be refused: %v", err)
	}
	err = dnsSyncSource(lookupOf(map[string]string{"NATIVE_OPS_DNS_ZONES": "example.com, victim.example"}), nil)(context.Background(), dir, logf)
	if err == nil || !strings.Contains(err.Error(), "DO_API_TOKEN is not set") {
		t.Fatalf("an allowed zone without a token must say what to sync: %v", err)
	}
}

func TestLazyDOReadsTheTokenAtEachCall(t *testing.T) {
	tok := ""
	l := lazyDO{func(string) string { return tok }}
	if _, err := l.ListRecords(context.Background(), "example.com"); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("no token yet must be a clear error: %v", err)
	}
	tok = "dop_x"
	if _, err := l.client(); err != nil {
		t.Fatalf("a token synced later must be used: %v", err)
	}
}
