package incus

import (
	"context"
	"encoding/json"
	"fmt"
)

// InstanceInfo is a trimmed view of `incus list --format json`.
type InstanceInfo struct {
	Name   string            `json:"name"`
	Status string            `json:"status"`
	Config map[string]string `json:"config"`
}

// ListInstances returns every instance with its config (including user.* keys).
func (c *Client) ListInstances(ctx context.Context) ([]InstanceInfo, error) {
	out, err := c.exec.Run(ctx, "incus list --format json")
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	var rows []InstanceInfo
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("parse instance list: %w", err)
	}
	return rows, nil
}

// SetInstanceConfig sets one instance config key (e.g. user.preview.expires).
func (c *Client) SetInstanceConfig(ctx context.Context, name, key, value string) error {
	if _, err := c.exec.Run(ctx, fmt.Sprintf("incus config set %s %s %q", name, key, value)); err != nil {
		return fmt.Errorf("set config %s %s: %w", name, key, err)
	}
	return nil
}
