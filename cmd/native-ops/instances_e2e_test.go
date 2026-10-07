package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/engine"
	"github.com/theta42/native-ops/pkg/remote"
	"github.com/theta42/native-ops/pkg/server"
)

// fakeIncusTenants is a stateful stand-in for a host with an edge and a static service (gitea): it keeps
// instances, volumes and the files pushed into them on disk, and logs every call.
func fakeIncusTenants(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	for _, d := range []string{"inst", "vol", "files"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	os.WriteFile(filepath.Join(dir, "inst", "edge"), []byte("template=\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "inst", "gitea"), []byte("template=\n"), 0o644)
	script := `#!/bin/bash
D=` + dir + `
echo "$*" >> $D/calls.log
key() { echo "$1" | tr '/ ' '__'; }
case "$1 $2" in
  "image alias"|"image list") echo '[]'; exit 0 ;;
esac
case "$1" in
  list)
    n="$2"; if [ -f "$D/inst/$n" ]; then echo '[{"name":"'$n'","state":{"network":{"eth0":{"addresses":[{"family":"inet","address":"10.0.100.'$(( $(echo $n | cksum | cut -d" " -f1) % 200 + 20 ))'","scope":"global"}]}}}}]'; else echo '[]'; fi ;;
  config)
    case "$2" in
      show)
        f="$D/inst/$3"; [ -f "$f" ] || { echo "Error: Instance not found" >&2; exit 1; }
        tpl=$(grep '^template=' $f | cut -d= -f2-)
        echo "config:"; [ -n "$tpl" ] && echo "  user.native-ops.template: $tpl"
        echo "  volatile.base_image: $(printf 'c%.0s' $(seq 64))"
        echo "profiles:"; echo "- default"; echo "- base"; echo "devices:"
        grep '^device=' $f | cut -d= -f2- | while IFS='|' read dn pool src path; do echo "  $dn:"; echo "    type: disk"; echo "    pool: $pool"; echo "    source: $src"; echo "    path: $path"; done ;;
      device) # config device add <inst> <name> disk pool=.. source=.. path=..
        shift 3; inst="$1"; dn="$2"; shift 3; pool=default; src=; path=
        for kv in "$@"; do case "$kv" in pool=*) pool=${kv#pool=};; source=*) src=${kv#source=};; path=*) path=${kv#path=};; esac; done
        echo "device=$dn|$pool|$src|$path" >> "$D/inst/$inst" ;;
      set) : ;;
    esac ;;
  launch)
    img="$2"; name="$3"; shift 3; tpl=
    while [ $# -gt 0 ]; do case "$1" in --config) case "$2" in user.native-ops.template=*) tpl=${2#user.native-ops.template=};; esac; shift 2;; *) shift;; esac; done
    echo "template=$tpl" > "$D/inst/$name" ;;
  storage)
    case "$3" in
      show) [ -f "$D/vol/$5" ] || { echo "Error: Storage volume not found" >&2; exit 1; } ;;
      create) touch "$D/vol/$5" ;;
      delete) rm -f "$D/vol/$5" ;;
    esac ;;
  profile) echo "name: p" ;;
  file)
    if [ "$2" = push ]; then dest="${@: -1}"; cat > "$D/files/$(key "$dest")"
    else src="$3"; f="$D/files/$(key "$src")"
      if [ -f "$f" ]; then cat "$f"
      elif [ "$src" = "edge/etc/caddy/Caddyfile" ]; then echo 'import /etc/caddy/sites/*.caddy'
      else echo "Error: Path not found" >&2; exit 1; fi
    fi ;;
  exec)
    inst="$2"; shift 3
    case "$1 $2" in
      "systemctl is-system-running") echo running ;;
      "rm -f") rm -f "$D/files/$(key "edge$3")" ;;
    esac ;;
  stop) : ;;
  delete) rm -f "$D/inst/$2" ;;
  "network get incusbr0 ipv4.address") echo '10.0.100.1/24' ;;
  *) echo "fake incus: unexpected: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "incus"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

func send(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func waitJob(t *testing.T, base, token string, out map[string]any) map[string]any {
	t.Helper()
	id := out["job"].(map[string]any)["id"].(string)
	var job map[string]any
	for i := 0; i < 300; i++ {
		_, job = send(t, "GET", base+"/v1/jobs/"+id, token, nil)
		if job["status"] != "running" {
			return job
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", id)
	return nil
}

// A tenant end to end through the real daemon wiring: a scoped token creates a demo, the host really has
// it (volume, env file where the unit reads it, the route with the allowed snippet), the same call again
// changes nothing, gitea cannot be touched, and deleting with purge takes the tenant and only its volume.
func TestATenantThroughTheDaemonAndTheStaticServicesLeftAlone(t *testing.T) {
	host := fakeIncusTenants(t)
	stateDir := t.TempDir()
	srv, closeFn, err := newDaemon(daemonConfig{StateDir: stateDir, Pool: "default", Exec: remote.NewLocalExecutor(),
		EnableInstances: true, InstancePolicy: engine.InstancePolicy{Profiles: []string{"base", "service"}, RouteImports: []string{"strip-forged-identity"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.WaitForJobs(10 * time.Second); closeFn() })
	store, _ := server.OpenTokenStore(filepath.Join(stateDir, "tokens.json"))
	fm, _, err := store.CreateScoped("fleet-manager", server.RoleDeployer, server.Scope{Names: []string{"demo-*"}, Images: []string{"opsavor-platform:*"}, Domains: []string{"*.opsavor.app"}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	spec := map[string]any{
		"template": "platform", "image": "opsavor-platform:latest", "profiles": []string{"base", "service"},
		"limits":  map[string]string{"limits.cpu": "1", "limits.memory": "1GB"},
		"volumes": []map[string]string{{"name": "demo-multi-data", "path": "/app/.data"}},
		"service": "platform",
		"env":     map[string]string{"PORT": "8787", "OPSAVOR_SEED": "multi", "OPSAVOR_CONTROL_TOKEN": "tenant-secret-1234"},
		"health":  map[string]any{"port": 8787},
		"domain":  "demo-multi.opsavor.app", "route_directives": []string{"import strip-forged-identity"},
	}
	code, out := send(t, "PUT", ts.URL+"/v1/instances/demo-multi", fm, spec)
	if code != 202 {
		t.Fatalf("%d %v", code, out)
	}
	job := waitJob(t, ts.URL, fm, out)
	if job["status"] != "succeeded" {
		t.Fatalf("job: %v", job)
	}
	read := func(rel string) string { b, _ := os.ReadFile(filepath.Join(host, rel)); return string(b) }
	if !strings.Contains(read("inst/demo-multi"), "template=platform") || !strings.Contains(read("inst/demo-multi"), "|demo-multi-data|") {
		t.Fatalf("the instance exists with its volume: %q", read("inst/demo-multi"))
	}
	if !strings.Contains(read("files/demo-multi_etc_default_platform"), "OPSAVOR_SEED=multi") {
		t.Fatalf("the environment is where the unit reads it: %q", read("files/demo-multi_etc_default_platform"))
	}
	site := read("files/edge_etc_caddy_sites_demo-multi.caddy")
	if !strings.HasPrefix(site, "demo-multi.opsavor.app {") || !strings.Contains(site, "import strip-forged-identity") {
		t.Fatalf("the route: %q", site)
	}
	if strings.Contains(read("calls.log"), "tenant-secret-1234") {
		t.Fatal("a tenant secret must not appear in any command line")
	}

	// The same call again converges without changing anything.
	before := read("calls.log")
	code, out = send(t, "PUT", ts.URL+"/v1/instances/demo-multi", fm, spec)
	if job := waitJob(t, ts.URL, fm, out); code != 202 || job["status"] != "succeeded" {
		t.Fatalf("second put: %d %v", code, job)
	}
	for _, l := range strings.Split(strings.TrimPrefix(read("calls.log"), before), "\n") {
		for _, verb := range []string{"launch", "storage volume create", "config device add"} {
			if strings.HasPrefix(l, verb) {
				t.Fatalf("a repeated PUT must not do %q again: %s", verb, l)
			}
		}
	}

	// The static services are out of reach, whatever the spec says.
	spec2 := map[string]any{"template": "platform", "image": "opsavor-platform:latest", "volumes": []map[string]string{{"name": "gitea-data", "path": "/d"}}}
	if code, out := send(t, "PUT", ts.URL+"/v1/instances/gitea", fm, spec2); code != 403 {
		t.Fatalf("gitea is not the fleet manager's: %d %v", code, out)
	}
	if code, _ := send(t, "DELETE", ts.URL+"/v1/instances/gitea", fm, nil); code != 403 {
		t.Fatalf("delete gitea: %d", code)
	}
	if _, err := os.Stat(filepath.Join(host, "inst", "gitea")); err != nil {
		t.Fatal("gitea must still exist")
	}

	// Deleting with purge removes the tenant, its route and its own volume.
	os.WriteFile(filepath.Join(host, "vol", "someone-elses-data"), nil, 0o644)
	code, out = send(t, "DELETE", ts.URL+"/v1/instances/demo-multi?purge_volumes=true", fm, nil)
	if job := waitJob(t, ts.URL, fm, out); code != 202 || job["status"] != "succeeded" {
		t.Fatalf("delete: %d %v", code, job)
	}
	if _, err := os.Stat(filepath.Join(host, "inst", "demo-multi")); err == nil {
		t.Fatal("the instance is gone")
	}
	if _, err := os.Stat(filepath.Join(host, "vol", "demo-multi-data")); err == nil {
		t.Fatal("its volume is purged")
	}
	if _, err := os.Stat(filepath.Join(host, "vol", "someone-elses-data")); err != nil {
		t.Fatal("no other volume is touched")
	}
	if read("files/edge_etc_caddy_sites_demo-multi.caddy") != "" {
		t.Fatal("its route is removed")
	}
}
