package topology

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// Option configures a liveManager at construction.
type Option func(*liveManager)

// WithDNSServer overrides the DHCP dns_server option emitted with each
// subnet's DHCPOptions row. The function is called per-subnet so callers can
// resolve from external-pool config at request time. Default returns
// "{8.8.8.8, 1.1.1.1}".
func WithDNSServer(fn func() string) Option {
	return func(m *liveManager) { m.dnsServer = fn }
}

// NewLiveManager returns a topology.Manager that drives OVN via the given
// client. The returned manager owns logical-resource lifecycle (VPC router,
// subnet switch + LRP + LSP-router, ENI LSP) and SG port-group memberships.
// External connectivity (IGW, EIP, NAT GW) lives in network/external.
func NewLiveManager(client ovn.Client, opts ...Option) Manager {
	m := &liveManager{
		ovn:       client,
		dnsServer: defaultDNSServer,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

type liveManager struct {
	ovn       ovn.Client
	dnsServer func() string
}

var _ Manager = (*liveManager)(nil)

func defaultDNSServer() string { return "{8.8.8.8, 1.1.1.1}" }

// EnsureVPC creates the OVN logical router for the VPC, idempotently.
func (m *liveManager) EnsureVPC(ctx context.Context, spec VPCSpec) error {
	if m.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	if spec.VPCID == "" {
		return fmt.Errorf("EnsureVPC: empty VPCID")
	}
	routerName := VPCRouter(spec.VPCID)
	if _, err := m.ovn.GetLogicalRouter(ctx, routerName); err == nil {
		return nil
	}
	cidr := ""
	if spec.CIDR.IsValid() {
		cidr = spec.CIDR.String()
	}
	lr := &nbdb.LogicalRouter{
		Name: routerName,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": spec.VPCID,
			"spinifex:vni":    strconv.FormatInt(spec.VNI, 10),
			"spinifex:cidr":   cidr,
		},
	}
	if err := m.ovn.CreateLogicalRouter(ctx, lr); err != nil {
		return fmt.Errorf("create logical router %q: %w", routerName, err)
	}
	slog.Info("topology: EnsureVPC created router", "router", routerName, "vpc_id", spec.VPCID, "cidr", cidr)
	return nil
}

// DeleteVPC removes the OVN logical router for the VPC and cascades through
// any subnets that belong to it.
func (m *liveManager) DeleteVPC(ctx context.Context, vpcID string) error {
	if m.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	routerName := VPCRouter(vpcID)

	switches, err := m.ovn.ListLogicalSwitches(ctx)
	if err != nil {
		slog.Warn("topology: DeleteVPC list switches", "err", err)
	} else {
		for _, ls := range switches {
			if ls.ExternalIDs["spinifex:vpc_id"] != vpcID {
				continue
			}
			if err := m.ovn.DeleteLogicalSwitch(ctx, ls.Name); err != nil {
				slog.Warn("topology: DeleteVPC cascade switch", "switch", ls.Name, "err", err)
			}
		}
	}

	dhcpOpts, err := m.ovn.ListDHCPOptions(ctx)
	if err != nil {
		slog.Warn("topology: DeleteVPC list DHCP options", "err", err)
	} else {
		for _, opts := range dhcpOpts {
			if opts.ExternalIDs["spinifex:vpc_id"] != vpcID {
				continue
			}
			if err := m.ovn.DeleteDHCPOptions(ctx, opts.UUID); err != nil {
				slog.Warn("topology: DeleteVPC cascade DHCP", "uuid", opts.UUID, "err", err)
			}
		}
	}

	if err := m.ovn.DeleteLogicalRouter(ctx, routerName); err != nil {
		return fmt.Errorf("delete logical router %q: %w", routerName, err)
	}
	slog.Info("topology: DeleteVPC removed router", "router", routerName, "vpc_id", vpcID)
	return nil
}

// EnsureSubnet creates the OVN logical switch, subnet→VPC router port pair,
// and DHCP options for the subnet.
func (m *liveManager) EnsureSubnet(ctx context.Context, spec SubnetSpec) error {
	if m.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	if spec.SubnetID == "" || spec.VPCID == "" {
		return fmt.Errorf("EnsureSubnet: SubnetID/VPCID required")
	}
	if !spec.CIDR.IsValid() {
		return fmt.Errorf("EnsureSubnet: invalid CIDR for subnet %q", spec.SubnetID)
	}
	gwIP, prefixBits, err := SubnetGatewayCIDR(spec.CIDR)
	if err != nil {
		return fmt.Errorf("invalid subnet CIDR %q: %w", spec.CIDR, err)
	}
	cidr := spec.CIDR.String()
	gwCIDR := fmt.Sprintf("%s/%d", gwIP, prefixBits)
	routerMAC := utils.HashMAC(spec.SubnetID)
	switchName := SubnetSwitch(spec.SubnetID)
	routerName := VPCRouter(spec.VPCID)
	routerPortName := SubnetRouterPort(spec.SubnetID)
	switchRouterPortName := SubnetSwitchRouterPort(spec.SubnetID)

	if _, err := m.ovn.GetLogicalSwitch(ctx, switchName); err == nil {
		return nil
	}

	ls := &nbdb.LogicalSwitch{
		Name: switchName,
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": spec.SubnetID,
			"spinifex:vpc_id":    spec.VPCID,
		},
	}
	if err := m.ovn.CreateLogicalSwitch(ctx, ls); err != nil {
		return fmt.Errorf("create logical switch %q: %w", switchName, err)
	}

	lrp := &nbdb.LogicalRouterPort{
		Name:     routerPortName,
		MAC:      routerMAC,
		Networks: []string{gwCIDR},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": spec.SubnetID,
			"spinifex:vpc_id":    spec.VPCID,
		},
	}
	if err := m.ovn.CreateLogicalRouterPort(ctx, routerName, lrp); err != nil {
		_ = m.ovn.DeleteLogicalSwitch(ctx, switchName)
		return fmt.Errorf("create router port %q: %w", routerPortName, err)
	}

	lsp := &nbdb.LogicalSwitchPort{
		Name:      switchRouterPortName,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			"router-port": routerPortName,
		},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": spec.SubnetID,
			"spinifex:vpc_id":    spec.VPCID,
		},
	}
	if err := m.ovn.CreateLogicalSwitchPort(ctx, switchName, lsp); err != nil {
		_ = m.ovn.DeleteLogicalRouterPort(ctx, routerName, routerPortName)
		_ = m.ovn.DeleteLogicalSwitch(ctx, switchName)
		return fmt.Errorf("create switch router port %q: %w", switchRouterPortName, err)
	}

	dhcpOpts := &nbdb.DHCPOptions{
		CIDR: cidr,
		Options: map[string]string{
			"server_id":  gwIP,
			"server_mac": routerMAC,
			"lease_time": "3600",
			"router":     gwIP,
			"dns_server": m.dnsServer(),
			"mtu":        "1442",
		},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": spec.SubnetID,
			"spinifex:vpc_id":    spec.VPCID,
		},
	}
	if _, err := m.ovn.CreateDHCPOptions(ctx, dhcpOpts); err != nil {
		slog.Warn("topology: EnsureSubnet DHCP options create failed", "cidr", cidr, "err", err)
	}

	slog.Info("topology: EnsureSubnet created topology",
		"switch", switchName,
		"router_port", routerPortName,
		"gateway", gwCIDR,
		"subnet_id", spec.SubnetID,
	)
	return nil
}

// DeleteSubnet tears down the subnet's logical switch, router port, switch
// router port, and DHCP options.
func (m *liveManager) DeleteSubnet(ctx context.Context, spec SubnetSpec) error {
	if m.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	switchName := SubnetSwitch(spec.SubnetID)
	routerName := VPCRouter(spec.VPCID)
	routerPortName := SubnetRouterPort(spec.SubnetID)
	switchRouterPortName := SubnetSwitchRouterPort(spec.SubnetID)

	if err := m.ovn.DeleteLogicalSwitchPort(ctx, switchName, switchRouterPortName); err != nil {
		slog.Warn("topology: DeleteSubnet switch router port", "port", switchRouterPortName, "err", err)
	}
	if err := m.ovn.DeleteLogicalRouterPort(ctx, routerName, routerPortName); err != nil {
		slog.Warn("topology: DeleteSubnet router port", "port", routerPortName, "err", err)
	}
	if spec.CIDR.IsValid() {
		if dhcpOpts, err := m.ovn.FindDHCPOptionsByCIDR(ctx, spec.CIDR.String()); err == nil {
			if err := m.ovn.DeleteDHCPOptions(ctx, dhcpOpts.UUID); err != nil {
				slog.Warn("topology: DeleteSubnet DHCP options", "cidr", spec.CIDR.String(), "err", err)
			}
		}
	}
	if err := m.ovn.DeleteLogicalSwitch(ctx, switchName); err != nil {
		return fmt.Errorf("delete logical switch %q: %w", switchName, err)
	}
	slog.Info("topology: DeleteSubnet removed topology", "switch", switchName, "subnet_id", spec.SubnetID)
	return nil
}

// EnsurePort creates the ENI's LogicalSwitchPort and joins it to its initial
// SG port groups in one OVSDB transaction.
//
// If the LSP already exists from a crashed prior attempt that didn't reach the
// port-group join, SG memberships converge here rather than waiting for the
// next reconciler pass — that gap would leave a port with zero ACLs (OVN
// default = unrestricted).
func (m *liveManager) EnsurePort(ctx context.Context, spec PortSpec) error {
	if m.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	if spec.PortID == "" || spec.SubnetID == "" {
		return fmt.Errorf("EnsurePort: PortID/SubnetID required")
	}
	portName := Port(spec.PortID)
	switchName := SubnetSwitch(spec.SubnetID)

	if _, err := m.ovn.GetLogicalSwitchPort(ctx, portName); err == nil {
		if _, err := m.reconcilePortSGs(ctx, portName, spec.SGIDs); err != nil {
			return fmt.Errorf("reconcile SGs for existing port %q: %w", portName, err)
		}
		return nil
	}

	addrStr := fmt.Sprintf("%s %s", spec.MAC.String(), spec.PrivateIP.String())
	lsp := &nbdb.LogicalSwitchPort{
		Name:         portName,
		Addresses:    []string{addrStr},
		PortSecurity: []string{addrStr},
		ExternalIDs: map[string]string{
			"spinifex:eni_id":    spec.PortID,
			"spinifex:subnet_id": spec.SubnetID,
			"spinifex:vpc_id":    spec.VPCID,
		},
	}
	if dhcpOpts, err := m.ovn.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", spec.SubnetID); err == nil {
		lsp.DHCPv4Options = &dhcpOpts.UUID
	}

	pgNames := make([]string, 0, len(spec.SGIDs))
	for _, sgID := range spec.SGIDs {
		pgNames = append(pgNames, SecurityGroupPortGroup(sgID))
	}
	if err := m.ovn.CreateLogicalSwitchPortInGroups(ctx, switchName, lsp, pgNames); err != nil {
		return fmt.Errorf("create logical switch port %q on %q: %w", portName, switchName, err)
	}
	slog.Info("topology: EnsurePort created LSP",
		"port", portName,
		"switch", switchName,
		"eni_id", spec.PortID,
		"ip", spec.PrivateIP.String(),
		"mac", spec.MAC.String(),
		"addr_str", addrStr,
		"sgs", spec.SGIDs,
		"port_groups", pgNames,
	)
	return nil
}

// DeletePort clears the ENI's port-group memberships and removes the LSP.
func (m *liveManager) DeletePort(ctx context.Context, spec PortSpec) error {
	if m.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	portName := Port(spec.PortID)
	switchName := SubnetSwitch(spec.SubnetID)

	if _, err := m.reconcilePortSGs(ctx, portName, nil); err != nil {
		return fmt.Errorf("clear port group memberships for %q: %w", portName, err)
	}
	if err := m.ovn.DeleteLogicalSwitchPort(ctx, switchName, portName); err != nil {
		return fmt.Errorf("delete logical switch port %q on %q: %w", portName, switchName, err)
	}
	slog.Info("topology: DeletePort removed LSP",
		"port", portName,
		"switch", switchName,
		"eni_id", spec.PortID,
	)
	return nil
}

// EnsureSGPortGroup creates the empty OVN port-group row for a security
// group. Idempotent: a second call with the same groupID is a no-op. ACL
// programming lives in network/policy.SecurityGroupManager — callers must
// invoke it after EnsureSGPortGroup to attach the infrastructure and tenant
// rule sets.
func (m *liveManager) EnsureSGPortGroup(ctx context.Context, groupID string) error {
	if m.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	if groupID == "" {
		return fmt.Errorf("EnsureSGPortGroup: empty groupID")
	}
	pgName := SecurityGroupPortGroup(groupID)
	if _, err := m.ovn.GetPortGroup(ctx, pgName); err == nil {
		return nil
	} else if !errors.Is(err, ovn.ErrPortGroupNotFound) {
		return fmt.Errorf("get port group %s: %w", pgName, err)
	}
	if err := m.ovn.CreatePortGroup(ctx, pgName, nil); err != nil {
		return fmt.Errorf("create port group %s: %w", pgName, err)
	}
	slog.Info("topology: created SG port group", "pg", pgName, "group_id", groupID)
	return nil
}

// DeleteSGPortGroup clears every ACL on the SG's port group and removes the
// port group row. Idempotent: missing port group is treated as success.
// Reference-integrity dictates ClearACLs before DeletePortGroup — libovsdb
// rejects deleting a port group with dangling ACL references.
func (m *liveManager) DeleteSGPortGroup(ctx context.Context, groupID string) error {
	if m.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	if groupID == "" {
		return fmt.Errorf("DeleteSGPortGroup: empty groupID")
	}
	pgName := SecurityGroupPortGroup(groupID)
	if _, err := m.ovn.GetPortGroup(ctx, pgName); err != nil {
		if errors.Is(err, ovn.ErrPortGroupNotFound) {
			return nil
		}
		return fmt.Errorf("get port group %s: %w", pgName, err)
	}
	if err := m.ovn.ClearACLs(ctx, pgName); err != nil {
		return fmt.Errorf("clear ACLs on %s: %w", pgName, err)
	}
	if err := m.ovn.DeletePortGroup(ctx, pgName); err != nil {
		return fmt.Errorf("delete port group %s: %w", pgName, err)
	}
	slog.Info("topology: deleted SG port group", "pg", pgName, "group_id", groupID)
	return nil
}

// SetPortSecurityGroups reconciles the port's port-group memberships against
// the declared list.
func (m *liveManager) SetPortSecurityGroups(ctx context.Context, portID string, sgIDs []string) error {
	if m.ovn == nil {
		return fmt.Errorf("OVN client not connected")
	}
	portName := Port(portID)
	if _, err := m.reconcilePortSGs(ctx, portName, sgIDs); err != nil {
		return fmt.Errorf("reconcile SGs for port %q: %w", portName, err)
	}
	return nil
}

// reconcilePortSGs computes the add/remove diff and applies it in a single
// OVSDB transaction so a 5-SG → different-5-SG modify never exposes an
// intermediate state with fewer port groups (which would let the OVN default
// = unrestricted apply for the gap). Returns true if any change applied.
func (m *liveManager) reconcilePortSGs(ctx context.Context, lspName string, desiredSGs []string) (bool, error) {
	desired := make(map[string]struct{}, len(desiredSGs))
	for _, sgID := range desiredSGs {
		desired[SecurityGroupPortGroup(sgID)] = struct{}{}
	}

	currentNames, err := m.ovn.ListPortGroupsForPort(ctx, lspName)
	if err != nil {
		return false, fmt.Errorf("list current port groups for %s: %w", lspName, err)
	}
	current := make(map[string]struct{}, len(currentNames))
	for _, name := range currentNames {
		current[name] = struct{}{}
	}

	addPGs := make([]string, 0)
	for name := range desired {
		if _, ok := current[name]; !ok {
			addPGs = append(addPGs, name)
		}
	}
	removePGs := make([]string, 0)
	for name := range current {
		if _, ok := desired[name]; !ok {
			removePGs = append(removePGs, name)
		}
	}

	if len(addPGs) == 0 && len(removePGs) == 0 {
		return false, nil
	}

	if err := m.ovn.UpdatePortGroupMemberships(ctx, lspName, addPGs, removePGs); err != nil {
		return false, err
	}
	slog.Info("topology: reconciled port group memberships",
		"port", lspName,
		"added", addPGs,
		"removed", removePGs,
		"desired", desiredSGs,
	)
	return true, nil
}

// SubnetGatewayCIDR returns the .1 host IP and prefix length for a subnet
// CIDR. IPv4-only.
func SubnetGatewayCIDR(prefix netip.Prefix) (string, int, error) {
	if !prefix.IsValid() {
		return "", 0, fmt.Errorf("invalid prefix")
	}
	addr := prefix.Masked().Addr()
	if !addr.Is4() {
		return "", 0, fmt.Errorf("only IPv4 supported: %s", prefix)
	}
	bytes := addr.As4()
	bytes[3]++
	return netip.AddrFrom4(bytes).String(), prefix.Bits(), nil
}

// FormatDNSServerList renders an OVN dns_server value from a list of IPs.
// OVN expects "{ip1, ip2}" form for multiple entries.
func FormatDNSServerList(ips []string) string {
	if len(ips) == 0 {
		return defaultDNSServer()
	}
	return "{" + strings.Join(ips, ", ") + "}"
}
