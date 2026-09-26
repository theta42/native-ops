package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeIncus puts a stand-in `incus` first on PATH. It answers the read commands plan uses from
// canned data and appends every invocation to a log, so a test can see exactly what plan ran.
func fakeIncus(t *testing.T, edgeExists, webExists bool) (logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	edge, web := "[]", "[]"
	if edgeExists {
		edge = `[{"name":"edge"}]`
	}
	if webExists {
		web = `[{"name":"web","state":{"network":{"eth0":{"addresses":[{"family":"inet","address":"10.0.100.5","scope":"global"}]}}}}]`
	}
	script := `#!/bin/bash
echo "$*" >> ` + logFile + `
case "$*" in
  "image alias list --format json"|"image list --format json") echo '[]' ;;
  "list edge --format json") echo '` + edge + `' ;;
  "list web --format json") echo '` + web + `' ;;
  "config show web") printf 'config:\n  limits.cpu: "1"\n  user.native-ops.image: docker:nginx:1\n  volatile.base_image: ` + strings.Repeat("c", 64) + `\nprofiles:\n- default\n- base\n- service\ndevices: {}\n' ;;
  "storage volume show default web-data") echo "Error: Storage volume not found" >&2; exit 1 ;;
  "file pull edge/etc/caddy/Caddyfile -") echo 'import /etc/caddy/sites/*.caddy' ;;
  "file pull edge/etc/caddy/sites/web.caddy -") echo "Error: Path not found" >&2; exit 1 ;;
  *) echo "fake incus: unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return logFile
}

func confDir(t *testing.T, service string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "services", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "fleet.yml"), []byte("name: test\ndomain: \"\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "services", "web", "service.yml"), []byte(service), 0o644)
	return dir
}

const freshService = `name: web
image: nginx:1
volumes:
  - {name: web-data, path: /data, pool: default}
limits: {limits.cpu: "1"}
routing: {domain: web.example.com, upstream_port: 80}
`

func plan(t *testing.T, dir string, extra ...string) (code int, out, errOut string) {
	t.Helper()
	var so, se bytes.Buffer
	code = runPlan(context.Background(), append([]string{"--config-dir", dir}, extra...), &so, &se)
	return code, so.String(), se.String()
}

func TestPlanExitsTwoWhenChangesArePendingAndChangesNothing(t *testing.T) {
	logFile := fakeIncus(t, true, false)
	code, out, errOut := plan(t, confDir(t, freshService))
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (changes pending)\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, want := range []string{"Nothing has been changed", "+ web (create)", "create-instance", "create-volume", "publish-route", "Plan: 1 to create"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	calls, _ := os.ReadFile(logFile)
	for _, verb := range []string{"launch", "delete", "stop", "start", "config set", "config device", "file push", "volume create", "snapshot", "exec", "reload"} {
		if strings.Contains(string(calls), verb) {
			t.Errorf("plan ran %q against the host:\n%s", verb, calls)
		}
	}
}

func TestPlanExitsZeroWhenNothingWouldChange(t *testing.T) {
	fakeIncus(t, true, true)
	code, out, errOut := plan(t, confDir(t, "name: web\nimage: nginx:1\nlimits: {limits.cpu: \"1\"}\n"))
	if code != 0 || !strings.Contains(out, "Plan: 0 to create, 0 to update, 1 unchanged, 0 blocked.") {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
}

func TestPlanExitsOneWhenApplyWouldFail(t *testing.T) {
	fakeIncus(t, false, false) // no edge instance to publish the route to
	code, out, _ := plan(t, confDir(t, freshService))
	if code != 1 || !strings.Contains(out, "BLOCKED") || !strings.Contains(out, "edge") {
		t.Fatalf("exit = %d, want 1 with a BLOCKED line naming the edge\n%s", code, out)
	}
}

func TestPlanExitsOneWhenTheHostCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "incus"), []byte("#!/bin/bash\necho 'boom' >&2\nexit 1\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	code, out, errOut := plan(t, confDir(t, freshService))
	if code != 1 || !strings.Contains(errOut, "Could not plan service web") {
		t.Fatalf("a host that cannot be read must not look like an empty plan: exit=%d\n%s\n%s", code, out, errOut)
	}
}

func TestPlanJSONIsMachineReadable(t *testing.T) {
	fakeIncus(t, true, false)
	code, out, _ := plan(t, confDir(t, freshService), "--json")
	var fp struct {
		Services []struct {
			Service string `json:"service"`
			Action  string `json:"action"`
			Changes []struct {
				Kind string `json:"kind"`
			} `json:"changes"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &fp); err != nil || code != 2 {
		t.Fatalf("exit=%d err=%v\n%s", code, err, out)
	}
	if len(fp.Services) != 1 || fp.Services[0].Service != "web" || fp.Services[0].Action != "create" || len(fp.Services[0].Changes) == 0 {
		t.Fatalf("got %+v", fp)
	}
}

func TestPlanOfOneServiceAndOfNoServices(t *testing.T) {
	fakeIncus(t, true, false)
	dir := confDir(t, freshService)
	if code, out, _ := plan(t, dir, "--service", "other"); code != 0 || !strings.Contains(out, "0 to create") {
		t.Fatalf("no matching service is an empty plan, exit=%d\n%s", code, out)
	}
	if code, _, errOut := plan(t, t.TempDir()); code != 1 || !strings.Contains(errOut, "fleet config") {
		t.Fatalf("a directory with no fleet.yml is an error, exit=%d %s", code, errOut)
	}
}

func TestLoadServicesIsSharedByApplyAndPlan(t *testing.T) {
	dir := confDir(t, freshService)
	os.MkdirAll(filepath.Join(dir, "services", "aaa"), 0o755)
	os.WriteFile(filepath.Join(dir, "services", "aaa", "service.yml"), []byte("name: aaa\nimage: x\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "services", "no-manifest"), 0o755)
	svcs, err := loadServices(dir, "")
	if err != nil || len(svcs) != 2 || svcs[0].Name != "aaa" || svcs[1].Name != "web" {
		t.Fatalf("got %v %v", svcs, err)
	}
	os.WriteFile(filepath.Join(dir, "services", "aaa", "service.yml"), []byte("name: [broken"), 0o644)
	if _, err := loadServices(dir, ""); err == nil {
		t.Fatal("a manifest that does not parse must be an error before anything is applied")
	}
}
