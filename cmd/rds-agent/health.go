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

// probeRunner runs the engine probe and reports its exit status. A non-zero
// exit is a result, not a fault, so it comes back as a code rather than an
// error; err is reserved for the probe failing to run at all.
type probeRunner func(ctx context.Context, name string, args ...string) (int, error)

// execProbeRunner runs the real probe binary.
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

// engineProbe reports whether the local engine is serving.
//
// It shells out to pg_isready rather than opening a TCP connection: an open port
// only proves the postmaster is listening, while pg_isready completes a startup
// exchange, which is what separates an engine that is serving from one still in
// recovery.
type engineProbe struct {
	run    probeRunner
	binary string
	host   string
	// port is set from the bootstrap config once it lands, from a different
	// goroutine than the heartbeat that reads it.
	port atomic.Int64
	// seenHealthy latches once the engine has answered. The agent is up well
	// before rds-init has finished initdb, so a down engine means "starting"
	// until it has served once and "unhealthy" after — the same observation,
	// read as a boot in progress or as a failure depending on which it can be.
	seenHealthy bool
}

func newEngineProbe(cfg config, run probeRunner) *engineProbe {
	p := &engineProbe{run: run, binary: cfg.PGIsReady, host: cfg.EngineHost}
	p.port.Store(int64(cfg.EnginePort))
	return p
}

// setPort points the probe at the port the control plane assigned.
func (p *engineProbe) setPort(port int) {
	p.port.Store(int64(port))
}

// Check probes the engine once and returns the health to report, with a message
// explaining anything that is not healthy. Only the heartbeat calls it, so the
// seenHealthy latch is single-goroutine state.
func (p *engineProbe) Check(ctx context.Context) (handlers_rds.EngineHealth, string) {
	port := strconv.FormatInt(p.port.Load(), 10)
	code, err := p.run(ctx, p.binary, "-h", p.host, "-p", port, "-q")
	switch {
	case err != nil:
		// The probe could not run — a missing binary, a broken image. Reporting
		// healthy on the strength of nothing is the one answer that would hide
		// it, so this degrades exactly like an engine that did not answer.
		return p.degraded(), fmt.Sprintf("engine probe could not run: %v", err)
	case code == 0:
		p.seenHealthy = true
		return handlers_rds.EngineHealthHealthy, ""
	case code == 1:
		// pg_isready's "rejecting connections": the postmaster is up but in
		// startup or crash recovery, which resolves on its own.
		return handlers_rds.EngineHealthStarting, "engine is rejecting connections (startup or recovery)"
	default:
		return p.degraded(), fmt.Sprintf("engine did not respond on %s:%s", p.host, port)
	}
}

// degraded is how a non-answering engine is reported: starting until it has
// answered once, unhealthy afterwards.
func (p *engineProbe) degraded() handlers_rds.EngineHealth {
	if p.seenHealthy {
		return handlers_rds.EngineHealthUnhealthy
	}
	return handlers_rds.EngineHealthStarting
}
