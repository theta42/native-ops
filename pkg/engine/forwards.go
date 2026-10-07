package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/theta42/native-ops/pkg/config"
	"github.com/theta42/native-ops/pkg/incus"
)

// managedForwardsKey records which proxy devices on an instance came from its manifest's `forwards:`, so a
// forward deleted from the manifest is removed from the host, while a proxy device somebody else added
// (one native-ops never declared) is left alone.
const managedForwardsKey = "user.native-ops.forwards"

// forwardChange is one thing apply does about an instance's host ports.
type forwardChange struct {
	Device string
	Props  map[string]string // the device to publish; nil when it is removed
	Remove bool
	Detail string // the plan's wording
}

// forwardsDrift works out what makes an instance's host ports match its manifest: a missing device is
// published, one whose listen/connect drifted is overridden, a matching one is left alone, and one that
// native-ops published earlier (managedForwardsKey) but the manifest no longer declares is removed. record
// is the managedForwardsKey value to write ("-" removes the key; "" means it already matches). The plan and
// apply both call it, so they make the same decisions, and applying its result then calling it again
// yields nothing.
func forwardsDrift(st *incus.InstanceState, forwards []config.PortForward) (changes []forwardChange, record string, err error) {
	declared := map[string]bool{}
	var names []string
	for _, fwd := range forwards {
		props, err := fwd.DeviceProps()
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", st.Name, err)
		}
		name := fwd.DeviceName()
		if declared[name] {
			return nil, "", fmt.Errorf("%s: two forwards are named %s", st.Name, name)
		}
		declared[name] = true
		names = append(names, name)
		existing, ok := st.Devices[name]
		switch {
		case ok && existing["type"] == "proxy" && existing["listen"] == props["listen"] && existing["connect"] == props["connect"]:
		case ok:
			changes = append(changes, forwardChange{Device: name, Props: props,
				Detail: fmt.Sprintf("%s: %s -> %s (was %s -> %s)", name, props["listen"], props["connect"], orNone(existing["listen"]), orNone(existing["connect"]))})
		default:
			changes = append(changes, forwardChange{Device: name, Props: props,
				Detail: fmt.Sprintf("%s: %s -> %s", name, props["listen"], props["connect"])})
		}
	}
	for _, name := range splitList(st.Config[managedForwardsKey]) {
		if declared[name] {
			continue
		}
		if d, ok := st.Devices[name]; ok && d["type"] == "proxy" {
			changes = append(changes, forwardChange{Device: name, Remove: true,
				Detail: fmt.Sprintf("%s: %s (no longer in the manifest)", name, orNone(d["listen"]))})
		}
	}
	sort.Strings(names)
	if want := strings.Join(names, ","); want != st.Config[managedForwardsKey] {
		record = want
		if record == "" {
			record = "-"
		}
	}
	return changes, record, nil
}

// ensureForwards converges the host ports an instance declares (`forwards:`): a proxy device for a
// protocol the edge cannot carry (git-over-SSH; a service terminating its own mail). It runs on a fresh
// deploy, on a converge and on an instance launch, so the forward is part of the manifest and survives an
// immutable replace rather than living only in a one-time `incus config device add`. A forward removed
// from the manifest is removed from the instance.
//
// It reports whether anything changed.
func ensureForwards(ctx context.Context, ic *incus.Client, name string, forwards []config.PortForward) (bool, error) {
	st, err := ic.CaptureInstanceState(ctx, name)
	if err != nil {
		return false, err
	}
	changes, record, err := forwardsDrift(st, forwards)
	if err != nil {
		return false, err
	}
	for _, c := range changes {
		if c.Remove {
			if err := ic.RemoveDevice(ctx, name, c.Device); err != nil {
				return true, fmt.Errorf("remove forward %s from %s: %w", c.Device, name, err)
			}
			continue
		}
		if err := ic.SetDevice(ctx, name, c.Device, c.Props); err != nil {
			return true, fmt.Errorf("publish forward %s on %s: %w", c.Device, name, err)
		}
	}
	switch record {
	case "":
	case "-":
		if err := ic.UnsetInstanceConfig(ctx, name, managedForwardsKey); err != nil {
			return true, err
		}
	default:
		if err := ic.SetInstanceConfig(ctx, name, managedForwardsKey, record); err != nil {
			return true, err
		}
	}
	return len(changes) > 0 || record != "", nil
}

// planForwards adds an existing instance's host-port changes to its plan.
func planForwards(p *ServicePlan, st *incus.InstanceState, forwards []config.PortForward) {
	changes, record, err := forwardsDrift(st, forwards)
	if err != nil {
		p.block("%v", err)
		return
	}
	for _, c := range changes {
		if c.Remove {
			p.add(ChangeRemovePort, "%s", c.Detail)
		} else {
			p.add(ChangePublishPort, "%s", c.Detail)
		}
	}
	if record != "" && len(changes) == 0 {
		// Only the bookkeeping differs (ports published before it existed): apply writes it, so say so.
		p.add(ChangePublishPort, "record %s as the host ports this manifest manages", orNone(strings.TrimPrefix(record, "-")))
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
