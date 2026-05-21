package external

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// linkLocalGatewayNetwork is the link-local /30 every IGW gateway LRP
// carries in distributed-NAT (physical uplink) mode. The LRP IP itself
// never goes on the wire — per-VM dnat_and_snat rules with
// external_mac/logical_port ensure each chassis SNATs locally using its
// own uplink MAC.
//
// Centralised NAT (veth uplink) cannot use this — the gateway LRP is the
// on-wire egress point and must hold a WAN-subnet IP from the pool's
// gw_lrp_range so upstream router ARP succeeds (RFC 826).
const linkLocalGatewayNetwork = "169.254.0.1/30"

// linkLocalGatewayNexthop is the upstream end of linkLocalGatewayNetwork
// used as the default route nexthop in pool-less or pool-Gateway-empty
// configurations. Real deployments override via pool.Gateway.
const linkLocalGatewayNexthop = "169.254.0.2"

// gatewayIPExtIDKey is the LRP external_ids key holding the gateway LRP
// IP allocated from the pool's gw_lrp_range. Persisted so reconcile can
// recover the assignment without re-allocating and so sibling allocations
// see it as "used".
const gatewayIPExtIDKey = "spinifex:gateway_ip"

// FindPool returns the first pool matching the given region/AZ, using the
// fallback order: AZ-scoped → region-scoped → unscoped. Returns nil when
// no pool matches.
func FindPool(pools []ExternalPoolConfig, region, az string) *ExternalPoolConfig {
	for i := range pools {
		p := &pools[i]
		if p.AZ != "" && p.AZ == az && p.Region == region {
			return p
		}
	}
	for i := range pools {
		p := &pools[i]
		if p.AZ == "" && p.Region != "" && p.Region == region {
			return p
		}
	}
	for i := range pools {
		p := &pools[i]
		if p.Region == "" && p.AZ == "" {
			return p
		}
	}
	return nil
}

// GatewayIPAllocator resolves the gateway LRP IP for a VPC under a given
// pool. Implementations:
//
//   - StaticRangeAllocator (this package): picks the next free IP in
//     pool.gw_lrp_range, persisting the assignment via the LRP's
//     external_ids so reconciles see it on retry.
//
//   - DHCP-backed allocator (lives in services/vpcd until bead
//     mulga-siv-125.3.3 lands): requests an upstream DHCP lease and reuses
//     it on idempotent re-attach.
//
//   - LinkLocalAllocator (this package): always returns ok=false; callers
//     fall back to linkLocalGatewayNetwork. Useful for distributed-NAT
//     deployments where the gateway LRP never goes on the wire.
//
// Allocate's ok=false return means "I have nothing to provide" (caller
// falls back to link-local); a non-nil error means "the allocation
// attempt failed" (caller aborts).
type GatewayIPAllocator interface {
	Allocate(ctx context.Context, vpcID string, pool *ExternalPoolConfig) (ip string, prefixLen int, ok bool, err error)
	Release(ctx context.Context, vpcID string) error
}

// LinkLocalAllocator always returns ok=false. Use in distributed-NAT
// deployments where the gateway LRP is link-local and per-VM dnat_and_snat
// handles ARP per chassis.
type LinkLocalAllocator struct{}

var _ GatewayIPAllocator = LinkLocalAllocator{}

// Allocate always returns ok=false (caller uses linkLocalGatewayNetwork).
func (LinkLocalAllocator) Allocate(_ context.Context, _ string, _ *ExternalPoolConfig) (string, int, bool, error) {
	return "", 0, false, nil
}

// Release is a no-op — link-local IPs are not held in any store.
func (LinkLocalAllocator) Release(_ context.Context, _ string) error { return nil }

// StaticRangeAllocator picks the next free IP in pool.gw_lrp_range for a
// VPC's gateway LRP. Reads existing LRP external_ids to compute the used
// set and persists the chosen IP via IGWManager's LRP create path —
// allocator itself doesn't write, only reads.
type StaticRangeAllocator struct {
	OVN ovn.Client
}

var _ GatewayIPAllocator = (*StaticRangeAllocator)(nil)

// NewStaticRangeAllocator constructs a StaticRangeAllocator backed by the
// given OVN client.
func NewStaticRangeAllocator(client ovn.Client) *StaticRangeAllocator {
	return &StaticRangeAllocator{OVN: client}
}

// Allocate returns the next free gateway LRP IP for vpcID in pool's
// gw_lrp_range. If the VPC's gateway LRP already exists with a recorded
// allocation, the existing IP is returned (idempotent). Returns ok=false
// when the pool has no usable range (caller falls back to link-local).
func (a *StaticRangeAllocator) Allocate(ctx context.Context, vpcID string, pool *ExternalPoolConfig) (string, int, bool, error) {
	start, end, prefix, ok := gwLrpRange(pool)
	if !ok {
		return "", 0, false, nil
	}

	gwPortName := topology.GatewayRouterPort(vpcID)
	lrps, err := a.OVN.ListLogicalRouterPorts(ctx)
	if err != nil {
		return "", 0, false, fmt.Errorf("list LRPs for gw IP allocation: %w", err)
	}
	used := make(map[uint32]struct{}, len(lrps))
	for _, lrp := range lrps {
		existing := lrp.ExternalIDs[gatewayIPExtIDKey]
		if existing == "" {
			continue
		}
		if lrp.Name == gwPortName {
			return existing, prefix, true, nil
		}
		if v := net.ParseIP(existing).To4(); v != nil {
			used[ipv4ToUint32(v)] = struct{}{}
		}
	}

	startU := ipv4ToUint32(start)
	endU := ipv4ToUint32(end)
	for n := startU; n <= endU; n++ {
		if _, taken := used[n]; taken {
			continue
		}
		return uint32ToIPv4(n).String(), prefix, true, nil
	}
	return "", 0, false, fmt.Errorf("gw_lrp_range exhausted for pool %q (%s-%s)", pool.Name, pool.GwLrpRangeStart, pool.GwLrpRangeEnd)
}

// Release is a no-op — the LRP external_id is the source of truth and is
// cleared when the LRP itself is deleted on IGW detach.
func (a *StaticRangeAllocator) Release(_ context.Context, _ string) error { return nil }

// gwLrpRange returns the per-VPC gateway LRP IP range for a pool. Priority:
//
//  1. Explicit pool.GwLrpRangeStart/End.
//  2. Auto-derived from pool.Gateway + pool.PrefixLen — last 16 host IPs of
//     the WAN subnet (broadcast - 16 .. broadcast - 1). When that range
//     overlaps the per-VM EIP range (RangeStart..RangeEnd), shift to the
//     16 IPs immediately below RangeStart.
//
// Returns ok=false when the pool is missing or the gateway/prefix is
// unparseable — link-local has no role here, the WAN-subnet IP is the only
// thing the upstream router will ARP-resolve.
func gwLrpRange(pool *ExternalPoolConfig) (start, end net.IP, prefix int, ok bool) {
	if pool == nil {
		return nil, nil, 0, false
	}
	prefix = pool.PrefixLen
	if prefix <= 0 || prefix > 32 {
		prefix = 24
	}

	if pool.GwLrpRangeStart != "" || pool.GwLrpRangeEnd != "" {
		s := net.ParseIP(pool.GwLrpRangeStart).To4()
		e := net.ParseIP(pool.GwLrpRangeEnd).To4()
		if s != nil && e != nil && ipv4ToUint32(s) <= ipv4ToUint32(e) {
			return s, e, prefix, true
		}
		slog.Warn("external: invalid explicit gw_lrp_range, attempting auto-derive",
			"pool", pool.Name, "start", pool.GwLrpRangeStart, "end", pool.GwLrpRangeEnd)
	}

	gw := net.ParseIP(pool.Gateway).To4()
	if gw == nil {
		return nil, nil, 0, false
	}
	mask := net.CIDRMask(prefix, 32)
	network := gw.Mask(mask)
	bcast := make(net.IP, 4)
	for i := range 4 {
		bcast[i] = network[i] | ^mask[i]
	}
	bcastU := ipv4ToUint32(bcast)
	if bcastU < 17 {
		return nil, nil, 0, false
	}
	autoEndU := bcastU - 1
	autoStartU := bcastU - 16

	if pool.RangeStart != "" && pool.RangeEnd != "" {
		rs := net.ParseIP(pool.RangeStart).To4()
		re := net.ParseIP(pool.RangeEnd).To4()
		if rs != nil && re != nil {
			rsU := ipv4ToUint32(rs)
			reU := ipv4ToUint32(re)
			if autoEndU >= rsU && autoStartU <= reU {
				if rsU < 17 {
					return nil, nil, 0, false
				}
				autoEndU = rsU - 1
				autoStartU = rsU - 16
			}
		}
	}

	netU := ipv4ToUint32(network)
	if autoStartU <= netU {
		autoStartU = netU + 1
	}
	if autoEndU >= bcastU {
		autoEndU = bcastU - 1
	}
	if autoStartU > autoEndU {
		return nil, nil, 0, false
	}

	gwU := ipv4ToUint32(gw)
	if gwU >= autoStartU && gwU <= autoEndU {
		switch gwU {
		case autoStartU:
			autoStartU++
		case autoEndU:
			autoEndU--
		default:
			autoStartU = gwU + 1
		}
		if autoStartU > autoEndU {
			return nil, nil, 0, false
		}
	}

	return uint32ToIPv4(autoStartU), uint32ToIPv4(autoEndU), prefix, true
}
