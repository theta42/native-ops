package status

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/remote"
)

const instancesJSON = `[
 {"name":"outline","status":"Running","type":"container","created_at":"2026-09-21T10:00:00Z","profiles":["base"],
  "config":{"image.description":"docker.io/outlinewiki/outline (OCI)","volatile.base_image":"ec66a97cbb5d0000","limits.cpu":"2","limits.memory":"1GB",
            "environment.SECRET_KEY":"SENTINEL-AAA-1","environment.DATABASE_URL":"SENTINEL-BBB-2","oci.entrypoint":"/bin/run",
            "user.user-data":"SENTINEL-CCC-3","user.native-ops.image":"docker:outline:1.10","volatile.eth0.hwaddr":"00:16:3e:aa"},
  "devices":{"data":{"type":"disk","pool":"default","source":"outline-data","path":"/var/lib/outline/data"}},
  "snapshots":[{},{}],
  "state":{"network":{"lo":{"addresses":[{"family":"inet","address":"127.0.0.1","scope":"local"}]},"eth0":{"addresses":[{"family":"inet","address":"10.0.100.50","scope":"global"},{"family":"inet6","address":"fe80::1","scope":"link"},{"family":"inet","address":"10.0.100.21","scope":"global"}]}}}},
 {"name":"manager","status":"Running","type":"container","created_at":"2026-09-25T10:00:00Z","profiles":["base","service"],
  "config":{"image.description":"Debian trixie amd64","volatile.base_image":"a4f08efe82ee1111"},
  "devices":{"data":{"type":"disk","source":"/var/lib/incus/storage-pools/default/custom/default_manager-data","path":"/app/.data"},
             "web":{"type":"proxy","listen":"tcp:0.0.0.0:80","connect":"tcp:127.0.0.1:80"}},
  "snapshots":[],"state":null},
 {"name":"old","status":"Stopped","type":"container","created_at":"2026-09-01T10:00:00Z","profiles":["base"],"config":{},"devices":{},"snapshots":[],"state":null}
]`

func TestParseInstancesNeverEmitsConfigValues(t *testing.T) {
	ins, err := ParseInstances(instancesJSON)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(ins)
	for _, secret := range []string{"SENTINEL-AAA-1", "SENTINEL-BBB-2", "SENTINEL-CCC-3", "00:16:3e"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("a config value leaked into the status output: %q\n%s", secret, blob)
		}
	}
	if ins[0].Name != "manager" || ins[1].Name != "old" || ins[2].Name != "outline" {
		t.Fatalf("instances must be sorted by name: %v", []string{ins[0].Name, ins[1].Name, ins[2].Name})
	}
	o := ins[2]
	if strings.Join(o.EnvKeys, ",") != "DATABASE_URL,SECRET_KEY" {
		t.Errorf("env keys are reported by name only: %v", o.EnvKeys)
	}
	if strings.Join(o.ConfigKeys, ",") != "oci.entrypoint,user.user-data" {
		t.Errorf("other config is reported by key name only: %v", o.ConfigKeys)
	}
	if o.Limits["limits.cpu"] != "2" || o.Recorded["image"] != "docker:outline:1.10" || o.BaseImage != "ec66a97cbb5d" || o.Snapshots != 2 {
		t.Errorf("limits, native-ops bookkeeping and the short base image are kept: %+v", o)
	}
	if strings.Join(o.IPv4, ",") != "10.0.100.21,10.0.100.50" {
		t.Errorf("global IPv4 only, sorted: %v", o.IPv4)
	}
}

func TestParseInstancesDevices(t *testing.T) {
	ins, _ := ParseInstances(instancesJSON)
	m := ins[0]
	if len(m.Devices) != 2 || m.Devices[0].Name != "data" || !m.Devices[0].HostPath || m.Devices[1].Listen != "tcp:0.0.0.0:80" {
		t.Fatalf("a disk with a source but no pool is a host path; a proxy keeps its listen/connect: %+v", m.Devices)
	}
	if ins[2].Devices[0].HostPath || ins[2].Devices[0].Pool != "default" {
		t.Errorf("a pool volume is not a host path: %+v", ins[2].Devices[0])
	}
	if _, err := ParseInstances("not json"); err == nil {
		t.Error("garbage must be an error")
	}
}

func TestParseVolumesAndSnapshots(t *testing.T) {
	vols, err := ParseVolumes("default", `[
	 {"name":"gitea-data","type":"custom","used_by":["/1.0/instances/gitea"],"config":{"security.shifted":"true"}},
	 {"name":"orphan","type":"custom","used_by":null,"config":{}},
	 {"name":"abc","type":"image","used_by":[]},
	 {"name":"c1","type":"container","used_by":[]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 2 || vols[0].Name != "gitea-data" || vols[0].UsedBy[0] != "gitea" || vols[0].Shifted != "true" || vols[1].UsedBy == nil {
		t.Fatalf("only custom volumes, used_by resolved to instance names, never nil: %+v", vols)
	}
	n, latest := ParseSnapshotNames("pre-update-20260101-000000,,2026,,false\ndaily-20260925-060000,,x\ndaily-20260926-060801,,x\n\nprechange-1,,x\n")
	if n != 4 || latest != "daily-20260926-060801" {
		t.Fatalf("count=%d latest=%q", n, latest)
	}
	if n, l := ParseSnapshotNames(""); n != 0 || l != "" {
		t.Fatal("empty means none")
	}
}

func TestParseImages(t *testing.T) {
	s, err := ParseImages(`[{"fingerprint":"aaaaaaaaaaaaaaaa","size":100000000,"uploaded_at":"2026-09-01","aliases":[{"name":"b:latest"},{"name":"a:v1"}]},{"fingerprint":"bbbbbbbbbbbbbbbb","size":50000000,"aliases":[]}]`)
	if err != nil || s.Count != 2 || s.Unaliased != 1 || s.TotalBytes != 150000000 || s.Aliases[0].Name != "a:v1" || s.Aliases[0].Fingerprint != "aaaaaaaaaaaa" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestAnalyzeFlagsWhatAnOperatorNeedsToSee(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	ins, _ := ParseInstances(instancesJSON)
	s := &Snapshot{Instances: ins,
		Volumes: []Volume{
			{Name: "fresh", UsedBy: []string{"x"}, Shifted: "true", LatestDaily: "daily-20260926-060000"},
			{Name: "stale", UsedBy: []string{"x"}, Shifted: "true", LatestDaily: "daily-20260920-060000"},
			{Name: "none", UsedBy: []string{"x"}, Shifted: "true"},
			{Name: "noshift", UsedBy: []string{"x"}, LatestDaily: "daily-20260926-060000"},
			{Name: "unused-noshift", LatestDaily: "daily-20260926-060000"},
		},
		Images: ImageSummary{Count: 60, Unaliased: 40, TotalBytes: 14_000_000_000}}
	joined := strings.Join(Analyze(s, now), "\n")
	for _, want := range []string{"instance old is Stopped", "mounts the host path /var/lib/incus", "volume stale: newest daily snapshot", "volume none has no daily snapshot", "volume noshift does not have security.shifted", "40 unaliased images"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning %q in:\n%s", want, joined)
		}
	}
	for _, not := range []string{"volume fresh", "unused-noshift"} {
		if strings.Contains(joined, not) {
			t.Errorf("must not warn about %q:\n%s", not, joined)
		}
	}
}

type fakeExec struct {
	fail map[string]error
	out  map[string]string
	cmds []string
}

func (f *fakeExec) Run(_ context.Context, cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	for sub, err := range f.fail {
		if strings.Contains(cmd, sub) {
			return "", err
		}
	}
	for sub, o := range f.out {
		if strings.Contains(cmd, sub) {
			return o, nil
		}
	}
	return "", nil
}
func (f *fakeExec) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (f *fakeExec) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (f *fakeExec) Close() error                                                    { return nil }

var _ remote.Executor = (*fakeExec)(nil)

func TestCollectDegradesToWarningsButRequiresTheInstanceList(t *testing.T) {
	ok := map[string]string{"hostname": "node-1\n", "incus --version": "7.4\n", "incus list": instancesJSON,
		"volume list":   `[{"name":"v1","type":"custom","used_by":["/1.0/instances/outline"],"config":{"security.shifted":"true"}}]`,
		"snapshot list": "daily-20260926-060000,,x\n", "image list": `[]`}
	f := &fakeExec{out: ok}
	s, err := Collect(context.Background(), f, "default")
	if err != nil || s.Host != "node-1" || s.Incus != "7.4" || len(s.Instances) != 3 || s.Volumes[0].Snapshots != 1 {
		t.Fatalf("%+v %v", s, err)
	}

	f = &fakeExec{out: ok, fail: map[string]error{"volume list": errors.New("pool gone"), "image list": errors.New("boom")}}
	s, err = Collect(context.Background(), f, "default")
	if err != nil || !strings.Contains(strings.Join(s.Warnings, "|"), "could not list volumes") || !strings.Contains(strings.Join(s.Warnings, "|"), "could not list images") {
		t.Fatalf("optional sections fail soft: %v %v", s.Warnings, err)
	}

	f = &fakeExec{out: ok, fail: map[string]error{"incus list": errors.New("no incus")}}
	if _, err := Collect(context.Background(), f, "default"); err == nil {
		t.Fatal("no instance list means no status")
	}
	if _, err := Collect(context.Background(), &fakeExec{}, "de fault;x"); err == nil {
		t.Fatal("an unsafe pool name must be rejected before any command runs")
	}
}

func TestCollectQuotesEverythingItInterpolates(t *testing.T) {
	f := &fakeExec{out: map[string]string{"incus list": `[]`, "volume list": `[{"name":"good-vol","type":"custom"},{"name":"bad;vol","type":"custom"}]`}}
	_, _ = Collect(context.Background(), f, "default")
	for _, c := range f.cmds {
		if strings.Contains(c, "bad;vol") {
			t.Fatalf("a volume name that is not safe must never reach a shell: %s", c)
		}
	}
}
