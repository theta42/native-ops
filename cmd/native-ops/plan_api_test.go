package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/remote"
	"github.com/theta42/native-ops/pkg/server"
	"github.com/theta42/native-ops/pkg/status"
)

// tarGz packs a directory the way a CI job would: tar -czf - -C conf .
func tarGz(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		h, _ := tar.FileInfoHeader(info, "")
		h.Name = "./" + filepath.ToSlash(rel)
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			_, err = tw.Write(b)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// The real wiring, end to end: an uploaded conf tree is unpacked, planned by planSource against
// a (fake) incus, and answered, and nothing but reads reaches incus.
func TestThePlanEndpointPlansAnUploadedConfAgainstTheHost(t *testing.T) {
	logFile := fakeIncus(t, true, false)
	conf := confDir(t, freshService)

	dir := t.TempDir()
	tokens, err := server.OpenTokenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	secret, _, _ := tokens.Create("ci", server.RoleDeployer)
	srv, err := server.New(server.Options{
		Tokens: tokens, Version: "test",
		Status: func(ctx context.Context) (*status.Snapshot, error) { return &status.Snapshot{}, nil },
		Plan:   planSource(remote.NewLocalExecutor(), []byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/plan?sha=0123abc&service=web", bytes.NewReader(tarGz(t, conf)))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/gzip")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Exit int    `json:"exit"`
		Hash string `json:"hash"`
		Text string `json:"text"`
		Plan struct {
			Services []struct {
				Service string `json:"service"`
				Action  string `json:"action"`
			} `json:"services"`
		} `json:"plan"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil || res.StatusCode != 200 {
		t.Fatalf("%d %v", res.StatusCode, err)
	}
	if out.Exit != 2 || len(out.Hash) != 64 || len(out.Plan.Services) != 1 || out.Plan.Services[0].Action != "create" ||
		!strings.Contains(out.Text, "create-instance") || !strings.Contains(out.Text, "publish-route") {
		t.Fatalf("got %+v", out)
	}
	calls, _ := os.ReadFile(logFile)
	for _, verb := range []string{"launch", "delete", "stop", "start", "config set", "config device", "file push", "volume create", "snapshot", "exec", "reload"} {
		if strings.Contains(string(calls), verb) {
			t.Errorf("planning an upload ran %q against the host:\n%s", verb, calls)
		}
	}

	// A manifest whose env_file points outside the tree is refused, not read.
	os.WriteFile(filepath.Join(conf, "services", "web", "service.yml"), []byte("name: web\nimage: x\nenv_file: ../../../../../etc/passwd\n"), 0o644)
	req, _ = http.NewRequest("POST", ts.URL+"/v1/plan", bytes.NewReader(tarGz(t, conf)))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/gzip")
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	var e struct{ Code, Error string }
	json.NewDecoder(res2.Body).Decode(&e)
	if res2.StatusCode != 400 || e.Code != "bad_config" || !strings.Contains(e.Error, "env_file") {
		t.Fatalf("an env_file outside the tree must be a 400 bad_config, got %d %+v", res2.StatusCode, e)
	}
}
