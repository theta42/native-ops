package engine

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
)

// forwardExec stands in for a host: `incus config show` returns the devices it holds, everything else
// is recorded. Enough to prove ensureForwards adds a missing proxy device, converges a changed one, and
// leaves a matching one alone.
type forwardExec struct {
	devices map[string]map[string]string
	cmds    []string
}

func (f *forwardExec) Run(_ context.Context, cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	if strings.HasPrefix(cmd, "incus config show ") {
		var b strings.Builder
		b.WriteString("config:\n  volatile.base_image: abc\n")
		b.WriteString("devices:\n")
		for name, d := range f.devices {
			b.WriteString("  " + name + ":\n")
			for k, v := range d {
				b.WriteString("    " + k + ": \"" + v + "\"\n")
			}
		}
		b.WriteString("profiles:\n  - default\n")
		return b.String(), nil
	}
	return "", nil
}
func (f *forwardExec) RunWithInput(context.Context, string, io.Reader) (string, error) {
	return "", nil
}
func (f *forwardExec) WriteFile(context.Context, string, []byte, os.FileMode) error { return nil }
func (f *forwardExec) Close() error                                                  { return nil }

func TestEnsureForwardsAddsMissingDevice(t *testing.T) {
	f := &forwardExec{devices: map[string]map[string]string{}}
	changed, err := ensureForwards(context.Background(), incus.NewClient(f), "gitea", []config.PortForward{
		{Name: "ssh-git", Listen: "0.0.0.0:2222", Connect: "127.0.0.1:2222"},
	})
	if err != nil {
		t.Fatalf("ensureForwards: %v", err)
	}
	if !changed {
		t.Error("expected changed=true when the device is missing")
	}
	joined := strings.Join(f.cmds, "\n")
	want := "incus config device add 'gitea' 'ssh-git' 'proxy' 'connect=tcp:127.0.0.1:2222' 'listen=tcp:0.0.0.0:2222'"
	if !strings.Contains(joined, want) {
		t.Errorf("expected command:\n  %s\ngot:\n%s", want, joined)
	}
}

func TestEnsureForwardsLeavesMatchingDeviceAlone(t *testing.T) {
	f := &forwardExec{devices: map[string]map[string]string{
		"ssh-git": {"type": "proxy", "listen": "tcp:0.0.0.0:2222", "connect": "tcp:127.0.0.1:2222"},
	}}
	changed, err := ensureForwards(context.Background(), incus.NewClient(f), "gitea", []config.PortForward{
		{Name: "ssh-git", Listen: "tcp:0.0.0.0:2222", Connect: "tcp:127.0.0.1:2222"},
	})
	if err != nil {
		t.Fatalf("ensureForwards: %v", err)
	}
	if changed {
		t.Error("a matching device must not change")
	}
	if strings.Contains(strings.Join(f.cmds, "\n"), "config device") {
		t.Errorf("no device command expected, got: %v", f.cmds)
	}
}

func TestEnsureForwardsConvergesDriftedDevice(t *testing.T) {
	f := &forwardExec{devices: map[string]map[string]string{
		"ssh-git": {"type": "proxy", "listen": "tcp:0.0.0.0:2233", "connect": "tcp:127.0.0.1:2222"},
	}}
	changed, err := ensureForwards(context.Background(), incus.NewClient(f), "gitea", []config.PortForward{
		{Name: "ssh-git", Listen: "0.0.0.0:2222", Connect: "127.0.0.1:2222"},
	})
	if err != nil {
		t.Fatalf("ensureForwards: %v", err)
	}
	if !changed {
		t.Error("a drifted device must change")
	}
	if !strings.Contains(strings.Join(f.cmds, "\n"), "listen=tcp:0.0.0.0:2222") {
		t.Errorf("expected the new listen address, got: %v", f.cmds)
	}
}
