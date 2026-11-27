package daemon

import (
	"fmt"
	"log/slog"
	"time"

	accountlifecyclev1 "github.com/mulgadc/spinifex/contracts/accountlifecycle/v1"
	migrationv1 "github.com/mulgadc/spinifex/contracts/migration/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/accounttermination"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/admission"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/executor"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go/jetstream"
)

// initMigration builds this node's migration executor and termination
// participant. This release writes no migration object and holds no session
// cache in the daemon.
func (d *Daemon) initMigration(js jetstream.JetStream, terminating admission.TerminationChecker) error {
	exec, err := executor.New(js, d.node, executor.NoObjects{}, nil)
	if err != nil {
		return fmt.Errorf("migration executor: %w", err)
	}
	participant, err := accounttermination.New(js, terminating, exec, executor.NoObjects{}, nil, time.Now)
	if err != nil {
		return fmt.Errorf("migration participant: %w", err)
	}
	d.migrationExecutor, d.migrationParticipant = exec, participant
	return nil
}

// migrationSubs serves the participant routes in the worker queue group: any
// node with this release can answer for any account.
func (d *Daemon) migrationSubs() []natsSub {
	if d.migrationParticipant == nil {
		slog.Error("Not serving the migration termination participant: it was not initialised", "node", d.node)
		return nil
	}
	name := migrationv1.ParticipantName
	return []natsSub{
		{accountlifecyclev1.InventorySubject(name), handleNATSRequest(d.node, d.migrationParticipant.Inventory), "spinifex-workers"},
		{accountlifecyclev1.TerminateSubject(name), handleNATSRequest(d.node, d.migrationParticipant.Terminate), "spinifex-workers"},
	}
}

// startMigrationExecutor runs the executor sweep on a timer and then marks
// this node ready to advertise the migration feature level.
func (d *Daemon) startMigrationExecutor() {
	if d.migrationExecutor == nil || d.migrationParticipant == nil {
		return
	}
	exec := d.migrationExecutor
	d.shutdownWg.Go(func() {
		ticker := time.NewTicker(executor.SweepInterval)
		defer ticker.Stop()
		exec.Run(d.ctx, ticker.C)
	})
	d.migrationReady.Store(true)
}

// capMigrationLevel keeps the advertised level below migration until this
// node serves the participant and runs the executor.
func (d *Daemon) capMigrationLevel(level int) int {
	if !d.migrationReady.Load() {
		return min(level, featurelevel.Migration-1)
	}
	return level
}
