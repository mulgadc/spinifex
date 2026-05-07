package vpcd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/services/vpcd/dhcp"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
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
	TopicUpdatePortSGs    = "vpc.update-port-sgs"
	TopicPortStatus       = "vpc.port-status"
	TopicIGWAttach        = "vpc.igw-attach"
	TopicIGWDetach        = "vpc.igw-detach"
	TopicAddNAT           = "vpc.add-nat"
	TopicDeleteNAT        = "vpc.delete-nat"
	TopicAddNATGateway    = "vpc.add-nat-gateway"
	TopicDeleteNATGateway = "vpc.delete-nat-gateway"
)

// gatewayPortNetwork is the link-local CIDR every IGW gateway-LRP carries
// in distributed-NAT (direct) mode. Per-VM dnat_and_snat handles ARP per
// chassis so the LRP IP itself never goes on the wire. The default route's
// OutputPort is set explicitly so the WAN nexthop need not be on this subnet.
//
// Centralized-NAT (veth/macvlan) bypasses this constant: the gateway LRP is
// the on-wire egress point and must hold a WAN-subnet IP from the pool's
// gw_lrp_range, otherwise the upstream router silently drops ARP requests
// from an off-subnet sender (RFC 826) and the default route never resolves
// (mulga-siv-36).
const gatewayPortNetwork = "169.254.0.1/30"

// gatewayIPExtID is the LRP external_ids key holding the gw IP allocated
// from the pool's gw_lrp_range. Persisted so reconcile/retrofit can recover
// the assignment without re-allocating, and so siblings see it as "used".
const gatewayIPExtID = "spinifex:gateway_ip"

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
//
// SecurityGroupIds carries the SG membership the port should join at create
// time so vpcd can wire OVN port-group membership atomically with the LSP
// create. Empty on delete-port (handleDeletePort discovers current
// memberships from the libovsdb cache).
type PortEvent struct {
	NetworkInterfaceId string   `json:"network_interface_id"`
	SubnetId           string   `json:"subnet_id"`
	VpcId              string   `json:"vpc_id"`
	PrivateIpAddress   string   `json:"private_ip_address"`
	MacAddress         string   `json:"mac_address"`
	SecurityGroupIds   []string `json:"security_group_ids,omitempty"`
}

// UpdatePortSGsEvent is published on vpc.update-port-sgs after
// ModifyNetworkInterfaceAttribute changes an ENI's SG membership. The payload
// is declarative — vpcd reads its libovsdb cache to discover current
// memberships and computes the diff against SecurityGroupIds.
type UpdatePortSGsEvent struct {
	NetworkInterfaceId string   `json:"network_interface_id"`
	PrivateIpAddress   string   `json:"private_ip_address"`
	SecurityGroupIds   []string `json:"security_group_ids"`
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
	// nc is used to talk to vpcd's DHCPManager via vpc.dhcp.acquire /
	// vpc.dhcp.release for gateway LRP IPs in centralized NAT on
	// source="dhcp" pools (mulga-siv-38). nil when no DHCP pool is wired or
	// the test stack supplies a static-only mock.
	nc *nats.Conn
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

// WithNATSConn wires the NATS connection used to talk to vpcd's DHCPManager
// for gateway-LRP DHCP-acquire (mulga-siv-38). Tests with mock OVN clients
// can omit this — the static / auto-derive paths still work.
func WithNATSConn(nc *nats.Conn) TopologyOption {
	return func(h *TopologyHandler) {
		h.nc = nc
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

// expectedGatewayPortNetwork returns the Networks CIDR a gateway LRP for
// vpcId should carry. In direct mode, or in centralized mode without a pool,
// it is the link-local fallback. In centralized mode with a DHCP pool the
// gateway IP is acquired via vpc.dhcp.acquire (mulga-siv-38). With a static
// pool the IP comes from gw_lrp_range or the auto-derived top-of-subnet
// block (mulga-siv-36). Returns the gw IP (empty when link-local) so
// callers can persist it to external_ids.
func (h *TopologyHandler) expectedGatewayPortNetwork(ctx context.Context, vpcId string) (network, gwIP string, err error) {
	if !h.useCentralizedNAT() {
		return gatewayPortNetwork, "", nil
	}
	pool := h.findExternalPool("", "")
	if pool == nil {
		return gatewayPortNetwork, "", nil
	}
	if pool.IsDHCP() {
		ip, prefix, dhcpErr := h.allocateGatewayLRPIPViaDHCP(ctx, vpcId, pool)
		if dhcpErr != nil {
			return "", "", dhcpErr
		}
		return fmt.Sprintf("%s/%d", ip, prefix), ip, nil
	}
	ip, prefix, ok, allocErr := h.allocateGatewayLRPIP(ctx, vpcId, pool)
	if allocErr != nil {
		return "", "", allocErr
	}
	if !ok {
		return gatewayPortNetwork, "", nil
	}
	return fmt.Sprintf("%s/%d", ip, prefix), ip, nil
}

// ensureGatewayPortNetworks rewrites the gateway LRP's Networks column in
// place when it drifts from the mode's expected value. CreateLogicalRouterPort
// is a no-op when the row exists, so reconcile of an upgraded cluster
// otherwise keeps the stale pool-IP networks (mulga-siv-26 D8) or, in
// centralized NAT, the off-subnet link-local that breaks ARP upstream
// (mulga-siv-36). Idempotent — no UPDATE is issued when already correct.
func (h *TopologyHandler) ensureGatewayPortNetworks(ctx context.Context, gwPortName string) error {
	lrp, err := h.ovn.GetLogicalRouterPort(ctx, gwPortName)
	if err != nil {
		return fmt.Errorf("get gateway router port %s: %w", gwPortName, err)
	}
	vpcId := lrp.ExternalIDs["spinifex:vpc_id"]
	if vpcId == "" {
		// No VPC tag — this is from the legacy "gw-vpc-..." naming or a
		// hand-edited row; fall back to link-local to keep behavior safe.
		if len(lrp.Networks) == 1 && lrp.Networks[0] == gatewayPortNetwork {
			return nil
		}
		slog.Info("vpcd: rewriting stale gateway port networks (no vpc_id tag)",
			"port", gwPortName, "old", lrp.Networks, "new", gatewayPortNetwork)
		lrp.Networks = []string{gatewayPortNetwork}
		return h.ovn.UpdateLogicalRouterPort(ctx, lrp)
	}
	wantNetwork, wantGwIP, err := h.expectedGatewayPortNetwork(ctx, vpcId)
	if err != nil {
		return fmt.Errorf("compute expected network for %s: %w", gwPortName, err)
	}
	currentGwIP := lrp.ExternalIDs[gatewayIPExtID]
	if len(lrp.Networks) == 1 && lrp.Networks[0] == wantNetwork && currentGwIP == wantGwIP {
		return nil
	}
	slog.Info("vpcd: rewriting stale gateway port networks",
		"port", gwPortName, "old", lrp.Networks, "new", wantNetwork,
		"old_gw_ip", currentGwIP, "new_gw_ip", wantGwIP)
	lrp.Networks = []string{wantNetwork}
	if lrp.ExternalIDs == nil {
		lrp.ExternalIDs = map[string]string{}
	}
	if wantGwIP == "" {
		delete(lrp.ExternalIDs, gatewayIPExtID)
	} else {
		lrp.ExternalIDs[gatewayIPExtID] = wantGwIP
	}
	if err := h.ovn.UpdateLogicalRouterPort(ctx, lrp); err != nil {
		return fmt.Errorf("update gateway router port %s: %w", gwPortName, err)
	}
	return nil
}

// RetrofitAllGatewayPortNetworks walks every LRP tagged
// spinifex:role=gateway and ensures Networks matches the current mode. Needed
// because Reconcile/ReconcileFromKV early-return when the external switch
// already exists, so reconcileIGW never runs against pre-existing topologies
// shipped by older builds (mulga-siv-26 D8 / mulga-siv-36). Idempotent — the
// underlying ensureGatewayPortNetworks no-ops when already correct.
func (h *TopologyHandler) RetrofitAllGatewayPortNetworks(ctx context.Context) {
	lrps, err := h.ovn.ListLogicalRouterPorts(ctx)
	if err != nil {
		slog.Warn("vpcd: gateway-port retrofit skipped — list LRPs failed", "err", err)
		return
	}
	for _, lrp := range lrps {
		if lrp.ExternalIDs["spinifex:role"] != "gateway" {
			continue
		}
		if err := h.ensureGatewayPortNetworks(ctx, lrp.Name); err != nil {
			slog.Error("vpcd: gateway-port retrofit failed", "port", lrp.Name, "err", err)
		}
	}
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
		{TopicUpdatePortSGs, h.handleUpdatePortSGs, true},
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

	// Idempotent: if the port already exists from a previous attempt that
	// crashed before joining its SG port groups, converge the memberships now
	// instead of waiting up to a full reconciler interval — that gap would
	// leave a port with zero ACLs (OVN default = unrestricted), defeating the
	// atomic-create guarantee on the recovery path.
	if _, err := h.ovn.GetLogicalSwitchPort(ctx, portName); err == nil {
		if err := h.reconcilePortSGs(ctx, portName, evt.PrivateIpAddress, evt.SecurityGroupIds); err != nil {
			slog.Error("vpcd: failed to reconcile SGs for existing port", "port", portName, "err", err)
			respond(msg, err)
			return
		}
		slog.Debug("vpcd: logical switch port already exists, reconciled SG memberships", "port", portName)
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

	// LSP create + switch-port mutate + SG port-group joins + per-SG address-set
	// inserts MUST be one OVSDB transaction. A two-step shape leaves a window
	// where the port exists outside any port group (OVN default = unrestricted)
	// or where peer SGs see the port as nonexistent because its IP isn't in the
	// `<pg>_ip4` address set yet — both opposite of the enforcement guarantee.
	pgNames := make([]string, 0, len(evt.SecurityGroupIds))
	for _, sgId := range evt.SecurityGroupIds {
		pgNames = append(pgNames, portGroupName(sgId))
	}
	if err := h.ovn.CreateLogicalSwitchPortInGroups(ctx, switchName, lsp, pgNames, evt.PrivateIpAddress); err != nil {
		slog.Error("vpcd: failed to create logical switch port", "port", portName, "switch", switchName, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: created logical switch port for ENI",
		"port", portName,
		"switch", switchName,
		"eni_id", evt.NetworkInterfaceId,
		"ip", evt.PrivateIpAddress,
		"sgs", evt.SecurityGroupIds,
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

	// Remove the LSP from every port group before deleting it. OVSDB rejects
	// deletes of a row still referenced by another row's set column (the port
	// group's ports set), so swallowing this error guarantees the next
	// DeleteLogicalSwitchPort fails with a misleading reference-integrity
	// error and hides the real first-step cause.
	if err := h.reconcilePortSGs(ctx, portName, evt.PrivateIpAddress, nil); err != nil {
		slog.Error("vpcd: failed to clear port group memberships before delete", "port", portName, "err", err)
		respond(msg, err)
		return
	}

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

// handleUpdatePortSGs reconciles the port group membership for an ENI's LSP
// against the desired SG list in the event. The payload is declarative — vpcd
// computes the add/remove diff from the current libovsdb cache state. Same
// helper is used by the reconciler.
func (h *TopologyHandler) handleUpdatePortSGs(msg *nats.Msg) {
	if h.ovn == nil {
		respond(msg, fmt.Errorf("OVN client not connected"))
		return
	}

	var evt UpdatePortSGsEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.update-port-sgs event", "err", err)
		respond(msg, err)
		return
	}

	ctx := context.Background()
	portName := "port-" + evt.NetworkInterfaceId

	if err := h.reconcilePortSGs(ctx, portName, evt.PrivateIpAddress, evt.SecurityGroupIds); err != nil {
		slog.Error("vpcd: failed to reconcile port SGs",
			"port", portName, "eni_id", evt.NetworkInterfaceId, "sgs", evt.SecurityGroupIds, "err", err)
		respond(msg, err)
		return
	}

	slog.Info("vpcd: updated port group memberships",
		"port", portName,
		"eni_id", evt.NetworkInterfaceId,
		"sgs", evt.SecurityGroupIds,
	)
	respond(msg, nil)
}

// reconcilePortSGs converges the OVN port-group membership of lspName against
// desiredSGs. Reads current membership from the libovsdb cache, computes
// add/remove sets, and applies each side incrementally. Idempotent — replaying
// the same desired state is a no-op. Self-healing — a previous partial failure
// converges on the next call. desiredSGs may be nil (delete-port path) to
// remove the port from every group.
//
// For each port-group join, the port's privateIP is also inserted into the
// matching address set (<pg>_ip4) so SG-to-SG rule matches like
// "ip4.src == $<pg>_ip4" resolve to this port. Removes mirror the inserts.
// privateIP captures the ENI's primary IP today; the helper is phrased as
// "all private IPs" so future secondary-IP support can extend without changing
// the call shape.
func (h *TopologyHandler) reconcilePortSGs(ctx context.Context, lspName, privateIP string, desiredSGs []string) error {
	desired := make(map[string]struct{}, len(desiredSGs))
	for _, sgId := range desiredSGs {
		desired[portGroupName(sgId)] = struct{}{}
	}

	currentNames, err := h.ovn.ListPortGroupsForPort(ctx, lspName)
	if err != nil {
		return fmt.Errorf("list current port groups for %s: %w", lspName, err)
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

	// Single OVSDB transaction so a 5-SG → different-5-SG modify never
	// exposes an intermediate state with fewer port groups (which would
	// let the OVN default = unrestricted apply for the gap).
	return h.ovn.UpdatePortGroupMemberships(ctx, lspName, privateIP, addPGs, removePGs)
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

	// Resolve external pool for this VPC's WAN nexthop and gw LRP IP.
	//
	// Direct mode: link-local LRP is fine — distributed NAT puts a per-VM
	// dnat_and_snat row with external_mac/logical_port on every chassis, so
	// the LRP IP itself never goes on the wire.
	//
	// Centralized mode (veth/macvlan): the LRP is the on-wire egress, so it
	// must hold a WAN-subnet IP. Allocate one from pool.GwLrpRange (or
	// auto-derive from the WAN subnet) so each VPC gets a distinct sender
	// IP (mulga-siv-36). Without this the upstream router silently drops
	// ARP for the WAN nexthop (RFC 826) and the default route never
	// resolves.
	// TODO: use VPC's region/AZ once we track it; for now use first matching pool.
	pool := h.findExternalPool("", "")
	gatewayNetwork := gatewayPortNetwork
	wanGateway := "169.254.0.2"
	gwLrpIP := ""

	switch {
	case pool != nil:
		wanGateway = pool.Gateway
		if h.useCentralizedNAT() {
			network, ip, allocErr := h.expectedGatewayPortNetwork(ctx, evt.VpcId)
			if allocErr != nil {
				slog.Error("vpcd: failed to allocate gw LRP IP for centralized NAT",
					"vpc_id", evt.VpcId, "pool", pool.Name, "err", allocErr)
				_ = h.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
				respond(msg, allocErr)
				return
			}
			gwLrpIP = ip
			gatewayNetwork = network
		}
		slog.Info("vpcd: using external pool for IGW",
			"pool", pool.Name,
			"source", pool.Source,
			"lrp_network", gatewayNetwork,
			"wan_gateway", wanGateway,
		)
	case h.externalMode == "pool":
		slog.Warn("vpcd: external mode is set but no matching pool found, using link-local fallback")
	}

	// 3. Create gateway router port on the VPC router connecting to external switch
	gwMAC := generateMAC("gw-" + evt.VpcId)
	lrpExtIDs := map[string]string{
		"spinifex:vpc_id": evt.VpcId,
		"spinifex:igw_id": evt.InternetGatewayId,
		"spinifex:role":   "gateway",
	}
	if gwLrpIP != "" {
		lrpExtIDs[gatewayIPExtID] = gwLrpIP
	}
	lrp := &nbdb.LogicalRouterPort{
		Name:        gwPortName,
		MAC:         gwMAC,
		Networks:    []string{gatewayNetwork},
		ExternalIDs: lrpExtIDs,
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
	// OutputPort must be set explicitly because the gateway router port uses
	// link-local 169.254.0.1/30, whose network does not contain the WAN
	// nexthop (e.g. 192.168.1.1). Without it OVN northd silently drops the
	// route from the southbound DB.
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
		"lrp_network", gatewayNetwork,
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

	// 7. Release any DHCP-acquired gateway LRP lease (mulga-siv-38). Best-
	//    effort — upstream server expires the lease on its own if this fails.
	h.releaseGatewayLRPLease(evt.VpcId)

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

	// Direct mode: link-local LRP. Centralized mode: allocate from
	// pool.GwLrpRange so the LRP can ARP on the WAN subnet (mulga-siv-36).
	pool := h.findExternalPool("", "")
	gatewayNetwork := gatewayPortNetwork
	wanGateway := "169.254.0.2"
	gwLrpIP := ""
	if pool != nil {
		wanGateway = pool.Gateway
		if h.useCentralizedNAT() {
			network, ip, allocErr := h.expectedGatewayPortNetwork(ctx, vpcId)
			if allocErr != nil {
				return fmt.Errorf("allocate gw LRP IP for %s: %w", vpcId, allocErr)
			}
			gwLrpIP = ip
			gatewayNetwork = network
		}
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
	reconcileLrpExtIDs := map[string]string{
		"spinifex:vpc_id": vpcId,
		"spinifex:role":   "gateway",
	}
	if gwLrpIP != "" {
		reconcileLrpExtIDs[gatewayIPExtID] = gwLrpIP
	}
	lrp := &nbdb.LogicalRouterPort{
		Name:        gwPortName,
		MAC:         gwMAC,
		Networks:    []string{gatewayNetwork},
		ExternalIDs: reconcileLrpExtIDs,
	}
	if err := h.ovn.CreateLogicalRouterPort(ctx, routerName, lrp); err != nil {
		_ = h.ovn.DeleteLogicalSwitch(ctx, extSwitchName)
		return fmt.Errorf("create gateway router port %s: %w", gwPortName, err)
	}
	// Stale-Networks self-heal lives in RetrofitAllGatewayPortNetworks at
	// startup — Reconcile gates this whole function on "ext switch missing",
	// so by the time we reach this line the LRP we just created cannot be
	// stale (mulga-siv-26 D8).

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

	// 6. Add default route (OutputPort required because the LRP uses link-local
	// 169.254.0.1/30, off-subnet from the WAN nexthop)
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
		"lrp_network", gatewayNetwork, "wan_gateway", wanGateway)
	return nil
}

// reconcileGatewayChassis brings every gateway LRP's HA-scheduling state into
// agreement with the live SBDB chassis list. Two-phase:
//
//  1. Delete Gateway_Chassis rows whose chassis_name no longer matches any
//     entry in validNames. These rows are inevitable when the OVS system-id
//     changes across a reboot (mulga-999): NB still references the old
//     chassis-* name, but the SB Chassis row is now under a UUID, so OVN
//     can't resolve the binding and cr-gw* port-bindings sit empty.
//
//  2. Re-bind every LRP tagged spinifex:role=gateway against validNames via
//     the idempotent SetGatewayChassis. Existing rows with matching priority
//     are no-ops; mismatched priorities are mutated; missing rows are
//     created.
//
// Called as the first step of ReconcileFromKV so chassis state is correct
// before any reconcileVPC / reconcileIGW work. Failures are logged and
// returned but do not abort the rest of the reconcile loop — partial
// progress is better than none.
func (h *TopologyHandler) reconcileGatewayChassis(ctx context.Context, validNames []string) error {
	valid := make(map[string]struct{}, len(validNames))
	for _, n := range validNames {
		valid[n] = struct{}{}
	}

	rows, err := h.ovn.ListGatewayChassis(ctx)
	if err != nil {
		return fmt.Errorf("list gateway_chassis: %w", err)
	}
	for _, gc := range rows {
		if _, ok := valid[gc.ChassisName]; ok {
			continue
		}
		// Gateway_Chassis.Name is "lrpName-chassisName" (see SetGatewayChassis).
		// The chassis suffix is unique because chassis names are globally
		// unique; trimming it leaves the owning LRP name.
		lrpName := strings.TrimSuffix(gc.Name, "-"+gc.ChassisName)
		if err := h.ovn.DeleteGatewayChassis(ctx, lrpName, gc.UUID); err != nil {
			slog.Warn("vpcd: failed to delete stale gateway_chassis",
				"row", gc.Name, "chassis_name", gc.ChassisName, "err", err)
			continue
		}
		slog.Warn("vpcd: deleted stale gateway_chassis",
			"row", gc.Name, "chassis_name", gc.ChassisName, "lrp", lrpName)
	}

	if len(validNames) == 0 {
		return nil
	}

	lrps, err := h.ovn.ListLogicalRouterPorts(ctx)
	if err != nil {
		return fmt.Errorf("list logical_router_port: %w", err)
	}
	for _, lrp := range lrps {
		if lrp.ExternalIDs["spinifex:role"] != "gateway" {
			continue
		}
		for i, name := range validNames {
			priority := max(20-(i*5), 1)
			if err := h.ovn.SetGatewayChassis(ctx, lrp.Name, name, priority); err != nil {
				slog.Warn("vpcd: failed to set gateway chassis on rebind",
					"lrp", lrp.Name, "chassis", name, "priority", priority, "err", err)
			}
		}
	}
	return nil
}

// --- Helpers ---

// gwLrpRange returns the per-VPC gateway LRP IP range for a pool. Priority:
//
//  1. Explicit pool.GwLrpRangeStart/End set in spinifex.toml.
//  2. Auto-derived from pool.Gateway + pool.PrefixLen — last 16 host IPs of
//     the WAN subnet (broadcast - 16 .. broadcast - 1). When that range
//     overlaps the per-VM EIP range (RangeStart..RangeEnd), shift to the
//     16 IPs immediately below RangeStart instead.
//
// Returns ok=false only when the pool is missing or the gateway/prefix is
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

	// 1. Explicit operator config wins.
	if pool.GwLrpRangeStart != "" || pool.GwLrpRangeEnd != "" {
		s := net.ParseIP(pool.GwLrpRangeStart).To4()
		e := net.ParseIP(pool.GwLrpRangeEnd).To4()
		if s != nil && e != nil && ipv4ToUint32(s) <= ipv4ToUint32(e) {
			return s, e, prefix, true
		}
		slog.Warn("vpcd: invalid explicit gw_lrp_range, attempting auto-derive",
			"pool", pool.Name, "start", pool.GwLrpRangeStart, "end", pool.GwLrpRangeEnd)
	}

	// 2. Auto-derive from subnet.
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
	autoEndU := bcastU - 1    // skip broadcast itself
	autoStartU := bcastU - 16 // 16-IP range

	// Shift below per-VM EIP range when overlap.
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

	// Clamp inside the subnet (network+1 .. broadcast-1) — never hand out
	// the network address, the broadcast, or an off-subnet IP.
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

	// Guard the gateway IP itself — never give a VPC the upstream nexthop.
	gwU := ipv4ToUint32(gw)
	if gwU >= autoStartU && gwU <= autoEndU {
		// Gateway is inside the auto range — shrink to exclude it.
		switch gwU {
		case autoStartU:
			autoStartU++
		case autoEndU:
			autoEndU--
		default:
			// Gateway in the middle: prefer the upper half.
			autoStartU = gwU + 1
		}
		if autoStartU > autoEndU {
			return nil, nil, 0, false
		}
	}

	return uint32ToIPv4(autoStartU), uint32ToIPv4(autoEndU), prefix, true
}

// gwLrpClientID is the DHCP client-id used for a VPC's gateway LRP lease.
// Stable across reboots — DHCPManager reuses the same lease on idempotent
// re-attach so the LRP IP doesn't change unless the upstream server
// reassigns it.
func gwLrpClientID(vpcId string) string { return "gw-lrp-" + vpcId }

// allocateGatewayLRPIPViaDHCP requests a DHCP lease from vpcd's DHCPManager
// for the gateway LRP of vpcId. The handler-side acquire is idempotent on
// client-id so retries return the same IP without a fresh DORA. Prefix
// comes from the lease's SubnetMask, falling back to pool.PrefixLen when
// the server omits option 1 (rare; should not happen in practice).
func (h *TopologyHandler) allocateGatewayLRPIPViaDHCP(ctx context.Context, vpcId string, pool *ExternalPoolConfig) (ip string, prefix int, err error) {
	_ = ctx
	if h.nc == nil {
		return "", 0, fmt.Errorf("vpcd: no NATS conn for DHCP gw LRP allocation (vpc %s, pool %s)", vpcId, pool.Name)
	}
	clientID := gwLrpClientID(vpcId)
	hostname := "spinifex-gw-" + vpcId
	vendorClass := "mulga-spinifex-gw-lrp"
	lease, dhcpErr := dhcp.RequestAcquire(h.nc, pool.DhcpBindBridge, clientID, hostname, vendorClass, pool.Name, "")
	if dhcpErr != nil {
		return "", 0, fmt.Errorf("dhcp acquire gw LRP IP for vpc %s: %w", vpcId, dhcpErr)
	}
	prefix = prefixFromMask(lease.SubnetMask)
	if prefix == 0 {
		prefix = pool.PrefixLen
	}
	if prefix <= 0 || prefix > 32 {
		return "", 0, fmt.Errorf("dhcp gw LRP for vpc %s: cannot determine prefix (mask=%q pool prefix=%d)", vpcId, lease.SubnetMask, pool.PrefixLen)
	}
	return lease.IP, prefix, nil
}

// releaseGatewayLRPLease asks vpcd's DHCPManager to drop the lease held for
// vpcId's gateway LRP. Best-effort — log on failure but never block IGW
// detach, since the upstream server will eventually expire the lease on
// its own.
func (h *TopologyHandler) releaseGatewayLRPLease(vpcId string) {
	if h.nc == nil {
		return
	}
	if err := dhcp.RequestRelease(h.nc, gwLrpClientID(vpcId)); err != nil {
		slog.Warn("vpcd: dhcp release for gw LRP failed", "vpc_id", vpcId, "err", err)
	}
}

// prefixFromMask converts a dotted-decimal mask ("255.255.255.0") to a
// prefix length. Returns 0 when the mask is empty or unparseable.
func prefixFromMask(mask string) int {
	if mask == "" {
		return 0
	}
	ip := net.ParseIP(mask).To4()
	if ip == nil {
		return 0
	}
	ones, bits := net.IPMask(ip).Size()
	if bits != 32 {
		return 0
	}
	return ones
}

// allocateGatewayLRPIP picks the next free IP in pool.GwLrpRange for the
// given VPC, scanning every existing LRP's spinifex:gateway_ip external_id
// to compute the used set. If gw-<vpcId> already has an allocation the
// existing IP is returned (idempotent reconcile). Returns ok=false when
// the pool has no gw_lrp_range — caller falls back to link-local.
func (h *TopologyHandler) allocateGatewayLRPIP(ctx context.Context, vpcId string, pool *ExternalPoolConfig) (ip string, prefix int, ok bool, err error) {
	start, end, prefix, ok := gwLrpRange(pool)
	if !ok {
		return "", 0, false, nil
	}

	gwPortName := "gw-" + vpcId
	lrps, err := h.ovn.ListLogicalRouterPorts(ctx)
	if err != nil {
		return "", 0, false, fmt.Errorf("list LRPs for gw IP allocation: %w", err)
	}
	used := make(map[uint32]struct{}, len(lrps))
	for _, lrp := range lrps {
		existing := lrp.ExternalIDs[gatewayIPExtID]
		if existing == "" {
			continue
		}
		if lrp.Name == gwPortName {
			// Idempotent: already allocated to this VPC.
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

func ipv4ToUint32(ip net.IP) uint32 {
	v := ip.To4()
	return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
}

func uint32ToIPv4(n uint32) net.IP {
	return net.IPv4(byte(n>>24&0xff), byte(n>>16&0xff), byte(n>>8&0xff), byte(n&0xff)).To4()
}

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

// generateMAC creates a deterministic locally-administered unicast MAC
// from a resource ID via utils.HashMAC. Inputs are vpcd-owned ids
// (subnet-..., gw-vpc-..., eni-...) which are unique on their own.
func generateMAC(resourceID string) string {
	return utils.HashMAC(resourceID)
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
