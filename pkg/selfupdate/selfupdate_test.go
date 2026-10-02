package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// release builds a release tarball whose binary is a shell script answering `version` with v.
func release(t *testing.T, member, v string) ([]byte, string) {
	t.Helper()
	script := "#!/bin/sh\necho \"native-ops " + v + " (test)\"\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(script))
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func setup(t *testing.T, version string) (*Updater, string, string) {
	t.Helper()
	member := "native-ops_" + version + "_linux_amd64"
	tgz, sum := release(t, member, version)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+version+"/"+member+".tar.gz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, BinaryName), []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Updater{Dir: dir, ReleaseBase: ts.URL, Arch: "amd64"}, dir, sum
}

func nolog(string, ...any) {}

func TestInstallSwapsInAVerifiedReleaseAndKeepsThePrevious(t *testing.T) {
	u, dir, sum := setup(t, "v9.9.9")
	if err := u.Install(context.Background(), "v9.9.9", sum, nolog); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command(filepath.Join(dir, BinaryName), "version").Output()
	if !strings.Contains(string(out), "v9.9.9") {
		t.Fatalf("the new binary is not in place: %q", out)
	}
	if prev, _ := os.ReadFile(filepath.Join(dir, PrevName)); !strings.Contains(string(prev), "echo old") {
		t.Fatal("the previous binary must be kept")
	}
	if _, err := os.Stat(filepath.Join(dir, PendingName)); err != nil {
		t.Fatal("the upgrade must be pending until the new binary commits")
	}
}

func TestInstallRefusesAndChangesNothing(t *testing.T) {
	for name, tc := range map[string]struct{ version, sha string }{
		"wrong checksum":  {"v9.9.9", strings.Repeat("0", 64)},
		"bad version":     {"latest", strings.Repeat("0", 64)},
		"bad checksum":    {"v9.9.9", "abc"},
		"missing release": {"v9.9.8", strings.Repeat("0", 64)},
	} {
		t.Run(name, func(t *testing.T) {
			u, dir, _ := setup(t, "v9.9.9")
			if err := u.Install(context.Background(), tc.version, tc.sha, nolog); err == nil {
				t.Fatal("must be refused")
			}
			if cur, _ := os.ReadFile(filepath.Join(dir, BinaryName)); !strings.Contains(string(cur), "echo old") {
				t.Fatal("the binary must be untouched")
			}
			for _, f := range []string{PrevName, PendingName, BinaryName + ".new"} {
				if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
					t.Fatalf("%s must not be left behind", f)
				}
			}
		})
	}
}

func TestInstallRefusesABinaryThatIsNotTheVersionAskedFor(t *testing.T) {
	// The tarball is named v9.9.9 but its binary says something else.
	member := "native-ops_v9.9.9_linux_amd64"
	tgz, sum := release(t, member, "v1.0.0")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(tgz) }))
	defer ts.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, BinaryName), []byte("#!/bin/sh\necho old\n"), 0o755)
	u := &Updater{Dir: dir, ReleaseBase: ts.URL, Arch: "amd64"}
	if err := u.Install(context.Background(), "v9.9.9", sum, nolog); err == nil || !strings.Contains(err.Error(), "not v9.9.9") {
		t.Fatalf("a binary that is not the version asked for must be refused: %v", err)
	}
}

func TestGuardRestoresThePreviousBinaryAfterRepeatedFailedStarts(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) { _ = os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755) }
	write(BinaryName, "new")
	write(PrevName, "old")
	write(PendingName, "v9.9.9\n")
	start := func() {
		cmd := exec.Command("/bin/sh", "-c", strings.TrimSuffix(strings.TrimPrefix(Guard(dir), "/bin/sh -c '"), "'"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("guard: %v %s", err, out)
		}
	}
	for i := 1; i <= MaxAttempts; i++ {
		start()
		if b, _ := os.ReadFile(filepath.Join(dir, BinaryName)); string(b) != "new" {
			t.Fatalf("start %d: the new binary must get %d tries", i, MaxAttempts)
		}
	}
	start()
	if b, _ := os.ReadFile(filepath.Join(dir, BinaryName)); string(b) != "old" {
		t.Fatal("after too many failed starts the previous binary must be back")
	}
	if _, err := os.Stat(filepath.Join(dir, PendingName)); err == nil {
		t.Fatal("the rollback must clear the pending upgrade")
	}
	// With nothing pending the guard does nothing.
	start()
	if b, _ := os.ReadFile(filepath.Join(dir, BinaryName)); string(b) != "old" {
		t.Fatal("the guard must leave a committed binary alone")
	}
}

func TestCommitClearsThePendingUpgradeOnceTheBinaryHasServed(t *testing.T) {
	CommitAfter = 10 * time.Millisecond
	defer func() { CommitAfter = 30 * time.Second }()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, PendingName), []byte("v9.9.9\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, AttemptsName), []byte("2\n"), 0o600)
	Commit(context.Background(), dir, nolog)
	for _, f := range []string{PendingName, AttemptsName} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			t.Fatalf("%s must be gone after a commit", f)
		}
	}
}

func TestTheShippedUnitCarriesTheGuard(t *testing.T) {
	unit, err := os.ReadFile(filepath.Join("..", "..", "deploy", "systemd", "native-ops-serve.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "ExecStartPre="+GuardUnitLine("/var/lib/native-ops/bin")+"\n") {
		t.Fatal("deploy/systemd/native-ops-serve.service must have ExecStartPre=GuardUnitLine(\"/var/lib/native-ops/bin\")")
	}
	if !strings.Contains(string(unit), "ExecStart=/var/lib/native-ops/bin/native-ops serve") {
		t.Fatal("the unit must start the binary in the state directory, which the daemon can replace")
	}
}
