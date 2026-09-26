package incus

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestParseRenderMergeEnv(t *testing.T) {
	live := ParseEnv("# comment\nA=1\nB=\"two words\"\nC='x'\n\nBAD LINE\nD=a=b\n")
	want := map[string]string{"A": "1", "B": "two words", "C": "x", "D": "a=b"}
	if len(live) != len(want) {
		t.Fatalf("got %v", live)
	}
	for k, v := range want {
		if live[k] != v {
			t.Errorf("%s = %q, want %q", k, live[k], v)
		}
	}

	merged, changed := MergeEnv(live, map[string]string{"A": "1", "B": "two words"})
	if changed || len(merged) != 4 {
		t.Fatalf("declared values that already match must not count as a change: %v %v", merged, changed)
	}
	merged, changed = MergeEnv(live, map[string]string{"A": "9", "NEW": "n"})
	if !changed || merged["A"] != "9" || merged["NEW"] != "n" || merged["D"] != "a=b" {
		t.Fatalf("declared keys converge and undeclared keys survive: %v", merged)
	}
}

func TestRenderEnvIsDeterministic(t *testing.T) {
	env := map[string]string{"Z": "1", "A": "2", "M": "3", "B": "4", "Q": "5"}
	first := RenderEnv("svc", env)
	for i := 0; i < 50; i++ {
		if RenderEnv("svc", env) != first {
			t.Fatal("RenderEnv must produce identical bytes for identical input")
		}
	}
	if !strings.HasPrefix(first, "# Managed by native-ops for svc\nA=2\nB=4\nM=3\nQ=5\nZ=1\n") {
		t.Fatalf("keys must be sorted: %q", first)
	}
	// What we write is what we read back.
	if back := ParseEnv(first); back["Z"] != "1" || len(back) != 5 {
		t.Fatalf("round trip failed: %v", back)
	}
}

func TestWriteEnvironmentFileUsesFilePushWithOwnershipAndSortedContent(t *testing.T) {
	ex := &stubExec{}
	c := NewClient(ex)
	if err := c.WriteEnvironmentFile(context.Background(), "web", "svc", map[string]string{"B": "2", "A": "1"}); err != nil {
		t.Fatal(err)
	}
	cmd := ex.cmds[0]
	for _, want := range []string{"incus file push --create-dirs --uid 0 --gid 0 --mode '0600' - 'web/etc/default/svc'"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in %s", want, cmd)
		}
	}
	m := regexp.MustCompile(`printf %s '([^']+)'`).FindStringSubmatch(cmd)
	if m == nil {
		t.Fatalf("no payload in %s", cmd)
	}
	raw, _ := base64.StdEncoding.DecodeString(m[1])
	if string(raw) != "# Managed by native-ops for svc\nA=1\nB=2\n" {
		t.Fatalf("payload = %q", raw)
	}
	if err := c.WriteEnvironmentFile(context.Background(), "web", "svc;rm", nil); err == nil {
		t.Error("unsafe service names must be rejected")
	}
}

func TestLaunchContainerQuotesEveryArgumentAndSortsConfig(t *testing.T) {
	ex := &stubExec{}
	c := NewClient(ex)
	cfg := map[string]string{"user.z": "last", "limits.cpu": "2", "user.note": "it's a \"test\" $(id)"}
	if err := c.LaunchContainer(context.Background(), strings.Repeat("a", 64), "rest-x", []string{"default", "base"}, cfg); err != nil {
		t.Fatal(err)
	}
	var launch string
	for _, cmd := range ex.cmds {
		if strings.HasPrefix(cmd, "incus launch") {
			launch = cmd
		}
	}
	want := `incus launch '` + strings.Repeat("a", 64) + `' 'rest-x' --profile 'default' --profile 'base' --config 'limits.cpu=2' --config 'user.note=it'\''s a "test" $(id)' --config 'user.z=last'`
	if launch != want {
		t.Fatalf("\n got %s\nwant %s", launch, want)
	}
	if err := c.LaunchContainer(context.Background(), "img", "bad name;x", nil, nil); err == nil {
		t.Error("unsafe instance names must be rejected")
	}
}

const attachedYAML = "devices:\n  app-.data:\n    path: /app/.data\n    pool: default\n    source: vol1\n    type: disk\nprofiles:\n- default\n"

func TestEnsureVolumeAttachedIsIdempotent(t *testing.T) {
	ctx := context.Background()

	ex := &stubExec{out: attachedYAML}
	if err := NewClient(ex).EnsureVolumeAttached(ctx, "web", "default", "vol1", "/app/.data", true); err != nil {
		t.Fatal(err)
	}
	if len(ex.cmds) != 1 || strings.Contains(ex.cmds[0], "device add") {
		t.Fatalf("an attached volume must not be attached again: %v", ex.cmds)
	}

	ex = &stubExec{out: "devices: {}\nprofiles:\n- default\n"}
	if err := NewClient(ex).EnsureVolumeAttached(ctx, "web", "default", "vol1", "/app/.data", true); err != nil {
		t.Fatal(err)
	}
	if len(ex.cmds) != 2 || !strings.Contains(ex.cmds[1], "incus config device add 'web' 'app-.data' disk 'pool=default' 'source=vol1' 'path=/app/.data'") {
		t.Fatalf("a missing volume is attached exactly once: %v", ex.cmds)
	}

	// Same device name, different volume: refuse rather than swap data underneath the service.
	ex = &stubExec{out: attachedYAML}
	err := NewClient(ex).EnsureVolumeAttached(ctx, "web", "default", "other-vol", "/app/.data", true)
	if err == nil || !strings.Contains(err.Error(), "does not match") || len(ex.cmds) != 1 {
		t.Fatalf("got %v / %v", err, ex.cmds)
	}
}

func TestNetworkSetIfChangedChecksBeforeSetting(t *testing.T) {
	cmd := NetworkSetIfChanged("incusbr0", "ipv4.address", "10.0.100.1/24")
	if !strings.Contains(cmd, `incus network get 'incusbr0' 'ipv4.address'`) || !strings.Contains(cmd, `|| incus network set 'incusbr0' 'ipv4.address=10.0.100.1/24'`) {
		t.Fatalf("got %s", cmd)
	}
}

func TestGlobalIPv4sIsSingleShotAndSorted(t *testing.T) {
	ex := &stubExec{out: `[{"name":"web","state":{"network":{"lo":{"addresses":[{"family":"inet","address":"127.0.0.1","scope":"local"}]},"eth0":{"addresses":[{"family":"inet","address":"10.0.100.50","scope":"global"},{"family":"inet6","address":"fe80::1","scope":"link"},{"family":"inet","address":"10.0.100.21","scope":"global"}]}}}}]`}
	ips, err := NewClient(ex).GlobalIPv4s(context.Background(), "web")
	if err != nil || len(ips) != 2 || ips[0] != "10.0.100.21" || ips[1] != "10.0.100.50" {
		t.Fatalf("got %v %v", ips, err)
	}
	if len(ex.cmds) != 1 {
		t.Fatalf("reading addresses must not change the container: %v", ex.cmds)
	}
}

// Regression, found on a real host: a profile list that already named "default"
// (as the live state of every instance does) launched WITHOUT the default
// profile, so the replacement had no root disk and the update could not relaunch anything.
func TestLaunchContainerAlwaysIncludesTheDefaultProfileInTheRightPlace(t *testing.T) {
	launch := func(profiles []string) string {
		ex := &stubExec{}
		if err := NewClient(ex).LaunchContainer(context.Background(), strings.Repeat("a", 64), "web", profiles, nil); err != nil {
			t.Fatal(err)
		}
		for _, c := range ex.cmds {
			if strings.HasPrefix(c, "incus launch") {
				return c
			}
		}
		t.Fatal("no launch command")
		return ""
	}
	img := `incus launch '` + strings.Repeat("a", 64) + `' 'web'`
	for name, tc := range map[string]struct {
		profiles []string
		want     string
	}{
		"manifest without default":      {[]string{"base", "service"}, img + " --profile 'default' --profile 'base' --profile 'service'"},
		"live state that lists default": {[]string{"default", "base", "service"}, img + " --profile 'default' --profile 'base' --profile 'service'"},
		"default listed last is kept":   {[]string{"base", "default"}, img + " --profile 'base' --profile 'default'"},
		"no profiles at all":            {nil, img + " --profile 'default'"},
	} {
		if got := launch(tc.profiles); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, tc.want)
		}
	}
}

const twoAddrJSON = `[{"name":"web","state":{"network":{"eth0":{"addresses":[{"family":"inet","address":"10.0.100.50","scope":"global"},{"family":"inet","address":"10.0.100.21","scope":"global"}]}}}}]`

func TestGetContainerIPOnlyReadsAndIsDeterministic(t *testing.T) {
	ex := &stubExec{out: twoAddrJSON}
	ip, err := NewClient(ex).GetContainerIP(context.Background(), "web")
	if err != nil || ip != "10.0.100.21" {
		t.Fatalf("got %q, %v", ip, err)
	}
	for _, cmd := range ex.cmds {
		if !strings.HasPrefix(cmd, "incus list") {
			t.Fatalf("looking up an address must not change the container (it used to push a static IP, a route and resolv.conf into it): %s", cmd)
		}
	}
}

func TestGetContainerIPExplainsAMissingLeaseInsteadOfWorkingAroundIt(t *testing.T) {
	old := containerIPTimeout
	containerIPTimeout = 50 * time.Millisecond
	defer func() { containerIPTimeout = old }()
	ex := &stubExec{out: `[{"name":"web","state":{"network":{}}}]`}
	_, err := NewClient(ex).GetContainerIP(context.Background(), "web")
	if err == nil || !strings.Contains(err.Error(), "DHCP") {
		t.Fatalf("got %v", err)
	}
	for _, cmd := range ex.cmds {
		if strings.Contains(cmd, "ip addr") || strings.Contains(cmd, "resolv.conf") || strings.Contains(cmd, "ip route") {
			t.Fatalf("no in-container networking hacks: %s", cmd)
		}
	}
}

// Found on a real host: right after `incus launch` systemd is not up yet, so an immediate
// `systemctl restart` failed with "Failed to connect to system scope bus". The update failed
// on it, and so did its rollback.
func TestRestartServiceWaitsForSystemdInAFreshContainer(t *testing.T) {
	old := systemdPoll
	systemdPoll = time.Millisecond
	defer func() { systemdPoll = old }()
	attempts := 0
	ex := &fnExec{fn: func(cmd string) (string, error) {
		if strings.Contains(cmd, "is-system-running") {
			if attempts++; attempts < 4 {
				return "", fmt.Errorf("command failed: %s (stderr: Failed to connect to system scope bus via local transport: No such file or directory): exit status 1", cmd)
			}
			return "starting\n", fmt.Errorf("exit status 1") // up, still booting: is-system-running exits non-zero
		}
		return "", nil
	}}
	if err := NewClient(ex).RestartService(context.Background(), "web", "svc"); err != nil {
		t.Fatal(err)
	}
	restart := -1
	for i, c := range ex.cmds {
		if strings.Contains(c, "systemctl restart 'svc'") {
			restart = i
		}
	}
	if attempts != 4 || restart != 4 {
		t.Fatalf("the restart must come only after systemd answers (attempts=%d, restart at %d): %v", attempts, restart, ex.cmds)
	}
}

func TestWaitSystemdGivesUpAndFailsFastWithoutSystemctl(t *testing.T) {
	oldW, oldP := systemdWait, systemdPoll
	systemdWait, systemdPoll = 30*time.Millisecond, time.Millisecond
	defer func() { systemdWait, systemdPoll = oldW, oldP }()

	ex := &fnExec{fn: func(cmd string) (string, error) {
		return "", fmt.Errorf("command failed: %s (stderr: Failed to connect to system scope bus): exit status 1", cmd)
	}}
	err := NewClient(ex).RestartService(context.Background(), "web", "svc")
	if err == nil || !strings.Contains(err.Error(), "did not come up") {
		t.Fatalf("got %v", err)
	}
	for _, c := range ex.cmds {
		if strings.Contains(c, "systemctl restart") {
			t.Fatal("must not try to restart a service while systemd is not answering")
		}
	}

	ex = &fnExec{fn: func(cmd string) (string, error) { return "", fmt.Errorf("Error: Command not found: exit status 127") }}
	systemdWait = time.Hour
	if err := NewClient(ex).RestartService(context.Background(), "web", "svc"); err == nil || !strings.Contains(err.Error(), "no systemctl") || len(ex.cmds) != 1 {
		t.Fatalf("a container without systemctl must fail at once, not after the timeout: %v (%d attempts)", err, len(ex.cmds))
	}
}
