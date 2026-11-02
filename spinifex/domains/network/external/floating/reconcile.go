package floating

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// ReconcileResult reports what a pass found.
type ReconcileResult struct {
	// Collected is the IDs of labelled addresses deleted as leaks.
	Collected []string
	// Suspect counts unbound labelled addresses held for the next pass.
	Suspect int
	// Missing is the binding keys whose address the provider no longer has.
	Missing []string
	// Skipped counts addresses without this deployment's label, left alone.
	Skipped int
}

// Reconcile squares the record against the provider.
//
// A labelled address with no binding leaked from an interrupted Allocate or
// Release, and bills until something deletes it. It is also exactly what an
// Allocate on another node looks like between its create and its record write,
// so an address is collected only when two consecutive passes find it unbound.
//
// A binding whose address the provider no longer has was deleted behind our
// back. It is reported, not dropped: the address cannot be re-created with the
// same IP, and the customer's allocation must not silently change meaning.
//
// It never touches an address without this deployment's label.
func (a *PoolAllocator) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult

	live, err := a.provider.List(ctx)
	if err != nil {
		return res, fmt.Errorf("floating reconcile: list addresses: %w", err)
	}
	rec, err := a.store.Get(ctx, a.cfg.Pool.Name)
	if err != nil && !errors.Is(err, kvstore.ErrNotFound) {
		return res, fmt.Errorf("floating reconcile: read bindings: %w", err)
	}

	claimed := make(map[string]struct{}, len(rec.Bindings))
	for _, b := range rec.Bindings {
		claimed[b.AddrID] = struct{}{}
	}

	liveIDs := make(map[string]struct{}, len(live))
	suspects := make(map[string]struct{})
	var errs []error
	for _, addr := range live {
		liveIDs[addr.ID] = struct{}{}
		if !a.ours(addr) {
			res.Skipped++
			continue
		}
		if _, ok := claimed[addr.ID]; ok {
			continue
		}
		if _, seen := a.suspects[addr.ID]; !seen {
			suspects[addr.ID] = struct{}{}
			continue
		}
		if err := a.destroy(ctx, addr.ID); err != nil {
			suspects[addr.ID] = struct{}{}
			errs = append(errs, fmt.Errorf("collect leaked %s: %w", addr.ID, err))
			continue
		}
		slog.WarnContext(ctx, "floating reconcile collected a leaked address",
			"pool", a.cfg.Pool.Name, "addr_id", addr.ID, "public_ip", addr.IP.String(),
			"label", addr.Label, "holder", addr.HolderID)
		res.Collected = append(res.Collected, addr.ID)
	}
	a.suspects = suspects
	res.Suspect = len(suspects)

	for key, b := range rec.Bindings {
		if _, ok := liveIDs[b.AddrID]; ok {
			continue
		}
		slog.ErrorContext(ctx, "floating reconcile: the provider no longer has an allocated address",
			"pool", a.cfg.Pool.Name, "public_ip", key, "addr_id", b.AddrID,
			"allocation_id", b.AllocationID, "eni_id", b.ENIID)
		res.Missing = append(res.Missing, key)
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("floating reconcile: %w", errors.Join(errs...))
	}
	return res, nil
}
