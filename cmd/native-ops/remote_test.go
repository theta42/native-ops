package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/theta42/native-ops/pkg/server"
)

func TestPackTreeIsAcceptedByTheDaemonsExtractor(t *testing.T) {
	src := t.TempDir()
	for name, body := range map[string]string{
		"fleet.yml":                       "name: prod\n",
		"services/web/service.yml":        "image: x\n",
		"edge/Caddyfile":                  "import /etc/caddy/sites/*.caddy\n",
		".git/config":                     "[core]\n",
		"templates/platform/template.yml": "name: platform\n",
	} {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := packTree(src)
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := server.ExtractTarGz(bytes.NewReader(b), dest); err != nil {
		t.Fatalf("the daemon refuses what remote sends: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "services", "web", "service.yml")); string(got) != "image: x\n" {
		t.Fatalf("content lost: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git must not be sent")
	}
}

func TestPackTreeRefusesASymlink(t *testing.T) {
	src := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "passwd")); err != nil {
		t.Skip(err)
	}
	if _, err := packTree(src); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlink must be an error: %v", err)
	}
}

func TestWaitFollowsAJobToItsEnd(t *testing.T) {
	var polls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/v1/jobs/j-1-abcdef12" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if polls.Add(1) < 2 {
			_, _ = w.Write([]byte(`{"status":"running"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"failed","error":"boom","log":"step 1\n"}`))
	}))
	defer ts.Close()
	c := &remoteClient{base: ts.URL, token: "tok", http: ts.Client()}
	if code := c.wait(context.Background(), "j-1-abcdef12"); code != 1 {
		t.Fatalf("a failed job must exit 1, got %d", code)
	}
	if polls.Load() != 2 {
		t.Fatalf("polled %d times", polls.Load())
	}
}
