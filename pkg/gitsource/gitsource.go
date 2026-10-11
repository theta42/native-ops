// Package gitsource reads a configuration repository's deploy tags from its git server (Gitea, or GitHub
// and GitHub Enterprise Server), so the daemon can deploy a tagged commit without trusting anything a CI
// job uploads: it resolves the tag itself, checks that the tag is protected (only the people the
// repository allows can push it), and downloads that commit's archive from the git server.
package gitsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// Source is one repository on a git server and the tags that deploy it.
type Source struct {
	BaseURL    string        // e.g. https://git.example.com, or https://github.com
	Repo       string        // owner/name
	TagPattern string        // glob of the tags that deploy, e.g. deploy-*
	Token      func() string // a token that can read the repository and its tag protections (or rulesets)
	// Provider is "gitea" or "github" (GitHub, or a GitHub Enterprise Server at BaseURL). Empty means
	// github for github.com, else gitea.
	Provider string
	HTTP     *http.Client
}

// Providers.
const (
	Gitea  = "gitea"
	GitHub = "github"
)

// provider is the API Source speaks.
func (s *Source) provider() string {
	if s.Provider != "" {
		return s.Provider
	}
	if u, err := url.Parse(s.BaseURL); err == nil && (u.Host == "github.com" || u.Host == "api.github.com") {
		return GitHub
	}
	return Gitea
}

var (
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	tagRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,100}$`)
	shaRe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Validate checks the configuration.
func (s *Source) Validate() error {
	u, err := url.Parse(s.BaseURL)
	if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
		return fmt.Errorf("git server URL %q is not an http(s) URL", s.BaseURL)
	}
	if !repoRe.MatchString(s.Repo) {
		return fmt.Errorf("deploy repository %q is not owner/name", s.Repo)
	}
	if _, err := path.Match(s.TagPattern, ""); err != nil || s.TagPattern == "" {
		return fmt.Errorf("deploy tag pattern %q is not a valid glob", s.TagPattern)
	}
	if p := s.provider(); p != Gitea && p != GitHub {
		return fmt.Errorf("git provider %q is not gitea or github", s.Provider)
	}
	return nil
}

// Commit resolves a deploy tag to the commit it points at. The tag must match the deploy pattern.
func (s *Source) Commit(ctx context.Context, tag string) (string, error) {
	if !tagRe.MatchString(tag) || strings.Contains(tag, "..") {
		return "", fmt.Errorf("%q is not a tag name", tag)
	}
	if ok, _ := path.Match(s.TagPattern, tag); !ok {
		return "", fmt.Errorf("tag %s does not match the deploy tags (%s)", tag, s.TagPattern)
	}
	if s.provider() == GitHub {
		return s.githubCommit(ctx, tag)
	}
	var t struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := s.get(ctx, "/tags/"+url.PathEscape(tag), &t); err != nil {
		return "", fmt.Errorf("look up tag %s: %w", tag, err)
	}
	if !shaRe.MatchString(t.Commit.SHA) {
		return "", fmt.Errorf("tag %s: the git server named no commit", tag)
	}
	return t.Commit.SHA, nil
}

// Protected reports whether a protection rule covers the tag, so that only the people the repository
// allows could have pushed it. It returns the rule's pattern. A token that cannot read the rules (its
// user is not an admin of the repository) is an error, never a pass.
func (s *Source) Protected(ctx context.Context, tag string) (string, error) {
	if s.provider() == GitHub {
		return s.githubProtected(ctx, tag)
	}
	var rules []struct {
		NamePattern string `json:"name_pattern"`
	}
	if err := s.get(ctx, "/tag_protections", &rules); err != nil {
		return "", fmt.Errorf("read the tag protections of %s (the token's user must be an admin of the repository): %w", s.Repo, err)
	}
	for _, r := range rules {
		if matchProtection(r.NamePattern, tag) {
			return r.NamePattern, nil
		}
	}
	return "", fmt.Errorf("tag %s is not protected in %s: anyone who can push to the repository could create it, so it does not deploy. Protect the deploy tags (repository settings, tags) first", tag, s.Repo)
}

// matchProtection is Gitea's tag protection matching: /regex/, or a glob.
func matchProtection(pattern, tag string) bool {
	if len(pattern) > 2 && strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") {
		re, err := regexp.Compile(pattern[1 : len(pattern)-1])
		return err == nil && re.MatchString(tag)
	}
	ok, _ := path.Match(pattern, tag)
	return ok
}

// Archive downloads the commit's tree as a gzip-compressed tar; the caller closes it.
func (s *Source) Archive(ctx context.Context, sha string) (io.ReadCloser, error) {
	if !shaRe.MatchString(sha) {
		return nil, fmt.Errorf("%q is not a commit", sha)
	}
	p := "/archive/" + sha + ".tar.gz"
	if s.provider() == GitHub {
		p = "/tarball/" + sha // a redirect to codeload, whose URL carries its own short-lived access
	}
	res, err := s.do(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("download commit %s: %w", sha[:12], err)
	}
	return res.Body, nil
}

func (s *Source) get(ctx context.Context, p string, out any) error {
	res, err := s.do(ctx, p)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
}

func (s *Source) do(ctx context.Context, p string) (*http.Response, error) {
	tok := ""
	if s.Token != nil {
		tok = s.Token()
	}
	if tok == "" {
		return nil, errors.New("no git server token on this daemon (sync NATIVE_OPS_GIT_TOKEN)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiBase()+"/repos/"+s.Repo+p, nil)
	if err != nil {
		return nil, err
	}
	if s.provider() == GitHub {
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	} else {
		req.Header.Set("Authorization", "token "+tok)
	}
	client := http.Client{Timeout: 2 * time.Minute}
	if s.HTTP != nil {
		client = *s.HTTP
	}
	// GitHub answers an archive with a redirect to another host (codeload), whose URL carries its own
	// access: the token never goes anywhere but the API it was given for.
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if next.URL.Host != via[0].URL.Host {
			next.Header.Del("Authorization")
		}
		return nil
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		res.Body.Close()
		return nil, fmt.Errorf("the git server answered %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return res, nil
}

// apiBase is where the repository API is: Gitea's /api/v1, api.github.com, or a GitHub Enterprise
// Server's /api/v3.
func (s *Source) apiBase() string {
	base := strings.TrimRight(s.BaseURL, "/")
	if s.provider() != GitHub {
		return base + "/api/v1"
	}
	if u, err := url.Parse(base); err == nil && (u.Host == "github.com" || u.Host == "api.github.com") {
		return "https://api.github.com"
	}
	return base + "/api/v3"
}

// githubCommit resolves a tag through the git refs API (an exact ref, so a branch of the same name is
// never taken for it), following an annotated tag to its commit.
func (s *Source) githubCommit(ctx context.Context, tag string) (string, error) {
	type object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	var ref struct {
		Object object `json:"object"`
	}
	if err := s.get(ctx, "/git/ref/tags/"+url.PathEscape(tag), &ref); err != nil {
		return "", fmt.Errorf("look up tag %s: %w", tag, err)
	}
	obj := ref.Object
	for i := 0; obj.Type == "tag" && i < 4; i++ { // an annotated tag (a tag of a tag, at most a few deep)
		if !shaRe.MatchString(obj.SHA) {
			break
		}
		var t struct {
			Object object `json:"object"`
		}
		if err := s.get(ctx, "/git/tags/"+obj.SHA, &t); err != nil {
			return "", fmt.Errorf("look up tag %s: %w", tag, err)
		}
		obj = t.Object
	}
	if obj.Type != "commit" || !shaRe.MatchString(obj.SHA) {
		return "", fmt.Errorf("tag %s: the git server named no commit", tag)
	}
	return obj.SHA, nil
}

// githubProtected finds an active tag ruleset (the repository's, or its organization's) that covers the
// tag and restricts both creating and updating it: only the ruleset's bypass list can then make the tag,
// or move it to another commit. A ruleset in evaluate mode, or one that only restricts deletion, does not
// count. The token needs to read the repository's rulesets (fine-grained: Metadata read).
func (s *Source) githubProtected(ctx context.Context, tag string) (string, error) {
	var list []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Target      string `json:"target"`
		Enforcement string `json:"enforcement"`
	}
	if err := s.get(ctx, "/rulesets?includes_parents=true&per_page=100", &list); err != nil {
		return "", fmt.Errorf("read the rulesets of %s (the token must be able to read them): %w", s.Repo, err)
	}
	for _, rs := range list {
		if rs.Target != "tag" || rs.Enforcement != "active" {
			continue
		}
		var full struct {
			Enforcement string `json:"enforcement"`
			Conditions  struct {
				RefName struct {
					Include []string `json:"include"`
					Exclude []string `json:"exclude"`
				} `json:"ref_name"`
			} `json:"conditions"`
			Rules []struct {
				Type string `json:"type"`
			} `json:"rules"`
		}
		if err := s.get(ctx, fmt.Sprintf("/rulesets/%d", rs.ID), &full); err != nil {
			return "", fmt.Errorf("read the ruleset %q of %s: %w", rs.Name, s.Repo, err)
		}
		if full.Enforcement != "active" || !refNameCovers(full.Conditions.RefName.Include, full.Conditions.RefName.Exclude, tag) {
			continue
		}
		restricts := map[string]bool{}
		for _, r := range full.Rules {
			restricts[r.Type] = true
		}
		if restricts["creation"] && restricts["update"] {
			return "ruleset " + rs.Name, nil
		}
	}
	return "", fmt.Errorf("tag %s is not protected in %s: no active tag ruleset covers it and restricts both creating and updating it, so anyone who can push could make or move it, and it does not deploy. Add one (repository settings, rules, rulesets: target tags, %q, restrict creations and updates) first", tag, s.Repo, "refs/tags/"+s.TagPattern)
}

// refNameCovers is GitHub's ruleset ref matching for a tag: included by a pattern (or ~ALL) and excluded
// by none. Patterns name refs (refs/tags/deploy-*) with fnmatch globs: * and ? stop at a slash, ** does
// not.
func refNameCovers(include, exclude []string, tag string) bool {
	ref := "refs/tags/" + tag
	match := func(patterns []string) bool {
		for _, p := range patterns {
			if p == "~ALL" || refGlob(p, ref) || (!strings.HasPrefix(p, "refs/") && refGlob(p, tag)) {
				return true
			}
		}
		return false
	}
	return match(include) && !match(exclude)
}

func refGlob(pattern, name string) bool {
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				re.WriteString(".*")
				i++
			} else {
				re.WriteString("[^/]*")
			}
		case '?':
			re.WriteString("[^/]")
		default:
			re.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	re.WriteString("$")
	ok, _ := regexp.MatchString(re.String(), name)
	return ok
}
