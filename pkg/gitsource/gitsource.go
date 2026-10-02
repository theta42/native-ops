// Package gitsource reads a configuration repository's deploy tags from a Gitea server, so the daemon can
// deploy a tagged commit without trusting anything a CI job uploads: it resolves the tag itself, checks
// that the tag is protected (only the people the repository allows can push it), and downloads that
// commit's archive from the git server.
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

// Source is one repository on a Gitea server and the tags that deploy it.
type Source struct {
	BaseURL    string        // e.g. https://git.example.com
	Repo       string        // owner/name
	TagPattern string        // glob of the tags that deploy, e.g. deploy-*
	Token      func() string // a token that can read the repository and its tag protections
	HTTP       *http.Client
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
	res, err := s.do(ctx, "/archive/"+sha+".tar.gz")
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
	u := strings.TrimRight(s.BaseURL, "/") + "/api/v1/repos/" + s.Repo + p
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+tok)
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
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
