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

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/types"
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
	// setup and independent of the WAN bridge (which is Linux-side in veth
	// mode). Named as a constant so the vpcd sanity check refers to the
	// contract, not a hardcoded string (D18).
	OvnExternalBridge = "br-ext"
)

// TopologyHandler translates VPC lifecycle NATS events into OVN NB DB operations.
type TopologyHandler struct {
	ovn           OVNClient
	externalMode  string
	externalPools []ExternalPoolConfig
	chassisNames  []string       // OVN chassis names for gateway HA scheduling
	natMode       policy.NATMode // distributed (direct uplink) or centralized (veth uplink)

	// lm + lmOnce cache the topology.Manager produced from h.ovn so each
	// adapter call doesn't re-construct (Phase 2.6, mulga-siv-129).
	lmOnce sync.Once
	lm     topology.Manager

	// sgm + sgmOnce cache the policy.SecurityGroupManager backed by h.ovn so
	// SG subscribers reuse one manager instance across events.
	sgmOnce sync.Once
	sgm     policy.SecurityGroupManager

	// natm + natmOnce cache the policy.NATManager backed by h.ovn. NAT mode is
	// the value injected via WithNATMode; the FlowsBarrier closure shells out
	// to ovn-nbctl --wait=hv sync via waitForFlowsHV.
	natmOnce sync.Once
	natm     policy.NATManager
	natmErr  error

	// igwm + igwmOnce cache the external.IGWManager. All IGW attach/detach
	// paths route through it.
	igwmOnce sync.Once
	igwm     external.IGWManager
	igwmErr  error
}

// NewTopologyHandler creates a new TopologyHandler with optional external network config.
// NAT mode defaults to Distributed (direct uplink) when WithNATMode is omitted;
// matches the pre-Slice-B behaviour where unset bridgeMode also implied
// distributed via useCentralizedNAT().
func NewTopologyHandler(ovn OVNClient, opts ...TopologyOption) *TopologyHandler {
	h := &TopologyHandler{ovn: ovn, natMode: policy.NATModeDistributed}
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

// WithNATMode sets the NAT mode used by lazily-constructed managers. Derived
// upstream from the uplink/bridge mode (direct→distributed, veth→centralized).
func WithNATMode(mode policy.NATMode) TopologyOption {
	return func(h *TopologyHandler) {
		h.natMode = mode
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
	ctx := context.Background()
	// vpc.create-vpc and vpc.create-subnet are independent NATS events with no
	// inter-event ordering guarantee. Tenant bootstrap publishes both within
	// <1ms, so EnsureSubnet can race the parent VPC's CreateLogicalRouter and
	// fail with "logical router not found". EnsureVPC is idempotent on the
	// router name (mulga-siv-133).
	if err := h.EnsureVPC(ctx, topology.VPCSpec{VPCID: evt.VpcId}); err != nil {
		slog.Error("vpcd: EnsureVPC (subnet pre-create) failed", "vpc_id", evt.VpcId, "err", err)
		respond(msg, err)
		return
	}
	if err := h.EnsureSubnet(ctx, topology.SubnetSpec{
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

// handleIGWAttach decodes the event and delegates to
// external.IGWManager.AttachIGW.
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

	igw, err := h.igwManager()
	if err != nil {
		slog.Error("vpcd: IGW manager init failed", "err", err)
		respond(msg, err)
		return
	}
	if err := igw.AttachIGW(ctx, external.IGWSpec{
		VPCID:             evt.VpcId,
		InternetGatewayID: evt.InternetGatewayId,
	}); err != nil {
		slog.Error("vpcd: AttachIGW failed",
			"vpc_id", evt.VpcId, "igw_id", evt.InternetGatewayId, "err", err)
		respond(msg, err)
		return
	}
	respond(msg, nil)
}

// handleIGWDetach decodes the event and delegates the OVN teardown to
// external.IGWManager.DetachIGW. Static gateway LRP IPs live in OVN
// external_ids and are cleared when the LRP itself is deleted.
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

	igw, err := h.igwManager()
	if err != nil {
		slog.Error("vpcd: IGW manager init failed", "err", err)
		respond(msg, err)
		return
	}
	if err := igw.DetachIGW(ctx, evt.VpcId); err != nil {
		slog.Error("vpcd: DetachIGW failed", "vpc_id", evt.VpcId, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("vpcd: detached internet gateway from VPC",
		"igw_id", evt.InternetGatewayId, "vpc_id", evt.VpcId)
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

// --- Helpers ---

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
