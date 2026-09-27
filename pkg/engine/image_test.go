package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theta42/native-ops/pkg/incus"
)

func TestBuildImageQuotesEveryArgument(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "scripts", "build-image.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ex := &fakeExec{}
	if err := BuildImage(context.Background(), ex, dir, "platform", "feat/x-1.0", nil); err != nil {
		t.Fatal(err)
	}
	want := "bash " + incus.ShQuote(script) + " " + incus.ShQuote("platform") + " " + incus.ShQuote("feat/x-1.0")
	if len(ex.cmds) != 1 || ex.cmds[0] != want {
		t.Fatalf("\n got %v\nwant [%s]", ex.cmds, want)
	}
}

func TestBuildImageRejectsUnsafeInput(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	os.WriteFile(filepath.Join(dir, "scripts", "build-image.sh"), []byte("#!/bin/bash\n"), 0o755)
	for name, tc := range map[string]struct{ app, ref string }{
		"empty app":                {"", "main"},
		"empty ref":                {"platform", ""},
		"app with a shell in it":   {"x; rm -rf /", "main"},
		"app with a slash":         {"platform/x", "main"},
		"uppercase app":            {"Platform", "main"},
		"ref with a substitution":  {"platform", "x$(id)"},
		"ref with a backtick":      {"platform", "x`id`"},
		"ref with a space":         {"platform", "x y"},
		"ref that climbs":          {"platform", "../../etc"},
		"ref starting with a dash": {"platform", "-rf"},
	} {
		t.Run(name, func(t *testing.T) {
			ex := &fakeExec{}
			if err := BuildImage(context.Background(), ex, dir, tc.app, tc.ref, nil); err == nil {
				t.Fatal("expected rejection")
			}
			if len(ex.cmds) != 0 {
				t.Fatalf("unsafe input must be rejected before anything runs: %v", ex.cmds)
			}
		})
	}
}

func TestBuildImageRequiresTheRecipeScript(t *testing.T) {
	ex := &fakeExec{}
	if err := BuildImage(context.Background(), ex, t.TempDir(), "platform", "main", nil); err == nil {
		t.Fatal("expected an error when scripts/build-image.sh does not exist")
	}
	if len(ex.cmds) != 0 {
		t.Fatalf("nothing should run without a recipe: %v", ex.cmds)
	}
}

func TestBuildImageReportsAFailure(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	os.WriteFile(filepath.Join(dir, "scripts", "build-image.sh"), []byte("#!/bin/bash\n"), 0o755)
	ex := &fakeExec{err: errors.New("boom")}
	if err := BuildImage(context.Background(), ex, dir, "platform", "main", nil); err == nil {
		t.Fatal("a failed build must be reported")
	}
}

func TestBuildImageLogsProgressAndTheBuildsOwnOutput(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	os.WriteFile(filepath.Join(dir, "scripts", "build-image.sh"), []byte("#!/bin/bash\n"), 0o755)
	ex := &fakeExec{out: "[build] Done: opsavor-platform:main (abc123)"}
	var lines []string
	logf := func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	if err := BuildImage(context.Background(), ex, dir, "platform", "main", logf); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "")
	if !strings.Contains(joined, "platform") || !strings.Contains(joined, ex.out) {
		t.Fatalf("expected progress and the build's own output, got: %q", lines)
	}

	// A nil logf (every existing caller before this test) must not panic.
	if err := BuildImage(context.Background(), ex, dir, "platform", "main", nil); err != nil {
		t.Fatal(err)
	}
}

// TestBuildImageActuallyNeutralizesTheInjection proves the fix against a REAL shell, exactly the way
// remote.LocalExecutor.Run invokes one (bash -c <command>): before the fix (fmt's %q, not shell
// quoting), a ref of the shape used here ran the embedded command and left evidence on disk.
func TestBuildImageActuallyNeutralizesTheInjection(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	script := filepath.Join(dir, "scripts", "build-image.sh")
	os.WriteFile(script, []byte("#!/bin/bash\necho ran with: \"$@\"\n"), 0o755)

	marker := filepath.Join(t.TempDir(), "pwned")
	payload := "x$(touch " + marker + ")"
	if ValidImageRef(payload) {
		t.Fatalf("this payload must already be rejected by validation: %q", payload)
	}
	// Confirm what the constructed command would do if validation were somehow bypassed, by building
	// it the same way BuildImage does and running it for real.
	cmd := "bash " + incus.ShQuote(script) + " " + incus.ShQuote("platform") + " " + incus.ShQuote(payload)
	out, _ := exec.Command("bash", "-c", cmd).CombinedOutput()
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("the payload executed even though it was shell-quoted: %s", out)
	}
	if !strings.Contains(string(out), payload) {
		t.Fatalf("the ref should reach the script as one literal argument, got: %s", out)
	}
}
