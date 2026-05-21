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

	"github.com/mulgadc/spinifex/spinifex/network/external"
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

// DeleteSGPortGroupByName removes the OVN port group given its raw name.
func (h *TopologyHandler) DeleteSGPortGroupByName(ctx context.Context, pgName string) error {
	return h.topologyManager().DeleteSGPortGroupByName(ctx, pgName)
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

// natManager returns the lazily-constructed policy.NATManager. NAT mode is
// derived from the configured bridge mode (veth → centralized, direct →
// distributed) and the FlowsBarrier is bound to waitForFlowsHV so the EIP
// subscriber blocks on hypervisor flow-install before responding.
func (h *TopologyHandler) natManager() (policy.NATManager, error) {
	h.natmOnce.Do(func() {
		mode := policy.NATModeDistributed
		if h.useCentralizedNAT() {
			mode = policy.NATModeCentralized
		}
		h.natm, h.natmErr = policy.NewNATManager(h.ovn, mode, policy.WithFlowsBarrier(waitForFlowsHV))
	})
	return h.natm, h.natmErr
}

// igwManager returns the lazily-constructed external.IGWManager. Only the
// static-pool / distributed-NAT IGW subscriber path goes through it; the
// DHCP-coupled centralised path is held in attachIGWLegacy until bead
// mulga-siv-125.3.3 removes the vpcd-local DHCP manager.
func (h *TopologyHandler) igwManager() (external.IGWManager, error) {
	h.igwmOnce.Do(func() {
		nm, err := h.natManager()
		if err != nil {
			h.igwmErr = err
			return
		}
		mode := policy.NATModeDistributed
		if h.useCentralizedNAT() {
			mode = policy.NATModeCentralized
		}
		var poolCfg *external.ExternalPoolConfig
		if p := h.findExternalPool("", ""); p != nil {
			shared := external.ExternalPoolConfig{
				Name:            p.Name,
				Source:          p.Source,
				RangeStart:      p.RangeStart,
				RangeEnd:        p.RangeEnd,
				Gateway:         p.Gateway,
				GatewayIP:       p.GatewayIP,
				PrefixLen:       p.PrefixLen,
				DNSServers:      p.DNSServers,
				Region:          p.Region,
				AZ:              p.AZ,
				DhcpBindBridge:  p.DhcpBindBridge,
				GwLrpRangeStart: p.GwLrpRangeStart,
				GwLrpRangeEnd:   p.GwLrpRangeEnd,
			}
			poolCfg = &shared
		}
		h.igwm, h.igwmErr = external.NewIGWManager(external.IGWManagerConfig{
			OVN:          h.ovn,
			Routes:       policy.NewRouteManager(h.ovn),
			NAT:          nm,
			Pool:         poolCfg,
			Allocator:    external.NewStaticRangeAllocator(h.ovn),
			Chassis:      h.chassisNames,
			NATMode:      mode,
			FlowsBarrier: waitForFlowsHV,
		})
	})
	return h.igwm, h.igwmErr
}
