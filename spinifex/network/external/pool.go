package external

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// linkLocalGatewayNetwork is the link-local /30 the IGW gateway LRP carries
// in distributed-NAT mode. LRP IP never goes on the wire — per-VM dnat_and_snat
// with external_mac/logical_port handles upstream ARP per chassis.
// Centralised NAT requires a WAN-subnet IP from gw_lrp_range instead.
const linkLocalGatewayNetwork = "169.254.0.1/30"

const linkLocalGatewayNexthop = "169.254.0.2"

// gatewayIPExtIDKey is the LRP external_ids key for the allocated gateway IP.
const gatewayIPExtIDKey = "spinifex:gateway_ip"

// FindPool returns the first matching pool: AZ-scoped → region-scoped → unscoped.
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

// GatewayIPAllocator resolves the gateway LRP IP for a VPC.
// ok=false means caller falls back to link-local; error means abort.
// nexthop is the upstream gateway the VPC's default route should point at;
// empty means caller falls back to pool.Gateway / link-local.
type GatewayIPAllocator interface {
	Allocate(ctx context.Context, vpcID string, pool *ExternalPoolConfig) (ip string, prefixLen int, nexthop string, ok bool, err error)
	Release(ctx context.Context, vpcID string) error
}

// LinkLocalAllocator always returns ok=false (distributed-NAT, link-local LRP).
type LinkLocalAllocator struct{}

var _ GatewayIPAllocator = LinkLocalAllocator{}

func (LinkLocalAllocator) Allocate(_ context.Context, _ string, _ *ExternalPoolConfig) (string, int, string, bool, error) {
	return "", 0, "", false, nil
}

func (LinkLocalAllocator) Release(_ context.Context, _ string) error { return nil }

// StaticRangeAllocator picks the next free IP in pool.gw_lrp_range.
// Read-only: persistence is via IGWManager's LRP external_ids.
type StaticRangeAllocator struct {
	OVN ovn.Client
}

var _ GatewayIPAllocator = (*StaticRangeAllocator)(nil)

func NewStaticRangeAllocator(client ovn.Client) *StaticRangeAllocator {
	return &StaticRangeAllocator{OVN: client}
}

// Allocate returns the next free gateway LRP IP for vpcID. Idempotent: if the
// LRP already exists with a recorded allocation, returns the existing IP.
// Nexthop comes from pool.Gateway (operator-supplied for static pools).
func (a *StaticRangeAllocator) Allocate(ctx context.Context, vpcID string, pool *ExternalPoolConfig) (string, int, string, bool, error) {
	start, end, prefix, ok := gwLrpRange(pool)
	if !ok {
		return "", 0, "", false, nil
	}

	gwPortName := topology.GatewayRouterPort(vpcID)
	lrps, err := a.OVN.ListLogicalRouterPorts(ctx)
	if err != nil {
		return "", 0, "", false, fmt.Errorf("list LRPs for gw IP allocation: %w", err)
	}
	used := make(map[uint32]struct{}, len(lrps))
	for _, lrp := range lrps {
		existing := lrp.ExternalIDs[gatewayIPExtIDKey]
		if existing == "" {
			continue
		}
		if lrp.Name == gwPortName {
			return existing, prefix, pool.Gateway, true, nil
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
		return uint32ToIPv4(n).String(), prefix, pool.Gateway, true, nil
	}
	return "", 0, "", false, fmt.Errorf("gw_lrp_range exhausted for pool %q (%s-%s)", pool.Name, pool.GwLrpRangeStart, pool.GwLrpRangeEnd)
}

// Release is a no-op — LRP external_id is source of truth, cleared on detach.
func (a *StaticRangeAllocator) Release(_ context.Context, _ string) error { return nil }

// gwLrpRange returns the gateway LRP IP range: explicit pool.GwLrpRange* if set,
// else last 16 host IPs of the WAN subnet (shifted below RangeStart on overlap).
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
