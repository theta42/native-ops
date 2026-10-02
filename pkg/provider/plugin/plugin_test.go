package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/provider"
)

// writePlugin puts an executable script at <dir>/providers/dns/<name>.
func writePlugin(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, "providers", "dns", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestPluginContract(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "seen")
	writePlugin(t, dir, "acme.sh", `
case "$1" in
  list)   echo '[{"id":"7","type":"A","name":"@","value":"203.0.113.9","ttl":300}]' ;;
  sync)   { echo "args: $*"; cat; } > `+out+` ;;
  delete) echo "delete $*" > `+out+` ;;
esac
`)
	p, err := NewScriptDNSProvider("acme", dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "acme" {
		t.Fatalf("name %q", p.Name())
	}
	ctx := context.Background()

	recs, err := p.ListRecords(ctx, "example.com")
	if err != nil || len(recs) != 1 || recs[0].ID != "7" || recs[0].Value != "203.0.113.9" {
		t.Fatalf("list: %+v %v", recs, err)
	}

	if err := p.SyncRecords(ctx, "example.com", []provider.DNSRecord{{Type: "A", Name: "*", Value: "203.0.113.9"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !strings.Contains(string(got), "args: sync --domain example.com") || !strings.Contains(string(got), `"action":"sync"`) || !strings.Contains(string(got), `"name":"*"`) {
		t.Fatalf("sync must get the action and domain as arguments and the records as JSON on stdin: %s", got)
	}

	if err := p.DeleteRecord(ctx, "example.com", "7"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); strings.TrimSpace(string(got)) != "delete delete --domain example.com --id 7" {
		t.Fatalf("delete args: %q", got)
	}
}

func TestPluginLookupAndErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewScriptDNSProvider("missing", dir); err == nil {
		t.Fatal("a plugin that does not exist must be an error")
	}
	writePlugin(t, dir, "broken.py", "echo oops >&2; exit 3\n")
	p, err := NewScriptDNSProvider("broken", dir)
	if err != nil {
		t.Fatalf("the .py candidate must be found: %v", err)
	}
	if _, err := p.ListRecords(context.Background(), "example.com"); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("a failing plugin's stderr must be in the error: %v", err)
	}
	writePlugin(t, dir, "garbage", "echo not-json\n")
	g, _ := NewScriptDNSProvider("garbage", dir)
	if _, err := g.ListRecords(context.Background(), "example.com"); err == nil {
		t.Fatal("output that is not JSON must be an error")
	}
}
