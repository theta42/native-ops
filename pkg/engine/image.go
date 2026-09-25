package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/theta42/native-ops/pkg/remote"
)

// BuildImage builds and publishes an application image from a git ref using the
// config repo's image recipe. The recipe graph is app-specific and lives in the
// conf repo (images/<app>/build.sh, invoked by scripts/build-image.sh); the
// engine only knows how to run it, keeping the engine generic.
//
// Usage: native-ops image build <app> <ref> --config-dir <native-ops-conf>
func BuildImage(ctx context.Context, exec remote.Executor, configDir, app, ref string) error {
	if app == "" || ref == "" {
		return fmt.Errorf("image build requires an app and a ref")
	}
	script := filepath.Join(configDir, "scripts", "build-image.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("no image builder at %s (expected scripts/build-image.sh in the config repo): %w", script, err)
	}
	out, err := exec.Run(ctx, fmt.Sprintf("bash %q %q %q", script, app, ref))
	if err != nil {
		return fmt.Errorf("build image %s@%s: %w\n%s", app, ref, err, out)
	}
	return nil
}
