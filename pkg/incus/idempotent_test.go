package incus

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/config"
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
	if strings.Contains(cmd, "A=1") || strings.Contains(cmd, "printf") {
		t.Fatalf("the content must go over stdin, never in the command line: %s", cmd)
	}
	if len(ex.stdins) != 1 || ex.stdins[0] != "# Managed by native-ops for svc\nA=1\nB=2\n" {
		t.Fatalf("payload = %q", ex.stdins)
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
	if err == nil || !strings.Contains(err.Error(), "would hide that data") || len(ex.cmds) != 1 {
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

func TestValidUserName(t *testing.T) {
	for _, s := range []string{"platform", "www-data", "_apt", "a", strings.Repeat("a", 32)} {
		if !ValidUserName(s) {
			t.Errorf("%q should be a valid user name", s)
		}
	}
	for _, s := range []string{"Root", "1abc", "-x", "has space", "a;b", "", strings.Repeat("a", 33), "a/b"} {
		if ValidUserName(s) {
			t.Errorf("%q should not be a valid user name", s)
		}
	}
}

// seqExec answers Run calls one at a time from a queue, so a test can script a read followed by a
// conditional write, the way EnsurePathOwner uses its executor.
type seqExec struct {
	cmds  []string
	queue []struct {
		out string
		err error
	}
}

func (s *seqExec) Run(_ context.Context, cmd string) (string, error) {
	s.cmds = append(s.cmds, cmd)
	if len(s.queue) == 0 {
		return "", nil
	}
	r := s.queue[0]
	s.queue = s.queue[1:]
	return r.out, r.err
}
func (s *seqExec) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (s *seqExec) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (s *seqExec) Close() error                                                    { return nil }
func (s *seqExec) give(out string, err error) {
	s.queue = append(s.queue, struct {
		out string
		err error
	}{out, err})
}

func TestEnsurePathOwnerOnlyChownsWhenTheCurrentOwnerDiffers(t *testing.T) {
	ex := &seqExec{}
	c := NewClient(ex)

	// Already owned by platform: a read, no write.
	ex.give("platform\n", nil)
	if err := c.EnsurePathOwner(context.Background(), "demo-multi", "/app/.data", "platform"); err != nil {
		t.Fatal(err)
	}
	if len(ex.cmds) != 1 || !strings.Contains(ex.cmds[0], "stat -c %U") {
		t.Fatalf("expected exactly one read, got %v", ex.cmds)
	}

	// Owned by root (a fresh mount): a read, then exactly the chown asked for.
	ex.cmds = nil
	ex.give("root\n", nil)
	if err := c.EnsurePathOwner(context.Background(), "demo-multi", "/app/.data", "platform"); err != nil {
		t.Fatal(err)
	}
	if len(ex.cmds) != 2 {
		t.Fatalf("expected a read and a write, got %v", ex.cmds)
	}
	want := "incus exec " + ShQuote("demo-multi") + " -- chown " + ShQuote("platform") + " " + ShQuote("/app/.data")
	if ex.cmds[1] != want {
		t.Fatalf("\n got %s\nwant %s", ex.cmds[1], want)
	}

	// The read itself fails: reported, no chown attempted.
	ex.cmds = nil
	ex.give("", fmt.Errorf("no such file"))
	if err := c.EnsurePathOwner(context.Background(), "demo-multi", "/app/.data", "platform"); err == nil {
		t.Fatal("a failed read must be reported")
	}
	if len(ex.cmds) != 1 {
		t.Fatalf("a failed read must not be followed by a chown: %v", ex.cmds)
	}

	// The chown itself fails: reported.
	ex.cmds = nil
	ex.give("root\n", nil)
	ex.give("", fmt.Errorf("permission denied"))
	if err := c.EnsurePathOwner(context.Background(), "demo-multi", "/app/.data", "platform"); err == nil {
		t.Fatal("a failed chown must be reported")
	}
}

func TestEnsurePathOwnerRejectsUnsafeInput(t *testing.T) {
	for name, tc := range map[string]struct{ container, path, owner string }{
		"bad container": {"bad;x", "/app/.data", "platform"},
		"bad owner":     {"demo", "/app/.data", "root; rm -rf /"},
		"relative path": {"demo", "app/.data", "platform"},
		"empty path":    {"demo", "", "platform"},
	} {
		t.Run(name, func(t *testing.T) {
			ex := &stubExec{}
			c := NewClient(ex)
			if err := c.EnsurePathOwner(context.Background(), tc.container, tc.path, tc.owner); err == nil {
				t.Fatal("expected rejection")
			}
			if len(ex.cmds) != 0 {
				t.Fatalf("unsafe input must be rejected before anything runs: %v", ex.cmds)
			}
		})
	}
}

func TestSetEnvKeysReplacesAppendsAndKeepsTheRest(t *testing.T) {
	in := "# Managed by hand\nA=1\nRELEASE_TAG=v1\nB=2\nRELEASE_TAG=dup\n"
	got := SetEnvKeys(in, map[string]string{"RELEASE_TAG": "v2", "Z": "26", "M": "13"})
	want := "# Managed by hand\nA=1\nRELEASE_TAG=v2\nB=2\nM=13\nZ=26\n"
	if got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
	if SetEnvKeys(in, nil) != in {
		t.Fatal("no keys, no change")
	}
	if got := SetEnvKeys("", map[string]string{"K": "v"}); got != "K=v\n" {
		t.Fatalf("an empty file gets the key: %q", got)
	}
}

// The bridge's address is set only on a host with no instances: run the command against a fake incus
// for each case and see whether it set anything.
func TestBridgeAddressIsOnlySetOnAHostWithoutInstances(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cases := []struct {
		name, live, list string
		listFails, sets  bool
	}{
		{name: "already right", live: "10.0.101.1/24", list: "web", sets: false},
		{name: "already right, empty host", live: "10.0.101.1/24", list: "", sets: false},
		{name: "fresh host", live: "10.183.7.1/24", list: "", sets: true},
		{name: "live host", live: "10.0.100.1/24", list: "web\nedge", sets: false},
		{name: "instances unreadable", live: "10.0.100.1/24", listFails: true, sets: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "log")
			listExit := "0"
			if c.listFails {
				listExit = "1"
			}
			fake := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$1 $2\" in\n" +
				"  \"network get\") echo '" + c.live + "' ;;\n" +
				"  \"list --format\") printf '" + c.list + "'; exit " + listExit + " ;;\n" +
				"esac\n"
			if err := os.WriteFile(filepath.Join(dir, "incus"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bash, "-c", BridgeAddressIfUnused("incusbr0", "10.0.101.1/24"))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the command must never fail a bootstrap: %v %s", err, out)
			}
			b, _ := os.ReadFile(log)
			set := strings.Contains(string(b), "network set incusbr0 ipv4.address=10.0.101.1/24")
			if set != c.sets {
				t.Fatalf("set=%v, want %v; incus was called with:\n%s", set, c.sets, b)
			}
		})
	}
}

// A client that was not told the host's bridge leaves the network alone; one that was makes sure of
// that bridge, by the same commands as before for the default one, except that the address is no
// longer forced onto a live bridge.
func TestEnsureProfileMakesSureOfTheConfiguredBridgeOnly(t *testing.T) {
	ctx := context.Background()
	ex := &stubExec{}
	if err := NewClient(ex).EnsureProfile(ctx, "service"); err != nil {
		t.Fatal(err)
	}
	for _, c := range ex.cmds {
		if strings.Contains(c, "network") {
			t.Fatalf("no bridge was set, yet the network was touched: %s", c)
		}
	}

	ex = &stubExec{}
	c := NewClient(ex)
	if err := c.SetBridge(config.NetworkConfig{BridgeName: config.DefaultBridgeName, IPv4CIDR: config.DefaultIPv4CIDR}); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureProfile(ctx, "service"); err != nil {
		t.Fatal(err)
	}
	// The commands before this change, word for word, but for the address line.
	want := []string{
		"incus storage list | grep -q default || incus storage create default dir || true",
		"incus profile device show default | grep -q 'path: /' || incus profile device add default root disk path=/ pool=default || true",
		"incus network show incusbr0 >/dev/null 2>&1 || incus network create incusbr0 || true",
		BridgeAddressIfUnused("incusbr0", "10.0.100.1/24"),
		`[ "$(incus network get 'incusbr0' 'ipv4.nat' 2>/dev/null)" = 'true' ] || incus network set 'incusbr0' 'ipv4.nat=true' || true`,
		`[ "$(incus network get 'incusbr0' 'ipv6.address' 2>/dev/null)" = 'none' ] || incus network set 'incusbr0' 'ipv6.address=none' || true`,
		"incus profile device show default | grep -q 'network: incusbr0' || incus profile device add default eth0 nic network=incusbr0 name=eth0 || true",
		"incus profile show 'service'",
	}
	if strings.Join(ex.cmds, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(ex.cmds, "\n"), strings.Join(want, "\n"))
	}

	ex = &stubExec{}
	c = NewClient(ex)
	if err := c.SetBridge(config.NetworkConfig{BridgeName: "br-inst", IPv4CIDR: "10.0.101.0/24"}); err != nil {
		t.Fatal(err)
	}
	_ = c.EnsureProfile(ctx, "service")
	joined := strings.Join(ex.cmds, "\n")
	if strings.Contains(joined, "incusbr0") || strings.Contains(joined, "10.0.100.") || !strings.Contains(joined, "'ipv4.address=10.0.101.1/24'") || !strings.Contains(joined, "nic network=br-inst") {
		t.Fatalf("a host's own bridge is used throughout:\n%s", joined)
	}
	if err := NewClient(ex).SetBridge(config.NetworkConfig{BridgeName: "incusbr0", IPv4CIDR: "bogus"}); err == nil {
		t.Fatal("a bridge without a usable subnet is refused")
	}
}
