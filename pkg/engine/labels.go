package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/theta42/native-ops/pkg/incus"
)

// Environments is the closed set of values the `environment` label may take. It is fixed in native-ops, not
// configured per host, so every deployment means the same thing by it and a token scoped to
// `environment=production` cannot be defeated by a spelling. Adding a value is a reviewed change here.
var Environments = []string{"production", "staging", "testing", "development", "demo"}

// EnvironmentLabel is the label that names what an instance is for.
const EnvironmentLabel = "environment"

// MaxLabels is the most labels one instance may carry.
const MaxLabels = 16

var (
	labelKeyRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	labelValueRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

// ValidLabelKey reports whether key is a usable label name.
func ValidLabelKey(key string) bool { return labelKeyRe.MatchString(key) }

// ValidateLabels reports why labels cannot be accepted: at most MaxLabels, a plain key and a plain value
// each, and `environment` one of Environments. Any other label is free-form.
func ValidateLabels(labels map[string]string) error {
	if len(labels) > MaxLabels {
		return fmt.Errorf("at most %d labels", MaxLabels)
	}
	for k, v := range labels {
		if !labelKeyRe.MatchString(k) {
			return fmt.Errorf("label name %q is not allowed (a lowercase letter, then lowercase letters, digits or dashes, at most 32 characters)", k)
		}
		if !labelValueRe.MatchString(v) {
			return fmt.Errorf("the value of label %s is not allowed (1-63 letters, digits, dots, dashes or underscores)", k)
		}
		if k == EnvironmentLabel && !contains(Environments, v) {
			return fmt.Errorf("environment %q is not one of %s", v, strings.Join(Environments, ", "))
		}
	}
	return nil
}

// reconcileLabels makes an instance carry exactly the labels in want: it sets the ones that are new or
// changed and removes the ones that are not wanted. cfg is the instance's current config.
func (m *InstanceManager) reconcileLabels(ctx context.Context, name string, cfg map[string]string, want map[string]string) error {
	have := incus.LabelsOf(cfg)
	for _, k := range sortedKeys(want) {
		if v, ok := have[k]; !ok || v != want[k] {
			if err := m.incus.SetInstanceConfig(ctx, name, incus.LabelKey(k), want[k]); err != nil {
				return err
			}
		}
	}
	for _, k := range sortedKeys(have) {
		if _, ok := want[k]; !ok {
			if err := m.incus.UnsetInstanceConfig(ctx, name, incus.LabelKey(k)); err != nil {
				return err
			}
		}
	}
	return nil
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
