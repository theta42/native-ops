// Package backup implements off-host volume backup and restore to any
// S3-compatible object store. It snapshots a custom Incus volume, exports it
// to a compressed artifact, uploads it with a SHA-256 manifest, and can
// restore it (in place, keeping a copy of the volume it replaces) or under a new name.
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
	"regexp"
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
//
// Object keys are <prefix>/<host>/<volume>/<timestamp>.tar.gz, with <prefix>/<host>/<volume>/latest.json
// as the manifest, so hosts that share a bucket never overwrite each other's backups. Backups written
// before keys carried the host (<prefix><volume>/...) are still listed and restorable, but nothing new is
// written there and retention never deletes them (they may be another host's).
type Manager struct {
	incus   *incus.Client
	store   ObjectStore
	cfg     *config.BackupConfig
	host    string
	hostErr error
	now     func() time.Time
}

var hostNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// New builds a manager. store may be nil when only local operations are used. The host in the object
// keys is cfg.HostName, else the OS host name.
func New(exec remote.Executor, store ObjectStore, cfg *config.BackupConfig) *Manager {
	m := &Manager{incus: incus.NewClient(exec), store: store, cfg: cfg, now: func() time.Time { return time.Now().UTC() }}
	m.host = strings.TrimSpace(cfg.HostName)
	if m.host == "" {
		h, err := os.Hostname()
		if err != nil {
			m.hostErr = fmt.Errorf("backup: no host name for the object keys: %w", err)
		}
		m.host = h
	}
	if m.hostErr == nil && (!hostNameRe.MatchString(m.host) || strings.Contains(m.host, "..")) {
		m.hostErr = fmt.Errorf("backup: host name %q cannot be part of an object key (letters, digits, '.', '_', '-')", m.host)
	}
	return m
}

// Host is the host name in this manager's object keys.
func (m *Manager) Host() string { return m.host }

// base is the configured prefix as a key path ("" or ending in "/").
func (m *Manager) base() string {
	p := strings.TrimPrefix(m.cfg.Prefix, "/")
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// keyPrefix is where this host's backups of a volume live: <prefix>/<host>/<volume>/.
func (m *Manager) keyPrefix(volume string) string {
	return m.base() + m.host + "/" + volume + "/"
}

// legacyKeyPrefix is where a volume's backups lived before keys carried the host: exactly the old
// formula (the prefix as written, then the volume), so old objects are found where they were put.
func (m *Manager) legacyKeyPrefix(volume string) string {
	return strings.TrimPrefix(m.cfg.Prefix, "/") + volume + "/"
}

func (m *Manager) objectKey(volume string, t time.Time) string {
	return m.keyPrefix(volume) + t.UTC().Format("20060102T150405Z") + ".tar.gz"
}

func (m *Manager) manifestKey(volume string) string {
	return m.keyPrefix(volume) + "latest.json"
}

func (m *Manager) legacyManifestKey(volume string) string {
	return m.legacyKeyPrefix(volume) + "latest.json"
}

// EnsureBucket creates the destination bucket if the store supports it and
// it does not already exist.
func (m *Manager) EnsureBucket(ctx context.Context) error {
	if m.store == nil {
		return errors.New("backup: no object store configured")
	}
	creator, ok := m.store.(interface{ CreateBucket(context.Context) error })
	if !ok {
		return errors.New("backup: object store does not support bucket creation")
	}
	return creator.CreateBucket(ctx)
}

// CreateVolume snapshots, exports and uploads one custom volume, then writes
// its manifest. The transient snapshot is removed unless KeepLocalSnapshots.
func (m *Manager) CreateVolume(ctx context.Context, pool, volume string) (*Manifest, error) {
	if m.store == nil {
		return nil, errors.New("backup: no object store configured")
	}
	if m.hostErr != nil {
		return nil, m.hostErr
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

// ListVolume returns the stored objects, newest first, and (if present) the manifest of the latest
// backup: this host's, and the ones written before keys carried the host (legacy layout).
func (m *Manager) ListVolume(ctx context.Context, volume string) ([]s3.Object, *Manifest, error) {
	if m.store == nil {
		return nil, nil, errors.New("backup: no object store configured")
	}
	var objs []s3.Object
	if m.hostErr == nil {
		own, err := m.store.ListObjects(ctx, m.keyPrefix(volume))
		if err != nil {
			return nil, nil, err
		}
		objs = own
	}
	legacy, err := m.listLegacy(ctx, volume)
	if err != nil {
		return nil, nil, err
	}
	objs = append(objs, legacy...)
	man, err := m.readManifest(ctx, volume)
	if err != nil && !errors.Is(err, ErrNoManifest) {
		return nil, nil, err
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].LastModified.After(objs[j].LastModified) })
	return objs, man, nil
}

// listLegacy lists a volume's objects in the layout without the host. Only keys directly under
// <prefix><volume>/ count: a host named like the volume has its own keys below that, and they are not
// legacy objects.
func (m *Manager) listLegacy(ctx context.Context, volume string) ([]s3.Object, error) {
	lp := m.legacyKeyPrefix(volume)
	if m.hostErr == nil && lp == m.keyPrefix(volume) {
		return nil, nil
	}
	objs, err := m.store.ListObjects(ctx, lp)
	if err != nil {
		return nil, err
	}
	var out []s3.Object
	for _, o := range objs {
		if rest := strings.TrimPrefix(o.Key, lp); rest != "" && !strings.Contains(rest, "/") {
			out = append(out, o)
		}
	}
	return out, nil
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
// Otherwise the existing volume is copied aside (see preRestoreName) and then replaced.
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
	} else {
		wantSHA = m.knownSHA(ctx, o.Volume, key)
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
	var stopped []string
	// Whatever happens below, the containers that were stopped for the restore are started again.
	restart := func(err error) error {
		for _, c := range stopped {
			if serr := m.incus.StartContainer(ctx, c); serr != nil && err == nil {
				err = serr
			}
		}
		return err
	}
	for _, c := range deps {
		if err := m.incus.StopContainer(ctx, c); err != nil {
			return restart(err)
		}
		stopped = append(stopped, c)
	}
	// Always leave a recovery point behind, even if the import fails. It has to be a separate volume:
	// a snapshot of the volume would be deleted together with it a moment later.
	pre := preRestoreName(o.Volume, m.now())
	if err := m.incus.DuplicateVolume(ctx, o.Pool, o.Volume, pre); err != nil {
		return restart(fmt.Errorf("refusing to restore without a copy of the current volume: %w", err))
	}
	if err := m.incus.DeleteVolume(ctx, o.Pool, o.Volume); err != nil {
		return restart(fmt.Errorf("%w (the current data is unchanged; a copy of it is in volume %s)", err, pre))
	}
	if err := m.incus.ImportVolume(ctx, o.Pool, artifact, o.Volume); err != nil {
		return restart(fmt.Errorf("import failed; volume %s is gone and its data is in volume %s (copy it back with `incus storage volume copy %s/%s %s/%s`): %w",
			o.Volume, pre, o.Pool, pre, o.Pool, o.Volume, err))
	}
	return restart(nil)
}

// preRestoreName is the volume an in-place restore copies the current data to: the volume's name and
// the time, cut to fit Incus' 63-character limit. It is kept after the restore, so a restore that
// brought back the wrong thing can itself be undone; delete it once the restored data is checked.
func preRestoreName(volume string, now time.Time) string {
	suffix := "-pre-restore-" + now.UTC().Format("20060102t150405")
	if max := 63 - len(suffix); len(volume) > max {
		volume = strings.TrimRight(volume[:max], ".-_")
	}
	return volume + suffix
}

// PruneAll applies retention to every allowlisted (or all) custom volume,
// one volume at a time so keys never rank across volumes.
func (m *Manager) PruneAll(ctx context.Context, pool string) ([]string, error) {
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
	var deleted []string
	for _, v := range names {
		del, err := m.Prune(ctx, v)
		if err != nil {
			return deleted, err
		}
		deleted = append(deleted, del...)
	}
	return deleted, nil
}

// Prune applies retention to a volume's objects, keeping the newest
// RetainDaily overall, the newest object in each of the last
// RetainMonthly calendar months, and every object younger than RetainDays
// days; the newest object is always kept. latest.json is never removed.
//
// Only this host's objects (<prefix>/<host>/<volume>/) are ever considered: another host's, and the ones
// in the layout without the host, are never deleted.
func (m *Manager) Prune(ctx context.Context, volume string) ([]string, error) {
	if m.cfg.RetainDaily == 0 && m.cfg.RetainMonthly == 0 && m.cfg.RetainDays == 0 {
		return nil, nil
	}
	if m.store == nil {
		return nil, errors.New("backup: no object store configured")
	}
	if m.hostErr != nil {
		return nil, m.hostErr
	}
	prefix := m.keyPrefix(volume)
	objs, err := m.store.ListObjects(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, o := range objs {
		// Belt and braces: a key outside this host's prefix, or below it, is never a candidate.
		if !strings.HasPrefix(o.Key, prefix) || strings.Contains(strings.TrimPrefix(o.Key, prefix), "/") || !strings.HasSuffix(o.Key, ".tar.gz") {
			continue
		}
		keys = append(keys, o.Key)
	}
	del := selectPrune(keys, m.cfg.RetainDaily, m.cfg.RetainMonthly, m.cfg.RetainDays, m.now())
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

// knownSHA is the checksum a manifest (this host's, or the legacy one) records for key, or "".
func (m *Manager) knownSHA(ctx context.Context, volume, key string) string {
	keys := []string{m.legacyManifestKey(volume)}
	if m.hostErr == nil {
		keys = append([]string{m.manifestKey(volume)}, keys...)
	}
	for _, mk := range keys {
		if man, err := m.readManifestAt(ctx, mk, volume); err == nil && man.Key == key {
			return man.SHA256
		}
	}
	return ""
}

// readManifest reads a volume's manifest: this host's, else the one in the layout without the host.
func (m *Manager) readManifest(ctx context.Context, volume string) (*Manifest, error) {
	if m.hostErr == nil {
		man, err := m.readManifestAt(ctx, m.manifestKey(volume), volume)
		if !errors.Is(err, ErrNoManifest) {
			return man, err
		}
	}
	return m.readManifestAt(ctx, m.legacyManifestKey(volume), volume)
}

func (m *Manager) readManifestAt(ctx context.Context, key, volume string) (*Manifest, error) {
	rc, _, err := m.store.GetObject(ctx, key)
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
func selectPrune(keys []string, daily, monthly, days int, now time.Time) []string {
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
	if len(items) > 0 {
		keep[items[0].key] = true // the newest backup is never pruned
	}
	if days > 0 {
		cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
		for _, it := range items {
			if it.ts.After(cutoff) {
				keep[it.key] = true
			}
		}
	}
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
