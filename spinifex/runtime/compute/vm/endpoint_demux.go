package vm

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
)

// EndpointDemuxer reads and sets the per-tap flows that send a guest's TCP to
// gateway-endpoint destinations to the node's endpoint proxy. Both methods
// leave the tap's IMDS flows alone.
type EndpointDemuxer interface {
	// EndpointDemuxStates reads every listed ENI's demux in one query; an
	// error means nothing was read.
	EndpointDemuxStates(ctx context.Context, eniIDs []string) (EndpointDemuxStates, error)
	// EnsureEndpointDemux makes eniID's demux exactly entries; empty removes it.
	EnsureEndpointDemux(ctx context.Context, eniID string, entries []netip.AddrPort) error
	// RemoveEndpointDemuxCookie removes the flows under a cookie no ENI owns.
	RemoveEndpointDemuxCookie(ctx context.Context, cookie uint64) error
}

// EndpointDemuxStates is one read of the node's demux flows: the destinations
// each listed ENI's tap sends to the proxy, and the cookies no listed ENI owns.
type EndpointDemuxStates struct {
	ByENI   map[string][]netip.AddrPort
	Unowned []uint64
}

// EndpointIntent is what the node's demux should be: the subnets whose route
// table holds a gateway endpoint, and the destinations a guest reaches it at.
type EndpointIntent struct {
	Subnets map[string]bool
	Entries []netip.AddrPort
}

// endpointDemux is the manager's view of its endpoint demux, for the rollback
// drain. clear is set only by a finished pass that left no flow on the node,
// so a running pass and a fresh process, which may inherit flows, read held.
type endpointDemux struct {
	clear atomic.Bool
	// pass serialises reconcile passes, which the KV watch and the IMDS tick
	// can both start.
	pass sync.Mutex
}

// EndpointDemuxHeld reports whether this node may hold an endpoint demux flow:
// true until a pass has read the bridge and left none. A manager with no
// demuxer installs none.
func (m *Manager) EndpointDemuxHeld() bool {
	if m.deps.EndpointDemuxer == nil || m.deps.EndpointIntent == nil {
		return false
	}
	return !m.endpointDemux.clear.Load()
}

// ReconcileEndpointDemux makes each running guest's demux match its subnet's
// intent: the entries if its subnet routes to an endpoint, none otherwise.
// Each change runs under the guest's ENI lock, after re-checking the guest.
func (m *Manager) ReconcileEndpointDemux(ctx context.Context) error {
	d, intentOf := m.deps.EndpointDemuxer, m.deps.EndpointIntent
	if d == nil || intentOf == nil {
		return nil
	}
	m.endpointDemux.pass.Lock()
	defer m.endpointDemux.pass.Unlock()
	// A pass may install flows from intent read before a delete, so the node
	// reads held from before that read until the pass ends.
	m.endpointDemux.clear.Store(false)

	intent, err := intentOf(ctx)
	if err != nil {
		// Existing flows stay: the proxy re-checks the route on every accept.
		return fmt.Errorf("read endpoint intent: %w", err)
	}
	want := slices.Clone(intent.Entries)
	slices.SortFunc(want, netip.AddrPort.Compare)
	want = slices.Compact(want)

	targets := m.imdsDatapathTargets()
	eniIDs := make([]string, len(targets))
	for i, t := range targets {
		eniIDs[i] = t.eniID
	}
	readCtx, cancel := context.WithTimeout(ctx, imdsStateReadTimeout)
	states, err := d.EndpointDemuxStates(readCtx, eniIDs)
	cancel()
	if err != nil {
		return fmt.Errorf("read endpoint demux: %w", err)
	}

	// Only this loop installs demux flows, so a cookie no running guest owns
	// was left by a teardown that failed to clear it, and would hold rollback.
	held := m.removeUnownedEndpointDemux(ctx, states.Unowned)
	var mu sync.Mutex
	sem := make(chan struct{}, imdsRepairConcurrency)
	var wg sync.WaitGroup
	for _, t := range targets {
		var tw []netip.AddrPort
		if intent.Subnets[t.subnetID] {
			tw = want
		}
		have := slices.Clone(states.ByENI[t.eniID])
		slices.SortFunc(have, netip.AddrPort.Compare)
		if slices.Equal(have, tw) {
			mu.Lock()
			held = held || len(have) > 0
			mu.Unlock()
			continue
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			left := m.ensureEndpointDemux(ctx, t, have, tw)
			mu.Lock()
			held = held || left
			mu.Unlock()
		})
	}
	wg.Wait()
	m.endpointDemux.clear.Store(!held)
	return nil
}

// removeUnownedEndpointDemux removes each unowned cookie's flows and reports
// whether any may remain.
func (m *Manager) removeUnownedEndpointDemux(ctx context.Context, cookies []uint64) bool {
	left := false
	for _, c := range cookies {
		rmCtx, cancel := context.WithTimeout(ctx, imdsRepairTimeout)
		err := m.deps.EndpointDemuxer.RemoveEndpointDemuxCookie(rmCtx, c)
		cancel()
		if err != nil {
			slog.Error("endpoint: removing a demux no running guest owns failed; retrying next pass",
				"cookie", fmt.Sprintf("%#x", c), "err", err)
			left = true
			continue
		}
		slog.Info("endpoint: removed a demux no running guest owns", "cookie", fmt.Sprintf("%#x", c))
	}
	return left
}

// ensureEndpointDemux sets one guest's demux under its ENI lock, so it cannot
// race the guest's teardown, and reports whether the tap may still hold flows.
func (m *Manager) ensureEndpointDemux(ctx context.Context, t imdsDatapathTarget, have, want []netip.AddrPort) bool {
	unlock := m.imdsLocks.lock(t.eniID)
	defer unlock()

	if !m.stillIMDSTarget(t) {
		// The guest's teardown clears its flows; until then they may remain.
		return len(have) > 0
	}
	setCtx, cancel := context.WithTimeout(ctx, imdsRepairTimeout)
	defer cancel()
	if err := m.deps.EndpointDemuxer.EnsureEndpointDemux(setCtx, t.eniID, want); err != nil {
		slog.Error("endpoint: demux update failed; retrying next pass",
			"instance", t.instanceID, "eni", t.eniID, "had", len(have), "want", len(want), "err", err)
		return len(have) > 0 || len(want) > 0
	}
	slog.Info("endpoint: demux updated", "instance", t.instanceID, "eni", t.eniID,
		"had", len(have), "want", len(want))
	return len(want) > 0
}
