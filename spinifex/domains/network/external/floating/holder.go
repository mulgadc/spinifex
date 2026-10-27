package floating

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// ClaimLocalAddresses makes the provider deliver each address to the node its
// owner runs on, and takes back any address this node holds that no one owns.
//
// Ownership is decided by local OVS: a plugged tap is a guest running here, and
// a NAT gateway's address belongs on its VPC's gateway chassis. The provider's
// listing is the authority on where an address is now, read once per pass, so
// an address moved in the provider's console is repaired rather than believed.
//
// What the pass could not do is reported per address in Stalled, verified by
// reading the address back rather than trusting the attach's acknowledgement.
func (a *PoolAllocator) ClaimLocalAddresses(ctx context.Context) (external.ClaimResult, error) {
	var res external.ClaimResult

	rec, err := a.store.Get(ctx, a.cfg.Pool.Name)
	if errors.Is(err, kvstore.ErrNotFound) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("floating holder: read bindings: %w", err)
	}
	if len(rec.Bindings) == 0 {
		return res, nil
	}

	var plugged map[string]struct{}
	if a.cfg.LocalPorts != nil {
		if plugged, err = a.cfg.LocalPorts(ctx); err != nil {
			return res, fmt.Errorf("floating holder: list local OVS ports: %w", err)
		}
	}

	live, err := a.provider.List(ctx)
	if err != nil {
		return res, fmt.Errorf("floating holder: list provider addresses: %w", err)
	}
	byID := make(map[string]Address, len(live))
	for _, addr := range live {
		byID[addr.ID] = addr
	}

	for key, b := range rec.Bindings {
		addr, ok := byID[b.AddrID]
		if !ok {
			// Reconcile reports an address the provider no longer has.
			continue
		}
		local, err := a.ownsBinding(ctx, b, plugged)
		if err != nil {
			return res, err
		}
		switch {
		case local:
			res.Local++
			a.claim(ctx, key, b, addr, &res)
		case !b.owned() && addr.HolderID == a.cfg.HostID:
			a.surrender(ctx, key, b)
		}
	}
	return res, nil
}

// claim puts one owned address on this node. A failure is recorded as stalled
// rather than returned, because the guests behind the other addresses are as
// unreachable as this one if the pass gives up here.
func (a *PoolAllocator) claim(ctx context.Context, key string, b Binding, addr Address, res *external.ClaimResult) {
	host := a.cfg.HostID
	if addr.HolderID == host {
		a.recordHolder(ctx, key, b.AddrID, host)
		return
	}
	stall := func(reason string) {
		res.Stalled = append(res.Stalled, external.StalledAddress{PublicIP: key, ENIID: b.ENIID, Reason: reason})
	}
	if addr.HolderID != "" {
		if err := a.provider.Detach(ctx, b.AddrID); err != nil && !errors.Is(err, ErrNotFound) {
			stall(fmt.Sprintf("detach %s from %s: %v", b.AddrID, addr.HolderID, err))
			return
		}
	}
	if err := a.provider.Attach(ctx, b.AddrID, host); err != nil {
		stall(fmt.Sprintf("attach %s to %s: %v", b.AddrID, host, err))
		return
	}
	got, err := a.provider.Get(ctx, b.AddrID)
	if err != nil {
		stall(fmt.Sprintf("read back %s after attach: %v", b.AddrID, err))
		return
	}
	if got.HolderID != host {
		stall(fmt.Sprintf("provider accepted the attach of %s to %s and reports holder %q", b.AddrID, host, got.HolderID))
		return
	}
	slog.WarnContext(ctx, "floating claimed an address whose owner runs here",
		"pool", a.cfg.Pool.Name, "public_ip", key, "addr_id", b.AddrID,
		"eni_id", b.ENIID, "gateway_vpc_id", b.GatewayVPCID, "from_host", addr.HolderID, "to_host", host)
	a.recordHolder(ctx, key, b.AddrID, host)
	res.Claimed = append(res.Claimed, key)
}

// surrender detaches an address this node holds with no owner, which is what a
// disassociate leaves: still allocated and billing, answered by no guest.
func (a *PoolAllocator) surrender(ctx context.Context, key string, b Binding) {
	if err := a.provider.Detach(ctx, b.AddrID); err != nil && !errors.Is(err, ErrNotFound) {
		slog.WarnContext(ctx, "floating could not detach an unowned address; retrying next pass",
			"pool", a.cfg.Pool.Name, "public_ip", key, "addr_id", b.AddrID, "error", err)
		return
	}
	slog.InfoContext(ctx, "floating detached an address with no owner",
		"pool", a.cfg.Pool.Name, "public_ip", key, "addr_id", b.AddrID)
	a.recordHolder(ctx, key, b.AddrID, "")
}

// ownsBinding answers whether this node is where b's traffic is handled. An
// address with neither an ENI nor a gateway has no node, and is left alone.
func (a *PoolAllocator) ownsBinding(ctx context.Context, b Binding, plugged map[string]struct{}) (bool, error) {
	if b.ENIID != "" {
		_, local := plugged[topology.Port(b.ENIID)]
		return local, nil
	}
	if b.GatewayVPCID == "" || a.cfg.LocalGateway == nil {
		return false, nil
	}
	local, err := a.cfg.LocalGateway(ctx, b.GatewayVPCID)
	if err != nil {
		return false, fmt.Errorf("floating holder: gateway chassis for %s: %w", b.GatewayVPCID, err)
	}
	return local, nil
}

// recordHolder writes the observed holder under CAS, only while the binding
// still names the address this pass acted on. A failed write is logged: the
// provider already agrees, and the next pass rewrites it.
func (a *PoolAllocator) recordHolder(ctx context.Context, key, addrID, holder string) {
	err := a.store.Mutate(ctx, a.cfg.Pool.Name, func(r *Record) (bool, error) {
		b, ok := r.Bindings[key]
		if !ok || b.AddrID != addrID || b.Holder == holder {
			return false, nil
		}
		b.Holder = holder
		r.Bindings[key] = b
		return true, nil
	})
	if err != nil {
		slog.WarnContext(ctx, "floating could not record the holder; next pass retries",
			"pool", a.cfg.Pool.Name, "public_ip", key, "error", err)
	}
}
