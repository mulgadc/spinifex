package ocinet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
)

// ReconcileResult reports what a pass found, so a caller can log it and a test
// can assert on it without reading the fake's internals.
type ReconcileResult struct {
	// Collected is the OCIDs of private IPs deleted as leaks.
	Collected []string
	// Stale is the binding keys dropped because OCI no longer has the objects.
	Stale []string
	// Skipped is the private IPs left alone because they are not ours.
	Skipped int
}

// Reconcile squares our record against OCI, in both directions. It is required
// on startup rather than optional: Allocate creates OCI objects before writing
// the record, so a crash in between leaves a reserved public IP that costs
// money and holds one of the 50 per-region slots, with nothing referencing it.
// Nothing else will ever find that address.
//
// The two directions fix different faults:
//
//   - An object on the VNIC carrying our prefix with no binding is a leak from
//     an interrupted Allocate. Delete it.
//   - A binding naming objects OCI no longer has is a stale record from an
//     interrupted Release, or from someone deleting the address in the console.
//     Drop it, so the address is not reported to a customer as theirs.
//
// **It never touches an object without DisplayNamePrefix.** The reference host
// carries operator-created secondary addresses on the same VNIC, and a
// reconcile that collected those would take the node's own networking down. The
// prefix is the entire safety property here, so an object whose name we cannot
// read is skipped, not guessed at.
func (a *PoolAllocator) Reconcile(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult

	live, err := a.client.ListPrivateIPs(ctx, a.cfg.VNICID)
	if err != nil {
		return res, fmt.Errorf("ocinet reconcile: list private ips on %s: %w", a.cfg.VNICID, err)
	}
	// A pool that has never allocated has no record, which is an empty binding
	// set rather than a failure — and the reconcile still has work to do, since
	// the leak this pass exists to find is precisely an OCI object created
	// before its binding was written.
	rec, err := a.store.Get(ctx, a.cfg.Pool.Name)
	if err != nil && !errors.Is(err, kvstore.ErrNotFound) {
		return res, fmt.Errorf("ocinet reconcile: read bindings: %w", err)
	}

	claimed := make(map[string]struct{}, len(rec.Bindings))
	for _, b := range rec.Bindings {
		claimed[b.PrivateIPID] = struct{}{}
	}

	// Direction 1: OCI has objects we do not claim.
	for _, p := range live {
		if p.IsPrimary || !strings.HasPrefix(p.DisplayName, DisplayNamePrefix) {
			res.Skipped++
			continue
		}
		if _, ok := claimed[p.ID]; ok {
			continue
		}
		publicIPID := ""
		if pub, err := a.client.GetPublicIPByPrivateIPID(ctx, p.ID); err == nil {
			publicIPID = pub.ID
		} else if !errors.Is(err, oci.ErrNotFound) {
			return res, fmt.Errorf("ocinet reconcile: look up public ip for %s: %w", p.ID, err)
		}
		if err := a.deletePair(ctx, publicIPID, p.ID); err != nil {
			return res, fmt.Errorf("ocinet reconcile: collect leaked %s: %w", p.ID, err)
		}
		slog.WarnContext(ctx, "ocinet reconcile collected a leaked address",
			"pool", a.cfg.Pool.Name, "private_ip_id", p.ID, "private_ip", p.Address.String(),
			"public_ip_id", publicIPID, "display_name", p.DisplayName)
		res.Collected = append(res.Collected, p.ID)
	}

	// Direction 2: we claim objects OCI does not have.
	byID := make(map[string]struct{}, len(live))
	for _, p := range live {
		byID[p.ID] = struct{}{}
	}
	// Only this node's own bindings are this node's to judge. The record is one
	// key per pool and therefore cluster-wide, while `live` is one VNIC — so a
	// binding held by another node looks exactly like a binding whose objects
	// OCI has lost. Dropping those makes every node delete its peers' addresses
	// on startup, and direction 1 on the owning node then collects the now
	// unclaimed objects as leaks: a live address is destroyed under a running
	// instance. A binding naming no VNIC cannot be attributed, so it is left
	// alone rather than guessed at.
	var stale []string
	var foreign int
	for key, b := range rec.Bindings {
		if _, ok := byID[b.PrivateIPID]; ok {
			continue
		}
		if b.VNICID != a.cfg.VNICID {
			foreign++
			continue
		}
		stale = append(stale, key)
	}
	if foreign > 0 {
		slog.DebugContext(ctx, "ocinet reconcile left other nodes' bindings alone",
			"pool", a.cfg.Pool.Name, "vnic_id", a.cfg.VNICID, "foreign", foreign)
	}
	if len(stale) > 0 {
		err = a.store.Mutate(ctx, a.cfg.Pool.Name, func(r *Record) (bool, error) {
			changed := false
			for _, key := range stale {
				// Re-check under CAS: another node may have re-allocated this
				// address between our list and this mutation, in which case the
				// binding is live again and dropping it would strand a real one.
				b, ok := r.Bindings[key]
				if !ok {
					continue
				}
				if _, still := byID[b.PrivateIPID]; still {
					continue
				}
				// Re-check ownership too: the re-allocation may have been to
				// another node's VNIC, which is a live address elsewhere.
				if b.VNICID != a.cfg.VNICID {
					continue
				}
				delete(r.Bindings, key)
				changed = true
			}
			return changed, nil
		})
		if err != nil {
			return res, fmt.Errorf("ocinet reconcile: drop stale bindings: %w", err)
		}
		for _, key := range stale {
			slog.WarnContext(ctx, "ocinet reconcile dropped a stale binding",
				"pool", a.cfg.Pool.Name, "public_ip", key)
		}
		res.Stale = stale
	}

	return res, nil
}
