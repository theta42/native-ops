package engine

import (
	"context"
	"fmt"
	"log"

	"github.com/theta42/native-ops/pkg/incus"
	"github.com/theta42/native-ops/pkg/remote"
)

// MigrationManager orchestrates cross-host workload transfers.
type MigrationManager struct {
	exec remote.Executor
}

func NewMigrationManager(exec remote.Executor) *MigrationManager {
	return &MigrationManager{exec: exec}
}

type MigrationParams struct {
	SourceRemote string // e.g. "droplet-nyc1-01"
	TargetRemote string // e.g. "pve-node-02"
	InstanceName string // e.g. "rest-acme"
	VolumeName   string // e.g. "rest-acme-data"
}

// Migrate coordinates moving an instance and its storage volume across remotes.
func (m *MigrationManager) Migrate(ctx context.Context, p MigrationParams) error {
	log.Printf("==> [Migration] Moving %s from %s to %s...\n", p.InstanceName, p.SourceRemote, p.TargetRemote)

	client := incus.NewClient(m.exec)

	// 1. Quiesce source container
	log.Printf("    Stopping source container %s on %s...\n", p.InstanceName, p.SourceRemote)
	stopCmd := fmt.Sprintf("incus stop %s:%s --timeout 15", p.SourceRemote, p.InstanceName)
	_, _ = m.exec.Run(ctx, stopCmd)

	// 2. Perform cross-remote migration
	log.Printf("    Transferring volume and instance to %s...\n", p.TargetRemote)
	if err := client.MigrateInstance(ctx, p.SourceRemote, p.TargetRemote, p.InstanceName, p.VolumeName); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}

	// 3. Start container on target
	log.Printf("    Starting instance %s on target %s...\n", p.InstanceName, p.TargetRemote)
	startCmd := fmt.Sprintf("incus start %s:%s", p.TargetRemote, p.InstanceName)
	if _, err := m.exec.Run(ctx, startCmd); err != nil {
		return fmt.Errorf("start migrated instance on target: %w", err)
	}

	// 4. Cleanup old container on source
	log.Printf("    Cleaning up old instance on source %s...\n", p.SourceRemote)
	delCmd := fmt.Sprintf("incus delete %s:%s", p.SourceRemote, p.InstanceName)
	_, _ = m.exec.Run(ctx, delCmd)

	log.Printf("==> [Migration] Migration completed successfully!\n")
	return nil
}
