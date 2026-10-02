package gitsource

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

func fakeGitea(t *testing.T, protections []string) *Source {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token git-read" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/repos/acme/conf/tags/deploy-2026.10.03":
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "deploy-2026.10.03", "commit": map[string]any{"sha": commit}})
		case "/api/v1/repos/acme/conf/tag_protections":
			rules := []map[string]any{}
			for _, p := range protections {
				rules = append(rules, map[string]any{"name_pattern": p})
			}
			_ = json.NewEncoder(w).Encode(rules)
		case "/api/v1/repos/acme/conf/archive/" + commit + ".tar.gz":
			_, _ = w.Write([]byte("tarball"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return &Source{BaseURL: ts.URL, Repo: "acme/conf", TagPattern: "deploy-*", Token: func() string { return "git-read" }}
}

func TestResolveCheckAndFetchAProtectedTag(t *testing.T) {
	ctx := context.Background()
	for _, rule := range []string{"deploy-*", "/^deploy-[0-9.]+$/"} {
		s := fakeGitea(t, []string{"platform-v*", rule})
		sha, err := s.Commit(ctx, "deploy-2026.10.03")
		if err != nil || sha != commit {
			t.Fatalf("commit: %q %v", sha, err)
		}
		if got, err := s.Protected(ctx, "deploy-2026.10.03"); err != nil || got != rule {
			t.Fatalf("rule %q must protect the tag: %q %v", rule, got, err)
		}
		rc, err := s.Archive(ctx, sha)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if string(b) != "tarball" {
			t.Fatalf("archive: %q", b)
		}
	}
}

func TestAnUnprotectedOrMismatchedTagDoesNotDeploy(t *testing.T) {
	ctx := context.Background()
	s := fakeGitea(t, []string{"platform-v*"})
	if _, err := s.Protected(ctx, "deploy-2026.10.03"); err == nil || !strings.Contains(err.Error(), "not protected") {
		t.Fatalf("an unprotected tag must not deploy: %v", err)
	}
	for _, tag := range []string{"v1.0.0", "deploy-x/../../etc", "-rf"} {
		if _, err := s.Commit(ctx, tag); err == nil {
			t.Errorf("%q must be refused", tag)
		}
	}
	if _, err := s.Commit(ctx, "deploy-missing"); err == nil {
		t.Fatal("a tag the git server does not have must be an error")
	}
	s.Token = func() string { return "" }
	if _, err := s.Commit(ctx, "deploy-2026.10.03"); err == nil || !strings.Contains(err.Error(), "NATIVE_OPS_GIT_TOKEN") {
		t.Fatalf("no token must say what to sync: %v", err)
	}
	s.Token = func() string { return "wrong" }
	if _, err := s.Protected(ctx, "deploy-2026.10.03"); err == nil {
		t.Fatal("a token that cannot read the rules must fail, never pass")
	}
}

func TestValidate(t *testing.T) {
	good := Source{BaseURL: "https://git.example.com", Repo: "acme/conf", TagPattern: "deploy-*"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Source{
		{BaseURL: "ftp://x", Repo: "acme/conf", TagPattern: "deploy-*"},
		{BaseURL: "https://x", Repo: "conf", TagPattern: "deploy-*"},
		{BaseURL: "https://x", Repo: "acme/conf", TagPattern: "["},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
}
