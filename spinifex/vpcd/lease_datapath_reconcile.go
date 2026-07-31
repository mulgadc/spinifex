package vpcd

import (
	"context"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
)

// reconcileGatewayLeases points every VPC gateway port at the address its lease
// actually holds. The two can diverge without any lease ever changing: an LRP
// configured by a static pool keeps its address when the pool is switched to
// DHCP, so the VPC forwards on an address nothing leases while the lease it does
// hold goes unused. Both addresses are then effectively lost.
//
// RebindGatewayIP is idempotent, so a port already on its lease address costs an
// OVSDB read and nothing more.
func reconcileGatewayLeases(ctx context.Context, mgr *dhcp.Manager, igwMgr external.IGWManager) {
	if mgr == nil || igwMgr == nil {
		return
	}
	entries, err := mgr.Leases(ctx)
	if err != nil {
		slog.Warn("vpcd: cannot reconcile gateway leases", "err", err)
		return
	}

	for _, e := range entries {
		if e.Purpose != dhcp.PurposeGatewayLRP || e.Lease == nil {
			continue
		}
		if e.VPCID == "" {
			slog.Warn("vpcd: gw-lrp lease carries no vpc_id; cannot reconcile",
				"client_id", e.Lease.ClientID, "ip", e.Lease.IP)
			continue
		}
		prefixLen, _ := e.Lease.SubnetMask.Size()
		if err := igwMgr.RebindGatewayIP(ctx, e.VPCID, e.Lease.IP.String(), prefixLen); err != nil {
			slog.Error("vpcd: reconciling gateway port to its lease address failed",
				"vpc_id", e.VPCID, "lease_ip", e.Lease.IP, "err", err)
		}
	}
}
