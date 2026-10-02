package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func newHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKnownHostsTrustsOnFirstUseAndRefusesAChangedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh", "known_hosts")
	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 22}
	first, second := newHostKey(t), newHostKey(t)

	cb, err := knownHostsCallback(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("203.0.113.7:22", addr, first); err != nil {
		t.Fatalf("first use must be accepted and recorded: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "203.0.113.7") {
		t.Fatalf("the key was not recorded: %q", b)
	}
	// A new callback re-reads the file, as the next run would.
	cb, _ = knownHostsCallback(path, false)
	if err := cb("203.0.113.7:22", addr, first); err != nil {
		t.Fatalf("the recorded key must be accepted: %v", err)
	}
	if err := cb("203.0.113.7:22", addr, second); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a changed key must be refused: %v", err)
	}
}

func TestKnownHostsStrictRefusesAnUnknownHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	cb, err := knownHostsCallback(path, true)
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.8"), Port: 22}
	if err := cb("203.0.113.8:22", addr, newHostKey(t)); err == nil {
		t.Fatal("strict mode must refuse a host it does not know")
	}
	if b, _ := os.ReadFile(path); len(b) != 0 {
		t.Fatalf("strict mode must not record anything: %q", b)
	}
}

func TestKnownAlgorithmsAreTheRecordedTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	ed := newHostKey(t)
	line := knownhosts.Line([]string{knownhosts.Normalize("203.0.113.7:22")}, ed)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The server is asked for the type on record, so it does not answer with, say, its ECDSA key
	// (which the check would take for a changed key).
	if got := knownAlgorithms(path, "203.0.113.7:22"); len(got) != 1 || got[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("algorithms for a host with an ed25519 key on record: %v", got)
	}
	if got := knownAlgorithms(path, "198.51.100.1:22"); got != nil {
		t.Fatalf("an unknown host has no preference: %v", got)
	}
}
