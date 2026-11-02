package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	loop "github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/reconciler"
	"github.com/nats-io/nats.go/jetstream"
)

// Host ingress cadence. The resync is short because a gateway IP rebind after a
// DHCP lease move writes NB, not KV, so no watch announces it. The retry covers
// an IGW whose gateway LRP the attaching node has not created yet.
const (
	defaultHostIngressResync = 30 * time.Second
	hostIngressPendingRetry  = 5 * time.Second
)

// HostIngressRoutes is one host's table of routed-NAT VPC ingress routes, VPC
// CIDR to the gateway LRP transit IP the traffic enters OVN through.
type HostIngressRoutes interface {
	// List returns only the routes Ensure installed, keyed by CIDR.
	List(ctx context.Context) (map[string]string, error)
	Ensure(ctx context.Context, cidr, via string) error
	Remove(ctx context.Context, cidr string) error
}

// HostIngress converges this host's VPC ingress routes from intent. Every node
// runs one, without the drift lease: the routes are the host's own, and the
// attach event that used to install them reaches one vpcd of the queue group.
type HostIngress struct {
	// OVN is read only, for each attached VPC's gateway LRP transit IP.
	OVN    ovn.Client
	Routes HostIngressRoutes
	// LoadIntent reads intent fresh each pass. An error abandons the pass, so
	// an unreadable store never prunes a route.
	LoadIntent func(ctx context.Context) (IntentState, error)
	// PreferredVPC wins a CIDR shared by several attached VPCs; see ingressOutranks.
	PreferredVPC string
	// Resync is the backstop between passes. Zero means 30s.
	Resync time.Duration
}

// hostIngressPlan is the route table a pass converges to. held names CIDRs whose
// winning VPC has no resolvable gateway IP yet: whatever route they have is kept
// as it is, since the winner is known but where it enters OVN is not.
type hostIngressPlan struct {
	want    map[string]string
	held    map[string]struct{}
	pending bool
}

// ingressOutranks reports whether VPC a takes a shared CIDR from VPC b: the
// preferred VPC first, then the lower VPC ID. Every node ranks the same intent
// the same way, so the holder never depends on which node saw which event.
func ingressOutranks(a, b, preferred string) bool {
	if a == preferred || b == preferred {
		return a == preferred && b != preferred
	}
	return a < b
}

// planHostIngress picks one gateway per CIDR across the attached VPCs in intent.
// gatewayIP is asked only about each CIDR's winner; "" marks it pending.
func planHostIngress(intent IntentState, preferred string, gatewayIP func(vpcID string) string) hostIngressPlan {
	winners := map[string]string{}
	for vpcID := range intent.IGWs {
		vpc, ok := intent.VPCs[vpcID]
		if !ok || !vpc.CIDR.IsValid() {
			continue
		}
		cidr := vpc.CIDR.Masked().String()
		if cur, ok := winners[cidr]; !ok || ingressOutranks(vpcID, cur, preferred) {
			winners[cidr] = vpcID
		}
	}
	plan := hostIngressPlan{want: map[string]string{}, held: map[string]struct{}{}}
	for cidr, vpcID := range winners {
		if gw := gatewayIP(vpcID); gw != "" {
			plan.want[cidr] = gw
			continue
		}
		plan.held[cidr] = struct{}{}
		plan.pending = true
	}
	return plan
}

// gatewayTransitIP is the stamped gateway IP of vpcID's gateway LRP, falling
// back to its network address. "" when the port is absent or unreadable.
func (h *HostIngress) gatewayTransitIP(ctx context.Context, vpcID string) string {
	lrp, err := h.OVN.GetLogicalRouterPort(ctx, topology.GatewayRouterPort(vpcID))
	if err != nil || lrp == nil {
		return ""
	}
	if ip := lrp.ExternalIDs[policy.GatewayIPExtIDKey]; ip != "" {
		return ip
	}
	return gatewayLRPIP(lrp)
}

// Pass converges the routes once. It asks to be revisited while a winning VPC's
// gateway is still being created; the loop's resync covers everything else.
func (h *HostIngress) Pass(ctx context.Context) (time.Duration, error) {
	intent, err := h.LoadIntent(ctx)
	if err != nil {
		return 0, fmt.Errorf("load intent for host ingress routes: %w", err)
	}
	have, err := h.Routes.List(ctx)
	if err != nil {
		return 0, err
	}
	plan := planHostIngress(intent, h.PreferredVPC, func(vpcID string) string {
		return h.gatewayTransitIP(ctx, vpcID)
	})

	var errs []error
	for cidr, via := range plan.want {
		if have[cidr] == via {
			continue
		}
		if err := h.Routes.Ensure(ctx, cidr, via); err != nil {
			errs = append(errs, err)
		}
	}
	for cidr, via := range have {
		_, wanted := plan.want[cidr]
		_, held := plan.held[cidr]
		if wanted || held {
			continue
		}
		slog.InfoContext(ctx, "reconcile/host-ingress: pruning route no attached VPC holds",
			"vpc_cidr", cidr, "gw_lrp_ip", via)
		if err := h.Routes.Remove(ctx, cidr); err != nil {
			errs = append(errs, err)
		}
	}

	var revisit time.Duration
	if plan.pending {
		revisit = hostIngressPendingRetry
	}
	return revisit, errors.Join(errs...)
}

// RunHostIngress runs h on every intent change, every nudge and every
// resync until ctx ends. nudge carries the attach and detach events
// this node handled, which can land before the gateway they announce.
func RunHostIngress(ctx context.Context, js jetstream.JetStream, h *HostIngress, nudge <-chan struct{}) {
	resync := h.Resync
	if resync <= 0 {
		resync = defaultHostIngressResync
	}
	loop.Run(ctx, loop.Config{
		Name:      "vpcd/host-ingress",
		Sources:   []loop.Source{intentSource(js)},
		Reconcile: h.Pass,
		Trigger:   nudge,
		Resync:    resync,
		Debounce:  driftDebounce,
	})
}
