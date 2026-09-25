package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theta42/native-ops/pkg/remote"
)

type fakeExec struct{ cmds []string }

func (f *fakeExec) Run(_ context.Context, command string) (string, error) {
	f.cmds = append(f.cmds, command)
	return "", nil
}
func (f *fakeExec) RunWithInput(context.Context, string, io.Reader) (string, error) { return "", nil }
func (f *fakeExec) WriteFile(context.Context, string, []byte, os.FileMode) error    { return nil }
func (f *fakeExec) Close() error                                                    { return nil }

func TestPreviewName(t *testing.T) {
	cases := map[[2]string]string{
		{"platform", "feat/savy"}: "preview-platform-feat-savy",
		{"platform", "PR-73!!"}:   "preview-platform-pr-73",
		{"platform", "main"}:      "preview-platform-main",
		{"my app", "fix___thing"}: "preview-my-app-fix-thing",
	}
	for in, want := range cases {
		if got := PreviewName(in[0], in[1]); got != want {
			t.Errorf("PreviewName(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
	// Never exceeds the name budget, never ends in a dash, always starts clean.
	long := PreviewName("platform", strings.Repeat("branch-name-", 10))
	if len(long) > 40 || strings.HasSuffix(long, "-") || !strings.HasPrefix(long, "preview-") {
		t.Errorf("long preview name not sanitized: %q", long)
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	list := []Preview{
		{Name: "old", Expires: now.Add(-time.Hour)},
		{Name: "fresh", Expires: now.Add(time.Hour)},
		{Name: "nottl"}, // zero Expires never expires
	}
	got := expired(list, now)
	if len(got) != 1 || got[0].Name != "old" {
		t.Fatalf("expired() = %+v, want only [old]", got)
	}
}

func TestBuildImageRunsConfRecipe(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "build-image.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	exec := &fakeExec{}
	if err := BuildImage(context.Background(), exec, dir, "platform", "feat/x"); err != nil {
		t.Fatal(err)
	}
	if len(exec.cmds) != 1 || !strings.Contains(exec.cmds[0], "build-image.sh") ||
		!strings.Contains(exec.cmds[0], `"platform"`) || !strings.Contains(exec.cmds[0], `"feat/x"`) {
		t.Fatalf("unexpected command: %v", exec.cmds)
	}
	if err := BuildImage(context.Background(), exec, t.TempDir(), "platform", "main"); err == nil {
		t.Fatal("expected an error when the conf has no build-image.sh")
	}
}

var _ remote.Executor = (*fakeExec)(nil)
