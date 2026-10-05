package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/labels"
)

// Environments, EnvironmentLabel, MaxLabels, ValidLabelKey and ValidateLabels are the label vocabulary of
// pkg/labels, under the names the engine and the API already use.
var Environments = labels.Environments

const (
	EnvironmentLabel = labels.Environment
	MaxLabels        = labels.Max
)

func ValidLabelKey(key string) bool { return labels.ValidKey(key) }

func ValidateLabels(l map[string]string) error { return labels.Validate(l) }

// reconcileLabels makes an instance carry exactly the labels in want: it sets the ones that are new or
// changed and removes the ones that are not wanted. cfg is the instance's current config.
func (m *InstanceManager) reconcileLabels(ctx context.Context, name string, cfg map[string]string, want map[string]string) error {
	return reconcileLabels(ctx, m.incus, name, cfg, want)
}

// reconcileLabels is reconcileLabels for any caller holding an incus client (the service deployer too).
func reconcileLabels(ctx context.Context, ic *incus.Client, name string, cfg map[string]string, want map[string]string) error {
	have := incus.LabelsOf(cfg)
	for _, k := range sortedKeys(want) {
		if v, ok := have[k]; !ok || v != want[k] {
			if err := ic.SetInstanceConfig(ctx, name, incus.LabelKey(k), want[k]); err != nil {
				return err
			}
		}
	}
	for _, k := range sortedKeys(have) {
		if _, ok := want[k]; !ok {
			if err := ic.UnsetInstanceConfig(ctx, name, incus.LabelKey(k)); err != nil {
				return err
			}
		}
	}
	return nil
}

// labelsDrift lists how an instance's labels differ from the declared set, as "k: old -> new" parts, in a
// stable order. A manifest that declares no `labels:` (nil) declares nothing, so it never drifts.
func labelsDrift(cfg map[string]string, want map[string]string) []string {
	if want == nil {
		return nil
	}
	have := incus.LabelsOf(cfg)
	var parts []string
	for _, k := range sortedKeys(want) {
		if v, ok := have[k]; !ok || v != want[k] {
			parts = append(parts, fmt.Sprintf("%s: %s -> %s", k, orNone(have[k]), want[k]))
		}
	}
	for _, k := range sortedKeys(have) {
		if _, ok := want[k]; !ok {
			parts = append(parts, fmt.Sprintf("%s: %s -> (removed)", k, have[k]))
		}
	}
	return parts
}

// SetLabels changes an instance's labels live, with no restart: set adds or changes, remove deletes. The
// result must still be a valid set.
func (m *InstanceManager) SetLabels(ctx context.Context, name string, set map[string]string, remove []string) error {
	if !incus.ValidName(name) {
		return fmt.Errorf("invalid instance name %q", name)
	}
	st, err := m.incus.CaptureInstanceState(ctx, name)
	if err != nil {
		return err
	}
	want := map[string]string{}
	for k, v := range incus.LabelsOf(st.Config) {
		want[k] = v
	}
	for _, k := range remove {
		delete(want, k)
	}
	for k, v := range set {
		want[k] = v
	}
	if err := ValidateLabels(want); err != nil {
		return err
	}
	m.log("==> [Instance] Labels of %s -> %s\n", name, FormatLabels(want))
	return m.reconcileLabels(ctx, name, st.Config, want)
}

// FormatLabels renders labels as k=v pairs in a stable order, for logs and audit lines.
func FormatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(labels))
	for _, k := range sortedKeys(labels) {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, " ")
}

// SetLabels changes the labels of a tenant instance (see InstanceManager.SetLabels).
func (i *Instances) SetLabels(ctx context.Context, name string, set map[string]string, remove []string, logf func(string, ...any)) error {
	return i.manager(logf).SetLabels(ctx, name, set, remove)
}
