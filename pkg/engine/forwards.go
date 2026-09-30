package engine

import (
	"context"
	"fmt"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
)

// ensureForwards converges the host ports an instance declares (`forwards:`): a proxy device for a
// protocol the edge cannot carry (git-over-SSH; a service terminating its own mail). It adds a missing
// device, overrides one whose listen/connect has drifted, and leaves a matching one alone. It runs on a
// fresh deploy, on a converge and on an instance launch, so the forward is part of the manifest and
// survives an immutable replace rather than living only in a one-time `incus config device add`.
//
// It reports whether anything changed.
func ensureForwards(ctx context.Context, ic *incus.Client, name string, forwards []config.PortForward) (bool, error) {
	if len(forwards) == 0 {
		return false, nil
	}
	st, err := ic.CaptureInstanceState(ctx, name)
	if err != nil {
		return false, err
	}
	changed := false
	for _, fwd := range forwards {
		props, err := fwd.DeviceProps()
		if err != nil {
			return changed, fmt.Errorf("%s: %w", name, err)
		}
		if existing, ok := st.Devices[fwd.Name]; ok &&
			existing["type"] == "proxy" && existing["listen"] == props["listen"] && existing["connect"] == props["connect"] {
			continue
		}
		if err := ic.SetDevice(ctx, name, fwd.Name, props); err != nil {
			return changed, fmt.Errorf("publish forward %s on %s: %w", fwd.Name, name, err)
		}
		changed = true
	}
	return changed, nil
}
