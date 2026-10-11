package gitsource

import (
	"context"
	"encoding/json"
	"fmt"
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

const tagObject = "fedcba9876543210fedcba9876543210fedcba98"

// fakeGitHub serves the parts of GitHub's REST API a deploy reads: refs, annotated tags, rulesets and the
// tarball (a redirect to another host, as codeload is).
func fakeGitHub(t *testing.T, rulesets []map[string]any) *Source {
	t.Helper()
	codeload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest) // the token must not follow the redirect to another host
			return
		}
		_, _ = w.Write([]byte("tarball"))
	}))
	t.Cleanup(codeload.Close)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-read" || r.Header.Get("X-GitHub-Api-Version") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch p := r.URL.Path; {
		case p == "/api/v3/repos/acme/conf/git/ref/tags/deploy-2026.10.03": // a lightweight tag
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "refs/tags/deploy-2026.10.03", "object": map[string]any{"type": "commit", "sha": commit}})
		case p == "/api/v3/repos/acme/conf/git/ref/tags/deploy-annotated":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]any{"type": "tag", "sha": tagObject}})
		case p == "/api/v3/repos/acme/conf/git/tags/"+tagObject:
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]any{"type": "commit", "sha": commit}})
		case p == "/api/v3/repos/acme/conf/rulesets":
			list := []map[string]any{}
			for _, rs := range rulesets {
				list = append(list, map[string]any{"id": rs["id"], "name": rs["name"], "target": rs["target"], "enforcement": rs["enforcement"]})
			}
			_ = json.NewEncoder(w).Encode(list)
		case strings.HasPrefix(p, "/api/v3/repos/acme/conf/rulesets/"):
			for _, rs := range rulesets {
				if p == "/api/v3/repos/acme/conf/rulesets/"+fmt.Sprint(rs["id"]) {
					_ = json.NewEncoder(w).Encode(rs)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		case p == "/api/v3/repos/acme/conf/tarball/"+commit:
			http.Redirect(w, r, codeload.URL+"/acme/conf/legacy.tar.gz/"+commit+"?token=short-lived", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return &Source{BaseURL: ts.URL, Provider: GitHub, Repo: "acme/conf", TagPattern: "deploy-*", Token: func() string { return "gh-read" }}
}

func ruleset(id int, target, enforcement string, include []string, rules ...string) map[string]any {
	rs := []map[string]any{}
	for _, r := range rules {
		rs = append(rs, map[string]any{"type": r})
	}
	return map[string]any{"id": id, "name": fmt.Sprintf("rs%d", id), "target": target, "enforcement": enforcement,
		"conditions": map[string]any{"ref_name": map[string]any{"include": include, "exclude": []string{}}}, "rules": rs}
}

func TestGitHubResolvesChecksAndFetchesAProtectedTag(t *testing.T) {
	ctx := context.Background()
	s := fakeGitHub(t, []map[string]any{
		ruleset(1, "branch", "active", []string{"~DEFAULT_BRANCH"}, "creation", "update"),
		ruleset(2, "tag", "active", []string{"refs/tags/deploy-*"}, "creation", "update", "deletion"),
	})
	for _, tag := range []string{"deploy-2026.10.03", "deploy-annotated"} {
		if sha, err := s.Commit(ctx, tag); err != nil || sha != commit {
			t.Fatalf("commit of %s: %q %v", tag, sha, err)
		}
	}
	if rule, err := s.Protected(ctx, "deploy-2026.10.03"); err != nil || rule != "ruleset rs2" {
		t.Fatalf("protected: %q %v", rule, err)
	}
	rc, err := s.Archive(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "tarball" {
		t.Fatalf("archive: %q", b)
	}
	if _, err := s.Commit(ctx, "deploy-missing"); err == nil {
		t.Fatal("a tag GitHub does not have must be an error")
	}
}

// Only a ruleset that is enforced, covers the tag, and stops both making and moving it, protects it.
func TestGitHubRulesetsThatDoNotProtectTheTag(t *testing.T) {
	ctx := context.Background()
	for name, rs := range map[string]map[string]any{
		"evaluate mode":       ruleset(3, "tag", "evaluate", []string{"refs/tags/deploy-*"}, "creation", "update"),
		"disabled":            ruleset(3, "tag", "disabled", []string{"refs/tags/deploy-*"}, "creation", "update"),
		"other tags":          ruleset(3, "tag", "active", []string{"refs/tags/v*"}, "creation", "update"),
		"creation only":       ruleset(3, "tag", "active", []string{"refs/tags/deploy-*"}, "creation"),
		"deletion only":       ruleset(3, "tag", "active", []string{"refs/tags/deploy-*"}, "deletion"),
		"a branch ruleset":    ruleset(3, "branch", "active", []string{"refs/tags/deploy-*"}, "creation", "update"),
		"star stops at slash": ruleset(3, "tag", "active", []string{"refs/*"}, "creation", "update"),
	} {
		s := fakeGitHub(t, []map[string]any{rs})
		if _, err := s.Protected(ctx, "deploy-2026.10.03"); err == nil || !strings.Contains(err.Error(), "not protected") {
			t.Errorf("%s must not protect the tag: %v", name, err)
		}
	}
	for _, include := range [][]string{{"~ALL"}, {"refs/tags/**"}, {"refs/**"}, {"deploy-*"}} {
		s := fakeGitHub(t, []map[string]any{ruleset(4, "tag", "active", include, "creation", "update")})
		if _, err := s.Protected(ctx, "deploy-2026.10.03"); err != nil {
			t.Errorf("include %v must protect the tag: %v", include, err)
		}
	}
	excluded := ruleset(5, "tag", "active", []string{"~ALL"}, "creation", "update")
	excluded["conditions"].(map[string]any)["ref_name"].(map[string]any)["exclude"] = []string{"refs/tags/deploy-*"}
	if _, err := fakeGitHub(t, []map[string]any{excluded}).Protected(ctx, "deploy-2026.10.03"); err == nil {
		t.Error("an excluded tag is not protected")
	}
	s := fakeGitHub(t, nil)
	s.Token = func() string { return "wrong" }
	if _, err := s.Protected(ctx, "deploy-2026.10.03"); err == nil {
		t.Fatal("a token that cannot read the rulesets must fail, never pass")
	}
}

func TestTheProviderAndAPIFollowTheURL(t *testing.T) {
	for _, c := range []struct{ url, provider, want, api string }{
		{"https://github.com", "", GitHub, "https://api.github.com"},
		{"https://git.example.com", "", Gitea, "https://git.example.com/api/v1"},
		{"https://ghe.example.com/", GitHub, GitHub, "https://ghe.example.com/api/v3"},
	} {
		s := Source{BaseURL: c.url, Provider: c.provider, Repo: "acme/conf", TagPattern: "deploy-*"}
		if s.provider() != c.want || s.apiBase() != c.api || s.Validate() != nil {
			t.Errorf("%s (%q): %s %s %v", c.url, c.provider, s.provider(), s.apiBase(), s.Validate())
		}
	}
	if err := (&Source{BaseURL: "https://x", Provider: "gitlab", Repo: "a/b", TagPattern: "d-*"}).Validate(); err == nil {
		t.Error("an unknown provider must be refused")
	}
}
