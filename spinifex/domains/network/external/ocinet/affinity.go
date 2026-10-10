package ocinet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// ClaimResult reports what an affinity pass moved, so a caller can log it and a
// test can assert on it without reading the fake's internals.
type ClaimResult struct {
	// Claimed is the public addresses whose private half this node took over.
	Claimed []string
	// Local is how many bindings this node already owned, moved or not.
	Local int
	// Stalled is every address whose guest runs here and which OCI is not
	// delivering here, after the pass has tried to move it and re-read the
	// answer. Empty is the healthy result, and a non-empty entry names a guest
	// that is running and unreachable on its public address.
	Stalled []StalledAddress
}

// StalledAddress is one address the pass could not put where its guest is.
//
// It carries the ENI rather than the instance because that is what this layer
// knows: a caller that wants to name the guest has the port-to-instance map and
// this package does not.
type StalledAddress struct {
	PublicIP string
	ENIID    string
	Reason   string
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
//
// A NAT gateway's address has no tap to look for, and the same reasoning gives
// the same answer from a different local fact: it is a logical router's SNAT
// source, so it belongs on the VPC's gateway chassis, and whether that is this
// node is something the chassisredirect port binding says locally.
//
// What the pass could not do is reported rather than returned as one error. A
// denial or a throttle is per-address, the guests behind the others are just as
// dark, and the caller needs to know which guest to call impaired — so every
// address that did not arrive comes back in Stalled, verified against OCI's own
// answer rather than against its acknowledgement of the request.
func (a *PoolAllocator) ClaimLocalAddresses(ctx context.Context) (ClaimResult, error) {
	var res ClaimResult
	if a.localPorts == nil && a.localGateway == nil {
		return res, nil
	}

	var plugged map[string]struct{}
	if a.localPorts != nil {
		var err error
		plugged, err = a.localPorts(ctx)
		if err != nil {
			return res, fmt.Errorf("ocinet affinity: list local OVS ports: %w", err)
		}
	}
	// No taps and no gateway is a node carrying nothing, which owns no
	// addresses. It is also what a broken OVS looks like, so there is nothing to
	// claim either way and the pass costs no API call.
	if len(plugged) == 0 && a.localGateway == nil {
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
		if b.PrivateIPID == "" {
			continue
		}
		local, err := a.ownsBinding(ctx, b, plugged)
		if err != nil {
			return res, err
		}
		if !local {
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

	// One address failing must not abandon the rest: a throttle or a denial is
	// per-object, and the guests behind the other addresses are as unreachable
	// as this one if the pass gives up here.
	for key := range mine {
		b := rec.Bindings[key]
		if _, ok := held[b.PrivateIPID]; ok {
			continue
		}
		moved, err := a.client.MovePrivateIP(ctx, b.PrivateIPID, a.cfg.VNICID)
		if err != nil {
			res.Stalled = append(res.Stalled, StalledAddress{PublicIP: key, ENIID: b.ENIID,
				Reason: fmt.Sprintf("move %s (%s) to %s: %v", b.PrivateAddr, b.PrivateIPID, a.cfg.VNICID, err)})
			continue
		}
		slog.WarnContext(ctx, "ocinet claimed an address whose datapath runs here",
			"pool", a.cfg.Pool.Name, "public_ip", key, "private_ip", b.PrivateAddr,
			"eni_id", b.ENIID, "gateway_vpc_id", b.GatewayVPCID,
			"from_vnic", b.VNICID, "to_vnic", moved.VNICID)

		if err := a.recordVNIC(ctx, key, b.PrivateIPID, moved.VNICID); err != nil {
			return res, err
		}
		res.Claimed = append(res.Claimed, key)
	}

	if err := a.verifyClaimed(ctx, rec, &res); err != nil {
		return res, err
	}
	return res, nil
}

// verifyClaimed re-reads what OCI delivers here and checks that every address
// this pass moved actually arrived.
//
// The move's own response is OCI agreeing to the request, not OCI having done
// it, and the difference is the whole failure this guards: a guest that is
// running, reported healthy, and answering on nothing. One extra call, and only
// when something moved.
func (a *PoolAllocator) verifyClaimed(ctx context.Context, rec Record, res *ClaimResult) error {
	if len(res.Claimed) == 0 {
		return nil
	}
	held, err := a.heldPrivateIPIDs(ctx)
	if err != nil {
		return err
	}
	landed := res.Claimed[:0:0]
	for _, key := range res.Claimed {
		b := rec.Bindings[key]
		if _, ok := held[b.PrivateIPID]; ok {
			landed = append(landed, key)
			continue
		}
		res.Stalled = append(res.Stalled, StalledAddress{PublicIP: key, ENIID: b.ENIID,
			Reason: fmt.Sprintf("OCI accepted the move of %s (%s) and does not deliver it to %s",
				b.PrivateAddr, b.PrivateIPID, a.cfg.VNICID)})
	}
	res.Claimed = landed
	return nil
}

// ownsBinding answers whether this node is where b's traffic is handled, and so
// whose VNIC OCI has to deliver it to.
//
// A guest address belongs where its tap is plugged. A NAT gateway's belongs on
// the VPC's gateway chassis: it is the SNAT source for a logical router, so the
// node that forwards is the node OCI must see it leave from. An address that is
// neither — a bare EIP nobody has associated yet — has no node, and leaving it
// where it was allocated is correct until something claims it.
func (a *PoolAllocator) ownsBinding(ctx context.Context, b Binding, plugged map[string]struct{}) (bool, error) {
	if b.ENIID != "" {
		_, local := plugged[topology.Port(b.ENIID)]
		return local, nil
	}
	if b.GatewayVPCID == "" || a.localGateway == nil {
		return false, nil
	}
	local, err := a.localGateway(ctx, b.GatewayVPCID)
	if err != nil {
		return false, fmt.Errorf("ocinet affinity: gateway chassis for %s: %w", b.GatewayVPCID, err)
	}
	return local, nil
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
