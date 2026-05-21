package vpcd

// Phase 2.6 (mulga-siv-129) adapter: TopologyHandler still implements
// topology.Manager so existing NATS subscribers and tests stay green, but the
// concrete logic now lives in network/topology.NewLiveManager. The shim here
// constructs the live manager lazily on first use, passing TopologyHandler's
// per-VPC external-pool resolver as the DHCP dns_server callback.
//
// Once all NATS subscribers route through the live manager directly (the
// remainder of mulga-siv-129), this adapter — and TopologyHandler's
// topology.Manager conformance — can be deleted along with topology.go.

import (
	"context"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

var _ topology.Manager = (*TopologyHandler)(nil)

func (h *TopologyHandler) topologyManager() topology.Manager {
	h.lmOnce.Do(func() {
		h.lm = topology.NewLiveManager(h.ovn, topology.WithDNSServer(h.lmDNSServer))
	})
	return h.lm
}

// lmDNSServer is the callback handed to topology.NewLiveManager so per-subnet
// DHCPOptions inherit the external pool's configured DNS list. Mirrors the
// legacy dnsServer() method without depending on TopologyHandler.findExternalPool's
// region/AZ keys (Phase 2.6 leaves multi-AZ routing to Phase 3).
func (h *TopologyHandler) lmDNSServer() string {
	pool := h.findExternalPool("", "")
	if pool != nil && len(pool.DNSServers) > 0 {
		return "{" + strings.Join(pool.DNSServers, ", ") + "}"
	}
	return "{8.8.8.8, 1.1.1.1}"
}

// EnsureVPC creates the OVN logical router for the VPC, idempotently.
func (h *TopologyHandler) EnsureVPC(ctx context.Context, spec topology.VPCSpec) error {
	return h.topologyManager().EnsureVPC(ctx, spec)
}

// DeleteVPC removes the OVN logical router for the VPC and cascades through
// any subnets that belong to it.
func (h *TopologyHandler) DeleteVPC(ctx context.Context, vpcID string) error {
	return h.topologyManager().DeleteVPC(ctx, vpcID)
}

// EnsureSubnet creates the OVN logical switch, subnet→VPC router port pair,
// and DHCP options for the subnet.
func (h *TopologyHandler) EnsureSubnet(ctx context.Context, spec topology.SubnetSpec) error {
	return h.topologyManager().EnsureSubnet(ctx, spec)
}

// DeleteSubnet tears down the subnet's logical switch, router port, switch
// router port, and DHCP options.
func (h *TopologyHandler) DeleteSubnet(ctx context.Context, spec topology.SubnetSpec) error {
	return h.topologyManager().DeleteSubnet(ctx, spec)
}

// EnsurePort creates the ENI's LogicalSwitchPort and joins it to its initial
// SG port groups in one OVSDB transaction.
func (h *TopologyHandler) EnsurePort(ctx context.Context, spec topology.PortSpec) error {
	return h.topologyManager().EnsurePort(ctx, spec)
}

// DeletePort clears the ENI's port-group memberships and removes the LSP.
func (h *TopologyHandler) DeletePort(ctx context.Context, spec topology.PortSpec) error {
	return h.topologyManager().DeletePort(ctx, spec)
}

// SetPortSecurityGroups reconciles the port's port-group memberships against
// the declared list.
func (h *TopologyHandler) SetPortSecurityGroups(ctx context.Context, portID string, sgIDs []string) error {
	return h.topologyManager().SetPortSecurityGroups(ctx, portID, sgIDs)
}

// EnsureSGPortGroup creates the OVN port group for a security group.
func (h *TopologyHandler) EnsureSGPortGroup(ctx context.Context, groupID string) error {
	return h.topologyManager().EnsureSGPortGroup(ctx, groupID)
}

// DeleteSGPortGroup removes the OVN port group for a security group.
func (h *TopologyHandler) DeleteSGPortGroup(ctx context.Context, groupID string) error {
	return h.topologyManager().DeleteSGPortGroup(ctx, groupID)
}

// securityGroupManager returns the lazily-constructed policy SG manager
// backed by the same OVN client as the live topology manager. Subscribers
// use it to apply ACL sets against the SG's port group.
func (h *TopologyHandler) securityGroupManager() policy.SecurityGroupManager {
	h.sgmOnce.Do(func() {
		h.sgm = policy.NewSecurityGroupManager(h.ovn)
	})
	return h.sgm
}
