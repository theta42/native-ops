package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/remote"
)

type rule struct {
	match string
	out   string
	err   error
}

// scriptExec answers commands from a rule list (first substring match wins,
// default success/empty) and records every command in order.
type scriptExec struct {
	rules []rule
	cmds  []string
}

func (s *scriptExec) Run(_ context.Context, cmd string) (string, error) {
	s.cmds = append(s.cmds, cmd)
	for _, r := range s.rules {
		if strings.Contains(cmd, r.match) {
			return r.out, r.err
		}
	}
	return "", nil
}
func (s *scriptExec) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (s *scriptExec) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (s *scriptExec) Close() error                                                    { return nil }

var _ remote.Executor = (*scriptExec)(nil)

// index returns the position of the first command containing sub, or -1.
func (s *scriptExec) index(sub string) int {
	for i, c := range s.cmds {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}
func (s *scriptExec) idx(sub string) []int {
	var out []int
	for i, c := range s.cmds {
		if strings.Contains(c, sub) {
			out = append(out, i)
		}
	}
	return out
}
func (s *scriptExec) count(sub string) int {
	n := 0
	for _, c := range s.cmds {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

var (
	oldFP  = strings.Repeat("a", 64)
	newFP  = strings.Repeat("b", 64)
	envTxt = "PORT=8787\nSECRET_TOKEN=s3cr3t-value\n"
)

const runningJSON = `[{"name":"rest-x","state":{"network":{"eth0":{"addresses":[{"family":"inet","address":"10.0.0.5","scope":"global"}]}}}}]`

func configYAML(baseImage string) string {
	base := ""
	if baseImage != "" {
		base = "  volatile.base_image: " + baseImage + "\n"
	}
	return "architecture: x86_64\nconfig:\n  image.os: Debian\n  limits.cpu: \"2\"\n  limits.memory: 1GB\n" + base +
		"  volatile.eth0.hwaddr: 00:16:3e:aa:bb:cc\n" +
		"devices:\n  data:\n    path: /app/.data\n    pool: default\n    source: rest-x-data\n    type: disk\n" +
		"ephemeral: false\nprofiles:\n- default\n- base\n- service\nstateful: false\n"
}

func baseRules(baseImage string) []rule {
	return []rule{
		{match: "incus image alias list", out: `[{"name":"app:v2","target":"` + newFP + `"}]`},
		{match: "incus config show", out: configYAML(baseImage)},
		{match: "incus file pull", out: envTxt},
		{match: "incus list", out: runningJSON},
	}
}

func newMgr(exec *scriptExec, gate func(context.Context, string, config.HealthCheckConfig) error) *InstanceManager {
	m := NewInstanceManager(exec)
	if gate != nil {
		m.healthGate = gate
	}
	return m
}

func TestUpdateHappyPathCarriesConfigAndSnapshotsFirst(t *testing.T) {
	ex := &scriptExec{rules: baseRules(oldFP)}
	err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{Service: "platform"})
	if err != nil {
		t.Fatal(err)
	}
	snap, del, launch := ex.index("incus storage volume snapshot create 'default' 'rest-x-data' 'pre-update-"), ex.index("incus delete"), ex.index("incus launch")
	if snap < 0 || !(snap < del && del < launch) {
		t.Fatalf("want snapshot < delete < launch, got %d %d %d\n%v", snap, del, launch, ex.cmds)
	}
	l := ex.cmds[launch]
	for _, want := range []string{newFP, "--profile 'base'", "--profile 'service'", "--config 'limits.cpu=2'", "--config 'limits.memory=1GB'"} {
		if !strings.Contains(l, want) {
			t.Errorf("launch missing %q: %s", want, l)
		}
	}
	for _, bad := range []string{"volatile.", "image.os", "docker:"} {
		if strings.Contains(l, bad) {
			t.Errorf("launch must not replay %q: %s", bad, l)
		}
	}
	dev := ex.index("incus config device add 'rest-x' 'data' 'disk'")
	if dev < 0 || !strings.Contains(ex.cmds[dev], "'source=rest-x-data'") || !strings.Contains(ex.cmds[dev], "'path=/app/.data'") {
		t.Fatalf("data volume not re-attached: %v", ex.cmds)
	}
	push := ex.index("incus file push")
	if push < dev || !strings.Contains(ex.cmds[push], base64.StdEncoding.EncodeToString([]byte(envTxt))) {
		t.Fatalf("env file must be restored byte-for-byte after the device: %v", ex.cmds)
	}
	if ex.index("systemctl restart 'platform'") < push {
		t.Errorf("service must restart after the env file is restored")
	}
}

func TestUpdateAbortsUntouchedWhenSnapshotFails(t *testing.T) {
	ex := &scriptExec{rules: append([]rule{{match: "incus storage volume snapshot", err: errors.New("no space left")}}, baseRules(oldFP)...)}
	err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("expected a snapshot abort, got %v", err)
	}
	if ex.index("incus delete") >= 0 || ex.index("incus launch") >= 0 || ex.index("incus stop") >= 0 {
		t.Fatalf("a failed snapshot must not be followed by any change: %v", ex.cmds)
	}
}

func TestUpdateSkipSnapshotIsExplicit(t *testing.T) {
	ex := &scriptExec{rules: baseRules(oldFP)}
	if err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{SkipSnapshot: true}); err != nil {
		t.Fatal(err)
	}
	if ex.index("volume snapshot") >= 0 {
		t.Fatalf("SkipSnapshot must not snapshot: %v", ex.cmds)
	}
}

func TestUpdateRollsBackToPreviousImageOnUnhealthy(t *testing.T) {
	ex := &scriptExec{rules: baseRules(oldFP)}
	calls := 0
	gate := func(context.Context, string, config.HealthCheckConfig) error {
		calls++
		if calls == 1 {
			return errors.New("healthcheck failed")
		}
		return nil
	}
	hc := config.HealthCheckConfig{Path: "/health", Port: 8787}
	err := newMgr(ex, gate).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{HealthCheck: hc})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected a rolled-back error (non-zero exit for CI), got %v", err)
	}
	if ex.count("incus launch") != 2 {
		t.Fatalf("want a launch of the new and then the previous image: %v", ex.cmds)
	}
	first, second := ex.cmds[ex.index("incus launch")], ex.cmds[len(ex.cmds)-1-lastLaunchFromEnd(ex)]
	if !strings.Contains(first, newFP) || !strings.Contains(second, oldFP) {
		t.Fatalf("rollback must relaunch the previous image %s, got %q then %q", oldFP, first, second)
	}
	if ex.count("incus config device add 'rest-x' 'data'") != 2 || ex.count("incus file push") != 2 {
		t.Fatalf("rollback must restore devices and env too: %v", ex.cmds)
	}
}

// lastLaunchFromEnd returns how many commands follow the last "incus launch".
func lastLaunchFromEnd(s *scriptExec) int {
	for i := len(s.cmds) - 1; i >= 0; i-- {
		if strings.Contains(s.cmds[i], "incus launch") {
			return len(s.cmds) - 1 - i
		}
	}
	return -1
}

func TestUpdateReportsFailedRollbackAndUnknownPreviousImage(t *testing.T) {
	always := func(context.Context, string, config.HealthCheckConfig) error { return errors.New("unhealthy") }
	hc := config.HealthCheckConfig{Path: "/health", Port: 8787}

	ex := &scriptExec{rules: baseRules(oldFP)}
	err := newMgr(ex, always).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{HealthCheck: hc})
	if err == nil || !strings.Contains(err.Error(), "rollback ALSO failed") {
		t.Fatalf("got %v", err)
	}

	ex = &scriptExec{rules: baseRules("")}
	err = newMgr(ex, always).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{HealthCheck: hc})
	if err == nil || !strings.Contains(err.Error(), "previous image is unknown") || ex.count("incus launch") != 1 {
		t.Fatalf("got %v (launches=%d)", err, ex.count("incus launch"))
	}
}

func TestUpdateWithoutEnvFileStillWorks(t *testing.T) {
	rules := append([]rule{{match: "incus file pull", err: errors.New("Error: file not found")}}, baseRules(oldFP)...)
	ex := &scriptExec{rules: rules}
	if err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ex.index("incus file push") >= 0 {
		t.Fatalf("no env file existed, so none may be written: %v", ex.cmds)
	}
}

func TestUpdateRefusesWhenEnvCannotBeReadForAnyOtherReason(t *testing.T) {
	// A transient failure must never be mistaken for "no env file to carry over".
	rules := append([]rule{{match: "incus file pull", err: errors.New("connection refused")}}, baseRules(oldFP)...)
	ex := &scriptExec{rules: rules}
	if err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{}); err == nil {
		t.Fatal("expected an error")
	}
	if ex.index("incus delete") >= 0 || ex.index("volume snapshot") >= 0 {
		t.Fatalf("nothing may change when state capture fails: %v", ex.cmds)
	}
}

func TestUpdateRejectsUnsafeNamesAndUnknownImages(t *testing.T) {
	ex := &scriptExec{}
	if err := newMgr(ex, nil).Update(context.Background(), "x; rm -rf /", "app:v2", UpdateOptions{}); err == nil || len(ex.cmds) != 0 {
		t.Fatalf("unsafe name must be rejected before running anything: %v %v", err, ex.cmds)
	}
	if err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{Service: "a b"}); err == nil || len(ex.cmds) != 0 {
		t.Fatalf("unsafe service must be rejected: %v", err)
	}
	ex = &scriptExec{rules: []rule{{match: "incus image alias list", out: `[]`}}}
	err := newMgr(ex, nil).Update(context.Background(), "rest-x", "missing:v9", UpdateOptions{})
	if err == nil || ex.index("incus delete") >= 0 || ex.index("incus launch") >= 0 {
		t.Fatalf("an unresolved alias must fail before any change (and never be pulled from Docker Hub): %v %v", err, ex.cmds)
	}
}

func TestUpdateFallsBackToDeviceOverride(t *testing.T) {
	rules := append([]rule{{match: "incus config device add", err: errors.New("The device already exists")}}, baseRules(oldFP)...)
	ex := &scriptExec{rules: rules}
	if err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ex.index("incus config device override 'rest-x' 'data'") < 0 {
		t.Fatalf("a device provided by a profile must be overridden, not re-added: %v", ex.cmds)
	}
}

// mutatingUpdateCmds are the commands that change an instance or its data.
func mutating(ex *scriptExec) int {
	n := 0
	for _, p := range []string{"incus launch", "incus delete", "incus stop", "incus file push", "incus storage volume snapshot", "incus config device", "incus exec"} {
		n += ex.count(p)
	}
	return n
}

func TestUpdateToTheImageItAlreadyRunsIsANoOp(t *testing.T) {
	// The alias resolves to newFP and the container's base image is newFP.
	yaml := strings.Replace(configYAML(newFP), "  limits.cpu: \"2\"\n", "  limits.cpu: \"2\"\n  user.native-ops.image: app:v2\n", 1)
	rules := []rule{
		{match: "incus image alias list", out: `[{"name":"app:v2","target":"` + newFP + `"}]`},
		{match: "incus config show", out: yaml},
		{match: "incus file pull", out: envTxt},
		{match: "incus list", out: runningJSON},
	}
	ex := &scriptExec{rules: rules}
	healthy := func(context.Context, string, config.HealthCheckConfig) error { return nil }
	if err := newMgr(ex, healthy).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{Service: "platform", HealthCheck: config.HealthCheckConfig{Path: "/health", Port: 8787}}); err != nil {
		t.Fatal(err)
	}
	if n := mutating(ex) + ex.count("incus config set"); n != 0 {
		t.Fatalf("an instance already at the requested image must not be touched: %v", ex.cmds)
	}
}

func TestUpdateNoOpRecordsTheReferenceOnceAndForceStillReplaces(t *testing.T) {
	// Running newFP but launched before references were recorded: adopt it, do not replace.
	ex := &scriptExec{rules: baseRules(newFP)}
	if err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if ex.count("incus config set 'rest-x' 'user.native-ops.image=app:v2'") != 1 || ex.count("incus delete") != 0 || ex.count("incus launch") != 0 {
		t.Fatalf("want exactly one metadata write and no replacement: %v", ex.cmds)
	}

	ex = &scriptExec{rules: baseRules(newFP)}
	if err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if ex.count("incus launch") != 1 {
		t.Fatalf("Force must replace even when already current: %v", ex.cmds)
	}
}

func TestUpdateNoOpWithUnhealthyInstanceFailsInsteadOfReplacing(t *testing.T) {
	ex := &scriptExec{rules: baseRules(newFP)}
	gate := func(context.Context, string, config.HealthCheckConfig) error { return errors.New("503") }
	err := newMgr(ex, gate).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{HealthCheck: config.HealthCheckConfig{Path: "/health", Port: 8787}})
	if err == nil || !strings.Contains(err.Error(), "not healthy, not replacing") {
		t.Fatalf("got %v", err)
	}
	if ex.count("incus delete") != 0 || ex.count("incus launch") != 0 {
		t.Fatalf("a current-but-unhealthy instance must be reported, not replaced: %v", ex.cmds)
	}
}

func TestUpdateRecordsTheNewReferenceOnTheReplacementAndKeepsTheOldOneOnRollback(t *testing.T) {
	ex := &scriptExec{rules: baseRules(oldFP)}
	if err := newMgr(ex, nil).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if l := ex.cmds[ex.index("incus launch")]; !strings.Contains(l, "--config 'user.native-ops.image=app:v2'") {
		t.Fatalf("the replacement must record the reference it was deployed from: %s", l)
	}

	yaml := strings.Replace(configYAML(oldFP), "  limits.cpu: \"2\"\n", "  limits.cpu: \"2\"\n  user.native-ops.image: app:v1\n", 1)
	rules := append([]rule{{match: "incus config show", out: yaml}}, baseRules(oldFP)...)
	ex = &scriptExec{rules: rules}
	gate := func(_ context.Context, _ string, _ config.HealthCheckConfig) error {
		if ex.count("incus launch") == 1 { // first launch is the new image
			return errors.New("503")
		}
		return nil
	}
	_ = newMgr(ex, gate).Update(context.Background(), "rest-x", "app:v2", UpdateOptions{HealthCheck: config.HealthCheckConfig{Path: "/h", Port: 1}})
	launches := pick(ex.cmds, ex.idx("incus launch"))
	if len(launches) != 2 || !strings.Contains(launches[1], "user.native-ops.image=app:v1") || strings.Contains(launches[1], "app:v2") {
		t.Fatalf("a rollback must restore the previous recorded reference: %v", launches)
	}
}
