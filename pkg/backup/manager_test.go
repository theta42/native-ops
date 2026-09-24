package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/s3"
)

type fakeExec struct {
	cmds       []string
	exportBody []byte
}

func (f *fakeExec) Run(_ context.Context, command string) (string, error) {
	f.cmds = append(f.cmds, command)
	switch {
	case strings.Contains(command, "storage volume export"):
		if m := regexp.MustCompile(`(\S+\.tar\.gz)`).FindStringSubmatch(command); m != nil {
			if err := os.WriteFile(m[1], f.exportBody, 0o644); err != nil {
				return "", err
			}
		}
	case strings.Contains(command, "storage volume list"):
		return `[{"name":"rest-sicily-data","type":"custom"},{"name":"gitea-data","type":"custom"}]`, nil
	case strings.HasPrefix(command, "incus list"):
		return `[]`, nil
	}
	return "", nil
}
func (f *fakeExec) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (f *fakeExec) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (f *fakeExec) Close() error                                                    { return nil }

func (f *fakeExec) has(sub string) bool {
	for _, c := range f.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

type fakeStore struct{ objects map[string][]byte }

func newFakeStore() *fakeStore { return &fakeStore{objects: map[string][]byte{}} }

func (s *fakeStore) PutObject(_ context.Context, key string, body io.Reader, _ int64, _, _ string) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.objects[key] = b
	return nil
}
func (s *fakeStore) GetObject(_ context.Context, key string) (io.ReadCloser, int64, error) {
	b, ok := s.objects[key]
	if !ok {
		return nil, 0, s3.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(b))), int64(len(b)), nil
}
func (s *fakeStore) HeadObject(_ context.Context, key string) (*s3.Object, error) {
	b, ok := s.objects[key]
	if !ok {
		return nil, s3.ErrNotFound
	}
	return &s3.Object{Key: key, Size: int64(len(b))}, nil
}
func (s *fakeStore) ListObjects(_ context.Context, prefix string) ([]s3.Object, error) {
	var out []s3.Object
	for k, b := range s.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, s3.Object{Key: k, Size: int64(len(b))})
		}
	}
	return out, nil
}
func (s *fakeStore) DeleteObject(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

func testCfg() *config.BackupConfig {
	c := &config.BackupConfig{Endpoint: "https://nyc3.digitaloceanspaces.com", Region: "nyc3", Bucket: "b", Prefix: "incus/"}
	c.ApplyDefaults()
	return c
}

func TestCreateVolumeUploadsArtifactAndManifest(t *testing.T) {
	fixed := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	exec := &fakeExec{exportBody: []byte("hello-backup")}
	store := newFakeStore()
	mgr := New(exec, store, testCfg())
	mgr.now = func() time.Time { return fixed }

	man, err := mgr.CreateVolume(context.Background(), "default", "rest-sicily-data")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello-backup"))
	if man.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha = %s", man.SHA256)
	}
	if man.Key != "incus/rest-sicily-data/20260924T120000Z.tar.gz" {
		t.Fatalf("key = %s", man.Key)
	}
	if _, ok := store.objects[man.Key]; !ok {
		t.Fatal("artifact not uploaded")
	}
	if _, ok := store.objects["incus/rest-sicily-data/latest.json"]; !ok {
		t.Fatal("manifest not uploaded")
	}
	if !exec.has("storage volume snapshot create") {
		t.Fatal("no snapshot taken")
	}
	if !exec.has("storage volume snapshot delete") {
		t.Fatal("transient snapshot not cleaned up")
	}
}

func TestCreateVolumesHonoursAllowlist(t *testing.T) {
	cfg := testCfg()
	cfg.Volumes = []string{"rest-sicily-data"}
	mgr := New(&fakeExec{exportBody: []byte("x")}, newFakeStore(), cfg)
	mans, err := mgr.CreateVolumes(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(mans) != 1 || mans[0].Volume != "rest-sicily-data" {
		t.Fatalf("unexpected backups: %+v", mans)
	}
}

func TestRestoreRejectsChecksumMismatch(t *testing.T) {
	store := newFakeStore()
	store.objects["incus/vol/20260101T000000Z.tar.gz"] = []byte("real-bytes")
	store.objects["incus/vol/latest.json"] = []byte(`{"volume":"vol","key":"incus/vol/20260101T000000Z.tar.gz","sha256":"deadbeef"}`)
	mgr := New(&fakeExec{}, store, testCfg())
	err := mgr.Restore(context.Background(), RestoreOptions{Volume: "vol", AsName: "vol-v2"})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
}

func TestRestoreAsNameImports(t *testing.T) {
	exec := &fakeExec{}
	store := newFakeStore()
	body := []byte("payload")
	store.objects["incus/vol/20260101T000000Z.tar.gz"] = body
	sum := sha256.Sum256(body)
	store.objects["incus/vol/latest.json"] = []byte(`{"volume":"vol","key":"incus/vol/20260101T000000Z.tar.gz","sha256":"` + hex.EncodeToString(sum[:]) + `"}`)
	mgr := New(exec, store, testCfg())
	if err := mgr.Restore(context.Background(), RestoreOptions{Volume: "vol", AsName: "vol-v2"}); err != nil {
		t.Fatal(err)
	}
	if !exec.has("storage volume import") {
		t.Fatal("no import issued")
	}
	if exec.has("incus stop") {
		t.Fatal("restore-as-name must not touch dependents")
	}
}

func TestSelectPrune(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	keys := []string{
		"p/vol/20260924T120000Z.tar.gz",
		"p/vol/20260923T120000Z.tar.gz",
		"p/vol/20260922T120000Z.tar.gz",
		"p/vol/20260901T120000Z.tar.gz",
		"p/vol/20260801T120000Z.tar.gz",
	}
	del := strings.Join(selectPrune(keys, 1, 2, now), ",")
	// keep newest (Sep 24) + newest of the last two months (Sep 24, Aug 1)
	for _, want := range []string{"20260923", "20260922", "20260901"} {
		if !strings.Contains(del, want) {
			t.Errorf("expected %s to be pruned; got %v", want, del)
		}
	}
	for _, keep := range []string{"20260924", "20260801"} {
		if strings.Contains(del, keep) {
			t.Errorf("expected %s to be kept; got %v", keep, del)
		}
	}
}

func TestParseObjectTime(t *testing.T) {
	ts, ok := parseObjectTime("p/vol/20260924T120000Z.tar.gz")
	if !ok || ts.UTC().Format(time.RFC3339) != "2026-09-24T12:00:00Z" {
		t.Fatalf("parseObjectTime = %v, %v", ts, ok)
	}
	if _, ok := parseObjectTime("p/vol/latest.json"); ok {
		t.Fatal("manifest should not parse as a timed object")
	}
}
