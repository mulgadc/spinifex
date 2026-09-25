package ocinet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// ClaimResult reports what an affinity pass moved, so a caller can log it and a
// test can assert on it without reading the fake's internals.
type ClaimResult struct {
	// Claimed is the public addresses whose private half this node took over.
	Claimed []string
	// Local is how many bindings this node already owned, moved or not.
	Local int
}

// ClaimLocalAddresses makes OCI agree with where the guests actually are.
//
// An OCI address is delivered to whichever VNIC carries its private half, and
// nothing about allocating one consults the node the instance will run on: the
// AWS request is served by an arbitrary queue-group worker, so the private IP
// lands on that worker's VNIC. It is then never moved. A guest that starts on a
// different node than it stopped on — which stop/start does routinely, since the
// start is only *preferentially* routed back to LastNode — keeps a correct OVN
// rule and a correct host route on its new node while OCI keeps delivering to
// the old one. The address goes dark and stays dark.
//
// The fix is one call: OCI reassigns a secondary private IP between VNICs in
// the same subnet server-side, keeping the OCID, so the reserved public IP
// attached to it follows without being touched and the customer's address never
// changes. There is no window in which the address belongs to neither node, and
// so nothing for the losing node to be told — it drops its now-unwanted host
// route on its own next pass, exactly as it would for a released address.
//
// Ownership is decided by local OVS, not by the record: a tap plugged in here
// is a guest running here, and it is the same authority the host EIP binder
// already uses to decide which addresses this node plumbs. Asking OCI which
// private IPs are on our own VNIC costs one call per pass and is authoritative,
// so a record that has drifted — an address moved in the console — is repaired
// rather than believed.
func (a *PoolAllocator) ClaimLocalAddresses(ctx context.Context) (ClaimResult, error) {
	var res ClaimResult
	if a.localPorts == nil {
		return res, nil
	}

	plugged, err := a.localPorts(ctx)
	if err != nil {
		return res, fmt.Errorf("ocinet affinity: list local OVS ports: %w", err)
	}
	// No taps at all is a node running no guests, which owns no addresses. It is
	// also what a broken OVS looks like, so there is nothing to claim either way
	// and the pass costs no API call.
	if len(plugged) == 0 {
		return res, nil
	}

	rec, err := a.store.Get(ctx, a.cfg.Pool.Name)
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			return res, nil
		}
		return res, fmt.Errorf("ocinet affinity: read bindings: %w", err)
	}

	mine := make(map[string]struct{})
	for key, b := range rec.Bindings {
		// An unassociated EIP has no guest and so no node; leaving it where it
		// was allocated is correct until an associate gives it an owner.
		if b.ENIID == "" || b.PrivateIPID == "" {
			continue
		}
		if _, local := plugged[topology.Port(b.ENIID)]; !local {
			continue
		}
		res.Local++
		mine[key] = struct{}{}
	}
	if len(mine) == 0 {
		return res, nil
	}

	// One call, not one per binding: this is the authoritative answer to "what
	// does OCI already deliver to me", and every binding is judged against it.
	held, err := a.heldPrivateIPIDs(ctx)
	if err != nil {
		return res, err
	}

	for key := range mine {
		b := rec.Bindings[key]
		if _, ok := held[b.PrivateIPID]; ok {
			continue
		}
		moved, err := a.client.MovePrivateIP(ctx, b.PrivateIPID, a.cfg.VNICID)
		if err != nil {
			return res, fmt.Errorf("ocinet affinity: move %s (%s) to %s: %w",
				b.PrivateAddr, b.PrivateIPID, a.cfg.VNICID, err)
		}
		slog.WarnContext(ctx, "ocinet claimed an address whose guest runs here",
			"pool", a.cfg.Pool.Name, "public_ip", key, "private_ip", b.PrivateAddr,
			"eni_id", b.ENIID, "from_vnic", b.VNICID, "to_vnic", moved.VNICID)

		if err := a.recordVNIC(ctx, key, b.PrivateIPID, moved.VNICID); err != nil {
			return res, err
		}
		res.Claimed = append(res.Claimed, key)
	}
	return res, nil
}

// heldPrivateIPIDs is the set of private IP OCIDs OCI currently carries on this
// node's external VNIC.
func (a *PoolAllocator) heldPrivateIPIDs(ctx context.Context) (map[string]struct{}, error) {
	live, err := a.client.ListPrivateIPs(ctx, a.cfg.VNICID)
	if err != nil {
		return nil, fmt.Errorf("ocinet affinity: list private ips on %s: %w", a.cfg.VNICID, err)
	}
	held := make(map[string]struct{}, len(live))
	for _, p := range live {
		held[p.ID] = struct{}{}
	}
	return held, nil
}

// recordVNIC writes the new owner back under CAS. The move already happened, so
// a binding that changed underneath us is re-read rather than overwritten: only
// the VNIC is ours to update, and only while it still names the object we moved.
func (a *PoolAllocator) recordVNIC(ctx context.Context, key, privateIPID, vnicID string) error {
	err := a.store.Mutate(ctx, a.cfg.Pool.Name, func(r *Record) (bool, error) {
		b, ok := r.Bindings[key]
		if !ok || b.PrivateIPID != privateIPID || b.VNICID == vnicID {
			return false, nil
		}
		b.VNICID = vnicID
		r.Bindings[key] = b
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("ocinet affinity: record new VNIC for %s: %w", key, err)
	}
	return nil
}
