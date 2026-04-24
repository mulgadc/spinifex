package vpcd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
)

// NATS topics for VPC lifecycle events published by the daemon.
const (
	TopicVPCCreate        = "vpc.create"
	TopicVPCDelete        = "vpc.delete"
	TopicSubnetCreate     = "vpc.create-subnet"
	TopicSubnetDelete     = "vpc.delete-subnet"
	TopicCreatePort       = "vpc.create-port"
	TopicDeletePort       = "vpc.delete-port"
	TopicPortStatus       = "vpc.port-status"
	TopicIGWAttach        = "vpc.igw-attach"
	TopicIGWDetach        = "vpc.igw-detach"
	TopicAddNAT           = "vpc.add-nat"
	TopicDeleteNAT        = "vpc.delete-nat"
	TopicAddNATGateway    = "vpc.add-nat-gateway"
	TopicDeleteNATGateway = "vpc.delete-nat-gateway"
)

// VPCEvent is published on vpc.create after a VPC is persisted.
type VPCEvent struct {
	VpcId     string `json:"vpc_id"`
	CidrBlock string `json:"cidr_block"`
	VNI       int64  `json:"vni"`
}

// SubnetEvent is published on vpc.create-subnet / vpc.delete-subnet.
type SubnetEvent struct {
	SubnetId  string `json:"subnet_id"`
	VpcId     string `json:"vpc_id"`
	CidrBlock string `json:"cidr_block"`
}

// PortEvent is published on vpc.create-port / vpc.delete-port.
type PortEvent struct {
	NetworkInterfaceId string `json:"network_interface_id"`
	SubnetId           string `json:"subnet_id"`
	VpcId              string `json:"vpc_id"`
	PrivateIpAddress   string `json:"private_ip_address"`
	MacAddress         string `json:"mac_address"`
}

// NATEvent is published on vpc.add-nat / vpc.delete-nat for 1:1 public IP NAT.
type NATEvent struct {
	VpcId      string `json:"vpc_id"`
	ExternalIP string `json:"external_ip"`
	LogicalIP  string `json:"logical_ip"`
	PortName   string `json:"port_name"` // logical port for distributed NAT
	MAC        string `json:"mac"`       // external MAC for distributed NAT
}

// NATGatewayEvent is published on vpc.add-nat-gateway / vpc.delete-nat-gateway.
type NATGatewayEvent struct {
	VpcId        string `json:"vpc_id"`
	NatGatewayId string `json:"nat_gateway_id"`
	PublicIp     string `json:"public_ip"`
	SubnetCidr   string `json:"subnet_cidr"` // private subnet CIDR for SNAT rule
}

// Bridge mode constants for external connectivity.
const (
	// BridgeModeMacvlan uses a macvlan sub-interface on the WAN NIC. SSH-safe
	// for single-NIC hosts but requires centralized NAT and MAC alignment.
	BridgeModeMacvlan = "macvlan"
	// BridgeModeDirect adds the WAN NIC directly to br-external as an OVS port.
	// Enables distributed NAT and avoids macvlan workarounds. Only safe when
	// the WAN NIC is NOT the SSH/management NIC.
	BridgeModeDirect = "direct"
	// BridgeModeVeth uses a veth pair to link a Linux bridge (br-wan) to an
	// OVS bridge (br-ext). Requires centralized NAT like macvlan because the
	// Linux bridge intermediary breaks distributed NAT hairpin routing.
	BridgeModeVeth = "veth"
	// OvnExternalBridge is the OVS bridge that ovn-bridge-mappings targets
	// for the "external" localnet. Owned by setup-ovn.sh's ovn-bridge-mappings
	// setup and independent of dhcp_bind_bridge (which is Linux-side in veth
	// mode). Named as a constant so the vpcd sanity check refers to the
	// contract, not a hardcoded string (D18).
	OvnExternalBridge = "br-ext"
)

// TopologyHandler translates VPC lifecycle NATS events into OVN NB DB operations.
type TopologyHandler struct {
	ovn           OVNClient
	externalMode  string
	externalPools []ExternalPoolConfig
	chassisNames  []string // OVN chassis names for gateway HA scheduling
	bridgeMode    string   // "direct" or "macvlan" — controls NAT mode and localnet options
}

// NewTopologyHandler creates a new TopologyHandler with optional external network config.
func NewTopologyHandler(ovn OVNClient, opts ...TopologyOption) *TopologyHandler {
	h := &TopologyHandler{ovn: ovn}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// TopologyOption configures a TopologyHandler.
type TopologyOption func(*TopologyHandler)

// WithExternalNetwork configures external connectivity for public subnets.
func WithExternalNetwork(mode string, pools []ExternalPoolConfig) TopologyOption {
	return func(h *TopologyHandler) {
		h.externalMode = mode
		h.externalPools = pools
	}
}

// WithChassisNames sets the OVN chassis names for gateway HA scheduling.
func WithChassisNames(names []string) TopologyOption {
	return func(h *TopologyHandler) {
		h.chassisNames = names
	}
}

// WithBridgeMode sets the external bridge mode ("direct", "macvlan", or "veth").
// Direct bridge enables distributed NAT; macvlan and veth use centralized NAT.
// Defaults to macvlan if not set (backward-compatible).
func WithBridgeMode(mode string) TopologyOption {
	return func(h *TopologyHandler) {
		h.bridgeMode = mode
	}
}

// isMacvlanMode returns true if the external bridge uses a macvlan interface.
// This is the default when bridgeMode is unset for backward compatibility.
func (h *TopologyHandler) isMacvlanMode() bool {
	return h.bridgeMode == BridgeModeMacvlan || h.bridgeMode == ""
}

// useCentralizedNAT returns true if the bridge mode requires centralized NAT.
// Macvlan and veth modes both need centralized NAT — macvlan because of MAC
// filtering, veth because the Linux bridge intermediary breaks distributed NAT
// hairpin routing. Only direct bridge mode supports distributed NAT.
func (h *TopologyHandler) useCentralizedNAT() bool {
	return h.bridgeMode != BridgeModeDirect
}

// ensureLocalnetOptions aligns options on an existing localnet port with the
// current bridge mode. CreateLogicalSwitchPort only seeds options at first
// creation; once created, stale options persist forever (e.g. vpcd came up in
// the wrong mode once — pre-Fix 1 — or an operator cleared options manually).
// This read-before-write is a no-op when already correct, so it is safe to
// call on every reconcile.
//
// network_name=external is required regardless of bridge mode — it binds the
// port to the ovn-bridge-mappings entry for the external OVS bridge.
// Centralized mode (veth/macvlan): options:nat-addresses=router must be set.
// Distributed mode (direct): options:nat-addresses must be absent.
func (h *TopologyHandler) ensureLocalnetOptions(ctx context.Context, extPortName string) error {
	lsp, err := h.ovn.GetLogicalSwitchPort(ctx, extPortName)
	if err != nil {
		return fmt.Errorf("get localnet port %s: %w", extPortName, err)
	}
	wantNat := h.useCentralizedNAT()
	currentNat, hasNat := lsp.Options["nat-addresses"]
	currentNetName, hasNetName := lsp.Options["network_name"]
	natOK := (wantNat && hasNat && currentNat == "router") || (!wantNat && !hasNat)
	netNameOK := hasNetName && currentNetName == "external"
	if natOK && netNameOK {
		return nil
	}
	if lsp.Options == nil {
		lsp.Options = map[string]string{}
	}
	lsp.Options["network_name"] = "external"
	if wantNat {
		lsp.Options["nat-addresses"] = "router"
	} else {
		delete(lsp.Options, "nat-addresses")
	}
	slog.Info("vpcd: retrofitting localnet options",
		"port", extPortName, "bridge_mode", h.bridgeMode,
		"nat-addresses", lsp.Options["nat-addresses"],
		"network_name", lsp.Options["network_name"])
	if err := h.ovn.UpdateLogicalSwitchPort(ctx, lsp); err != nil {
		return fmt.Errorf("update localnet port %s options: %w", extPortName, err)
	}
	return nil
}

// RetrofitAllExternalLocalnetOptions walks every OVN logical switch tagged
// spinifex:role=external and calls ensureLocalnetOptions on its localnet
// port. OVN is the source of truth — NATS KV records for IGWs may be absent
// or stale (external switch was created via a live event whose KV record
// later expired, or an operator cleared options manually). Walking OVN
// directly catches all of these on every vpcd startup. Idempotent.
func (h *TopologyHandler) RetrofitAllExternalLocalnetOptions(ctx context.Context) {
	switches, err := h.ovn.ListLogicalSwitches(ctx)
	if err != nil {
		slog.Warn("vpcd: retrofit skipped — list logical switches failed", "err", err)
		return
	}
	for i := range switches {
		ls := &switches[i]
		if ls.ExternalIDs["spinifex:role"] != "external" {
			continue
		}
		vpcID := ls.ExternalIDs["spinifex:vpc_id"]
		if vpcID == "" {
			continue
		}
		extPortName := "ext-port-" + vpcID
		if err := h.ensureLocalnetOptions(ctx, extPortName); err != nil {
			slog.Error("vpcd: retrofit localnet options failed", "port", extPortName, "err", err)
		}
	}
}

// dnsServer returns the DNS server string for OVN DHCP options.
// Uses dns_servers from the external pool config (auto-detected by admin init).
// Falls back to 8.8.8.8 and 1.1.1.1 if none configured.
// OVN DHCP expects the format "{ip1, ip2}" for multiple servers.
func (h *TopologyHandler) dnsServer() string {
	pool := h.findExternalPool("", "")
	if pool != nil && len(pool.DNSServers) > 0 {
		return "{" + strings.Join(pool.DNSServers, ", ") + "}"
	}
	// Fallback: public DNS
	return "{8.8.8.8, 1.1.1.1}"
}

// findExternalPool returns the first pool matching the given region/AZ,
// using the fallback order: AZ-scoped → region-scoped → unscoped.
func (h *TopologyHandler) findExternalPool(region, az string) *ExternalPoolConfig {
	// 1. AZ-scoped match
	for i := range h.externalPools {
		p := &h.externalPools[i]
		if p.AZ != "" && p.AZ == az && p.Region == region {
			return p
		}
	}
	// 2. Region-scoped (no AZ)
	for i := range h.externalPools {
		p := &h.externalPools[i]
		if p.AZ == "" && p.Region != "" && p.Region == region {
			return p
		}
	}
	// 3. Unscoped (no region, no AZ — global pool)
	for i := range h.externalPools {
		p := &h.externalPools[i]
		if p.Region == "" && p.AZ == "" {
			return p
		}
	}
	return nil
}

// Subscribe registers NATS subscriptions for VPC lifecycle topics.
// Global topology events (VPC/subnet/IGW) use a queue group so exactly one vpcd
// instance handles each event — all instances connect to the same OVN NB DB, so
// any one can process these. Per-node port events use regular subscriptions so
// all instances see them (future: route to the specific node).
func (h *TopologyHandler) Subscribe(nc *nats.Conn) ([]*nats.Subscription, error) {
	type sub struct {
		topic   string
		handler nats.MsgHandler
		queue   bool // true = use queue group (one handler), false = fan-out (all handlers)
	}

	subs := []sub{
		{TopicVPCCreate, h.handleVPCCreate, true},
		{TopicVPCDelete, h.handleVPCDelete, true},
		{TopicSubnetCreate, h.handleSubnetCreate, true},
		{TopicSubnetDelete, h.handleSubnetDelete, true},
		{TopicCreatePort, h.handleCreatePort, true},
		{TopicDeletePort, h.handleDeletePort, true},
		{TopicIGWAttach, h.handleIGWAttach, true},
		{TopicIGWDetach, h.handleIGWDetach, true},
		{TopicAddNAT, h.handleAddNAT, true},
		{TopicDeleteNAT, h.handleDeleteNAT, true},
		{TopicAddNATGateway, h.handleAddNATGateway, true},
		{TopicDeleteNATGateway, h.handleDeleteNATGateway, true},
		{TopicCreateSG, h.handleCreateSG, true},
		{TopicDeleteSG, h.handleDeleteSG, true},
		{TopicUpdateSG, h.handleUpdateSG, true},
	}

	var result []*nats.Subscription
	for _, s := range subs {
		var natsSub *nats.Subscription
		var err error
		if s.queue {
			natsSub, err = nc.QueueSubscribe(s.topic, "vpcd-workers", s.handler)
		} else {
			natsSub, err = nc.Subscribe(s.topic, s.handler)
		}
		if err != nil {
			for _, r := range result {
				_ = r.Unsubscribe()
			}
			return nil, fmt.Errorf("subscribe %s: %w", s.topic, err)
		}
		result = append(result, natsSub)
		slog.Info("Subscribed to VPC topic", "topic", s.topic)
	}

	return result, nil
}

// --- VPC (LogicalRouter) ---

func (h *TopologyHandler) handleVPCCreate(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt VPCEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.create event", "err", err)
		respond(msg, err)
		return
	}

	routerName := "vpc-" + evt.VpcId
	ctx := context.Background()

	// Idempotent: skip if router already exists (another vpcd instance may have created it)
	if _, err := h.ovn.GetLogicalRouter(ctx, routerName); err == nil {
		slog.Debug("vpcd: logical router already exists, skipping", "router", routerName)
		respond(msg, nil)
		return
	}

	lr := &nbdb.LogicalRouter{
		Name: routerName,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": evt.VpcId,
			"spinifex:vni":    strconv.FormatInt(evt.VNI, 10),
		},
	}

	if err := h.ovn.CreateLogicalRouter(ctx, lr); err != nil {
		slog.Error("vpcd: failed to create logical router", "router", routerName, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: created logical router for VPC", "router", routerName, "vpc_id", evt.VpcId)
	respond(msg, nil)
}

func (h *TopologyHandler) handleVPCDelete(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt VPCEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.delete event", "err", err)
		respond(msg, err)
		return
	}

	routerName := "vpc-" + evt.VpcId
	ctx := context.Background()

	// List all switches to find ones belonging to this VPC
	switches, err := h.ovn.ListLogicalSwitches(ctx)
	if err != nil {
		slog.Warn("vpcd: failed to list switches during VPC delete", "err", err)
	} else {
		for _, ls := range switches {
			if ls.ExternalIDs["spinifex:vpc_id"] == evt.VpcId {
				// Delete switch ports first
				for range ls.Ports {
					// Ports are UUIDs; best-effort cleanup
				}
				if err := h.ovn.DeleteLogicalSwitch(ctx, ls.Name); err != nil {
					slog.Warn("vpcd: failed to delete switch during VPC cascade", "switch", ls.Name, "err", err)
				}
			}
		}
	}

	// Delete DHCP options for this VPC
	dhcpOpts, err := h.ovn.ListDHCPOptions(ctx)
	if err != nil {
		slog.Warn("vpcd: failed to list DHCP options during VPC delete", "err", err)
	} else {
		for _, opts := range dhcpOpts {
			if opts.ExternalIDs["spinifex:vpc_id"] == evt.VpcId {
				if err := h.ovn.DeleteDHCPOptions(ctx, opts.UUID); err != nil {
					slog.Warn("vpcd: failed to delete DHCP options during VPC cascade", "uuid", opts.UUID, "err", err)
				}
			}
		}
	}

	if err := h.ovn.DeleteLogicalRouter(ctx, routerName); err != nil {
		slog.Error("vpcd: failed to delete logical router", "router", routerName, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: deleted logical router for VPC", "router", routerName, "vpc_id", evt.VpcId)
	respond(msg, nil)
}

// --- Subnet (LogicalSwitch + RouterPort + DHCP) ---

func (h *TopologyHandler) handleSubnetCreate(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt SubnetEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.create-subnet event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	switchName := "subnet-" + evt.SubnetId
	routerName := "vpc-" + evt.VpcId
	routerPortName := "rtr-" + evt.SubnetId
	switchRouterPortName := "rtr-port-" + evt.SubnetId

	// Parse subnet CIDR to compute gateway IP
	gwIP, mask, err := subnetGateway(evt.CidrBlock)
	if err != nil {
		slog.Error("vpcd: invalid subnet CIDR", "cidr", evt.CidrBlock, "err", err)
		respond(msg, err)
		return
	}
	gwCIDR := fmt.Sprintf("%s/%d", gwIP, mask)

	// Generate a deterministic MAC for the router port
	routerMAC := generateMAC(evt.SubnetId)

	// Idempotent: skip if switch already exists (another vpcd instance may have created it)
	if _, err := h.ovn.GetLogicalSwitch(ctx, switchName); err == nil {
		slog.Debug("vpcd: subnet topology already exists, skipping", "switch", switchName)
		respond(msg, nil)
		return
	}

	// 1. Create LogicalSwitch
	ls := &nbdb.LogicalSwitch{
		Name: switchName,
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": evt.SubnetId,
			"spinifex:vpc_id":    evt.VpcId,
		},
	}
	if err := h.ovn.CreateLogicalSwitch(ctx, ls); err != nil {
		slog.Error("vpcd: failed to create logical switch", "switch", switchName, "err", err)
		respond(msg, err)
		return
	}

	// 2. Create LogicalRouterPort on the VPC router
	lrp := &nbdb.LogicalRouterPort{
		Name:     routerPortName,
		MAC:      routerMAC,
		Networks: []string{gwCIDR},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": evt.SubnetId,
			"spinifex:vpc_id":    evt.VpcId,
		},
	}
	if err := h.ovn.CreateLogicalRouterPort(ctx, routerName, lrp); err != nil {
		slog.Error("vpcd: failed to create router port", "port", routerPortName, "err", err)
		// Best-effort cleanup
		_ = h.ovn.DeleteLogicalSwitch(ctx, switchName)
		respond(msg, err)
		return
	}

	// 3. Create LogicalSwitchPort (type=router) connecting switch to router
	lsp := &nbdb.LogicalSwitchPort{
		Name:      switchRouterPortName,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			"router-port": routerPortName,
		},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": evt.SubnetId,
			"spinifex:vpc_id":    evt.VpcId,
		},
	}
	if err := h.ovn.CreateLogicalSwitchPort(ctx, switchName, lsp); err != nil {
		slog.Error("vpcd: failed to create switch router port", "port", switchRouterPortName, "err", err)
		_ = h.ovn.DeleteLogicalRouterPort(ctx, routerName, routerPortName)
		_ = h.ovn.DeleteLogicalSwitch(ctx, switchName)
		respond(msg, err)
		return
	}

	// 4. Create DHCP_Options for the subnet
	dhcpOpts := &nbdb.DHCPOptions{
		CIDR: evt.CidrBlock,
		Options: map[string]string{
			"server_id":  gwIP,
			"server_mac": routerMAC,
			"lease_time": "3600",
			"router":     gwIP,
			"dns_server": h.dnsServer(),
			"mtu":        "1442", // Geneve overhead
		},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": evt.SubnetId,
			"spinifex:vpc_id":    evt.VpcId,
		},
	}
	if _, err := h.ovn.CreateDHCPOptions(ctx, dhcpOpts); err != nil {
		slog.Error("vpcd: failed to create DHCP options", "cidr", evt.CidrBlock, "err", err)
		// Non-fatal: switch and router port are still useful
	}

	slog.Info("vpcd: created subnet topology",
		"switch", switchName,
		"router_port", routerPortName,
		"gateway", gwCIDR,
		"subnet_id", evt.SubnetId,
	)
	respond(msg, nil)
}

func (h *TopologyHandler) handleSubnetDelete(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt SubnetEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.delete-subnet event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	switchName := "subnet-" + evt.SubnetId
	routerName := "vpc-" + evt.VpcId
	routerPortName := "rtr-" + evt.SubnetId
	switchRouterPortName := "rtr-port-" + evt.SubnetId

	// 1. Delete switch router port
	if err := h.ovn.DeleteLogicalSwitchPort(ctx, switchName, switchRouterPortName); err != nil {
		slog.Warn("vpcd: failed to delete switch router port", "port", switchRouterPortName, "err", err)
	}

	// 2. Delete router port
	if err := h.ovn.DeleteLogicalRouterPort(ctx, routerName, routerPortName); err != nil {
		slog.Warn("vpcd: failed to delete router port", "port", routerPortName, "err", err)
	}

	// 3. Delete DHCP options for this subnet
	dhcpOpts, err := h.ovn.FindDHCPOptionsByCIDR(ctx, evt.CidrBlock)
	if err == nil {
		if err := h.ovn.DeleteDHCPOptions(ctx, dhcpOpts.UUID); err != nil {
			slog.Warn("vpcd: failed to delete DHCP options", "cidr", evt.CidrBlock, "err", err)
		}
	}

	// 4. Delete the logical switch
	if err := h.ovn.DeleteLogicalSwitch(ctx, switchName); err != nil {
		slog.Error("vpcd: failed to delete logical switch", "switch", switchName, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: deleted subnet topology", "switch", switchName, "subnet_id", evt.SubnetId)
	respond(msg, nil)
}

// --- Port (LogicalSwitchPort for VM/ENI) ---

func (h *TopologyHandler) handleCreatePort(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt PortEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.create-port event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	portName := "port-" + evt.NetworkInterfaceId
	switchName := "subnet-" + evt.SubnetId

	// Idempotent: skip if port already exists
	if _, err := h.ovn.GetLogicalSwitchPort(ctx, portName); err == nil {
		slog.Debug("vpcd: logical switch port already exists, skipping", "port", portName)
		respond(msg, nil)
		return
	}

	addrStr := fmt.Sprintf("%s %s", evt.MacAddress, evt.PrivateIpAddress)

	lsp := &nbdb.LogicalSwitchPort{
		Name:         portName,
		Addresses:    []string{addrStr},
		PortSecurity: []string{addrStr},
		ExternalIDs: map[string]string{
			"spinifex:eni_id":    evt.NetworkInterfaceId,
			"spinifex:subnet_id": evt.SubnetId,
			"spinifex:vpc_id":    evt.VpcId,
		},
	}

	// Look up DHCP options for the subnet and attach to the port
	dhcpOpts, err := h.ovn.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", evt.SubnetId)
	if err != nil {
		slog.Warn("vpcd: DHCP options not found for subnet, port will not have DHCP", "subnet", evt.SubnetId, "err", err)
	} else {
		lsp.DHCPv4Options = &dhcpOpts.UUID
	}

	if err := h.ovn.CreateLogicalSwitchPort(ctx, switchName, lsp); err != nil {
		slog.Error("vpcd: failed to create logical switch port", "port", portName, "switch", switchName, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: created logical switch port for ENI",
		"port", portName,
		"switch", switchName,
		"eni_id", evt.NetworkInterfaceId,
		"ip", evt.PrivateIpAddress,
	)
	respond(msg, nil)
}

func (h *TopologyHandler) handleDeletePort(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt PortEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.delete-port event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	portName := "port-" + evt.NetworkInterfaceId
	switchName := "subnet-" + evt.SubnetId

	if err := h.ovn.DeleteLogicalSwitchPort(ctx, switchName, portName); err != nil {
		slog.Error("vpcd: failed to delete logical switch port", "port", portName, "switch", switchName, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: deleted logical switch port for ENI",
		"port", portName,
		"switch", switchName,
		"eni_id", evt.NetworkInterfaceId,
	)
	respond(msg, nil)
}

// --- Internet Gateway (external connectivity + NAT) ---

func (h *TopologyHandler) handleIGWAttach(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt types.IGWEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.igw-attach event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	routerName := "vpc-" + evt.VpcId
	extSwitchName := "ext-" + evt.VpcId
	extPortName := "ext-port-" + evt.VpcId
	gwPortName := "gw-" + evt.VpcId
	switchGWPortName := "gw-port-" + evt.VpcId

	// Idempotent: skip if external switch already exists
	if _, err := h.ovn.GetLogicalSwitch(ctx, extSwitchName); err == nil {
		slog.Debug("vpcd: IGW topology already exists, skipping", "switch", extSwitchName)
		respond(msg, nil)
		return
	}

	// 1. Create external logical switch (localnet for physical uplink)
	extSwitch := &nbdb.LogicalSwitch{
		Name: extSwitchName,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": evt.VpcId,
			"spinifex:igw_id": evt.InternetGatewayId,
			"spinifex:role":   "external",
		},
	}
	if err := h.ovn.CreateLogicalSwitch(ctx, extSwitch); err != nil {
		slog.Error("vpcd: failed to create external switch", "switch", extSwitchName, "err", err)
		respond(msg, err)
		return
	}

	// 2. Create localnet port on external switch (maps to physical network)
	localnetOpts := map[string]string{
		"network_name": "external",
	}
	// nat-addresses=router: OVN sends gratuitous ARPs for all NAT external IPs
	// using the router port MAC. Required for centralized NAT modes (macvlan,
	// veth) so that ARP replies for NAT IPs reach hosts correctly. Not needed
	// for direct bridge since OVS sees all traffic on the wire without MAC
	// filtering and distributed NAT handles ARP per-chassis.
	if h.useCentralizedNAT() {
		localnetOpts["nat-addresses"] = "router"
	}
	localnetPort := &nbdb.LogicalSwitchPort{
		Name:      extPortName,
		Type:      "localnet",
		Addresses: []string{"unknown"},
		Options:   localnetOpts,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": evt.VpcId,
			"spinifex:igw_id": evt.InternetGatewayId,
		},
	}
	if err := h.ovn.CreateLogicalSwitchPort(ctx, extSwitchName, localnetPort); err != nil {
		slog.Error("vpcd: failed to create localnet port", "port", extPortName, "err", err)
		_ = h.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		respond(msg, err)
		return
	}

	// Resolve external pool for this VPC's gateway
	// TODO: use VPC's region/AZ once we track it; for now use first matching pool
	pool := h.findExternalPool("", "")
	gatewayIP := "169.254.0.1"
	gatewayNetwork := "169.254.0.1/30"
	wanGateway := "169.254.0.2"

	if pool != nil {
		gip := pool.GatewayIP
		if gip == "" {
			gip = pool.RangeStart // Default: first IP in range
		}
		if gip != "" {
			// Static pool: use the pool's IP for the gateway router port
			gatewayIP = gip
			prefixLen := pool.PrefixLen
			if prefixLen == 0 {
				prefixLen = 24
			}
			gatewayNetwork = fmt.Sprintf("%s/%d", gip, prefixLen)
		}
		// DHCP-sourced pools have no GatewayIP/RangeStart — keep the
		// link-local defaults for the OVN router port but still use the
		// pool's WAN gateway for the default route.
		wanGateway = pool.Gateway
		slog.Info("vpcd: using external pool for IGW",
			"pool", pool.Name,
			"gateway_ip", gatewayIP,
			"wan_gateway", wanGateway,
		)
	} else if h.externalMode == "pool" {
		slog.Warn("vpcd: external mode is set but no matching pool found, using link-local fallback")
	}

	// 3. Create gateway router port on the VPC router connecting to external switch
	gwMAC := generateMAC("gw-" + evt.VpcId)
	lrp := &nbdb.LogicalRouterPort{
		Name:     gwPortName,
		MAC:      gwMAC,
		Networks: []string{gatewayNetwork},
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": evt.VpcId,
			"spinifex:igw_id": evt.InternetGatewayId,
			"spinifex:role":   "gateway",
		},
	}
	if err := h.ovn.CreateLogicalRouterPort(ctx, routerName, lrp); err != nil {
		slog.Error("vpcd: failed to create gateway router port", "port", gwPortName, "err", err)
		_ = h.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		respond(msg, err)
		return
	}

	// 4. Create switch port connecting external switch to router
	switchGWPort := &nbdb.LogicalSwitchPort{
		Name:      switchGWPortName,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			"router-port": gwPortName,
		},
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": evt.VpcId,
			"spinifex:igw_id": evt.InternetGatewayId,
		},
	}
	if err := h.ovn.CreateLogicalSwitchPort(ctx, extSwitchName, switchGWPort); err != nil {
		slog.Error("vpcd: failed to create switch gateway port", "port", switchGWPortName, "err", err)
		_ = h.ovn.DeleteLogicalRouterPort(ctx, routerName, gwPortName)
		_ = h.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		respond(msg, err)
		return
	}

	// 5. No blanket SNAT rule — AWS behavior requires that only instances with
	// public IPs (via MapPublicIpOnLaunch or EIPs) can route through the IGW.
	// Per-VM dnat_and_snat rules created by handleAddNAT provide both inbound
	// DNAT and outbound SNAT for public instances. Private subnet instances
	// (no public IP, no NAT rule) cannot reach the internet — their packets
	// leave the router with a private source IP that the upstream router drops.
	// A future NAT Gateway feature will add scoped SNAT for private subnets.

	// 6. Add default route pointing to the WAN gateway
	// OutputPort must be set explicitly because DHCP-sourced pools use a
	// link-local gateway port (169.254.0.1/30) whose network does not contain
	// the WAN nexthop (e.g. 192.168.1.1). Without it OVN northd silently
	// drops the route from the southbound DB.
	defaultRoute := &nbdb.LogicalRouterStaticRoute{
		IPPrefix:   "0.0.0.0/0",
		Nexthop:    wanGateway,
		OutputPort: &gwPortName,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": evt.VpcId,
			"spinifex:igw_id": evt.InternetGatewayId,
		},
	}
	if err := h.ovn.AddStaticRoute(ctx, routerName, defaultRoute); err != nil {
		slog.Warn("vpcd: failed to add default route", "router", routerName, "err", err)
	}

	// 7. Schedule gateway chassis for HA — tells OVN which hosts can handle external traffic
	if len(h.chassisNames) > 0 {
		for i, chassis := range h.chassisNames {
			priority := max(
				// First chassis gets highest priority
				20-(i*5), 1)
			if err := h.ovn.SetGatewayChassis(ctx, gwPortName, chassis, priority); err != nil {
				slog.Warn("vpcd: failed to set gateway chassis", "port", gwPortName, "chassis", chassis, "priority", priority, "err", err)
			} else {
				slog.Info("vpcd: set gateway chassis", "port", gwPortName, "chassis", chassis, "priority", priority)
			}
		}
	} else {
		slog.Warn("vpcd: no chassis names configured — gateway port has no chassis binding, external traffic will not flow")
	}

	slog.Info("vpcd: attached internet gateway to VPC",
		"igw_id", evt.InternetGatewayId,
		"vpc_id", evt.VpcId,
		"ext_switch", extSwitchName,
		"gw_port", gwPortName,
		"gateway_ip", gatewayIP,
		"wan_gateway", wanGateway,
		"chassis_count", len(h.chassisNames),
	)
	respond(msg, nil)
}

func (h *TopologyHandler) handleIGWDetach(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt types.IGWEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.igw-detach event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	routerName := "vpc-" + evt.VpcId
	extSwitchName := "ext-" + evt.VpcId
	extPortName := "ext-port-" + evt.VpcId
	gwPortName := "gw-" + evt.VpcId
	switchGWPortName := "gw-port-" + evt.VpcId

	// 1. Delete default route
	if err := h.ovn.DeleteStaticRoute(ctx, routerName, "0.0.0.0/0"); err != nil {
		slog.Warn("vpcd: failed to delete default route", "router", routerName, "err", err)
	}

	// 2. Delete SNAT rule(s) for this IGW
	// Find VPC CIDR for the SNAT rule
	router, err := h.ovn.GetLogicalRouter(ctx, routerName)
	if err != nil {
		slog.Warn("vpcd: failed to get router for NAT cleanup", "router", routerName, "err", err)
	} else {
		vpcCIDR := router.ExternalIDs["spinifex:cidr"]
		if vpcCIDR == "" {
			vpcCIDR = "10.0.0.0/8"
			slog.Warn("vpcd: VPC CIDR missing from router metadata, using overbroad fallback for NAT cleanup",
				"router", routerName, "fallbackCIDR", vpcCIDR)
		}
		if err := h.ovn.DeleteNAT(ctx, routerName, "snat", vpcCIDR); err != nil {
			slog.Warn("vpcd: failed to delete SNAT rule", "router", routerName, "err", err)
		}
	}

	// 3. Delete switch gateway port
	if err := h.ovn.DeleteLogicalSwitchPort(ctx, extSwitchName, switchGWPortName); err != nil {
		slog.Warn("vpcd: failed to delete switch gateway port", "port", switchGWPortName, "err", err)
	}

	// 4. Delete gateway router port
	if err := h.ovn.DeleteLogicalRouterPort(ctx, routerName, gwPortName); err != nil {
		slog.Warn("vpcd: failed to delete gateway router port", "port", gwPortName, "err", err)
	}

	// 5. Delete localnet port
	if err := h.ovn.DeleteLogicalSwitchPort(ctx, extSwitchName, extPortName); err != nil {
		slog.Warn("vpcd: failed to delete localnet port", "port", extPortName, "err", err)
	}

	// 6. Delete external switch
	if err := h.ovn.DeleteLogicalSwitch(ctx, extSwitchName); err != nil {
		slog.Error("vpcd: failed to delete external switch", "switch", extSwitchName, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: detached internet gateway from VPC",
		"igw_id", evt.InternetGatewayId,
		"vpc_id", evt.VpcId,
	)
	respond(msg, nil)
}

// --- NAT (dnat_and_snat for public IPs) ---

func (h *TopologyHandler) handleAddNAT(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt NATEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.add-nat event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	routerName := "vpc-" + evt.VpcId

	// NAT mode depends on bridge type:
	// - Macvlan: centralized NAT (no ExternalMAC/LogicalPort). OVN uses the
	//   router port MAC for all ARP replies so the macvlan can receive inbound
	//   unicast. Distributed NAT would announce the VM's MAC which the macvlan
	//   filters out. For single-node, centralized NAT has zero overhead.
	// - Direct bridge: distributed NAT (ExternalMAC + LogicalPort set). DNAT
	//   is processed on the VM's own chassis — no hairpin through the gateway
	//   chassis. This is the preferred mode for multi-node performance.
	natRule := &nbdb.NAT{
		Type:       "dnat_and_snat",
		ExternalIP: evt.ExternalIP,
		LogicalIP:  evt.LogicalIP,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id":    evt.VpcId,
			"spinifex:public_ip": evt.ExternalIP,
		},
	}
	if !h.useCentralizedNAT() && evt.MAC != "" && evt.PortName != "" {
		natRule.ExternalMAC = &evt.MAC
		natRule.LogicalPort = &evt.PortName
		slog.Debug("vpcd: using distributed NAT (direct bridge)",
			"external_ip", evt.ExternalIP, "port", evt.PortName, "mac", evt.MAC)
	}

	// Remove any stale NAT rule for the same external IP before adding the new
	// one. Search ALL routers, not just the target — stale rules may exist on a
	// different VPC's router when vpc.delete-nat (fire-and-forget) hasn't been
	// processed before the IP was reused by a new VPC.
	if removed, err := h.ovn.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", evt.ExternalIP); err != nil {
		slog.Warn("vpcd: failed to clean up stale NAT rules for external IP", "external_ip", evt.ExternalIP, "err", err)
	} else if removed > 0 {
		slog.Info("vpcd: cleaned up stale NAT rules before re-add", "external_ip", evt.ExternalIP, "removed", removed)
	}

	if err := h.ovn.AddNAT(ctx, routerName, natRule); err != nil {
		slog.Error("vpcd: failed to add dnat_and_snat rule", "router", routerName, "externalIP", evt.ExternalIP, "logicalIP", evt.LogicalIP, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: added dnat_and_snat rule",
		"router", routerName,
		"external_ip", evt.ExternalIP,
		"logical_ip", evt.LogicalIP,
		"port", evt.PortName,
	)
	respond(msg, nil)
}

func (h *TopologyHandler) handleDeleteNAT(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt NATEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.delete-nat event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	routerName := "vpc-" + evt.VpcId

	if err := h.ovn.DeleteNAT(ctx, routerName, "dnat_and_snat", evt.LogicalIP); err != nil {
		slog.Error("vpcd: failed to delete dnat_and_snat rule", "router", routerName, "logicalIP", evt.LogicalIP, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: deleted dnat_and_snat rule",
		"router", routerName,
		"logical_ip", evt.LogicalIP,
	)
	respond(msg, nil)
}

// --- Reconciliation (called on startup, not via NATS) ---

// reconcileVPC creates the OVN logical router for a VPC if it doesn't exist.
func (h *TopologyHandler) reconcileVPC(ctx context.Context, vpcId, cidr string) error {
	routerName := "vpc-" + vpcId

	lr := &nbdb.LogicalRouter{
		Name: routerName,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": vpcId,
			"spinifex:cidr":   cidr,
		},
	}
	if err := h.ovn.CreateLogicalRouter(ctx, lr); err != nil {
		return fmt.Errorf("create router %s: %w", routerName, err)
	}

	slog.Info("vpcd reconcile: created VPC router", "router", routerName, "vpc_id", vpcId)
	return nil
}

// reconcileSubnet creates the OVN logical switch, router port, and DHCP options for a subnet.
func (h *TopologyHandler) reconcileSubnet(ctx context.Context, subnetId, vpcId, cidr string) error {
	switchName := "subnet-" + subnetId
	routerName := "vpc-" + vpcId
	routerPortName := "rtr-" + subnetId
	switchRouterPortName := "rtr-port-" + subnetId

	gwIP, mask, err := subnetGateway(cidr)
	if err != nil {
		return fmt.Errorf("compute gateway for %s: %w", cidr, err)
	}
	gwCIDR := fmt.Sprintf("%s/%d", gwIP, mask)
	routerMAC := generateMAC(subnetId)

	// 1. Create LogicalSwitch
	ls := &nbdb.LogicalSwitch{
		Name: switchName,
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": subnetId,
			"spinifex:vpc_id":    vpcId,
		},
	}
	if err := h.ovn.CreateLogicalSwitch(ctx, ls); err != nil {
		return fmt.Errorf("create switch %s: %w", switchName, err)
	}

	// 2. Create LogicalRouterPort
	lrp := &nbdb.LogicalRouterPort{
		Name:     routerPortName,
		MAC:      routerMAC,
		Networks: []string{gwCIDR},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": subnetId,
			"spinifex:vpc_id":    vpcId,
		},
	}
	if err := h.ovn.CreateLogicalRouterPort(ctx, routerName, lrp); err != nil {
		_ = h.ovn.DeleteLogicalSwitch(ctx, switchName)
		return fmt.Errorf("create router port %s: %w", routerPortName, err)
	}

	// 3. Create LogicalSwitchPort (type=router)
	lsp := &nbdb.LogicalSwitchPort{
		Name:      switchRouterPortName,
		Type:      "router",
		Addresses: []string{"router"},
		Options:   map[string]string{"router-port": routerPortName},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": subnetId,
			"spinifex:vpc_id":    vpcId,
		},
	}
	if err := h.ovn.CreateLogicalSwitchPort(ctx, switchName, lsp); err != nil {
		_ = h.ovn.DeleteLogicalRouterPort(ctx, routerName, routerPortName)
		_ = h.ovn.DeleteLogicalSwitch(ctx, switchName)
		return fmt.Errorf("create switch router port %s: %w", switchRouterPortName, err)
	}

	// 4. Create DHCP options
	dhcpOpts := &nbdb.DHCPOptions{
		CIDR: cidr,
		Options: map[string]string{
			"server_id":  gwIP,
			"server_mac": routerMAC,
			"lease_time": "3600",
			"router":     gwIP,
			"dns_server": h.dnsServer(),
			"mtu":        "1442",
		},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": subnetId,
			"spinifex:vpc_id":    vpcId,
		},
	}
	if _, err := h.ovn.CreateDHCPOptions(ctx, dhcpOpts); err != nil {
		slog.Warn("vpcd reconcile: failed to create DHCP options (non-fatal)", "cidr", cidr, "err", err)
	}

	slog.Info("vpcd reconcile: created subnet topology",
		"switch", switchName, "router_port", routerPortName, "gateway", gwCIDR)
	return nil
}

// reconcileIGW creates the OVN external switch, gateway router port, SNAT rule,
// default route, and gateway chassis for a VPC's internet gateway.
func (h *TopologyHandler) reconcileIGW(ctx context.Context, vpcId, igwId string) error {
	routerName := "vpc-" + vpcId
	extSwitchName := "ext-" + vpcId
	extPortName := "ext-port-" + vpcId
	gwPortName := "gw-" + vpcId
	switchGWPortName := "gw-port-" + vpcId

	// Resolve external pool
	pool := h.findExternalPool("", "")
	gatewayIP := "169.254.0.1"
	gatewayNetwork := "169.254.0.1/30"
	wanGateway := "169.254.0.2"

	if pool != nil {
		gip := pool.GatewayIP
		if gip == "" {
			gip = pool.RangeStart
		}
		if gip != "" {
			gatewayIP = gip
			prefixLen := pool.PrefixLen
			if prefixLen == 0 {
				prefixLen = 24
			}
			gatewayNetwork = fmt.Sprintf("%s/%d", gip, prefixLen)
		}
		wanGateway = pool.Gateway
	}

	// Build external IDs with optional IGW ID
	extIDs := map[string]string{
		"spinifex:vpc_id": vpcId,
		"spinifex:role":   "external",
	}
	if igwId != "" {
		extIDs["spinifex:igw_id"] = igwId
	}

	// 1. Create external logical switch
	extSwitch := &nbdb.LogicalSwitch{
		Name:        extSwitchName,
		ExternalIDs: extIDs,
	}
	if err := h.ovn.CreateLogicalSwitch(ctx, extSwitch); err != nil {
		return fmt.Errorf("create external switch %s: %w", extSwitchName, err)
	}

	// 2. Create localnet port
	portExtIDs := map[string]string{"spinifex:vpc_id": vpcId}
	if igwId != "" {
		portExtIDs["spinifex:igw_id"] = igwId
	}
	reconcileLocalnetOpts := map[string]string{
		"network_name": "external",
	}
	if h.useCentralizedNAT() {
		reconcileLocalnetOpts["nat-addresses"] = "router"
	}
	localnetPort := &nbdb.LogicalSwitchPort{
		Name:        extPortName,
		Type:        "localnet",
		Addresses:   []string{"unknown"},
		Options:     reconcileLocalnetOpts,
		ExternalIDs: portExtIDs,
	}
	if err := h.ovn.CreateLogicalSwitchPort(ctx, extSwitchName, localnetPort); err != nil {
		_ = h.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		return fmt.Errorf("create localnet port %s: %w", extPortName, err)
	}
	// Retrofit options on pre-existing ports whose mode no longer matches
	// (create above is a no-op when the port exists — seeds options only
	// on first creation; see ensureLocalnetOptions).
	if err := h.ensureLocalnetOptions(ctx, extPortName); err != nil {
		return fmt.Errorf("retrofit localnet options %s: %w", extPortName, err)
	}

	// 3. Create gateway router port
	gwMAC := generateMAC("gw-" + vpcId)
	lrp := &nbdb.LogicalRouterPort{
		Name:     gwPortName,
		MAC:      gwMAC,
		Networks: []string{gatewayNetwork},
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": vpcId,
			"spinifex:role":   "gateway",
		},
	}
	if err := h.ovn.CreateLogicalRouterPort(ctx, routerName, lrp); err != nil {
		_ = h.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		return fmt.Errorf("create gateway router port %s: %w", gwPortName, err)
	}

	// 4. Create switch port connecting external switch to router
	switchGWPort := &nbdb.LogicalSwitchPort{
		Name:      switchGWPortName,
		Type:      "router",
		Addresses: []string{"router"},
		Options:   map[string]string{"router-port": gwPortName},
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": vpcId,
		},
	}
	if err := h.ovn.CreateLogicalSwitchPort(ctx, extSwitchName, switchGWPort); err != nil {
		_ = h.ovn.DeleteLogicalRouterPort(ctx, routerName, gwPortName)
		_ = h.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		return fmt.Errorf("create switch gateway port %s: %w", switchGWPortName, err)
	}

	// 5. No blanket SNAT — per-VM dnat_and_snat rules handle public instances.
	// See handleIGWAttach comment for rationale (AWS parity).

	// 6. Add default route (OutputPort required for DHCP/link-local gateway ports)
	defaultRoute := &nbdb.LogicalRouterStaticRoute{
		IPPrefix:   "0.0.0.0/0",
		Nexthop:    wanGateway,
		OutputPort: &gwPortName,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": vpcId,
		},
	}
	if err := h.ovn.AddStaticRoute(ctx, routerName, defaultRoute); err != nil {
		slog.Warn("vpcd reconcile: failed to add default route", "err", err)
	}

	// 7. Schedule gateway chassis
	for i, chassis := range h.chassisNames {
		priority := max(20-(i*5), 1)
		if err := h.ovn.SetGatewayChassis(ctx, gwPortName, chassis, priority); err != nil {
			slog.Warn("vpcd reconcile: failed to set gateway chassis", "chassis", chassis, "err", err)
		}
	}

	slog.Info("vpcd reconcile: created IGW topology",
		"ext_switch", extSwitchName, "gw_port", gwPortName,
		"gateway_ip", gatewayIP, "wan_gateway", wanGateway)
	return nil
}

// --- Helpers ---

// subnetGateway computes the gateway IP (.1) from a CIDR string.
// Returns the gateway IP string and the prefix length.
func subnetGateway(cidr string) (string, int, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", 0, fmt.Errorf("parse CIDR %q: %w", cidr, err)
	}

	// Use the network address, increment last octet to .1
	gw := ipNet.IP.To4()
	if gw == nil {
		return "", 0, fmt.Errorf("only IPv4 supported, got %s", ip)
	}
	gw = make(net.IP, len(ipNet.IP.To4()))
	copy(gw, ipNet.IP.To4())
	gw[3]++

	ones, _ := ipNet.Mask.Size()
	return gw.String(), ones, nil
}

// generateMAC creates a deterministic MAC address from a resource ID.
// Uses the locally-administered unicast prefix 02:00:00.
func generateMAC(resourceID string) string {
	// Simple hash: use first 6 hex chars of resource ID after the prefix
	h := uint32(0)
	for _, c := range resourceID {
		h = h*31 + uint32(c) // #nosec G115 -- intentional overflow for hashing
	}
	return fmt.Sprintf("02:00:00:%02x:%02x:%02x", (h>>16)&0xff, (h>>8)&0xff, h&0xff)
}

// respond sends a simple JSON response to a NATS request.
func respond(msg *nats.Msg, err error) {
	if msg.Reply == "" {
		return // fire-and-forget, no reply expected
	}

	type response struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}

	resp := response{Success: true}
	if err != nil {
		resp.Success = false
		resp.Error = err.Error()
	}

	data, marshalErr := json.Marshal(resp)
	if marshalErr != nil {
		slog.Error("vpcd: failed to marshal NATS response", "err", marshalErr)
		data = []byte(`{"success":false,"error":"internal marshal failure"}`)
	}
	if err := msg.Respond(data); err != nil {
		slog.Error("vpcd: failed to respond to NATS request", "err", err)
	}
}

// handleAddNATGateway creates an OVN SNAT rule for a private subnet via a NAT Gateway.
// The SNAT rule rewrites source IPs from the private subnet CIDR to the NAT GW's public IP.
func (h *TopologyHandler) handleAddNATGateway(msg *nats.Msg) {
	var evt NATGatewayEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.add-nat-gateway event", "err", err)
		return
	}

	slog.Info("vpcd: adding NAT Gateway SNAT rule",
		"vpcId", evt.VpcId, "natGatewayId", evt.NatGatewayId,
		"publicIp", evt.PublicIp, "subnetCidr", evt.SubnetCidr)

	ctx := context.Background()
	routerName := "vpc-" + evt.VpcId

	snatRule := &nbdb.NAT{
		Type:       "snat",
		ExternalIP: evt.PublicIp,
		LogicalIP:  evt.SubnetCidr,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id":         evt.VpcId,
			"spinifex:nat_gateway_id": evt.NatGatewayId,
		},
	}

	if err := h.ovn.AddNAT(ctx, routerName, snatRule); err != nil {
		slog.Error("vpcd: failed to add NAT Gateway SNAT rule",
			"router", routerName, "publicIp", evt.PublicIp,
			"subnetCidr", evt.SubnetCidr, "err", err)
		return
	}

	slog.Info("vpcd: NAT Gateway SNAT rule added",
		"router", routerName, "publicIp", evt.PublicIp, "subnetCidr", evt.SubnetCidr)
}

// handleDeleteNATGateway removes the OVN SNAT rule for a private subnet's NAT Gateway.
func (h *TopologyHandler) handleDeleteNATGateway(msg *nats.Msg) {
	var evt NATGatewayEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.delete-nat-gateway event", "err", err)
		return
	}

	slog.Info("vpcd: removing NAT Gateway SNAT rule",
		"vpcId", evt.VpcId, "natGatewayId", evt.NatGatewayId, "subnetCidr", evt.SubnetCidr)

	ctx := context.Background()
	routerName := "vpc-" + evt.VpcId

	if err := h.ovn.DeleteNAT(ctx, routerName, "snat", evt.SubnetCidr); err != nil {
		slog.Warn("vpcd: failed to delete NAT Gateway SNAT rule",
			"router", routerName, "subnetCidr", evt.SubnetCidr, "err", err)
		return
	}

	slog.Info("vpcd: NAT Gateway SNAT rule removed",
		"router", routerName, "subnetCidr", evt.SubnetCidr)
}
