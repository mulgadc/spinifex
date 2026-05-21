package vpcd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/dhcp"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// waitForFlowsHV shells out to `ovn-nbctl --wait=hv sync`, which bumps
// NB_Global.nb_cfg and blocks until every connected chassis has acknowledged
// the new sequence number — i.e. ovn-northd has compiled NB -> SB and
// ovn-controller has installed the resulting flows. Used after IGW attach
// so newly-launched VMs aren't unreachable while their gateway chassis is
// still catching up (mulga-siv-105).
//
// Bounded by ovn-nbctl --timeout=30 (seconds). On overrun we log a WARN
// and return nil — the caller continues. In practice flows converge within
// seconds; a 30s overrun means something OVN-side is wedged enough that
// failing VPC create wouldn't improve things.
//
// Declared as a var so tests can stub it.
var waitForFlowsHV = func() error {
	start := time.Now()
	cmd := sudoCommand("ovn-nbctl",
		"--no-leader-only",
		"--timeout=30",
		"--wait=hv",
		"sync",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("vpcd: OVN flows-ready barrier overran; continuing without confirmation",
			"elapsed", time.Since(start),
			"err", err,
			"output", strings.TrimSpace(string(out)),
		)
		return nil
	}
	slog.Debug("vpcd: OVN flows-ready barrier complete", "elapsed", time.Since(start))
	return nil
}

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
	TopicCreateSG         = "vpc.create-sg"
	TopicDeleteSG         = "vpc.delete-sg"
	TopicUpdateSG         = "vpc.update-sg"
)

// gatewayPortNetwork is the link-local CIDR every IGW gateway-LRP carries
// in distributed-NAT (direct) mode. Per-VM dnat_and_snat handles ARP per
// chassis so the LRP IP itself never goes on the wire. The default route's
// OutputPort is set explicitly so the WAN nexthop need not be on this subnet.
//
// Centralized-NAT (veth) bypasses this constant: the gateway LRP is
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
	// BridgeModeDirect adds the WAN NIC directly to br-external as an OVS port.
	// Enables distributed NAT. Only safe when the WAN NIC is NOT the SSH/
	// management NIC.
	BridgeModeDirect = "direct"
	// BridgeModeVeth uses a veth pair to link a Linux bridge (br-wan) to an
	// OVS bridge (br-ext). Requires centralized NAT because the Linux bridge
	// intermediary breaks distributed NAT hairpin routing.
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
	bridgeMode    string   // "direct" or "veth" — controls NAT mode and localnet options
	// nc is used to talk to vpcd's DHCPManager via vpc.dhcp.acquire /
	// vpc.dhcp.release for gateway LRP IPs in centralized NAT on
	// source="dhcp" pools (mulga-siv-38). nil when no DHCP pool is wired or
	// the test stack supplies a static-only mock.
	nc *nats.Conn

	// lm + lmOnce cache the topology.Manager produced from h.ovn so each
	// adapter call doesn't re-construct (Phase 2.6, mulga-siv-129).
	lmOnce sync.Once
	lm     topology.Manager

	// sgm + sgmOnce cache the policy.SecurityGroupManager backed by h.ovn so
	// SG subscribers reuse one manager instance across events.
	sgmOnce sync.Once
	sgm     policy.SecurityGroupManager

	// natm + natmOnce cache the policy.NATManager backed by h.ovn. NAT mode
	// resolves from h.bridgeMode at first use; the FlowsBarrier closure
	// shells out to ovn-nbctl --wait=hv sync via waitForFlowsHV.
	natmOnce sync.Once
	natm     policy.NATManager
	natmErr  error
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

// WithBridgeMode sets the external bridge mode ("direct" or "veth").
// Direct bridge enables distributed NAT; veth uses centralized NAT.
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

// useCentralizedNAT returns true if the bridge mode requires centralized NAT.
// Veth mode needs centralized NAT because the Linux bridge intermediary breaks
// distributed NAT hairpin routing. Only direct bridge mode supports distributed NAT.
func (h *TopologyHandler) useCentralizedNAT() bool {
	return h.bridgeMode == BridgeModeVeth
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
// Centralized mode (veth): options:nat-addresses=router must be set.
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
	spec := topology.VPCSpec{VPCID: evt.VpcId, VNI: evt.VNI}
	if evt.CidrBlock != "" {
		cidr, err := netip.ParsePrefix(evt.CidrBlock)
		if err != nil {
			slog.Error("vpcd: invalid CIDR in vpc.create event", "cidr", evt.CidrBlock, "err", err)
			respond(msg, err)
			return
		}
		spec.CIDR = cidr
	}
	if err := h.EnsureVPC(context.Background(), spec); err != nil {
		slog.Error("vpcd: EnsureVPC failed", "vpc_id", evt.VpcId, "err", err)
		respond(msg, err)
		return
	}
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
	if err := h.DeleteVPC(context.Background(), evt.VpcId); err != nil {
		slog.Error("vpcd: DeleteVPC failed", "vpc_id", evt.VpcId, "err", err)
		respond(msg, err)
		return
	}
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
	cidr, err := netip.ParsePrefix(evt.CidrBlock)
	if err != nil {
		slog.Error("vpcd: invalid CIDR in vpc.create-subnet event", "cidr", evt.CidrBlock, "err", err)
		respond(msg, err)
		return
	}
	if err := h.EnsureSubnet(context.Background(), topology.SubnetSpec{
		SubnetID: evt.SubnetId,
		VPCID:    evt.VpcId,
		CIDR:     cidr,
	}); err != nil {
		slog.Error("vpcd: EnsureSubnet failed", "subnet_id", evt.SubnetId, "err", err)
		respond(msg, err)
		return
	}
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
	spec := topology.SubnetSpec{SubnetID: evt.SubnetId, VPCID: evt.VpcId}
	if evt.CidrBlock != "" {
		if cidr, perr := netip.ParsePrefix(evt.CidrBlock); perr == nil {
			spec.CIDR = cidr
		}
	}
	if err := h.DeleteSubnet(context.Background(), spec); err != nil {
		slog.Error("vpcd: DeleteSubnet failed", "subnet_id", evt.SubnetId, "err", err)
		respond(msg, err)
		return
	}
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
	ip, err := netip.ParseAddr(evt.PrivateIpAddress)
	if err != nil {
		slog.Error("vpcd: invalid private IP in vpc.create-port event", "ip", evt.PrivateIpAddress, "err", err)
		respond(msg, err)
		return
	}
	mac, err := net.ParseMAC(evt.MacAddress)
	if err != nil {
		slog.Error("vpcd: invalid MAC in vpc.create-port event", "mac", evt.MacAddress, "err", err)
		respond(msg, err)
		return
	}
	if err := h.EnsurePort(context.Background(), topology.PortSpec{
		PortID:    evt.NetworkInterfaceId,
		SubnetID:  evt.SubnetId,
		VPCID:     evt.VpcId,
		PrivateIP: ip,
		MAC:       mac,
		SGIDs:     evt.SecurityGroupIds,
	}); err != nil {
		slog.Error("vpcd: EnsurePort failed", "eni_id", evt.NetworkInterfaceId, "err", err)
		respond(msg, err)
		return
	}
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
	if err := h.DeletePort(context.Background(), topology.PortSpec{
		PortID:   evt.NetworkInterfaceId,
		SubnetID: evt.SubnetId,
		VPCID:    evt.VpcId,
	}); err != nil {
		slog.Error("vpcd: DeletePort failed", "eni_id", evt.NetworkInterfaceId, "err", err)
		respond(msg, err)
		return
	}
	respond(msg, nil)
}

// handleUpdatePortSGs reconciles the port group membership for an ENI's LSP
// against the desired SG list in the event. The payload is declarative — the
// manager computes the add/remove diff from current OVN state.
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
	if err := h.SetPortSecurityGroups(context.Background(), evt.NetworkInterfaceId, evt.SecurityGroupIds); err != nil {
		slog.Error("vpcd: SetPortSecurityGroups failed",
			"eni_id", evt.NetworkInterfaceId, "sgs", evt.SecurityGroupIds, "err", err)
		respond(msg, err)
		return
	}
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
	// using the router port MAC. Required for centralized NAT (veth mode) so
	// that ARP replies for NAT IPs reach hosts correctly. Not needed for
	// direct bridge since OVS sees all traffic on the wire without MAC
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
	// Centralized mode (veth): the LRP is the on-wire egress, so it
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

	// Block until ovn-northd compiles + the gateway chassis has installed
	// flows for this VPC. Without this, a subsequent RunInstances ->
	// vpc.add-nat can complete while the datapath is still dark, and the
	// new VM is unreachable on its public IP for the duration of the
	// flow-install latency (typically seconds; observed up to minutes when
	// the chassis was loaded during CI). mulga-siv-105.
	_ = waitForFlowsHV()
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

// handleAddNAT decodes the EIP event and delegates to
// policy.NATManager.AddEIP. The manager owns the idempotency-skip,
// stale-rule cleanup, distributed-NAT mode selection, and post-write
// flow-install barrier.
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
	nm, err := h.natManager()
	if err != nil {
		slog.Error("vpcd: NAT manager init failed", "err", err)
		respond(msg, err)
		return
	}
	if err := nm.AddEIP(context.Background(), policy.EIPSpec{
		VPCID:      evt.VpcId,
		ExternalIP: evt.ExternalIP,
		LogicalIP:  evt.LogicalIP,
		PortName:   evt.PortName,
		MAC:        evt.MAC,
	}); err != nil {
		slog.Error("vpcd: AddEIP failed",
			"vpc_id", evt.VpcId, "external_ip", evt.ExternalIP,
			"logical_ip", evt.LogicalIP, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("vpcd: added dnat_and_snat rule",
		"vpc_id", evt.VpcId, "external_ip", evt.ExternalIP,
		"logical_ip", evt.LogicalIP, "port", evt.PortName)
	respond(msg, nil)
}

// handleDeleteNAT decodes the event and delegates to
// policy.NATManager.DeleteEIP. Idempotency on already-absent rules is
// handled by the manager.
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
	nm, err := h.natManager()
	if err != nil {
		slog.Error("vpcd: NAT manager init failed", "err", err)
		respond(msg, err)
		return
	}
	if err := nm.DeleteEIP(context.Background(), evt.VpcId, evt.LogicalIP); err != nil {
		slog.Error("vpcd: DeleteEIP failed",
			"vpc_id", evt.VpcId, "logical_ip", evt.LogicalIP, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("vpcd: deleted dnat_and_snat rule",
		"vpc_id", evt.VpcId, "logical_ip", evt.LogicalIP)
	respond(msg, nil)
}

// --- Reconciliation (called on startup, not via NATS) ---

// reconcileIGW ensures the OVN external switch, localnet port, gateway router
// port, router-bridging LSP, default route, and gateway chassis bindings exist
// for a VPC's internet gateway. Every step is Get-then-Create idempotent, with
// no cascade rollback on failure: partial topology is safe — the next
// reconcile pass heals whatever is missing.
func (h *TopologyHandler) reconcileIGW(ctx context.Context, vpcId, igwId string) error {
	routerName := "vpc-" + vpcId
	extSwitchName := "ext-" + vpcId
	extPortName := "ext-port-" + vpcId
	gwPortName := "gw-" + vpcId
	switchGWPortName := "gw-port-" + vpcId

	pool := h.findExternalPool("", "")
	wanGateway := "169.254.0.2"
	if pool != nil {
		wanGateway = pool.Gateway
	}

	// 1. External logical switch
	if _, err := h.ovn.GetLogicalSwitch(ctx, extSwitchName); err != nil {
		extIDs := map[string]string{
			"spinifex:vpc_id": vpcId,
			"spinifex:role":   "external",
		}
		if igwId != "" {
			extIDs["spinifex:igw_id"] = igwId
		}
		if err := h.ovn.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{
			Name:        extSwitchName,
			ExternalIDs: extIDs,
		}); err != nil {
			return fmt.Errorf("create external switch %s: %w", extSwitchName, err)
		}
	}

	// 2. Localnet port. ensureLocalnetOptions runs unconditionally — it is a
	// read-before-write no-op when the options are already correct, and the
	// recovery path (port exists with stale/missing options) needs it.
	if _, err := h.ovn.GetLogicalSwitchPort(ctx, extPortName); err != nil {
		portExtIDs := map[string]string{"spinifex:vpc_id": vpcId}
		if igwId != "" {
			portExtIDs["spinifex:igw_id"] = igwId
		}
		opts := map[string]string{"network_name": "external"}
		if h.useCentralizedNAT() {
			opts["nat-addresses"] = "router"
		}
		if err := h.ovn.CreateLogicalSwitchPort(ctx, extSwitchName, &nbdb.LogicalSwitchPort{
			Name:        extPortName,
			Type:        "localnet",
			Addresses:   []string{"unknown"},
			Options:     opts,
			ExternalIDs: portExtIDs,
		}); err != nil {
			return fmt.Errorf("create localnet port %s: %w", extPortName, err)
		}
	}
	if err := h.ensureLocalnetOptions(ctx, extPortName); err != nil {
		return fmt.Errorf("retrofit localnet options %s: %w", extPortName, err)
	}

	// 3. Gateway router port. expectedGatewayPortNetwork is only consulted
	// when we actually create the LRP — RetrofitAllGatewayPortNetworks
	// handles drift on pre-existing ports separately.
	if _, err := h.ovn.GetLogicalRouterPort(ctx, gwPortName); err != nil {
		gatewayNetwork := gatewayPortNetwork
		gwLrpIP := ""
		if pool != nil && h.useCentralizedNAT() {
			network, ip, allocErr := h.expectedGatewayPortNetwork(ctx, vpcId)
			if allocErr != nil {
				return fmt.Errorf("allocate gw LRP IP for %s: %w", vpcId, allocErr)
			}
			gwLrpIP = ip
			gatewayNetwork = network
		}
		lrpExtIDs := map[string]string{
			"spinifex:vpc_id": vpcId,
			"spinifex:role":   "gateway",
		}
		if gwLrpIP != "" {
			lrpExtIDs[gatewayIPExtID] = gwLrpIP
		}
		if err := h.ovn.CreateLogicalRouterPort(ctx, routerName, &nbdb.LogicalRouterPort{
			Name:        gwPortName,
			MAC:         generateMAC("gw-" + vpcId),
			Networks:    []string{gatewayNetwork},
			ExternalIDs: lrpExtIDs,
		}); err != nil {
			return fmt.Errorf("create gateway router port %s: %w", gwPortName, err)
		}
	}

	// 4. Switch port connecting external switch to router
	if _, err := h.ovn.GetLogicalSwitchPort(ctx, switchGWPortName); err != nil {
		if err := h.ovn.CreateLogicalSwitchPort(ctx, extSwitchName, &nbdb.LogicalSwitchPort{
			Name:        switchGWPortName,
			Type:        "router",
			Addresses:   []string{"router"},
			Options:     map[string]string{"router-port": gwPortName},
			ExternalIDs: map[string]string{"spinifex:vpc_id": vpcId},
		}); err != nil {
			return fmt.Errorf("create switch gateway port %s: %w", switchGWPortName, err)
		}
	}

	// 5. No blanket SNAT — per-VM dnat_and_snat rules handle public instances.
	// See handleIGWAttach comment for rationale (AWS parity).

	// 6. Default route. AddStaticRoute is non-idempotent (every retry leaves
	// a fresh duplicate row), so look up first via FindStaticRoute. An
	// existing row with mismatched nexthop/output is treated as an operator
	// override and left alone.
	existing, err := h.ovn.FindStaticRoute(ctx, routerName, "0.0.0.0/0")
	if err != nil {
		slog.Warn("vpcd reconcile: failed to query default route", "router", routerName, "err", err)
	}
	switch {
	case existing == nil && err == nil:
		if addErr := h.ovn.AddStaticRoute(ctx, routerName, &nbdb.LogicalRouterStaticRoute{
			IPPrefix:    "0.0.0.0/0",
			Nexthop:     wanGateway,
			OutputPort:  &gwPortName,
			ExternalIDs: map[string]string{"spinifex:vpc_id": vpcId},
		}); addErr != nil {
			slog.Warn("vpcd reconcile: failed to add default route", "err", addErr)
		}
	case existing != nil:
		existingPort := ""
		if existing.OutputPort != nil {
			existingPort = *existing.OutputPort
		}
		if existing.Nexthop != wanGateway || existingPort != gwPortName {
			slog.Warn("vpcd reconcile: default route differs from expected, leaving existing entry in place",
				"router", routerName,
				"existing_nexthop", existing.Nexthop, "want_nexthop", wanGateway,
				"existing_output_port", existingPort, "want_output_port", gwPortName)
		}
	}

	// 7. Schedule gateway chassis (already idempotent via SetGatewayChassis).
	for i, chassis := range h.chassisNames {
		priority := max(20-(i*5), 1)
		if err := h.ovn.SetGatewayChassis(ctx, gwPortName, chassis, priority); err != nil {
			slog.Warn("vpcd reconcile: failed to set gateway chassis", "chassis", chassis, "err", err)
		}
	}

	slog.Info("vpcd reconcile: ensured IGW topology",
		"ext_switch", extSwitchName, "gw_port", gwPortName, "wan_gateway", wanGateway)
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

// handleAddNATGateway is a thin wrapper that decodes the NATS event and
// delegates the SNAT installation to policy.NATManager.AddNATGateway.
func (h *TopologyHandler) handleAddNATGateway(msg *nats.Msg) {
	var evt NATGatewayEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.add-nat-gateway event", "err", err)
		return
	}
	nm, err := h.natManager()
	if err != nil {
		slog.Error("vpcd: NAT manager init failed", "err", err)
		return
	}
	if err := nm.AddNATGateway(context.Background(), policy.NATGWSpec{
		VPCID:        evt.VpcId,
		NATGatewayID: evt.NatGatewayId,
		PublicIP:     evt.PublicIp,
		SubnetCIDR:   evt.SubnetCidr,
	}); err != nil {
		slog.Error("vpcd: AddNATGateway failed",
			"vpc_id", evt.VpcId, "natgw_id", evt.NatGatewayId,
			"public_ip", evt.PublicIp, "subnet_cidr", evt.SubnetCidr, "err", err)
		return
	}
	slog.Info("vpcd: NAT Gateway SNAT rule added",
		"vpc_id", evt.VpcId, "natgw_id", evt.NatGatewayId,
		"public_ip", evt.PublicIp, "subnet_cidr", evt.SubnetCidr)
}

// handleDeleteNATGateway delegates SNAT teardown to policy.NATManager.
func (h *TopologyHandler) handleDeleteNATGateway(msg *nats.Msg) {
	var evt NATGatewayEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("vpcd: failed to unmarshal vpc.delete-nat-gateway event", "err", err)
		return
	}
	nm, err := h.natManager()
	if err != nil {
		slog.Error("vpcd: NAT manager init failed", "err", err)
		return
	}
	if err := nm.DeleteNATGateway(context.Background(), evt.VpcId, evt.SubnetCidr); err != nil {
		slog.Warn("vpcd: DeleteNATGateway failed",
			"vpc_id", evt.VpcId, "subnet_cidr", evt.SubnetCidr, "err", err)
		return
	}
	slog.Info("vpcd: NAT Gateway SNAT rule removed",
		"vpc_id", evt.VpcId, "natgw_id", evt.NatGatewayId, "subnet_cidr", evt.SubnetCidr)
}
