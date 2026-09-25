package caddy

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/remote"
)

type fakeExec struct {
	cmds     []string
	hasCaddy bool // whether /etc/caddy/Caddyfile already exists
}

func (f *fakeExec) Run(_ context.Context, command string) (string, error) {
	f.cmds = append(f.cmds, command)
	if strings.Contains(command, "test -f /etc/caddy/Caddyfile") && !f.hasCaddy {
		return "", fmt.Errorf("no such file")
	}
	return "", nil
}
func (f *fakeExec) RunWithInput(context.Context, string, io.Reader) (string, error) {
	return "", nil
}
func (f *fakeExec) WriteFile(context.Context, string, []byte, os.FileMode) error { return nil }
func (f *fakeExec) Close() error                                                 { return nil }

func (f *fakeExec) ran(sub string) bool {
	for _, c := range f.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

var _ remote.Executor = (*fakeExec)(nil)

// The edge is shared and may be hand-maintained: never overwrite its Caddyfile.
func TestEnsureBaseCaddyfileNeverOverwritesExisting(t *testing.T) {
	exec := &fakeExec{hasCaddy: true}
	mgr := NewEdgeManager(exec, "edge")
	if err := mgr.EnsureBaseCaddyfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exec.ran("cat > /etc/caddy/Caddyfile") {
		t.Fatalf("must not overwrite an existing Caddyfile; ran: %v", exec.cmds)
	}
}

func TestEnsureBaseCaddyfileSeedsWhenMissing(t *testing.T) {
	exec := &fakeExec{hasCaddy: false}
	mgr := NewEdgeManager(exec, "edge")
	if err := mgr.EnsureBaseCaddyfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exec.ran("cat > /etc/caddy/Caddyfile") {
		t.Fatalf("expected a seed write when no Caddyfile exists; ran: %v", exec.cmds)
	}
}

// Reload must be a graceful reload only: no edge restart, no resolv.conf rewrite.
func TestReloadIsGracefulOnly(t *testing.T) {
	exec := &fakeExec{}
	mgr := NewEdgeManager(exec, "edge")
	if err := mgr.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exec.ran("incus restart") {
		t.Fatalf("Reload must not restart the shared edge; ran: %v", exec.cmds)
	}
	if exec.ran("resolv.conf") {
		t.Fatalf("Reload must not rewrite the edge resolv.conf; ran: %v", exec.cmds)
	}
	if !exec.ran("caddy reload") {
		t.Fatalf("expected a caddy reload; ran: %v", exec.cmds)
	}
}
