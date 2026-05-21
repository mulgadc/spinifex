package policy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
)

// EIPSpec is a 1:1 Elastic IP NAT (dnat_and_snat) attaching ExternalIP to
// LogicalIP on the VPC's router. In NATModeDistributed, PortName and MAC
// MUST be set so OVN can install per-chassis flows; in NATModeCentralized
// they are ignored. Callers that target distributed mode without supplying
// both fields fall back to the centralised shape — the NAT still works but
// hairpins through the gateway chassis.
type EIPSpec struct {
	VPCID      string
	ExternalIP string
	LogicalIP  string
	PortName   string // OVN LSP name backing the VM
	MAC        string // external MAC for distributed NAT
}

// NATGWSpec is a NAT Gateway SNAT rule: every packet leaving the private
// subnet whose source is SubnetCIDR is rewritten to PublicIP. The OVN rule
// keys on (snat, SubnetCIDR) on the VPC's router.
type NATGWSpec struct {
	VPCID        string
	NATGatewayID string
	PublicIP     string
	SubnetCIDR   string
}

// NATManager owns the OVN NAT rule lifecycle for one cluster. It does not
// create the LogicalRouter — L2 (topology.Manager) owns that — and does
// not allocate public IPs (handlers/ec2/eip owns pool allocation).
//
// NAT mode is fixed at construction time from the host's UplinkMode and
// never changes at runtime.
type NATManager interface {
	// AddEIP installs a dnat_and_snat rule. In NATModeDistributed, PortName
	// and MAC are written to logical_port / external_mac so the rule fires
	// on the VM's own chassis. Stale dnat_and_snat rules referencing the
	// same ExternalIP on any router are removed first — an EIP can change
	// VPC association in our pool model and a leftover rule on the old
	// router would silently steal the public IP.
	AddEIP(ctx context.Context, eip EIPSpec) error

	// DeleteEIP removes the dnat_and_snat rule with the given LogicalIP on
	// the VPC's router. Returns nil when the rule is already absent
	// (matching the idempotent semantics required by the multiple cleanup
	// paths that publish vpc.delete-nat).
	DeleteEIP(ctx context.Context, vpcID, logicalIP string) error

	// AddNATGateway installs the snat rule for a NAT Gateway's private
	// subnet. The OVN key is (snat, SubnetCIDR); AddNATGateway therefore
	// rejects two NAT GWs on overlapping subnet CIDRs.
	AddNATGateway(ctx context.Context, gw NATGWSpec) error

	// DeleteNATGateway removes the snat rule keyed by SubnetCIDR on the
	// VPC's router. Idempotent.
	DeleteNATGateway(ctx context.Context, vpcID, subnetCIDR string) error

	// AddSNAT installs the IGW default-outbound snat rule rewriting the
	// VPC CIDR to ExternalIP. AWS-parity behaviour (only EIP-backed VMs
	// can egress) means callers in the IGW attach path typically skip
	// this — it exists for future deployments that opt into blanket SNAT.
	AddSNAT(ctx context.Context, vpcID, vpcCIDR, externalIP string) error

	// DeleteSNAT removes the IGW default-outbound snat rule for vpcCIDR.
	// Idempotent.
	DeleteSNAT(ctx context.Context, vpcID, vpcCIDR string) error
}

type natManager struct {
	ovn  ovn.Client
	mode NATMode
}

var _ NATManager = (*natManager)(nil)

// NewNATManager constructs a NATManager bound to one NAT mode. mode is
// resolved at startup from host.Wiring.UplinkMode() via NATModeFromUplinkMode
// and must not be NATModeUnknown.
func NewNATManager(client ovn.Client, mode NATMode) (NATManager, error) {
	if mode == NATModeUnknown {
		return nil, fmt.Errorf("NAT mode is unknown; resolve from host.Wiring.UplinkMode()")
	}
	return &natManager{ovn: client, mode: mode}, nil
}

func (m *natManager) AddEIP(ctx context.Context, eip EIPSpec) error {
	router := topology.VPCRouter(eip.VPCID)

	natRule := &nbdb.NAT{
		Type:       "dnat_and_snat",
		ExternalIP: eip.ExternalIP,
		LogicalIP:  eip.LogicalIP,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id":    eip.VPCID,
			"spinifex:public_ip": eip.ExternalIP,
		},
	}
	if m.mode == NATModeDistributed && eip.PortName != "" && eip.MAC != "" {
		mac := eip.MAC
		port := eip.PortName
		natRule.ExternalMAC = &mac
		natRule.LogicalPort = &port
	}

	// Search every router, not just the target — stale rules may exist on
	// a different VPC's router when vpc.delete-nat (fire-and-forget) hasn't
	// been processed before the IP was reused by a new VPC.
	if removed, err := m.ovn.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", eip.ExternalIP); err != nil {
		slog.Warn("policy: stale NAT cleanup failed before AddEIP", "external_ip", eip.ExternalIP, "err", err)
	} else if removed > 0 {
		slog.Info("policy: cleaned stale dnat_and_snat rules before AddEIP", "external_ip", eip.ExternalIP, "removed", removed)
	}

	if err := m.ovn.AddNAT(ctx, router, natRule); err != nil {
		return fmt.Errorf("add dnat_and_snat %s -> %s on %s: %w", eip.LogicalIP, eip.ExternalIP, router, err)
	}
	return nil
}

func (m *natManager) DeleteEIP(ctx context.Context, vpcID, logicalIP string) error {
	router := topology.VPCRouter(vpcID)
	if err := m.ovn.DeleteNAT(ctx, router, "dnat_and_snat", logicalIP); err != nil {
		if errors.Is(err, ovn.ErrNATNotFound) {
			return nil
		}
		return fmt.Errorf("delete dnat_and_snat %s on %s: %w", logicalIP, router, err)
	}
	return nil
}

func (m *natManager) AddNATGateway(ctx context.Context, gw NATGWSpec) error {
	router := topology.VPCRouter(gw.VPCID)

	snatRule := &nbdb.NAT{
		Type:       "snat",
		ExternalIP: gw.PublicIP,
		LogicalIP:  gw.SubnetCIDR,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id":         gw.VPCID,
			"spinifex:nat_gateway_id": gw.NATGatewayID,
		},
	}
	if err := m.ovn.AddNAT(ctx, router, snatRule); err != nil {
		return fmt.Errorf("add NAT GW snat %s -> %s on %s: %w", gw.SubnetCIDR, gw.PublicIP, router, err)
	}
	return nil
}

func (m *natManager) DeleteNATGateway(ctx context.Context, vpcID, subnetCIDR string) error {
	router := topology.VPCRouter(vpcID)
	if err := m.ovn.DeleteNAT(ctx, router, "snat", subnetCIDR); err != nil {
		if errors.Is(err, ovn.ErrNATNotFound) {
			return nil
		}
		return fmt.Errorf("delete NAT GW snat %s on %s: %w", subnetCIDR, router, err)
	}
	return nil
}

func (m *natManager) AddSNAT(ctx context.Context, vpcID, vpcCIDR, externalIP string) error {
	router := topology.VPCRouter(vpcID)
	snatRule := &nbdb.NAT{
		Type:       "snat",
		ExternalIP: externalIP,
		LogicalIP:  vpcCIDR,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": vpcID,
			"spinifex:role":   "igw-default-snat",
		},
	}
	if err := m.ovn.AddNAT(ctx, router, snatRule); err != nil {
		return fmt.Errorf("add IGW snat %s -> %s on %s: %w", vpcCIDR, externalIP, router, err)
	}
	return nil
}

func (m *natManager) DeleteSNAT(ctx context.Context, vpcID, vpcCIDR string) error {
	router := topology.VPCRouter(vpcID)
	if err := m.ovn.DeleteNAT(ctx, router, "snat", vpcCIDR); err != nil {
		if errors.Is(err, ovn.ErrNATNotFound) {
			return nil
		}
		return fmt.Errorf("delete IGW snat %s on %s: %w", vpcCIDR, router, err)
	}
	return nil
}
