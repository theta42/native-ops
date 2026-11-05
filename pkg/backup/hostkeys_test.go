package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/config"
)

func cfgFor(host, prefix string) *config.BackupConfig {
	c := &config.BackupConfig{Endpoint: "https://s3.example.com", Region: "r", Bucket: "b", Prefix: prefix, HostName: host}
	c.ApplyDefaults()
	return c
}

func manifestJSON(volume, key string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(`{"volume":"` + volume + `","key":"` + key + `","sha256":"` + hex.EncodeToString(sum[:]) + `"}`)
}

func TestObjectKeysCarryTheHost(t *testing.T) {
	for _, tc := range []struct{ prefix, want string }{
		{"incus/", "incus/h1/vol/"},
		{"incus", "incus/h1/vol/"}, // a prefix without its slash still gets one
		{"/incus/", "incus/h1/vol/"},
		{"", "h1/vol/"},
	} {
		m := New(&fakeExec{}, newFakeStore(), cfgFor("h1", tc.prefix))
		if got := m.keyPrefix("vol"); got != tc.want {
			t.Errorf("prefix %q: key prefix %q, want %q", tc.prefix, got, tc.want)
		}
	}
}

func TestTwoHostsWithAVolumeOfTheSameNameNeverCollide(t *testing.T) {
	store := newFakeStore()
	at := time.Date(2026, 11, 4, 3, 30, 0, 0, time.UTC)
	a := New(&fakeExec{exportBody: []byte("from-a")}, store, cfgFor("host-a", "backups/"))
	b := New(&fakeExec{exportBody: []byte("from-b")}, store, cfgFor("host-b", "backups/"))
	a.now = func() time.Time { return at }
	b.now = func() time.Time { return at } // the same second, on purpose
	ma, err := a.CreateVolume(context.Background(), "default", "app-data")
	if err != nil {
		t.Fatal(err)
	}
	mb, err := b.CreateVolume(context.Background(), "default", "app-data")
	if err != nil {
		t.Fatal(err)
	}
	if ma.Key == mb.Key {
		t.Fatalf("both hosts wrote %s", ma.Key)
	}
	if string(store.objects[ma.Key]) != "from-a" || string(store.objects[mb.Key]) != "from-b" {
		t.Fatal("one host overwrote the other's backup")
	}
	// Each host restores its own latest.
	execA := &fakeExec{}
	a2 := New(execA, store, cfgFor("host-a", "backups/"))
	if err := a2.Restore(context.Background(), RestoreOptions{Volume: "app-data", AsName: "app-data-check"}); err != nil {
		t.Fatal(err)
	}
	objs, man, err := a2.ListVolume(context.Background(), "app-data")
	if err != nil {
		t.Fatal(err)
	}
	if man == nil || man.Key != ma.Key || len(objs) != 2 { // host-a's artifact and manifest only
		t.Fatalf("host-a sees manifest %+v and objects %+v", man, objs)
	}
}

func TestLegacyBackupsStayListedAndRestorableButNewOnesUseTheHostLayout(t *testing.T) {
	store := newFakeStore()
	old := []byte("old-payload")
	store.objects["incus/vol/20260101T000000Z.tar.gz"] = old
	store.objects["incus/vol/latest.json"] = manifestJSON("vol", "incus/vol/20260101T000000Z.tar.gz", old)

	exec := &fakeExec{exportBody: []byte("new-payload")}
	m := New(exec, store, cfgFor("host-a", "incus/"))
	m.now = func() time.Time { return time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC) }

	// Before any new backup: latest resolves through the legacy manifest, and is verified.
	if err := m.Restore(context.Background(), RestoreOptions{Volume: "vol", AsName: "vol-old"}); err != nil {
		t.Fatalf("legacy latest must restore: %v", err)
	}
	objs, man, err := m.ListVolume(context.Background(), "vol")
	if err != nil {
		t.Fatal(err)
	}
	if man == nil || man.Key != "incus/vol/20260101T000000Z.tar.gz" || len(objs) != 2 {
		t.Fatalf("legacy list: %+v %+v", man, objs)
	}

	// A new backup goes only to the host layout; the legacy objects are untouched.
	nm, err := m.CreateVolume(context.Background(), "default", "vol")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(nm.Key, "incus/host-a/vol/") {
		t.Fatalf("new key %s", nm.Key)
	}
	for k := range store.objects {
		if !strings.HasPrefix(k, "incus/host-a/vol/") && !strings.HasPrefix(k, "incus/vol/2026") && k != "incus/vol/latest.json" {
			t.Fatalf("unexpected object %s", k)
		}
	}
	if string(store.objects["incus/vol/latest.json"]) != string(manifestJSON("vol", "incus/vol/20260101T000000Z.tar.gz", old)) {
		t.Fatal("the legacy manifest must not be rewritten")
	}

	// Now latest is the host's, the list has both layouts, and an explicit legacy key still restores
	// with its checksum checked against the legacy manifest.
	objs, man, err = m.ListVolume(context.Background(), "vol")
	if err != nil {
		t.Fatal(err)
	}
	if man.Key != nm.Key || len(objs) != 4 {
		t.Fatalf("after a new backup: %+v %d objects", man, len(objs))
	}
	if got := m.knownSHA(context.Background(), "vol", "incus/vol/20260101T000000Z.tar.gz"); got == "" {
		t.Fatal("an explicit legacy key must be verified against the legacy manifest")
	}
	store.objects["incus/vol/20260101T000000Z.tar.gz"] = []byte("tampered")
	if err := m.Restore(context.Background(), RestoreOptions{Volume: "vol", FromKey: "incus/vol/20260101T000000Z.tar.gz", AsName: "vol-old2"}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a legacy key is still checksum-verified: %v", err)
	}
}

func TestLegacyListingDoesNotPickUpAHostNamedLikeTheVolume(t *testing.T) {
	store := newFakeStore()
	// Host "data" has a volume "web"; on host "other", a volume is called "data".
	store.objects["p/data/web/20260101T000000Z.tar.gz"] = []byte("x")
	store.objects["p/data/web/latest.json"] = manifestJSON("web", "p/data/web/20260101T000000Z.tar.gz", []byte("x"))
	m := New(&fakeExec{}, store, cfgFor("other", "p/"))
	objs, man, err := m.ListVolume(context.Background(), "data")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 0 || man != nil {
		t.Fatalf("another host's keys are not legacy objects of volume data: %+v %+v", objs, man)
	}
}

func TestRetentionDeletesOnlyThisHostsObjects(t *testing.T) {
	store := newFakeStore()
	for _, k := range []string{
		"p/host-a/vol/20261101T000000Z.tar.gz", "p/host-a/vol/20261102T000000Z.tar.gz", "p/host-a/vol/20261103T000000Z.tar.gz",
		"p/host-b/vol/20261101T000000Z.tar.gz", "p/host-b/vol/20261102T000000Z.tar.gz",
		"p/vol/20261001T000000Z.tar.gz", "p/vol/20261002T000000Z.tar.gz", // legacy
		"p/host-a/vol/latest.json",
	} {
		store.objects[k] = []byte("x")
	}
	cfg := cfgFor("host-a", "p/")
	cfg.RetainDaily = 1
	m := New(&fakeExec{}, store, cfg)
	m.now = func() time.Time { return time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC) }
	del, err := m.Prune(context.Background(), "vol")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(del, ",") != "p/host-a/vol/20261102T000000Z.tar.gz,p/host-a/vol/20261101T000000Z.tar.gz" {
		t.Fatalf("deleted %v", del)
	}
	for _, k := range []string{"p/host-a/vol/20261103T000000Z.tar.gz", "p/host-a/vol/latest.json", "p/host-b/vol/20261101T000000Z.tar.gz",
		"p/host-b/vol/20261102T000000Z.tar.gz", "p/vol/20261001T000000Z.tar.gz", "p/vol/20261002T000000Z.tar.gz"} {
		if _, ok := store.objects[k]; !ok {
			t.Errorf("%s must survive this host's retention", k)
		}
	}
}

func TestRetainDaysKeepsRecentObjectsAndAlwaysTheNewest(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	keys := []string{"v/20261104T000000Z.tar.gz", "v/20261101T000000Z.tar.gz", "v/20261020T000000Z.tar.gz"}
	if del := selectPrune(keys, 0, 0, 7, now); len(del) != 1 || del[0] != "v/20261020T000000Z.tar.gz" {
		t.Fatalf("retain_days 7 deletes only the old one: %v", del)
	}
	old := []string{"v/20260101T000000Z.tar.gz", "v/20250101T000000Z.tar.gz"}
	if del := selectPrune(old, 0, 0, 7, now); len(del) != 1 || del[0] != "v/20250101T000000Z.tar.gz" {
		t.Fatalf("the newest backup is kept however old it is: %v", del)
	}
}

func TestAnUnusableHostNameRefusesToWrite(t *testing.T) {
	m := New(&fakeExec{}, newFakeStore(), cfgFor("bad/host", "p/"))
	if _, err := m.CreateVolume(context.Background(), "default", "vol"); err == nil || !strings.Contains(err.Error(), "host name") {
		t.Fatalf("want a host-name error, got %v", err)
	}
}

func TestTheHostNameFallsBackToTheOSHostName(t *testing.T) {
	m := New(&fakeExec{}, newFakeStore(), cfgFor("", "p/"))
	if m.Host() == "" {
		t.Fatal("no host name")
	}
}
