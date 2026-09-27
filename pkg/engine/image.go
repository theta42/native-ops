package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

var (
	imageAppRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	imageRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,100}$`)
)

// ValidImageApp reports whether app is a safe image-recipe name (a directory under images/ in the
// config repo): lowercase letters, digits and dashes, the same shape as an instance template name.
func ValidImageApp(app string) bool { return imageAppRe.MatchString(app) }

// ValidImageRef reports whether ref is a plausible, safe git ref (a branch or tag name): letters,
// digits, dots, dashes, underscores and slashes, never "..", and never starting with "-" (which a
// naive downstream command could otherwise take for a flag).
func ValidImageRef(ref string) bool {
	return imageRefRe.MatchString(ref) && !strings.Contains(ref, "..") && ref[0] != '-'
}

// BuildImage builds and publishes an application image from a git ref using the
// config repo's image recipe. The recipe graph is app-specific and lives in the
// conf repo (images/<app>/build.sh, invoked by scripts/build-image.sh); the
// engine only knows how to run it, keeping the engine generic. logf, if not nil,
// receives progress; the build's own output (often the only sign of life for
// several minutes) is passed to it once the command returns, success or not.
//
// Usage: native-ops image build <app> <ref> --config-dir <native-ops-conf>
func BuildImage(ctx context.Context, exec remote.Executor, configDir, app, ref string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if !ValidImageApp(app) {
		return fmt.Errorf("%q is not a valid image name (lowercase letters, digits and dashes)", app)
	}
	if !ValidImageRef(ref) {
		return fmt.Errorf("%q is not a valid git ref", ref)
	}
	script := filepath.Join(configDir, "scripts", "build-image.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("no image builder at %s (expected scripts/build-image.sh in the config repo): %w", script, err)
	}
	// incus.ShQuote, not fmt's %q: %q is Go string-escaping, not POSIX shell quoting, and this string is
	// handed to a shell (remote.Executor.Run runs "bash -c <this>"). %q leaves $(...) and backticks
	// inside its double quotes exactly as shell-active as if unquoted -- confirmed exploitable before
	// this fix. The validation above already forbids the characters that would need it, but every
	// command built for a shell gets the same quoting here regardless, the way the rest of this
	// codebase always does (see pkg/incus.ShQuote's other callers).
	cmd := fmt.Sprintf("bash %s %s %s", incus.ShQuote(script), incus.ShQuote(app), incus.ShQuote(ref))
	logf("building %s@%s...\n", app, ref)
	out, err := exec.Run(ctx, cmd)
	if err != nil {
		return fmt.Errorf("build image %s@%s: %w\n%s", app, ref, err, out)
	}
	logf("%s\n", out)
	return nil
}
