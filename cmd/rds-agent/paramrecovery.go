package main

import (
	"context"
	"log/slog"
	"time"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// How long the engine is given to come up after a boot before the installed
// parameter set is treated as the reason it has not. Generous, because an
// ordinary cold start also runs crash recovery, and a rollback that fired
// against a cluster still replaying WAL would restart it for nothing.
const parameterRollbackAfter = 5 * time.Minute

// How often the wait re-checks. The engine reports through the same probe the
// heartbeat uses, so this costs a pg_isready per tick and nothing else.
const parameterRollbackPoll = 10 * time.Second

// The boot-time half of the parameter safety net. A static parameter only takes
// effect at a restart, so a value the engine's config check accepted can still
// stop it from starting — a shared_buffers the host cannot allocate is the usual
// shape. When that happens the bad config is on the persistent data volume, so
// every replace and every recovery boot fails the same way.
//
// This breaks that loop: once the engine has failed to come up for long enough,
// the last set it accepted is put back and the engine is restarted. The failure
// is reported rather than swallowed, so the instance comes back on the old
// parameters and the control plane still sees that the change did not land.
type paramGuard struct {
	engine parameterRecovery
	probe  *engineProbe
	cp     controlPlane
	id     identity
	after  time.Duration
	poll   time.Duration
}

func newParamGuard(engine parameterRecovery, probe *engineProbe, cp controlPlane) *paramGuard {
	return &paramGuard{engine: engine, probe: probe, cp: cp, after: parameterRollbackAfter, poll: parameterRollbackPoll}
}

// Runs once per agent lifetime: a rollback that did not bring the engine back
// means the parameters were not the cause, and repeating it would only churn a
// cluster that is failing for another reason.
func (g *paramGuard) Run(ctx context.Context) {
	if !g.waitForEngineDown(ctx) {
		return
	}

	// A restore that replaced the file and then failed has still moved the
	// instance onto the last accepted set, so the restart and the report below
	// are what it needs; returning here would leave it down and unaccounted for.
	restored, err := g.engine.RestoreLastKnownGoodParameters(ctx)
	if err != nil {
		slog.Error("rds-agent: restoring the last known good parameters failed",
			"err", err, "parametersReplaced", restored)
		if !restored {
			return
		}
	}
	if !restored {
		slog.Warn("rds-agent: engine is down but no different last accepted parameter set is available; not rolling back")
		return
	}

	message := handlers_rds.ParameterRollbackMessage
	slog.Error("rds-agent: "+message, "after", g.after)
	if g.cp != nil {
		if _, err := g.cp.SubmitState(ctx, g.id, handlers_rds.EngineHealthUnhealthy, message); err != nil {
			slog.Error("rds-agent: reporting the parameter rollback failed", "err", err)
		}
	}
	if err := g.engine.Restart(ctx); err != nil {
		slog.Error("rds-agent: restart after the parameter rollback failed", "err", err)
	}
}

// Reports true once the engine has been continuously absent for the whole
// window, false if it came up or the agent is shutting down. A postmaster that
// is up and replaying WAL resets the clock: it is making progress on its own, and
// restarting it would only throw the recovery away and start it again.
func (g *paramGuard) waitForEngineDown(ctx context.Context) bool {
	deadline := time.Now().Add(g.after)
	timer := time.NewTimer(g.poll)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
		switch state, _ := g.probe.state(ctx); state {
		case engineServing:
			return false
		case engineRecovering:
			deadline = time.Now().Add(g.after)
		}
		if time.Now().After(deadline) {
			return true
		}
		timer.Reset(g.poll)
	}
}
