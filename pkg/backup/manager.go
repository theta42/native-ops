// Package backup implements off-host volume backup and restore to any
// S3-compatible object store. It snapshots a custom Incus volume, exports it
// to a compressed artifact, uploads it with a SHA-256 manifest, and can
// restore it (in place, with a pre-restore snapshot) or under a new name.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
	"github.com/theta42/native-ops/pkg/s3"
)

// ErrNoManifest means a volume has no latest.json yet.
var ErrNoManifest = errors.New("backup: no manifest for volume")

// ObjectStore is the subset of the S3 client the manager needs.
type ObjectStore interface {
	PutObject(ctx context.Context, key string, body io.Reader, size int64, sha256hex, contentType string) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error)
	HeadObject(ctx context.Context, key string) (*s3.Object, error)
	ListObjects(ctx context.Context, prefix string) ([]s3.Object, error)
	DeleteObject(ctx context.Context, key string) error
}

// Manifest records what a backup object contains and how to verify it.
type Manifest struct {
	Volume    string    `json:"volume"`
	Pool      string    `json:"pool"`
	CreatedAt time.Time `json:"created_at"`
	Snapshot  string    `json:"snapshot"`
	Key       string    `json:"key"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	Tool      string    `json:"tool"`
}

// Manager coordinates Incus volume export/import with an object store.
type Manager struct {
	incus *incus.Client
	store ObjectStore
	cfg   *config.BackupConfig
	now   func() time.Time
}

// New builds a manager. store may be nil when only local operations are used.
func New(exec remote.Executor, store ObjectStore, cfg *config.BackupConfig) *Manager {
	return &Manager{incus: incus.NewClient(exec), store: store, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}
}

func (m *Manager) keyPrefix(volume string) string {
	return strings.TrimPrefix(m.cfg.Prefix, "/") + volume + "/"
}

func (m *Manager) objectKey(volume string, t time.Time) string {
	return m.keyPrefix(volume) + t.UTC().Format("20060102T150405Z") + ".tar.gz"
}

func (m *Manager) manifestKey(volume string) string {
	return m.keyPrefix(volume) + "latest.json"
}

// CreateVolume snapshots, exports and uploads one custom volume, then writes
// its manifest. The transient snapshot is removed unless KeepLocalSnapshots.
func (m *Manager) CreateVolume(ctx context.Context, pool, volume string) (*Manifest, error) {
	if m.store == nil {
		return nil, errors.New("backup: no object store configured")
	}
	if pool == "" {
		pool = "default"
	}
	now := m.now()
	snap := "backup-" + now.Format("20060102T150405Z")

	tmpDir, err := os.MkdirTemp("", "native-ops-backup-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	artifact := tmpDir + "/" + volume + ".tar.gz"

	if err := m.incus.CreateVolumeSnapshot(ctx, pool, volume, snap); err != nil {
		return nil, err
	}
	if err := m.incus.ExportVolumeSnapshot(ctx, pool, volume+"/"+snap, artifact); err != nil {
		return nil, err
	}

	sha, size, err := hashFile(artifact)
	if err != nil {
		return nil, err
	}
	key := m.objectKey(volume, now)

	f, err := os.Open(artifact)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := m.store.PutObject(ctx, key, f, size, sha, "application/gzip"); err != nil {
		return nil, fmt.Errorf("upload %s: %w", key, err)
	}

	// Verify the upload is retrievable and the right size before recording it.
	if head, err := m.store.HeadObject(ctx, key); err != nil {
		return nil, fmt.Errorf("verify upload %s: %w", key, err)
	} else if head.Size != size {
		return nil, fmt.Errorf("verify upload %s: size mismatch (want %d, got %d)", key, size, head.Size)
	}

	man := &Manifest{
		Volume: volume, Pool: pool, CreatedAt: now, Snapshot: snap,
		Key: key, SizeBytes: size, SHA256: sha, Tool: "native-ops",
	}
	if err := m.writeManifest(ctx, man); err != nil {
		return nil, err
	}

	if !m.cfg.KeepLocalSnapshots {
		// Best-effort: the backup is already uploaded and verified.
		_ = m.incus.DeleteVolumeSnapshot(ctx, pool, volume, snap)
	}
	return man, nil
}

// CreateVolumes backs up every allowlisted volume (or all custom volumes).
func (m *Manager) CreateVolumes(ctx context.Context, pool string) ([]*Manifest, error) {
	if pool == "" {
		pool = "default"
	}
	names := m.cfg.Volumes
	if len(names) == 0 {
		all, err := m.incus.ListCustomVolumes(ctx, pool)
		if err != nil {
			return nil, err
		}
		names = all
	}
	var out []*Manifest
	for _, v := range names {
		man, err := m.CreateVolume(ctx, pool, v)
		if err != nil {
			return out, fmt.Errorf("backup %s: %w", v, err)
		}
		out = append(out, man)
	}
	return out, nil
}

// ListVolume returns the stored objects and (if present) the manifest.
func (m *Manager) ListVolume(ctx context.Context, volume string) ([]s3.Object, *Manifest, error) {
	objs, err := m.store.ListObjects(ctx, m.keyPrefix(volume))
	if err != nil {
		return nil, nil, err
	}
	man, err := m.readManifest(ctx, volume)
	if err != nil && !errors.Is(err, ErrNoManifest) {
		return nil, nil, err
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].LastModified.After(objs[j].LastModified) })
	return objs, man, nil
}

// RestoreOptions controls a restore.
type RestoreOptions struct {
	Pool    string
	Volume  string
	FromKey string // object key; "", "latest" resolves via the manifest
	AsName  string // import under a new volume name (no dependents touched)
	Force   bool   // allow stopping dependent containers for an in-place restore
}

// Restore downloads a backup, verifies its SHA-256, and imports it. When
// AsName is set the volume is created under that name (non-destructive).
// Otherwise the existing volume is snapshotted (pre-restore) then replaced.
func (m *Manager) Restore(ctx context.Context, o RestoreOptions) error {
	if m.store == nil {
		return errors.New("backup: no object store configured")
	}
	if o.Pool == "" {
		o.Pool = "default"
	}
	key, wantSHA := o.FromKey, ""
	if key == "" || key == "latest" {
		man, err := m.readManifest(ctx, o.Volume)
		if err != nil {
			return err
		}
		key, wantSHA = man.Key, man.SHA256
	} else if man, err := m.readManifest(ctx, o.Volume); err == nil && man.Key == key {
		wantSHA = man.SHA256
	}

	tmpDir, err := os.MkdirTemp("", "native-ops-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	artifact := tmpDir + "/restore.tar.gz"

	if err := m.download(ctx, key, artifact); err != nil {
		return err
	}
	if wantSHA != "" {
		got, _, err := hashFile(artifact)
		if err != nil {
			return err
		}
		if got != wantSHA {
			return fmt.Errorf("checksum mismatch for %s: want %s got %s", key, wantSHA, got)
		}
	}

	if o.AsName != "" {
		return m.incus.ImportVolume(ctx, o.Pool, artifact, o.AsName)
	}

	deps, err := m.incus.VolumeDependents(ctx, o.Volume)
	if err != nil {
		return err
	}
	if len(deps) > 0 && !o.Force {
		return fmt.Errorf("volume %s is mounted by %s; re-run with --force to stop them", o.Volume, strings.Join(deps, ", "))
	}
	for _, c := range deps {
		if err := m.incus.StopContainer(ctx, c); err != nil {
			return err
		}
	}
	// Always leave a recovery point behind, even if the import fails.
	pre := "pre-restore-" + m.now().Format("20060102T150405Z")
	if err := m.incus.CreateVolumeSnapshot(ctx, o.Pool, o.Volume, pre); err != nil {
		return fmt.Errorf("refusing to restore without a pre-restore snapshot: %w", err)
	}
	if err := m.incus.DeleteVolume(ctx, o.Pool, o.Volume); err != nil {
		return err
	}
	if err := m.incus.ImportVolume(ctx, o.Pool, artifact, o.Volume); err != nil {
		return fmt.Errorf("import failed; volume %s@%s still holds the pre-restore state: %w", o.Volume, pre, err)
	}
	for _, c := range deps {
		if err := m.incus.StartContainer(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// Prune applies retention to a volume's objects, keeping the newest
// RetainDaily overall plus the newest object in each of the last
// RetainMonthly calendar months. latest.json is never removed.
func (m *Manager) Prune(ctx context.Context, volume string) ([]string, error) {
	if m.cfg.RetainDaily == 0 && m.cfg.RetainMonthly == 0 {
		return nil, nil
	}
	objs, err := m.store.ListObjects(ctx, m.keyPrefix(volume))
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".tar.gz") {
			continue
		}
		keys = append(keys, o.Key)
	}
	del := selectPrune(keys, m.cfg.RetainDaily, m.cfg.RetainMonthly, m.now())
	for _, k := range del {
		if err := m.store.DeleteObject(ctx, k); err != nil {
			return nil, fmt.Errorf("delete %s: %w", k, err)
		}
	}
	return del, nil
}

func (m *Manager) download(ctx context.Context, key, dest string) error {
	rc, _, err := m.store.GetObject(ctx, key)
	if err != nil {
		return fmt.Errorf("download %s: %w", key, err)
	}
	defer rc.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, rc); err != nil {
		return err
	}
	return nil
}

func (m *Manager) writeManifest(ctx context.Context, man *Manifest) error {
	buf, _ := json.MarshalIndent(man, "", "  ")
	return m.store.PutObject(ctx, m.manifestKey(man.Volume), strings.NewReader(string(buf)), int64(len(buf)), sha256Hex(buf), "application/json")
}

func (m *Manager) readManifest(ctx context.Context, volume string) (*Manifest, error) {
	rc, _, err := m.store.GetObject(ctx, m.manifestKey(volume))
	if errors.Is(err, s3.ErrNotFound) {
		return nil, ErrNoManifest
	}
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var man Manifest
	if err := json.NewDecoder(rc).Decode(&man); err != nil {
		return nil, fmt.Errorf("parse manifest for %s: %w", volume, err)
	}
	return &man, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// selectPrune returns the object keys that fall outside the retention policy.
func selectPrune(keys []string, daily, monthly int, now time.Time) []string {
	type item struct {
		key string
		ts  time.Time
	}
	var items []item
	for _, k := range keys {
		if ts, ok := parseObjectTime(k); ok {
			items = append(items, item{k, ts})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ts.After(items[j].ts) })

	keep := map[string]bool{}
	for i := 0; i < len(items) && i < daily; i++ {
		keep[items[i].key] = true
	}
	if monthly > 0 {
		months := map[string]bool{}
		for _, it := range items {
			mk := it.ts.Format("2006-01")
			if months[mk] {
				continue
			}
			// Count distinct months already claimed; stop after `monthly`.
			months[mk] = true
			keep[it.key] = true
			if len(months) >= monthly {
				break
			}
		}
	}
	var del []string
	for _, it := range items {
		if !keep[it.key] {
			del = append(del, it.key)
		}
	}
	return del
}

// parseObjectTime reads the YYYYMMDDTHHMMSSZ timestamp from an object key.
func parseObjectTime(key string) (time.Time, bool) {
	base := key
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".tar.gz")
	if len(base) < 16 {
		return time.Time{}, false
	}
	ts, err := time.Parse("20060102T150405Z", base[:16])
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}
