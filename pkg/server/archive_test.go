package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name string
	typ  byte
	body string
	link string
	mode int64
}

func tgz(t testing.TB, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// sandbox is an empty destination inside a parent that must stay empty apart from it: whatever
// an archive tries, nothing may appear beside dest.
func sandbox(t *testing.T) (parent, dest string) {
	t.Helper()
	parent = t.TempDir()
	dest = filepath.Join(parent, "dest")
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	return parent, dest
}

func assertNothingOutside(t *testing.T, parent string) {
	t.Helper()
	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 || entries[0].Name() != "dest" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("something was written outside the destination: %v", names)
	}
}

func TestExtractUnpacksAConfigTreeWithSafePermissions(t *testing.T) {
	parent, dest := sandbox(t)
	data := tgz(t,
		entry{name: "./", typ: tar.TypeDir},
		entry{name: "./fleet.yml", body: "name: f\n", mode: 0o4777}, // setuid + world writable in the archive
		entry{name: "services/", typ: tar.TypeDir, mode: 0o777},
		entry{name: "services/web/service.yml", body: "image: x\n"}, // parent directory not listed
	)
	if err := ExtractTarGz(bytes.NewReader(data), dest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "services", "web", "service.yml"))
	if err != nil || string(b) != "image: x\n" {
		t.Fatalf("content: %q %v", b, err)
	}
	for _, p := range []string{"fleet.yml", "services/web/service.yml"} {
		st, _ := os.Stat(filepath.Join(dest, p))
		if st.Mode().Perm() != 0o600 || st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			t.Errorf("%s has mode %v: the archive's permission bits mean nothing here", p, st.Mode())
		}
	}
	if st, _ := os.Stat(filepath.Join(dest, "services")); st.Mode().Perm() != 0o700 {
		t.Errorf("directories are 0700, got %v", st.Mode().Perm())
	}
	assertNothingOutside(t, parent)
}

func TestExtractRefusesEverythingThatCouldReachOutsideTheTree(t *testing.T) {
	hostile := map[string][]entry{
		"climbs with ..":                       {{name: "../evil", body: "x"}},
		"climbs in the middle":                 {{name: "a/../../evil", body: "x"}},
		"climbs from a dot prefix":             {{name: "./../evil", body: "x"}},
		"absolute path":                        {{name: "/tmp/evil", body: "x"}},
		"symlink out":                          {{name: "link", typ: tar.TypeSymlink, link: "/etc"}},
		"symlink relative out":                 {{name: "link", typ: tar.TypeSymlink, link: "../../.."}},
		"hard link":                            {{name: "hard", typ: tar.TypeLink, link: "/etc/passwd"}},
		"symlink then a write through it":      {{name: "dir", typ: tar.TypeSymlink, link: ".."}, {name: "dir/evil", body: "x"}},
		"character device":                     {{name: "dev", typ: tar.TypeChar}},
		"block device":                         {{name: "blk", typ: tar.TypeBlock}},
		"fifo":                                 {{name: "pipe", typ: tar.TypeFifo}},
		"backslash in a name":                  {{name: `..\evil`, body: "x"}},
		"the same file twice":                  {{name: "a", body: "1"}, {name: "a", body: "2"}},
		"a file where a directory was created": {{name: "d/f", body: "1"}, {name: "d", body: "2"}},
		"a directory where a file was":         {{name: "d", body: "1"}, {name: "d/f", body: "2"}},
		"a directory over a file":              {{name: "d", body: "1"}, {name: "d", typ: tar.TypeDir}},
	}
	for name, entries := range hostile {
		t.Run(name, func(t *testing.T) {
			parent, dest := sandbox(t)
			err := ExtractTarGz(bytes.NewReader(tgz(t, entries...)), dest)
			var ae *ArchiveError
			if !errors.As(err, &ae) {
				t.Fatalf("want an ArchiveError, got %v", err)
			}
			if strings.Contains(ae.Error(), parent) {
				t.Errorf("the message must not reveal a path on this host: %s", ae.Error())
			}
			assertNothingOutside(t, parent)
			if _, err := os.Lstat(filepath.Join(dest, "link")); err == nil {
				t.Error("a link must never be created")
			}
		})
	}
}

func TestExtractIsBoundedInEntriesAndInExpandedSize(t *testing.T) {
	defer func(f int, e, a int64) { maxArchiveFiles, maxExtractedBytes, maxArchiveBytes = f, e, a }(maxArchiveFiles, maxExtractedBytes, maxArchiveBytes)
	maxArchiveFiles, maxExtractedBytes = 10, 1<<20

	var many []entry
	for i := 0; i < 11; i++ {
		many = append(many, entry{name: "f" + string(rune('a'+i)), body: "x"})
	}
	_, dest := sandbox(t)
	if err := ExtractTarGz(bytes.NewReader(tgz(t, many...)), dest); err == nil || !strings.Contains(err.Error(), "more than 10 entries") {
		t.Fatalf("too many entries: %v", err)
	}

	// A bomb: a few hundred bytes compressed, far more than the limit expanded.
	bomb := tgz(t, entry{name: "zeros", body: strings.Repeat("\x00", 4<<20)})
	if len(bomb) > 8<<10 {
		t.Fatalf("test setup: the bomb should compress well, got %d bytes", len(bomb))
	}
	_, dest = sandbox(t)
	if err := ExtractTarGz(bytes.NewReader(bomb), dest); err == nil || !strings.Contains(err.Error(), "expands to more than") {
		t.Fatalf("expansion: %v", err)
	}
	if st, err := os.Stat(filepath.Join(dest, "zeros")); err == nil && st.Size() > maxExtractedBytes+1 {
		t.Fatalf("the bomb wrote %d bytes before it was stopped", st.Size())
	}

	// Split over many files: the total is what counts.
	var spread []entry
	for i := 0; i < 5; i++ {
		spread = append(spread, entry{name: "z" + string(rune('a'+i)), body: strings.Repeat("\x00", 300<<10)})
	}
	_, dest = sandbox(t)
	if err := ExtractTarGz(bytes.NewReader(tgz(t, spread...)), dest); err == nil {
		t.Fatal("the total across files must be bounded")
	}

	// The compressed upload itself.
	maxArchiveBytes = 512
	rnd := make([]byte, 4096)
	rand.New(rand.NewSource(1)).Read(rnd) // incompressible
	_, dest = sandbox(t)
	if err := ExtractTarGz(bytes.NewReader(tgz(t, entry{name: "big", body: string(rnd)})), dest); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("upload size: %v", err)
	}
}

func TestExtractRejectsAnythingThatIsNotACompleteTarGz(t *testing.T) {
	good := tgz(t, entry{name: "fleet.yml", body: "name: f\n"})
	cases := map[string][]byte{
		"empty":            {},
		"plain text":       []byte("name: f\n"),
		"gzip of garbage":  gz(t, []byte("not a tar archive at all, just text that is long enough to look plausible")),
		"truncated":        good[:len(good)/2],
		"a plain tar":      plainTar(t),
		"zip magic number": []byte("PK\x03\x04junk"),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, dest := sandbox(t)
			err := ExtractTarGz(bytes.NewReader(data), dest)
			var ae *ArchiveError
			if !errors.As(err, &ae) {
				t.Fatalf("want an ArchiveError, got %v", err)
			}
		})
	}
}

func gz(t testing.TB, b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func plainTar(t testing.TB) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "fleet.yml", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.Close()
	return buf.Bytes()
}
