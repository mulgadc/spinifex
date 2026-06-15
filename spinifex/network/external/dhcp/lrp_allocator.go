package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/mulgadc/spinifex/spinifex/network/external"
)

// Purpose tags written to the per-AZ lease bucket so operators can
// distinguish lease classes.
const (
	PurposeGatewayLRP    = "gw-lrp"
	PurposeEIP           = "eip"
	PurposeENIPublic     = "eni-public"
	PurposeNATGWExternal = "natgw-external"
)

// GatewayLRPClientID returns the DHCP client-id for vpcID. The "dhcp-" prefix
// keeps it disjoint from OVN object names ("gw-" is L2-owned).
func GatewayLRPClientID(vpcID string) string { return "dhcp-gw-lrp-" + vpcID }

// DHCPGatewayLRPAllocator obtains the per-VPC gateway LRP IP via DORA.
// Used when pool.Source == SourceDHCP with centralized NAT; distributed NAT
// uses LinkLocalAllocator instead (LRP IP never goes on wire).
type DHCPGatewayLRPAllocator struct {
	mgr *Manager
}

var _ external.GatewayIPAllocator = (*DHCPGatewayLRPAllocator)(nil)

// NewDHCPGatewayLRPAllocator wraps an existing in-process Manager.
func NewDHCPGatewayLRPAllocator(mgr *Manager) *DHCPGatewayLRPAllocator {
	return &DHCPGatewayLRPAllocator{mgr: mgr}
}

// Allocate DORAs once per vpcID; returns ok=false for non-DHCP pools.
// Empty Routers in the ACK is a hard error — fallback to link-local would
// yield 169.254.0.2 on the WAN subnet, which is unreachable.
func (a *DHCPGatewayLRPAllocator) Allocate(ctx context.Context, vpcID string, pool *external.ExternalPoolConfig) (string, int, string, bool, error) {
	if a == nil || a.mgr == nil {
		return "", 0, "", false, errors.New("dhcp gw-lrp allocator: nil manager")
	}
	if vpcID == "" {
		return "", 0, "", false, errors.New("dhcp gw-lrp allocator: vpcID required")
	}
	if !pool.IsDHCP() {
		return "", 0, "", false, nil
	}
	if pool.BindBridge == "" {
		return "", 0, "", false, fmt.Errorf("dhcp gw-lrp allocator: pool %q missing bind_bridge", pool.Name)
	}

	entry, err := a.mgr.handleAcquire(ctx, acquireWireRequest{
		Bridge:   pool.BindBridge,
		ClientID: GatewayLRPClientID(vpcID),
		Purpose:  PurposeGatewayLRP,
		PoolName: pool.Name,
		VPCID:    vpcID,
	})
	if err != nil {
		return "", 0, "", false, fmt.Errorf("dhcp gw-lrp acquire: %w", err)
	}
	if entry == nil || entry.Lease == nil || entry.Lease.IP == nil {
		return "", 0, "", false, errors.New("dhcp gw-lrp allocator: empty lease")
	}
	nexthop, err := firstRouter(entry.Lease.Routers)
	if err != nil {
		return "", 0, "", false, fmt.Errorf("dhcp gw-lrp allocator: pool %q: %w", pool.Name, err)
	}

	prefix := maskToPrefix(entry.Lease.SubnetMask)
	if prefix <= 0 {
		prefix = pool.PrefixLen
	}
	if prefix <= 0 {
		prefix = 24
	}
	return entry.Lease.IP.String(), prefix, nexthop, true, nil
}

// firstRouter returns the first non-nil router, or an error if none present.
// OVN's default route requires an on-link nexthop.
func firstRouter(routers []net.IP) (string, error) {
	for _, r := range routers {
		if r != nil {
			return r.String(), nil
		}
	}
	return "", errors.New("DHCP ACK carried no Router option (DHCP code 3); cannot install default route")
}

// Release issues DHCPRELEASE for the per-VPC lease. Idempotent on
// unknown vpcIDs (manager treats missing client-ids as no-op).
func (a *DHCPGatewayLRPAllocator) Release(ctx context.Context, vpcID string) error {
	if a == nil || a.mgr == nil || vpcID == "" {
		return nil
	}
	return a.mgr.handleRelease(ctx, GatewayLRPClientID(vpcID))
}

// maskToPrefix returns the prefix length encoded in an IPv4 subnet mask.
// Empty mask → 0 (caller falls back to pool.PrefixLen).
func maskToPrefix(m net.IPMask) int {
	if len(m) == 0 {
		return 0
	}
	ones, _ := m.Size()
	return ones
}
