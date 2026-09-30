package exonet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
)

// ReconcileResult reports what a pass found.
type ReconcileResult struct {
	// Collected is the IDs of marked EIPs deleted as leaks.
	Collected []string
	// Stale is the binding keys dropped because Exoscale no longer has the EIP.
	Stale []string
	// Skipped counts EIPs without this node's marker, left alone.
	Skipped int
}

// Reconcile squares the record against Exoscale in both directions:
//
//   - A marked EIP with no binding leaked from an interrupted Allocate or
//     Release. It bills and holds one of the organisation's few EIPs, and
//     nothing else will ever find it. Delete it.
//   - A binding whose EIP Exoscale no longer has was deleted behind our back.
//     Drop it, so the address is not reported to a customer as theirs.
//
// It never touches an EIP without this node's marker. The organisation can hold
// the operator's own addresses, and the marker is the whole safety property.
// Run it before the allocator serves, since it cannot tell a leak from an
// allocation that is between its create and its record write.
func (a *PoolAllocator) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult

	live, err := a.client.ListElasticIPs(ctx)
	if err != nil {
		return res, fmt.Errorf("exonet reconcile: list elastic IPs: %w", err)
	}
	rec, err := a.store.Get(ctx, a.cfg.Pool.Name)
	if err != nil && !errors.Is(err, kvstore.ErrNotFound) {
		return res, fmt.Errorf("exonet reconcile: read bindings: %w", err)
	}

	claimed := make(map[string]struct{}, len(rec.Bindings))
	for _, b := range rec.Bindings {
		claimed[b.EIPID] = struct{}{}
	}

	liveIDs := make(map[string]struct{}, len(live))
	for _, e := range live {
		liveIDs[e.ID] = struct{}{}
		if !a.ours(e) {
			res.Skipped++
			continue
		}
		if _, ok := claimed[e.ID]; ok {
			continue
		}
		if err := a.detachAndDelete(ctx, a.cfg.InstanceID, e.ID); err != nil {
			return res, fmt.Errorf("exonet reconcile: collect leaked %s: %w", e.ID, err)
		}
		slog.WarnContext(ctx, "exonet reconcile collected a leaked elastic IP",
			"pool", a.cfg.Pool.Name, "eip_id", e.ID, "public_ip", e.Address.String(), "description", e.Description)
		res.Collected = append(res.Collected, e.ID)
	}

	// Only bindings attached to this node are this node's to judge.
	var stale []string
	for key, b := range rec.Bindings {
		if _, ok := liveIDs[b.EIPID]; ok || b.NodeInstanceID != a.cfg.InstanceID {
			continue
		}
		stale = append(stale, key)
	}
	if len(stale) == 0 {
		return res, nil
	}
	err = a.store.Mutate(ctx, a.cfg.Pool.Name, func(r *Record) (bool, error) {
		changed := false
		for _, key := range stale {
			b, ok := r.Bindings[key]
			if !ok || b.NodeInstanceID != a.cfg.InstanceID {
				continue
			}
			if _, still := liveIDs[b.EIPID]; still {
				continue
			}
			delete(r.Bindings, key)
			changed = true
		}
		return changed, nil
	})
	if err != nil {
		return res, fmt.Errorf("exonet reconcile: drop stale bindings: %w", err)
	}
	for _, key := range stale {
		slog.WarnContext(ctx, "exonet reconcile dropped a stale binding",
			"pool", a.cfg.Pool.Name, "public_ip", key)
	}
	res.Stale = stale
	return res, nil
}
