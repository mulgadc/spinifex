package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"sync/atomic"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

// A non-zero exit is a result, not a fault, so it comes back as a code; err is
// reserved for the probe failing to run at all.
type probeRunner func(ctx context.Context, name string, args ...string) (int, error)

func execProbeRunner(ctx context.Context, name string, args ...string) (int, error) {
	err := exec.CommandContext(ctx, name, args...).Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

// Shells out to pg_isready rather than dialling the port, since only a startup
// exchange separates an engine that is serving from one still in recovery.
type engineProbe struct {
	run    probeRunner
	binary string
	host   string
	// Set from the bootstrap config once it lands, from a different goroutine
	// than the heartbeat that reads it.
	port atomic.Int64
	// Latches once the engine has answered. The agent is up well before initdb
	// finishes, so a down engine is "starting" until it has served once.
	seenHealthy bool
}

func newEngineProbe(cfg config, run probeRunner) *engineProbe {
	p := &engineProbe{run: run, binary: cfg.PGIsReady, host: cfg.EngineHost}
	p.port.Store(int64(cfg.EnginePort))
	return p
}

func (p *engineProbe) setPort(port int) {
	p.port.Store(int64(port))
}

// What the engine is doing, before the seenHealthy latch collapses "never up"
// and "was up and went away" into one health. The parameter rollback needs them
// apart: a postmaster that is up and replaying WAL will come back on its own, one
// that is not running at all after a parameter change will not.
type engineState int

const (
	// Not answering at all, or the probe could not run.
	engineAbsent engineState = iota
	// The postmaster is up and rejecting connections: startup or crash recovery.
	engineRecovering
	engineServing
)

// Only the heartbeat calls this, so the seenHealthy latch is single-goroutine
// state.
func (p *engineProbe) Check(ctx context.Context) (handlers_rds.EngineHealth, string) {
	state, message := p.state(ctx)
	switch state {
	case engineServing:
		p.seenHealthy = true
		return handlers_rds.EngineHealthHealthy, ""
	case engineRecovering:
		return handlers_rds.EngineHealthStarting, message
	default:
		return p.degraded(), message
	}
}

// The raw probe result. Safe to call from a goroutine other than the
// heartbeat's: it touches no latched state.
func (p *engineProbe) state(ctx context.Context) (engineState, string) {
	port := strconv.FormatInt(p.port.Load(), 10)
	code, err := p.run(ctx, p.binary, "-h", p.host, "-p", port, "-q")
	switch {
	case err != nil:
		// A missing binary or broken image. Reporting healthy on the strength of
		// nothing would hide it, so this reads as absent like an engine that did
		// not answer.
		return engineAbsent, fmt.Sprintf("engine probe could not run: %v", err)
	case code == 0:
		return engineServing, ""
	case code == 1:
		return engineRecovering, "engine is rejecting connections (startup or recovery)"
	default:
		return engineAbsent, fmt.Sprintf("engine did not respond on %s:%s", p.host, port)
	}
}

// A non-answering engine: starting until it has answered once, unhealthy after.
func (p *engineProbe) degraded() handlers_rds.EngineHealth {
	if p.seenHealthy {
		return handlers_rds.EngineHealthUnhealthy
	}
	return handlers_rds.EngineHealthStarting
}
