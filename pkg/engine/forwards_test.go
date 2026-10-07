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
	config  map[string]string
	cmds    []string
	apply   bool // apply device and config commands to devices/config
}

func (f *forwardExec) Run(_ context.Context, cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	if strings.HasPrefix(cmd, "incus config show ") {
		var b strings.Builder
		b.WriteString("config:\n  volatile.base_image: abc\n")
		for k, v := range f.config {
			b.WriteString("  " + k + ": \"" + v + "\"\n")
		}
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
	if f.apply {
		f.applyCmd(cmd)
	}
	return "", nil
}
func (f *forwardExec) RunWithInput(context.Context, string, io.Reader) (string, error) {
	return "", nil
}
func (f *forwardExec) WriteFile(context.Context, string, []byte, os.FileMode) error { return nil }
func (f *forwardExec) Close() error                                                 { return nil }

func TestEnsureForwardsAddsMissingDevice(t *testing.T) {
	f := &forwardExec{devices: map[string]map[string]string{}}
	changed, err := ensureForwards(context.Background(), incus.NewClient(f), "gitea", []config.PortForward{
		{Name: "ssh-git", Listen: "0.0.0.0:2222", Target: "2222"},
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
	}, config: map[string]string{managedForwardsKey: "ssh-git"}}
	changed, err := ensureForwards(context.Background(), incus.NewClient(f), "gitea", []config.PortForward{
		{Name: "ssh-git", Listen: "0.0.0.0:2222", Target: "2222"},
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
		{Name: "ssh-git", Listen: "0.0.0.0:2222", Target: "2222"},
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

// statefulForwards is forwardExec that also applies the device and config commands, so a test can apply and
// then plan again.
func statefulForwards(devices map[string]map[string]string, cfg map[string]string) *forwardExec {
	if cfg == nil {
		cfg = map[string]string{}
	}
	return &forwardExec{devices: devices, config: cfg, apply: true}
}

func (f *forwardExec) applyCmd(cmd string) {
	q := simQuotedRe.FindAllStringSubmatch(cmd, -1)
	arg := func(i int) string { return q[i][1] }
	kv := func(from int) map[string]string {
		m := map[string]string{}
		for _, x := range q[from:] {
			k, v, _ := strings.Cut(x[1], "=")
			m[k] = v
		}
		return m
	}
	switch {
	case strings.HasPrefix(cmd, "incus config device add "):
		d := kv(3)
		d["type"] = arg(2)
		f.devices[arg(1)] = d
	case strings.HasPrefix(cmd, "incus config device override "):
		d := kv(2)
		d["type"] = f.devices[arg(1)]["type"]
		f.devices[arg(1)] = d
	case strings.HasPrefix(cmd, "incus config device remove "):
		delete(f.devices, arg(1))
	case strings.HasPrefix(cmd, "incus config set "):
		k, v, _ := strings.Cut(arg(1), "=")
		f.config[k] = v
	case strings.HasPrefix(cmd, "incus config unset "):
		delete(f.config, arg(1))
	}
}

func driftOf(t *testing.T, f *forwardExec, forwards []config.PortForward) ([]forwardChange, string) {
	t.Helper()
	st, err := incus.NewClient(f).CaptureInstanceState(context.Background(), "gitea")
	if err != nil {
		t.Fatal(err)
	}
	ch, rec, err := forwardsDrift(st, forwards)
	if err != nil {
		t.Fatal(err)
	}
	return ch, rec
}

// A forward without a name is published as "<protocol>-<port>", and is then recognised as published: it
// used to be looked up and written under an empty name, so it never converged.
func TestAnUnnamedForwardConvergesAndThenStaysPut(t *testing.T) {
	f := statefulForwards(map[string]map[string]string{}, nil)
	fwd := []config.PortForward{{Listen: "25", Target: "2525"}}
	if _, err := ensureForwards(context.Background(), incus.NewClient(f), "gitea", fwd); err != nil {
		t.Fatal(err)
	}
	if d := f.devices["tcp-25"]; d["listen"] != "tcp:0.0.0.0:25" || d["connect"] != "tcp:127.0.0.1:2525" {
		t.Fatalf("the forward should be the device tcp-25, got %v", f.devices)
	}
	if ch, rec := driftOf(t, f, fwd); len(ch) != 0 || rec != "" {
		t.Fatalf("after apply nothing is left to do, got %+v %q", ch, rec)
	}
	if changed, _ := ensureForwards(context.Background(), incus.NewClient(f), "gitea", fwd); changed {
		t.Fatal("a second apply must change nothing")
	}
}

// A forward deleted from the manifest is removed from the host; a proxy device native-ops never declared
// is left alone.
func TestAForwardRemovedFromTheManifestIsRemovedButAForeignOneIsKept(t *testing.T) {
	f := statefulForwards(map[string]map[string]string{
		"ssh-git":  {"type": "proxy", "listen": "tcp:0.0.0.0:2222", "connect": "tcp:127.0.0.1:2222"},
		"smtp":     {"type": "proxy", "listen": "tcp:0.0.0.0:25", "connect": "tcp:127.0.0.1:2525"},
		"handmade": {"type": "proxy", "listen": "tcp:0.0.0.0:8443", "connect": "tcp:127.0.0.1:443"},
	}, map[string]string{managedForwardsKey: "smtp,ssh-git"})
	keep := []config.PortForward{{Name: "ssh-git", Listen: "2222", Target: "2222"}}

	p := &ServicePlan{Service: "gitea"}
	st, _ := incus.NewClient(f).CaptureInstanceState(context.Background(), "gitea")
	planForwards(p, st, keep)
	if len(p.Changes) != 1 || p.Changes[0].Kind != ChangeRemovePort || !strings.HasPrefix(p.Changes[0].Detail, "smtp:") {
		t.Fatalf("the plan should remove smtp and nothing else: %+v", p.Changes)
	}

	if _, err := ensureForwards(context.Background(), incus.NewClient(f), "gitea", keep); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.devices["smtp"]; ok {
		t.Fatal("smtp should have been removed")
	}
	if _, ok := f.devices["handmade"]; !ok {
		t.Fatal("a proxy device the manifest never declared must be left alone")
	}
	if f.config[managedForwardsKey] != "ssh-git" {
		t.Fatalf("the managed list should now be ssh-git, got %q", f.config[managedForwardsKey])
	}
	if ch, rec := driftOf(t, f, keep); len(ch) != 0 || rec != "" {
		t.Fatalf("after apply nothing is left to do, got %+v %q", ch, rec)
	}
}

// Ports published before the managed list existed: the plan says it records them, apply writes only that.
func TestPortsPublishedBeforeTheManagedListAreRecordedNotRepublished(t *testing.T) {
	f := statefulForwards(map[string]map[string]string{
		"ssh-git": {"type": "proxy", "listen": "tcp:0.0.0.0:2222", "connect": "tcp:127.0.0.1:2222"},
	}, nil)
	fwd := []config.PortForward{{Name: "ssh-git", Listen: "0.0.0.0:2222", Target: "2222"}}
	p := &ServicePlan{Service: "gitea"}
	st, _ := incus.NewClient(f).CaptureInstanceState(context.Background(), "gitea")
	planForwards(p, st, fwd)
	if len(p.Changes) != 1 || !strings.Contains(p.Changes[0].Detail, "record ssh-git") {
		t.Fatalf("the plan should say it records the managed ports: %+v", p.Changes)
	}
	if _, err := ensureForwards(context.Background(), incus.NewClient(f), "gitea", fwd); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.cmds, "\n"), "config device") {
		t.Fatalf("no device may be touched, got %v", f.cmds)
	}
	if ch, rec := driftOf(t, f, fwd); len(ch) != 0 || rec != "" {
		t.Fatalf("after apply nothing is left to do, got %+v %q", ch, rec)
	}
}
