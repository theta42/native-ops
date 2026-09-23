package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/theta42/native-ops/pkg/provider"
)

// ScriptDNSProvider executes an external python/bash script in native-ops-conf.
type ScriptDNSProvider struct {
	name       string
	scriptPath string
}

// NewScriptDNSProvider creates a DNS provider driven by an external script.
func NewScriptDNSProvider(name, configDir string) (*ScriptDNSProvider, error) {
	candidates := []string{
		filepath.Join(configDir, "providers", "dns", name+".py"),
		filepath.Join(configDir, "providers", "dns", name+".sh"),
		filepath.Join(configDir, "providers", "dns", name),
	}

	var foundPath string
	for _, p := range candidates {
		if pathExists(p) {
			foundPath = p
			break
		}
	}

	if foundPath == "" {
		return nil, fmt.Errorf("no DNS provider plugin found for '%s' in %s/providers/dns/", name, configDir)
	}

	return &ScriptDNSProvider{
		name:       name,
		scriptPath: foundPath,
	}, nil
}

func (p *ScriptDNSProvider) Name() string {
	return p.name
}

func (p *ScriptDNSProvider) SyncRecords(ctx context.Context, domain string, records []provider.DNSRecord) error {
	payload, err := json.Marshal(map[string]any{
		"action":  "sync",
		"domain":  domain,
		"records": records,
	})
	if err != nil {
		return fmt.Errorf("marshal DNS records: %w", err)
	}

	cmd := exec.CommandContext(ctx, p.scriptPath, "sync", "--domain", domain)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("DNS plugin error (%s): %s: %w", p.scriptPath, stderr.String(), err)
	}

	return nil
}

func (p *ScriptDNSProvider) ListRecords(ctx context.Context, domain string) ([]provider.DNSRecord, error) {
	cmd := exec.CommandContext(ctx, p.scriptPath, "list", "--domain", domain)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("DNS plugin error (%s): %s: %w", p.scriptPath, stderr.String(), err)
	}

	var records []provider.DNSRecord
	if err := json.Unmarshal(stdout.Bytes(), &records); err != nil {
		return nil, fmt.Errorf("parse plugin output: %w", err)
	}

	return records, nil
}

func (p *ScriptDNSProvider) DeleteRecord(ctx context.Context, domain string, recordID string) error {
	cmd := exec.CommandContext(ctx, p.scriptPath, "delete", "--domain", domain, "--id", recordID)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("DNS plugin error: %s: %w", stderr.String(), err)
	}
	return nil
}

func pathExists(path string) bool {
	cmd := exec.Command("test", "-f", path)
	return cmd.Run() == nil
}
