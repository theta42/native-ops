package serveflags

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNoSetMeansTheUnitsFlags(t *testing.T) {
	l, err := Load(t.TempDir())
	if err != nil || l.Managed || l.Pending {
		t.Fatalf("an empty state dir: %+v %v", l, err)
	}
}

func TestAStagedSetIsTriedThenCommitted(t *testing.T) {
	dir := t.TempDir()
	if err := Stage(dir, "  --enable-apply   --image-prefix opsavor- "); err != nil {
		t.Fatal(err)
	}
	l, err := Load(dir)
	if err != nil || !l.Managed || !l.Pending || l.Text != "--enable-apply --image-prefix opsavor-" ||
		!reflect.DeepEqual(l.Args, []string{"--enable-apply", "--image-prefix", "opsavor-"}) {
		t.Fatalf("a staged set: %+v %v", l, err)
	}
	old := CommitAfter
	CommitAfter = time.Millisecond
	t.Cleanup(func() { CommitAfter = old })
	var logged []string
	Commit(context.Background(), dir, func(f string, a ...any) { logged = append(logged, f) })
	if _, err := os.Stat(filepath.Join(dir, PendingName)); !os.IsNotExist(err) {
		t.Fatal("a committed set is no longer pending")
	}
	if l, _ := Load(dir); !l.Managed || l.Pending || l.Text != "--enable-apply --image-prefix opsavor-" || len(logged) != 1 {
		t.Fatalf("after the commit: %+v %v", l, logged)
	}
}

// A set that keeps the daemon from starting is given up after MaxAttempts starts, and the daemon goes back
// to what it ran before: a bad flag never leaves the host without its daemon.
func TestASetThatDoesNotStartIsGivenUp(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, FileName), []byte("--enable-apply\n"), 0o600)
	_ = Stage(dir, "--no-such-flag")
	for i := 1; i <= MaxAttempts; i++ {
		if l, _ := Load(dir); !l.Pending || l.Text != "--no-such-flag" {
			t.Fatalf("start %d tries the pending set: %+v", i, l)
		}
	}
	l, err := Load(dir)
	if err != nil || l.Pending || l.Text != "--enable-apply" || !strings.Contains(l.Note, "did not start") {
		t.Fatalf("after %d failed starts: %+v %v", MaxAttempts, l, err)
	}
	if _, err := os.Stat(filepath.Join(dir, PendingName)); !os.IsNotExist(err) {
		t.Fatal("the failed set is dropped")
	}
}

func TestAnEmptySetGoesBackToTheUnitsFlags(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, FileName), []byte("--enable-apply\n"), 0o600)
	_ = Stage(dir, "")
	if l, _ := Load(dir); l.Managed || !l.Pending {
		t.Fatalf("an empty staged set means the unit's flags: %+v", l)
	}
	old := CommitAfter
	CommitAfter = time.Millisecond
	t.Cleanup(func() { CommitAfter = old })
	Commit(context.Background(), dir, func(string, ...any) {})
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatal("committing an empty set removes the kept one")
	}
}

func TestValueAndMerge(t *testing.T) {
	args := []string{"--addr", "10.0.0.1:8686", "-state-dir=/srv/no", "--enable-apply"}
	if v, ok := Value(args, "addr"); !ok || v != "10.0.0.1:8686" {
		t.Fatalf("addr: %q %v", v, ok)
	}
	if v, ok := Value(args, "state-dir"); !ok || v != "/srv/no" {
		t.Fatalf("state-dir: %q %v", v, ok)
	}
	if _, ok := Value(args, "image-prefix"); ok {
		t.Fatal("a flag that is not there")
	}
	unit := []string{"--addr", "10.0.0.1:8686", "--state-dir", "/var/lib/native-ops", "--enable-edge-apply", "--enable-apply"}
	got := Merge(unit, []string{"--enable-apply", "--image-prefix", "x-"}, "/var/lib/native-ops")
	want := []string{"--addr", "10.0.0.1:8686", "--enable-apply", "--image-prefix", "x-", "--state-dir", "/var/lib/native-ops"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the unit's addr and state dir, and only the set's flags: %v", got)
	}
	// A set with its own --addr keeps it; whatever it says about the state dir, the unit's comes last.
	got = Merge(unit, []string{"--addr", "127.0.0.1:1", "--state-dir", "/tmp/evil"}, "/var/lib/native-ops")
	if v, _ := Value(got, "addr"); v != "127.0.0.1:1" {
		t.Fatalf("the set's addr: %v", got)
	}
	if v, _ := Value(got, "state-dir"); v != "/var/lib/native-ops" {
		t.Fatalf("the unit's state dir wins: %v", got)
	}
}
