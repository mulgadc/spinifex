package policy

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// defaultRoutePrefix is the IPv4 default-route prefix used by AddDefaultRoute
// / DeleteDefaultRoute. Held as a constant so the magic string isn't
// scattered across the package.
const defaultRoutePrefix = "0.0.0.0/0"

// RouteSpec is a single static route on a VPC's LogicalRouter. OutputPort is
// only required when Nexthop is not on a directly-connected subnet — e.g. the
// IGW default route uses 169.254.0.1/30 on the gateway LRP but the WAN
// nexthop sits on the uplink subnet, so OVN northd silently drops the route
// from southbound unless OutputPort pins it to the gateway LRP. The IGW
// attach path always sets it.
type RouteSpec struct {
	Prefix     netip.Prefix
	Nexthop    string
	OutputPort string
}

// RouteManager owns static routes on VPC LogicalRouters. The router itself
// is L2's responsibility; RouteManager only adds/removes route rows.
//
// Add operations are idempotent — a re-add with identical Nexthop+OutputPort
// is a no-op, a re-add with drifted fields deletes-then-adds. The historical
// vpcd path was non-idempotent (every retry left a duplicate row); the new
// API closes that bug at the contract level so the reconciler can replay
// freely.
type RouteManager interface {
	AddDefaultRoute(ctx context.Context, vpcID, nexthop, outputPort string) error
	DeleteDefaultRoute(ctx context.Context, vpcID string) error
	AddStaticRoute(ctx context.Context, vpcID string, route RouteSpec) error
	DeleteStaticRoute(ctx context.Context, vpcID string, prefix netip.Prefix) error
}

type routeManager struct {
	ovn ovn.Client
}

var _ RouteManager = (*routeManager)(nil)

// NewRouteManager constructs a RouteManager backed by the given OVN client.
func NewRouteManager(client ovn.Client) RouteManager {
	return &routeManager{ovn: client}
}

func (m *routeManager) AddDefaultRoute(ctx context.Context, vpcID, nexthop, outputPort string) error {
	prefix, err := netip.ParsePrefix(defaultRoutePrefix)
	if err != nil {
		return fmt.Errorf("parse default route prefix: %w", err)
	}
	return m.AddStaticRoute(ctx, vpcID, RouteSpec{
		Prefix:     prefix,
		Nexthop:    nexthop,
		OutputPort: outputPort,
	})
}

func (m *routeManager) DeleteDefaultRoute(ctx context.Context, vpcID string) error {
	return m.deleteByPrefixString(ctx, vpcID, defaultRoutePrefix)
}

func (m *routeManager) AddStaticRoute(ctx context.Context, vpcID string, route RouteSpec) error {
	router := topology.VPCRouter(vpcID)
	prefixStr := route.Prefix.String()

	existing, err := m.ovn.FindStaticRoute(ctx, router, prefixStr)
	if err != nil {
		return fmt.Errorf("find static route %s on %s: %w", prefixStr, router, err)
	}
	if existing != nil {
		if routeMatches(existing, route) {
			return nil
		}
		if err := m.ovn.DeleteStaticRoute(ctx, router, prefixStr); err != nil {
			return fmt.Errorf("delete drifted static route %s on %s: %w", prefixStr, router, err)
		}
	}

	row := &nbdb.LogicalRouterStaticRoute{
		IPPrefix: prefixStr,
		Nexthop:  route.Nexthop,
	}
	if route.OutputPort != "" {
		op := route.OutputPort
		row.OutputPort = &op
	}
	if err := m.ovn.AddStaticRoute(ctx, router, row); err != nil {
		return fmt.Errorf("add static route %s -> %s on %s: %w", prefixStr, route.Nexthop, router, err)
	}
	return nil
}

func (m *routeManager) DeleteStaticRoute(ctx context.Context, vpcID string, prefix netip.Prefix) error {
	return m.deleteByPrefixString(ctx, vpcID, prefix.String())
}

// deleteByPrefixString centralises the "delete and treat not-found as
// success" idempotency required by every Delete* entrypoint. Returns nil
// when the route is already absent — the only stable contract for callers
// driven by fire-and-forget events (IGW detach, NAT GW delete).
func (m *routeManager) deleteByPrefixString(ctx context.Context, vpcID, prefix string) error {
	router := topology.VPCRouter(vpcID)
	existing, err := m.ovn.FindStaticRoute(ctx, router, prefix)
	if err != nil {
		return fmt.Errorf("find static route %s on %s: %w", prefix, router, err)
	}
	if existing == nil {
		return nil
	}
	if err := m.ovn.DeleteStaticRoute(ctx, router, prefix); err != nil {
		return fmt.Errorf("delete static route %s on %s: %w", prefix, router, err)
	}
	return nil
}

func routeMatches(existing *nbdb.LogicalRouterStaticRoute, want RouteSpec) bool {
	if existing.Nexthop != want.Nexthop {
		return false
	}
	switch {
	case existing.OutputPort == nil && want.OutputPort == "":
		return true
	case existing.OutputPort == nil || want.OutputPort == "":
		return false
	default:
		return *existing.OutputPort == want.OutputPort
	}
}
