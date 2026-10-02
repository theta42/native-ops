// Package selfupdate replaces the running native-ops binary with a pinned release, so a daemon can be
// upgraded from CI with nobody logging in to its host.
//
// The binary lives in a directory the daemon may write (<state-dir>/bin), next to the files the upgrade
// uses:
//
//	native-ops          the binary systemd starts
//	native-ops.prev     the one it replaced, kept for a rollback
//	upgrade.pending     present from the swap until the new binary has served for a while
//	upgrade.attempts    starts of the new binary so far (written by the unit's guard, see Guard)
//
// The release is downloaded, checked against the SHA-256 the caller pinned, unpacked, and run once
// (`native-ops version` must name the version asked for) before anything is swapped. A binary that then
// cannot stay up is swapped back by the unit's guard, which runs before every start.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	BinaryName   = "native-ops"
	PrevName     = "native-ops.prev"
	PendingName  = "upgrade.pending"
	AttemptsName = "upgrade.attempts"

	// DefaultReleaseBase is where releases are downloaded from.
	DefaultReleaseBase = "https://github.com/theta42/native-ops/releases/download"
	// MaxAttempts is how many times the guard lets a new binary try to start before restoring the old one.
	MaxAttempts = 3
	maxDownload = 64 << 20
)

// CommitAfter is how long a new binary must serve before its upgrade counts as done (a variable so
// tests can shorten it).
var CommitAfter = 30 * time.Second

var (
	versionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-.][A-Za-z0-9.]+)?$`)
	shaRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Updater swaps the binary in Dir.
type Updater struct {
	Dir         string // the directory holding the running binary (<state-dir>/bin)
	ReleaseBase string // DefaultReleaseBase unless set (tests point it at a fake)
	Arch        string // runtime.GOARCH unless set
	HTTP        *http.Client
}

// Managed reports whether the running binary is the one in dir, i.e. whether this daemon was installed
// so that it can upgrade itself. A binary elsewhere (/usr/local/bin, root-owned) cannot be replaced by
// the daemon's user, and is upgraded by whoever installed it.
func Managed(dir string) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	exe, _ = filepath.EvalSymlinks(exe)
	want, _ := filepath.EvalSymlinks(filepath.Join(dir, BinaryName))
	return exe != "" && exe == want
}

// Validate checks a requested version and checksum.
func Validate(version, sha string) error {
	if !versionRe.MatchString(version) {
		return fmt.Errorf("version %q is not a release tag like v1.56.0", version)
	}
	if !shaRe.MatchString(sha) {
		return errors.New("sha256 must be the 64 lowercase hex characters of the release tarball (from its checksums.txt)")
	}
	return nil
}

// Install downloads version, checks it against sha (of the tarball, as in the release's
// checksums.txt), runs it once, and swaps it in, keeping the current binary as native-ops.prev and
// leaving upgrade.pending until the new binary commits. The running process is not affected: the caller
// restarts it.
func (u *Updater) Install(ctx context.Context, version, sha string, logf func(string, ...any)) error {
	if err := Validate(version, sha); err != nil {
		return err
	}
	arch := u.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	base := strings.TrimRight(u.ReleaseBase, "/")
	if base == "" {
		base = DefaultReleaseBase
	}
	client := u.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	name := fmt.Sprintf("native-ops_%s_linux_%s", version, arch)
	url := fmt.Sprintf("%s/%s/%s.tar.gz", base, version, name)

	logf("downloading %s", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, res.StatusCode)
	}
	tgz, err := io.ReadAll(io.LimitReader(res.Body, maxDownload+1))
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if len(tgz) > maxDownload {
		return errors.New("download: the release is larger than expected")
	}
	sum := sha256.Sum256(tgz)
	if got := hex.EncodeToString(sum[:]); got != sha {
		return fmt.Errorf("checksum mismatch: the download's SHA-256 is %s, not the pinned %s; nothing was changed", got, sha)
	}
	logf("checksum matches the pinned %s", sha[:12])

	newPath := filepath.Join(u.Dir, BinaryName+".new")
	if err := extract(tgz, name, newPath); err != nil {
		return err
	}
	defer os.Remove(newPath) // gone after the rename; cleans up a failed attempt

	// The new binary must run here and say it is the version asked for.
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, newPath, "version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), version) {
		return fmt.Errorf("the new binary does not run here or is not %s (%v): %s; nothing was changed", version, err, strings.TrimSpace(string(out)))
	}
	logf("the new binary runs: %s", strings.TrimSpace(string(out)))

	cur := filepath.Join(u.Dir, BinaryName)
	prev := filepath.Join(u.Dir, PrevName)
	if err := copyFile(cur, prev); err != nil {
		return fmt.Errorf("keep the current binary as %s: %w", PrevName, err)
	}
	_ = os.Remove(filepath.Join(u.Dir, AttemptsName))
	if err := os.WriteFile(filepath.Join(u.Dir, PendingName), []byte(version+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(newPath, cur); err != nil {
		_ = os.Remove(filepath.Join(u.Dir, PendingName))
		return fmt.Errorf("swap in the new binary: %w", err)
	}
	logf("installed %s; the previous binary is kept as %s and is restored if the new one cannot stay up", version, PrevName)
	return nil
}

// Commit marks a pending upgrade done, once the new binary has served for CommitAfter. It is called at
// startup; it returns at once when there is nothing pending.
func Commit(ctx context.Context, dir string, logf func(string, ...any)) {
	pending := filepath.Join(dir, PendingName)
	if _, err := os.Stat(pending); err != nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(CommitAfter):
	}
	_ = os.Remove(pending)
	_ = os.Remove(filepath.Join(dir, AttemptsName))
	logf("upgrade committed: this binary has served for %s", CommitAfter)
}

// Guard is the unit's ExecStartPre: it counts the starts of a pending upgrade and, after MaxAttempts
// that did not commit, puts the previous binary back. It is a shell line so that it works whatever
// the new binary does.
func Guard(dir string) string {
	return fmt.Sprintf(`/bin/sh -c 'd=%s; if [ -f "$d/%s" ]; then n=$(cat "$d/%s" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$d/%s"; if [ "$n" -gt %d ] && [ -f "$d/%s" ]; then mv -f "$d/%s" "$d/%s"; rm -f "$d/%s" "$d/%s"; echo "native-ops: upgrade did not start %d times; restored the previous binary" >&2; fi; fi'`,
		dir, PendingName, AttemptsName, AttemptsName, MaxAttempts, PrevName, PrevName, BinaryName, PendingName, AttemptsName, MaxAttempts)
}

// GuardUnitLine is Guard written for a systemd unit, where a literal $ is $$.
func GuardUnitLine(dir string) string { return strings.ReplaceAll(Guard(dir), "$", "$$") }

func extract(tgz []byte, member, dest string) error {
	gz, err := gzip.NewReader(strings.NewReader(string(tgz)))
	if err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("unpack: the release has no %s", member)
		}
		if err != nil {
			return fmt.Errorf("unpack: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != member {
			continue
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, cerr := io.Copy(f, io.LimitReader(tr, maxDownload))
		if err := f.Close(); err != nil || cerr != nil {
			return fmt.Errorf("unpack: %v %v", cerr, err)
		}
		return nil
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
