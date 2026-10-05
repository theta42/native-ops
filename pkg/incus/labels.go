package incus

import (
	"context"
	"fmt"
	"strings"
)

// LabelPrefix is where an instance's labels live in its config: user.native-ops.label.<key>. They sit next
// to the other user.native-ops.* bookkeeping keys, so they survive an update (the whole config is carried
// over), a rollback and a resize, and need nothing to run beyond Incus itself.
const LabelPrefix = "user.native-ops.label."

// LabelKey is the config key that holds the label called key.
func LabelKey(key string) string { return LabelPrefix + key }

// LabelsOf returns the labels in an instance's config, or nil when it has none.
func LabelsOf(cfg map[string]string) map[string]string {
	var out map[string]string
	for k, v := range cfg {
		if strings.HasPrefix(k, LabelPrefix) {
			if out == nil {
				out = map[string]string{}
			}
			out[strings.TrimPrefix(k, LabelPrefix)] = v
		}
	}
	return out
}

// UnsetInstanceConfig removes one instance config key.
func (c *Client) UnsetInstanceConfig(ctx context.Context, name, key string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid instance name %q", name)
	}
	if _, err := c.exec.Run(ctx, fmt.Sprintf("incus config unset %s %s", ShQuote(name), ShQuote(key))); err != nil {
		return fmt.Errorf("unset config %s %s: %w", name, key, err)
	}
	return nil
}
