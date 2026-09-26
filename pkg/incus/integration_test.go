//go:build integration

package incus

// Integration tests: the real client helpers against a real Incus server.
//
// The unit tests use fake executors, which cannot notice a wrong CLI form (a
// snapshot command written in the LXD-era syntax passed them for months). These
// run the actual commands. They are skipped unless built with the tag and an
// Incus server is reachable by the current user:
//
//	go test -tags integration -count=1 -v -run TestIntegration ./pkg/incus
//
// They create, and always remove, only instances and volumes named nops-it-*.
// The first run downloads an Alpine image (a few MB); set NOPS_IT_IMAGE to use another.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/remote"
)

func requireIncus(t *testing.T) (*Client, remote.Executor) {
	t.Helper()
	if _, err := exec.LookPath("incus"); err != nil {
		t.Skip("incus CLI not installed")
	}
	if out, err := exec.Command("incus", "info").CombinedOutput(); err != nil {
		t.Skipf("cannot talk to an Incus server: %v: %s", err, out)
	}
	ex := remote.NewLocalExecutor()
	return NewClient(ex), ex
}

// Every flag the code passes must exist in this Incus version.
func TestIntegrationCLIFormsExist(t *testing.T) {
	requireIncus(t)
	forms := []struct {
		cmd   string
		flags []string
	}{
		{"incus storage volume snapshot create", nil},
		{"incus storage volume copy", []string{"--volume-only", "--refresh"}},
		{"incus storage volume list", []string{"--format"}},
		{"incus storage volume delete", nil},
		{"incus copy", []string{"--instance-only", "--mode", "--refresh", "--stateless"}},
		{"incus file push", []string{"--create-dirs", "--uid", "--gid", "--mode"}},
		{"incus file pull", nil},
		{"incus config set", nil},
		{"incus config show", nil},
		{"incus config device add", nil},
		{"incus network get", nil},
		{"incus network set", nil},
		{"incus list", []string{"--format"}},
		{"incus stop", []string{"--timeout", "--force"}},
		{"incus delete", []string{"--force"}},
		{"incus launch", []string{"--profile", "--config"}},
		{"incus exec", nil},
		{"incus image alias list", []string{"--format"}},
	}
	for _, f := range forms {
		out, err := exec.Command("sh", "-c", f.cmd+" --help").CombinedOutput()
		if err != nil {
			t.Errorf("`%s --help` failed (is the command form valid for this Incus version?): %v\n%s", f.cmd, err, out)
			continue
		}
		for _, flag := range f.flags {
			if !strings.Contains(string(out), flag) {
				t.Errorf("`%s` does not document %s", f.cmd, flag)
			}
		}
	}
}

func TestIntegrationClientHelpers(t *testing.T) {
	c, ex := requireIncus(t)
	ctx := context.Background()
	image := os.Getenv("NOPS_IT_IMAGE")
	if image == "" {
		image = "images:alpine/3.22"
	}
	name := fmt.Sprintf("nops-it-%d", time.Now().Unix())
	vol := name + "-data"
	sh := func(cmd string) string {
		t.Helper()
		out, err := ex.Run(ctx, cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return out
	}
	t.Cleanup(func() {
		_, _ = ex.Run(ctx, "incus delete --force "+ShQuote(name))
		_, _ = ex.Run(ctx, "incus storage volume delete default "+ShQuote(vol))
	})

	// Launch with awkward config values: quoting must survive the shell and the YAML round trip.
	tricky := `it's a "test" $(id) & more`
	if err := c.LaunchContainer(ctx, image, name, []string{"default"}, map[string]string{
		ImageKey: image, "user.note": tricky, "limits.memory": "256MB",
	}); err != nil {
		t.Fatal(err)
	}

	if ok, err := c.InstanceExists(ctx, name); err != nil || !ok {
		t.Fatalf("InstanceExists = %v, %v", ok, err)
	}
	if ok, err := c.InstanceExists(ctx, name+"-nope"); err != nil || ok {
		t.Fatalf("a missing instance must be (false, nil): %v, %v", ok, err)
	}
	st, err := c.InstanceStatuses(ctx, "local")
	if err != nil || st[name] != "Running" {
		t.Fatalf("InstanceStatuses = %v, %v", st, err)
	}

	state, err := c.CaptureInstanceState(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.BaseImage) != 64 || state.Config["user.note"] != tricky || state.Config[ImageKey] != image {
		t.Fatalf("captured state does not match what was launched: base=%q config=%v", state.BaseImage, state.Config)
	}

	// Volumes: create, attach (twice), list.
	if err := c.EnsureVolume(ctx, "default", vol); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureVolume(ctx, "default", vol); err != nil {
		t.Fatalf("EnsureVolume must be repeatable: %v", err)
	}
	vols, err := c.CustomVolumes(ctx, "local", "default")
	if err != nil || !vols[vol] {
		t.Fatalf("CustomVolumes = %v, %v", vols, err)
	}
	for i := 0; i < 2; i++ {
		if err := c.EnsureVolumeAttached(ctx, name, "default", vol, "/data", false); err != nil {
			t.Fatalf("EnsureVolumeAttached #%d: %v", i+1, err)
		}
	}
	state, _ = c.CaptureInstanceState(ctx, name)
	if got := state.Volumes(); len(got) != 1 || got[0].Name != vol {
		t.Fatalf("attached volumes = %v", got)
	}

	// Env file: sorted, mode 0600, owned by root, and stable across writes.
	env := map[string]string{"B": "2", "A": "1"}
	if err := c.WriteEnvironmentFile(ctx, name, "svc", env); err != nil {
		t.Fatal(err)
	}
	got, found, err := c.PullFile(ctx, name, "/etc/default/svc")
	if err != nil || !found || got != RenderEnv("svc", env) {
		t.Fatalf("env file round trip: %q found=%v err=%v", got, found, err)
	}
	if perms := strings.TrimSpace(sh("incus exec " + ShQuote(name) + " -- stat -c '%a:%u:%g' /etc/default/svc")); perms != "600:0:0" {
		t.Fatalf("env file must be 0600 root:root, got %s", perms)
	}
	if _, found, err := c.PullFile(ctx, name, "/etc/default/does-not-exist"); err != nil || found {
		t.Fatalf("a missing file must be (found=false, nil), got found=%v err=%v", found, err)
	}

	// Snapshots use the `create` form, locally and through a remote reference.
	if err := c.SnapshotVolume(ctx, "default", vol, "it-snap-a"); err != nil {
		t.Fatal(err)
	}
	if err := c.SnapshotVolumeAt(ctx, "local", "default", vol, "it-snap-b"); err != nil {
		t.Fatal(err)
	}
	if snaps := sh("incus storage volume snapshot list default " + ShQuote(vol) + " --format csv"); !strings.Contains(snaps, "it-snap-a") || !strings.Contains(snaps, "it-snap-b") {
		t.Fatalf("snapshots not created: %q", snaps)
	}

	// Config writes use the key=value form.
	if err := c.SetInstanceConfig(ctx, name, "user.x", `a b'c`); err != nil {
		t.Fatal(err)
	}
	if err := c.ResizeLimits(ctx, name, map[string]string{"limits.cpu": "1"}); err != nil {
		t.Fatal(err)
	}
	state, _ = c.CaptureInstanceState(ctx, name)
	if state.Config["user.x"] != `a b'c` || state.Config["limits.cpu"] != "1" {
		t.Fatalf("config writes did not stick: %v", state.Config)
	}

	// Network settings: the guarded command must be valid shell for both branches.
	sh(NetworkSetIfChanged("incusbr0", "user.nops-it", "x"))
	sh(NetworkSetIfChanged("incusbr0", "user.nops-it", "x"))
	_, _ = ex.Run(ctx, "incus network unset incusbr0 user.nops-it")

	// Probe from inside the container: a listening server passes, a closed port fails.
	// (Alpine's core busybox has no httpd applet, so serve one fixed response with nc.)
	sh("incus exec " + ShQuote(name) + " -- sh -c '(while true; do printf \"HTTP/1.1 200 OK\\r\\nContent-Length: 3\\r\\nConnection: close\\r\\n\\r\\nok\\n\" | nc -l -p 8787; done) </dev/null >/dev/null 2>&1 &'")
	time.Sleep(2 * time.Second)
	if err := c.ProbeHTTPInContainer(ctx, "local", name, 8787, "/health"); err != nil {
		t.Fatalf("probe of a healthy endpoint: %v", err)
	}
	if err := c.ProbeHTTPInContainer(ctx, "local", name, 8788, "/health"); err == nil {
		t.Fatal("a closed port must fail the probe")
	}
	if err := c.WaitHTTPInContainer(ctx, "local", name, config.HealthCheckConfig{Path: "/health", Port: 8787, Timeout: 10}); err != nil {
		t.Fatal(err)
	}

	// Stop / start and status.
	if err := c.StopInstance(ctx, "local", name, 30); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.InstanceStatuses(ctx, "local"); st[name] != "Stopped" {
		t.Fatalf("after stop: %v", st[name])
	}
	if err := c.StartInstance(ctx, "local", name); err != nil {
		t.Fatal(err)
	}

	// Addresses (skipped if the default profile has no network).
	deadline := time.Now().Add(20 * time.Second)
	for {
		ips, err := c.GlobalIPv4s(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if len(ips) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Log("no global IPv4 (the default profile may have no network); skipping the address check")
			break
		}
		time.Sleep(time.Second)
	}
}
