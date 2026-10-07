package ocinet

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
)

// CheckAuthorisation makes the cheapest read in each permission class an
// allocation needs, so a credential that cannot allocate is known at startup
// rather than at the first launch that wants an address.
//
// Reads rather than a real assign-and-release round trip, deliberately: a full
// round trip would also fail when the subnet is simply out of addresses, and
// reporting that as an authorisation problem sends an operator to IAM for a
// capacity fault.
func (a *PoolAllocator) CheckAuthorisation(ctx context.Context) error {
	ips, err := a.client.ListPrivateIPs(ctx, a.cfg.VNICID)
	if err != nil {
		return fmt.Errorf("listing the private IPs on vnic %s failed, which needs `use vnics` and `manage private-ips` in compartment %s: %w",
			a.cfg.VNICID, a.cfg.CompartmentID, err)
	}

	subnetID := ""
	for _, ip := range ips {
		if ip.SubnetID != "" {
			subnetID = ip.SubnetID
			break
		}
	}
	if subnetID == "" {
		// The VNIC always has a primary private IP, so an empty list means the
		// credential read someone else's VNIC or the OCID is stale.
		return fmt.Errorf("vnic %s reports no private IP with a subnet, so the subnet permission cannot be checked", a.cfg.VNICID)
	}

	if _, err := a.client.GetSubnet(ctx, subnetID); err != nil {
		return fmt.Errorf("reading subnet %s failed, which needs `use subnets` in compartment %s; AssignPrivateIP requires SUBNET_ATTACH from that same verb: %w",
			subnetID, a.cfg.CompartmentID, err)
	}
	return nil
}

// reportAuthorisation logs the verdict and does not fail construction. The node
// still serves every other pool and NAT mode, and a credential fault should not
// be the reason a whole node refuses to start.
func reportAuthorisation(ctx context.Context, a *PoolAllocator, pool external.ExternalPoolConfig) {
	if err := a.CheckAuthorisation(ctx); err != nil {
		slog.ErrorContext(ctx, "ocinet credential cannot allocate an external address",
			"pool", pool.Name, "vnic_id", a.cfg.VNICID, "compartment", a.cfg.CompartmentID, "error", err)
		return
	}
	slog.InfoContext(ctx, "ocinet credential authorised to allocate",
		"pool", pool.Name, "vnic_id", a.cfg.VNICID, "compartment", a.cfg.CompartmentID)
}
