package main

import (
	"context"
	"errors"
	"os/exec"
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

// What the engine reports about itself, which is the only part of the probe
// that differs between engines. The port is passed in rather than read from the
// engine, so the atomic below stays the one place it is followed from.
type probeStateFn func(ctx context.Context, port int64) (engineState, string)

// Engine-neutral: the latch, the port, the degraded mapping and the translation
// to a reported health are shared, and only the state function is supplied by
// the engine the image bakes.
type engineProbe struct {
	stateFn probeStateFn
	// Set from the bootstrap config once it lands, from a different goroutine
	// than the heartbeat that reads it.
	port atomic.Int64
	// Latches once the engine has answered. The agent is up well before initdb
	// finishes, so a down engine is "starting" until it has served once.
	seenHealthy bool
}

func newEngineProbe(port int, stateFn probeStateFn) *engineProbe {
	p := &engineProbe{stateFn: stateFn}
	p.port.Store(int64(port))
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
	// The process is up but not yet serving: startup or crash recovery.
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
	return p.stateFn(ctx, p.port.Load())
}

// A non-answering engine: starting until it has answered once, unhealthy after.
func (p *engineProbe) degraded() handlers_rds.EngineHealth {
	if p.seenHealthy {
		return handlers_rds.EngineHealthUnhealthy
	}
	return handlers_rds.EngineHealthStarting
}
