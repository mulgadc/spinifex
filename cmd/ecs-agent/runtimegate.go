package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	ctrruntime "github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
)

// runtimeProbeTimeout bounds the liveness probe against a runtime the gate is
// already holding, so a wedged socket cannot stall the watch loop.
const runtimeProbeTimeout = 5 * time.Second

// defaultRuntimeRetry is how often the gate dials a runtime it does not have,
// and probes one it does. The node starts containerd and the agent back to
// back, so the first few dials are expected to fail.
const defaultRuntimeRetry = 10 * time.Second

// runtimeGate holds the container runtime the agent has resolved, or nothing
// when the socket has not answered. The runtime is resolved by a loop rather
// than once at boot: the node's service manager reports the container daemon
// started when the process spawned, not when its socket began accepting, so a
// dial that fails in the first second of boot is a transient condition.
//
// Every read of the runtime goes through the gate, so a runtime that arrives
// after boot is picked up on the next task without restarting the agent.
type runtimeGate struct {
	mu sync.RWMutex
	rt ctrruntime.Runtime
}

// newRuntimeGate returns a gate holding rt, which may be nil.
func newRuntimeGate(rt ctrruntime.Runtime) *runtimeGate {
	return &runtimeGate{rt: rt}
}

// get returns the current runtime, or nil when none is resolved.
func (g *runtimeGate) get() ctrruntime.Runtime {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.rt
}

// available reports whether a runtime is resolved. It is what registration
// consults to decide whether the host has any capacity to advertise.
func (g *runtimeGate) available() bool { return g.get() != nil }

// set replaces the held runtime, closing the one it displaces.
func (g *runtimeGate) set(rt ctrruntime.Runtime) {
	g.mu.Lock()
	old := g.rt
	g.rt = rt
	g.mu.Unlock()
	if old != nil && old != rt {
		_ = old.Close()
	}
}

// clear drops and closes the held runtime, returning whether it held one.
func (g *runtimeGate) clear() bool {
	g.mu.Lock()
	old := g.rt
	g.rt = nil
	g.mu.Unlock()
	if old == nil {
		return false
	}
	_ = old.Close()
	return true
}

// watch keeps the gate current until ctx is cancelled: it dials while the gate
// is empty, and probes while it is full, clearing the gate when the probe fails
// so the next tick re-dials. A containerd stopped and restarted underneath a
// running agent is therefore picked up the same way a slow boot is.
func (g *runtimeGate) watch(ctx context.Context, dial func() (ctrruntime.Runtime, error), interval time.Duration) {
	if interval <= 0 {
		interval = defaultRuntimeRetry
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.step(ctx, dial)
		}
	}
}

// step performs one pass of the watch loop: dial when empty, probe when full.
func (g *runtimeGate) step(ctx context.Context, dial func() (ctrruntime.Runtime, error)) {
	if rt := g.get(); rt != nil {
		probeCtx, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
		// A container list is what reconcile already asks the runtime at boot and
		// the cheapest thing it answers, so it doubles as the liveness probe.
		_, err := rt.List(probeCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("ecs-agent: container runtime stopped answering, will re-dial", "err", err)
			g.clear()
		}
		return
	}

	rt, err := dial()
	if err != nil {
		slog.Debug("ecs-agent: container runtime still unavailable", "err", err)
		return
	}
	slog.Info("ecs-agent: container runtime available")
	g.set(rt)
}
