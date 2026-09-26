package incus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/remote"
)

type stubExec struct {
	out  string
	err  error
	cmds []string
}

func (s *stubExec) Run(_ context.Context, cmd string) (string, error) {
	s.cmds = append(s.cmds, cmd)
	return s.out, s.err
}
func (s *stubExec) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (s *stubExec) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (s *stubExec) Close() error                                                    { return nil }

var _ remote.Executor = (*stubExec)(nil)

func TestRef(t *testing.T) {
	if Ref("dst", "web") != "dst:web" || Ref("", "web") != "web" {
		t.Fatal("Ref must join remote and name, and pass a bare name through")
	}
}

// Regression: the bare `snapshot <pool> <vol> <name>` form is not accepted by Incus.
func TestSnapshotsUseTheCreateForm(t *testing.T) {
	ex := &stubExec{}
	c := NewClient(ex)
	if err := c.SnapshotVolume(context.Background(), "default", "vol1", "s1"); err != nil {
		t.Fatal(err)
	}
	if err := c.SnapshotVolumeAt(context.Background(), "src", "default", "vol1", "s2"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"incus storage volume snapshot create 'default' 'vol1' 's1'",
		"incus storage volume snapshot create 'src:default' 'vol1' 's2'",
	}
	for i, w := range want {
		if ex.cmds[i] != w {
			t.Errorf("command %d = %q, want %q", i, ex.cmds[i], w)
		}
	}
}

func TestInstanceStatusesAndCustomVolumes(t *testing.T) {
	ex := &stubExec{out: `[{"name":"a","status":"Running"},{"name":"b","status":"Stopped"}]`}
	c := NewClient(ex)
	st, err := c.InstanceStatuses(context.Background(), "dst")
	if err != nil || st["a"] != "Running" || st["b"] != "Stopped" || len(st) != 2 {
		t.Fatalf("got %v %v", st, err)
	}
	if ex.cmds[0] != "incus list 'dst:' --format json" {
		t.Errorf("got %q", ex.cmds[0])
	}

	ex.out = `[{"name":"data","type":"custom"},{"name":"data2","type":"image"},{"name":"c1","type":"container"}]`
	vols, err := c.CustomVolumes(context.Background(), "dst", "default")
	if err != nil || !vols["data"] || len(vols) != 1 {
		t.Fatalf("only custom volumes count: %v %v", vols, err)
	}
	if ex.cmds[1] != "incus storage volume list 'dst:default' --format json" {
		t.Errorf("got %q", ex.cmds[1])
	}

	ex.err = errors.New("connection refused")
	if _, err := c.InstanceStatuses(context.Background(), "dst"); err == nil {
		t.Error("an unreachable remote must be an error, not an empty list")
	}
	if _, err := c.InstanceStatuses(context.Background(), "d'st"); err == nil {
		t.Error("unsafe remote names must be rejected")
	}
}

func TestCopyFlags(t *testing.T) {
	ex := &stubExec{}
	c := NewClient(ex)
	ctx := context.Background()
	_ = c.CopyVolume(ctx, "src", "dst", "default", "v", false)
	_ = c.CopyVolume(ctx, "src", "dst", "default", "v", true)
	_ = c.CopyInstance(ctx, "src", "dst", "web", false)
	_ = c.CopyInstance(ctx, "src", "dst", "web", true)
	want := []string{
		"incus storage volume copy 'src:default/v' 'dst:default/v' --volume-only",
		"incus storage volume copy 'src:default/v' 'dst:default/v' --volume-only --refresh",
		"incus copy 'src:web' 'dst:web' --mode=push --instance-only --stateless",
		"incus copy 'src:web' 'dst:web' --mode=push --instance-only --stateless --refresh",
	}
	for i, w := range want {
		if ex.cmds[i] != w {
			t.Errorf("command %d = %q, want %q", i, ex.cmds[i], w)
		}
	}
}

func TestProbeRunsInsideTheContainerAndQuotesTheURL(t *testing.T) {
	ex := &stubExec{}
	c := NewClient(ex)
	if err := c.ProbeHTTPInContainer(context.Background(), "dst", "rest-x", 8787, "/health"); err != nil {
		t.Fatal(err)
	}
	cmd := ex.cmds[0]
	for _, want := range []string{"incus exec 'dst:rest-x' -- sh -c ", "curl -fsS", "wget -q", " probe 'http://127.0.0.1:8787/health'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q: %s", want, cmd)
		}
	}
}

func TestWaitStopsRetryingWhenTheContainerHasNoHTTPClient(t *testing.T) {
	ex := &stubExec{err: errors.New("no curl or wget in the container")}
	c := NewClient(ex)
	err := c.WaitHTTPInContainer(context.Background(), "dst", "rest-x", config.HealthCheckConfig{Path: "/health", Port: 80, Timeout: 30})
	if err == nil || !strings.Contains(err.Error(), "cannot health check") {
		t.Fatalf("got %v", err)
	}
	if len(ex.cmds) != 1 {
		t.Fatalf("a missing tool is not transient and must not be retried: %v", ex.cmds)
	}
}

func TestWaitIsANoOpWithoutAPathAndSucceedsOnFirstOK(t *testing.T) {
	ex := &stubExec{}
	c := NewClient(ex)
	if err := c.WaitHTTPInContainer(context.Background(), "dst", "x", config.HealthCheckConfig{}); err != nil || len(ex.cmds) != 0 {
		t.Fatalf("no path means no check: %v %v", err, ex.cmds)
	}
	if err := c.WaitHTTPInContainer(context.Background(), "dst", "x", config.HealthCheckConfig{Path: "/h", Port: 9}); err != nil || len(ex.cmds) != 1 {
		t.Fatalf("got %v %v", err, ex.cmds)
	}
}

// Regression, found on a real host: executors include the command line in their error text, and the probe
// script used to contain the "no curl or wget" message literally, so every failed probe was mistaken for a
// missing client and never retried (a service that needs a few seconds to start was rolled back instantly).
func TestWaitRetriesARealFailureEvenThoughTheErrorQuotesTheProbeScript(t *testing.T) {
	ex := &fnExec{fn: func(cmd string) (string, error) {
		return "", fmt.Errorf("command failed: %s (stderr: curl: (7) Failed to connect to 127.0.0.1 port 8787): exit status 7", cmd)
	}}
	c := NewClient(ex)
	err := c.WaitHTTPInContainer(context.Background(), "dst", "rest-x", config.HealthCheckConfig{Path: "/health", Port: 8787, Timeout: 1, Interval: 1})
	if err == nil || !strings.Contains(err.Error(), "health check failed") || strings.Contains(err.Error(), "cannot health check") {
		t.Fatalf("a connection failure is a health failure, not a missing client: %v", err)
	}
	if len(ex.cmds) < 2 {
		t.Fatalf("the probe must be retried until the timeout, ran %d time(s)", len(ex.cmds))
	}
	if strings.Contains(probeScript, noClientMessage) {
		t.Fatal("the message must not appear literally in the script")
	}
}

type fnExec struct {
	fn   func(string) (string, error)
	cmds []string
}

func (f *fnExec) Run(_ context.Context, cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	return f.fn(cmd)
}
func (f *fnExec) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (f *fnExec) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (f *fnExec) Close() error                                                    { return nil }
