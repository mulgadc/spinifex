package vpcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/nats-io/nats.go"
)

const (
	// leaseReapInterval paces the orphan sweep. Well under the shortest lease a
	// server is likely to hand out, so an orphan is reclaimed long before an
	// operator would notice the pool shrinking.
	leaseReapInterval = 10 * time.Minute
	// leaseReapStartDelay lets the cluster settle first. A resource created
	// moments before vpcd started may not have its record committed yet, and a
	// sweep that raced it would read a live lease as an orphan.
	leaseReapStartDelay = 2 * time.Minute
	// ownerCheckTimeout bounds one existence lookup. An unanswered check keeps
	// the lease, so there is nothing to gain from waiting longer.
	ownerCheckTimeout = 20 * time.Second
)

// leaseOwnerResolver answers the reaper's "does this still have an owner?"
// question and tears down the orphaned datapath. The records live daemon-side,
// so the lookup is an RPC; the teardown is local, because the gateway port is
// vpcd's to remove.
type leaseOwnerResolver struct {
	nc     *nats.Conn
	igwMgr external.IGWManager
}

var _ dhcp.LeaseOwner = (*leaseOwnerResolver)(nil)

// Status asks the daemon whether the lease's resource survives. Every failure
// path returns OwnerUnknown, so an unreachable daemon costs a sweep rather than
// a fleet of released addresses.
func (r *leaseOwnerResolver) Status(ctx context.Context, e dhcp.Entry) (dhcp.OwnerStatus, error) {
	if e.Lease == nil {
		return dhcp.OwnerUnknown, errors.New("owner check: nil lease")
	}
	if r.nc == nil {
		return dhcp.OwnerUnknown, fmt.Errorf("no NATS connection to resolve owner of %s", e.Lease.ClientID)
	}
	payload, err := json.Marshal(dhcp.OwnerCheckRequest{
		ClientID: e.Lease.ClientID,
		Purpose:  e.Purpose,
		VPCID:    e.VPCID,
	})
	if err != nil {
		return dhcp.OwnerUnknown, fmt.Errorf("marshal owner-check for %s: %w", e.Lease.ClientID, err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, ownerCheckTimeout)
	defer cancel()
	msg, err := r.nc.RequestWithContext(reqCtx, dhcp.TopicOwnerCheck, payload)
	if err != nil {
		return dhcp.OwnerUnknown, fmt.Errorf("owner-check for %s: %w", e.Lease.ClientID, err)
	}
	var reply dhcp.OwnerCheckReply
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return dhcp.OwnerUnknown, fmt.Errorf("decode owner-check reply for %s: %w", e.Lease.ClientID, err)
	}
	status := dhcp.ParseOwnerStatus(reply.Status)
	if reply.Error != "" {
		return status, fmt.Errorf("owner-check for %s: %s", e.Lease.ClientID, reply.Error)
	}
	return status, nil
}

// Discard removes what the lease configured. Only a gateway LRP lease has a
// datapath of its own: an EIP or ENI-public address is already gone with the
// record that named it, and DetachIGW covers the port, its routes and its NAT.
func (r *leaseOwnerResolver) Discard(ctx context.Context, e dhcp.Entry) error {
	if e.Purpose != dhcp.PurposeGatewayLRP {
		return nil
	}
	if e.VPCID == "" {
		return fmt.Errorf("gw-lrp lease %s carries no vpc_id; cannot detach", e.Lease.ClientID)
	}
	if r.igwMgr == nil {
		return fmt.Errorf("no IGW manager to detach orphaned gateway for %s", e.VPCID)
	}
	slog.Warn("vpcd: detaching gateway for a VPC that no longer exists",
		"vpc_id", e.VPCID, "gateway_ip", e.Lease.IP)
	return r.igwMgr.DetachIGW(ctx, e.VPCID)
}

// runLeaseReaper sweeps orphaned leases on a timer until ctx ends. Timing lives
// here rather than in the manager so the schedule is the caller's to set.
func runLeaseReaper(ctx context.Context, mgr *dhcp.Manager, interval, startDelay time.Duration) {
	if mgr == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(startDelay):
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := mgr.ReapOrphans(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("vpcd: dhcp orphan sweep failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
