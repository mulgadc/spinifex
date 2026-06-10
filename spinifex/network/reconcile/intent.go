package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"

	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_natgw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	handlers_ec2_routetable "github.com/mulgadc/spinifex/spinifex/handlers/ec2/routetable"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// IntentState is the desired OVN state derived from the JetStream KV
// snapshot, scoped to the local AZ. Empty maps are valid.
type IntentState struct {
	VPCs        map[string]topology.VPCSpec
	Subnets     map[string]topology.SubnetSpec
	Ports       map[string]topology.PortSpec
	SGs         map[string]policy.SGSpec
	IGWs        map[string]external.IGWSpec // attached only
	EIPs        map[string]policy.EIPSpec   // keyed by logicalIP; associated only
	NATGWs      map[string]policy.NATGWSpec
	IGWRoutes   map[string]SubnetEgressIntent // per (subnet, dest) IGW egress reroute
	NATGWRoutes map[string]SubnetEgressIntent // per (subnet, dest) NATGW egress reroute
	DropGates   map[string]SubnetEgressIntent // per (subnet, dest=0.0.0.0/0) drop policy
}

// SubnetEgressIntent is a per-subnet default-route policy installed on the
// VPC LR. Mirrors the publishIGWRouteEvent fan-out: one entry per (subnet,
// destCIDR) that the source route table directs at an IGW or NAT gateway.
type SubnetEgressIntent struct {
	VPCID    string
	SubnetID string
	DestCIDR netip.Prefix
}

// subnetEgressKey is the IGWRoutes/NATGWRoutes map key.
func subnetEgressKey(subnetID string, prefix netip.Prefix) string {
	return subnetID + "|" + prefix.String()
}

// LoadIntentFromKV assembles IntentState scoped to localAZ. Missing buckets
// (first boot) are treated as empty. AZ filter: `vpc.AZ == "" || vpc.AZ ==
// localAZ`. Children inherit the filter transitively via their parent VPC.
func LoadIntentFromKV(ctx context.Context, js nats.JetStreamContext, localAZ string) (IntentState, error) {
	intent := IntentState{
		VPCs:        make(map[string]topology.VPCSpec),
		Subnets:     make(map[string]topology.SubnetSpec),
		Ports:       make(map[string]topology.PortSpec),
		SGs:         make(map[string]policy.SGSpec),
		IGWs:        make(map[string]external.IGWSpec),
		EIPs:        make(map[string]policy.EIPSpec),
		NATGWs:      make(map[string]policy.NATGWSpec),
		IGWRoutes:   make(map[string]SubnetEgressIntent),
		NATGWRoutes: make(map[string]SubnetEgressIntent),
		DropGates:   make(map[string]SubnetEgressIntent),
	}

	localVPCs, err := loadVPCs(js, localAZ, intent.VPCs)
	if err != nil {
		return IntentState{}, err
	}
	if err := loadSubnets(js, localVPCs, intent.Subnets); err != nil {
		return IntentState{}, err
	}
	if err := loadSGs(js, localVPCs, intent.SGs); err != nil {
		return IntentState{}, err
	}
	if err := loadPorts(js, localVPCs, intent.Ports); err != nil {
		return IntentState{}, err
	}
	if err := loadIGWs(js, localVPCs, intent.IGWs); err != nil {
		return IntentState{}, err
	}
	if err := loadEIPs(js, localVPCs, intent.EIPs); err != nil {
		return IntentState{}, err
	}
	routeTables, err := loadRouteTables(js, localVPCs)
	if err != nil {
		return IntentState{}, err
	}
	if err := loadNATGWs(js, localVPCs, intent.Subnets, routeTables, intent.NATGWs); err != nil {
		return IntentState{}, err
	}
	loadSubnetEgressRoutes(localVPCs, intent.Subnets, routeTables, intent.IGWRoutes, intent.NATGWRoutes)
	loadSubnetDropGates(localVPCs, intent.Subnets, intent.IGWs, intent.IGWRoutes, intent.NATGWRoutes, intent.DropGates)

	_ = ctx
	return intent, nil
}

// loadSubnetEgressRoutes fans IGW/NATGW routes out over the subnets that
// inherit them — explicit non-main associations plus, for the main RT,
// subnets in the same VPC with no explicit non-main association. Mirrors
// publishIGWRouteEvents/publishNatGatewayEvents so the reconcile pass
// reaches the same per-subnet set as the runtime event path.
func loadSubnetEgressRoutes(
	localVPCs map[string]struct{},
	subnets map[string]topology.SubnetSpec,
	routeTables []handlers_ec2_routetable.RouteTableRecord,
	igwOut, natgwOut map[string]SubnetEgressIntent,
) {
	subnetsByVPC := make(map[string][]string, len(localVPCs))
	for id, spec := range subnets {
		subnetsByVPC[spec.VPCID] = append(subnetsByVPC[spec.VPCID], id)
	}

	explicitByVPC := make(map[string]map[string]struct{}, len(routeTables))
	for _, rt := range routeTables {
		if _, ok := localVPCs[rt.VpcId]; !ok {
			continue
		}
		ex, ok := explicitByVPC[rt.VpcId]
		if !ok {
			ex = map[string]struct{}{}
			explicitByVPC[rt.VpcId] = ex
		}
		for _, assoc := range rt.Associations {
			if assoc.SubnetId == "" || assoc.Main {
				continue
			}
			ex[assoc.SubnetId] = struct{}{}
		}
	}

	for _, rt := range routeTables {
		if _, ok := localVPCs[rt.VpcId]; !ok {
			continue
		}
		targets := map[string]struct{}{}
		for _, assoc := range rt.Associations {
			if assoc.SubnetId == "" || assoc.Main {
				continue
			}
			targets[assoc.SubnetId] = struct{}{}
		}
		if rt.IsMain {
			for _, subnetID := range subnetsByVPC[rt.VpcId] {
				if _, ok := explicitByVPC[rt.VpcId][subnetID]; ok {
					continue
				}
				targets[subnetID] = struct{}{}
			}
		}
		for _, r := range rt.Routes {
			if r.State != "" && !strings.EqualFold(r.State, "active") {
				continue
			}
			prefix, err := netip.ParsePrefix(r.DestinationCidrBlock)
			if err != nil {
				slog.Warn("reconcile/intent: route CIDR parse failed",
					"route_table_id", rt.RouteTableId, "cidr", r.DestinationCidrBlock, "err", err)
				continue
			}
			var sink map[string]SubnetEgressIntent
			switch {
			case strings.HasPrefix(r.GatewayId, "igw-"):
				sink = igwOut
			case r.NatGatewayId != "":
				sink = natgwOut
			default:
				continue
			}
			for subnetID := range targets {
				sink[subnetEgressKey(subnetID, prefix)] = SubnetEgressIntent{
					VPCID:    rt.VpcId,
					SubnetID: subnetID,
					DestCIDR: prefix,
				}
			}
		}
	}
}

// loadSubnetDropGates emits one drop intent per subnet whose VPC has an
// attached IGW but which has no 0.0.0.0/0 reroute path (neither IGWRoutes
// nor NATGWRoutes carry an entry for it). VPCs without an attached IGW
// have no router-wide default static route, so lr_in_ip_routing already
// kills the packet — no drop policy needed.
func loadSubnetDropGates(
	localVPCs map[string]struct{},
	subnets map[string]topology.SubnetSpec,
	igws map[string]external.IGWSpec,
	igwRoutes, natgwRoutes map[string]SubnetEgressIntent,
	out map[string]SubnetEgressIntent,
) {
	defaultPrefix := netip.MustParsePrefix("0.0.0.0/0")
	for _, subnet := range subnets {
		if _, ok := localVPCs[subnet.VPCID]; !ok {
			continue
		}
		if _, ok := igws[subnet.VPCID]; !ok {
			continue
		}
		key := subnetEgressKey(subnet.SubnetID, defaultPrefix)
		if _, ok := igwRoutes[key]; ok {
			continue
		}
		if _, ok := natgwRoutes[key]; ok {
			continue
		}
		out[key] = SubnetEgressIntent{
			VPCID:    subnet.VPCID,
			SubnetID: subnet.SubnetID,
			DestCIDR: defaultPrefix,
		}
	}
}

// matchesLocalAZ enforces §11.1: empty AZ counts as local (legacy records).
func matchesLocalAZ(vpcAZ, localAZ string) bool {
	return vpcAZ == "" || vpcAZ == localAZ
}

func keyIsVersion(key string) bool { return key == utils.VersionKey }

func loadVPCs(js nats.JetStreamContext, localAZ string, out map[string]topology.VPCSpec) (map[string]struct{}, error) {
	localVPCs := make(map[string]struct{})

	kv, err := js.KeyValue(handlers_ec2_vpc.KVBucketVPCs)
	if err != nil {
		slog.Debug("reconcile/intent: VPC bucket not available, skipping", "err", err)
		return localVPCs, nil
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return localVPCs, nil
		}
		return nil, fmt.Errorf("list VPC keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(key)
		if err != nil {
			slog.Warn("reconcile/intent: VPC read failed", "key", key, "err", err)
			continue
		}
		var rec handlers_ec2_vpc.VPCRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("reconcile/intent: VPC unmarshal failed", "key", key, "err", err)
			continue
		}
		if !matchesLocalAZ(rec.AZ, localAZ) {
			continue
		}
		prefix, err := netip.ParsePrefix(rec.CidrBlock)
		if err != nil {
			slog.Warn("reconcile/intent: VPC CIDR parse failed", "vpc_id", rec.VpcId, "cidr", rec.CidrBlock, "err", err)
			continue
		}
		out[rec.VpcId] = topology.VPCSpec{
			VPCID: rec.VpcId,
			CIDR:  prefix,
			VNI:   rec.VNI,
		}
		localVPCs[rec.VpcId] = struct{}{}
	}
	return localVPCs, nil
}

func loadSubnets(js nats.JetStreamContext, localVPCs map[string]struct{}, out map[string]topology.SubnetSpec) error {
	kv, err := js.KeyValue(handlers_ec2_vpc.KVBucketSubnets)
	if err != nil {
		slog.Debug("reconcile/intent: subnet bucket not available, skipping", "err", err)
		return nil
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list subnet keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(key)
		if err != nil {
			slog.Warn("reconcile/intent: subnet read failed", "key", key, "err", err)
			continue
		}
		var rec handlers_ec2_vpc.SubnetRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("reconcile/intent: subnet unmarshal failed", "key", key, "err", err)
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		prefix, err := netip.ParsePrefix(rec.CidrBlock)
		if err != nil {
			slog.Warn("reconcile/intent: subnet CIDR parse failed", "subnet_id", rec.SubnetId, "cidr", rec.CidrBlock, "err", err)
			continue
		}
		out[rec.SubnetId] = topology.SubnetSpec{
			SubnetID: rec.SubnetId,
			VPCID:    rec.VpcId,
			CIDR:     prefix,
		}
	}
	return nil
}

func loadSGs(js nats.JetStreamContext, localVPCs map[string]struct{}, out map[string]policy.SGSpec) error {
	kv, err := js.KeyValue(handlers_ec2_vpc.KVBucketSecurityGroups)
	if err != nil {
		slog.Debug("reconcile/intent: SG bucket not available, skipping", "err", err)
		return nil
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list SG keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(key)
		if err != nil {
			slog.Warn("reconcile/intent: SG read failed", "key", key, "err", err)
			continue
		}
		var rec handlers_ec2_vpc.SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("reconcile/intent: SG unmarshal failed", "key", key, "err", err)
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		out[rec.GroupId] = policy.SGSpec{
			GroupID:      rec.GroupId,
			VPCID:        rec.VpcId,
			IngressRules: sgRulesToPolicyRules(rec.IngressRules),
			EgressRules:  sgRulesToPolicyRules(rec.EgressRules),
		}
	}
	return nil
}

func loadPorts(js nats.JetStreamContext, localVPCs map[string]struct{}, out map[string]topology.PortSpec) error {
	kv, err := js.KeyValue(handlers_ec2_vpc.KVBucketENIs)
	if err != nil {
		slog.Debug("reconcile/intent: ENI bucket not available, skipping", "err", err)
		return nil
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list ENI keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(key)
		if err != nil {
			slog.Warn("reconcile/intent: ENI read failed", "key", key, "err", err)
			continue
		}
		var rec handlers_ec2_vpc.ENIRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("reconcile/intent: ENI unmarshal failed", "key", key, "err", err)
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		addr, err := netip.ParseAddr(rec.PrivateIpAddress)
		if err != nil {
			slog.Warn("reconcile/intent: ENI IP parse failed", "eni", rec.NetworkInterfaceId, "ip", rec.PrivateIpAddress, "err", err)
			continue
		}
		mac, err := net.ParseMAC(rec.MacAddress)
		if err != nil {
			slog.Warn("reconcile/intent: ENI MAC parse failed", "eni", rec.NetworkInterfaceId, "mac", rec.MacAddress, "err", err)
			continue
		}
		out[rec.NetworkInterfaceId] = topology.PortSpec{
			PortID:    rec.NetworkInterfaceId,
			SubnetID:  rec.SubnetId,
			VPCID:     rec.VpcId,
			PrivateIP: addr,
			MAC:       mac,
			SGIDs:     append([]string(nil), rec.SecurityGroupIds...),
		}
	}
	return nil
}

func loadIGWs(js nats.JetStreamContext, localVPCs map[string]struct{}, out map[string]external.IGWSpec) error {
	kv, err := js.KeyValue(handlers_ec2_igw.KVBucketIGW)
	if err != nil {
		slog.Debug("reconcile/intent: IGW bucket not available, skipping", "err", err)
		return nil
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list IGW keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(key)
		if err != nil {
			slog.Warn("reconcile/intent: IGW read failed", "key", key, "err", err)
			continue
		}
		var rec handlers_ec2_igw.IGWRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("reconcile/intent: IGW unmarshal failed", "key", key, "err", err)
			continue
		}
		if rec.VpcId == "" || !strings.EqualFold(rec.State, "available") {
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		out[rec.VpcId] = external.IGWSpec{
			VPCID:             rec.VpcId,
			InternetGatewayID: rec.InternetGatewayId,
		}
	}
	return nil
}

func loadEIPs(js nats.JetStreamContext, localVPCs map[string]struct{}, out map[string]policy.EIPSpec) error {
	kv, err := js.KeyValue(handlers_ec2_eip.KVBucketEIPs)
	if err != nil {
		slog.Debug("reconcile/intent: EIP bucket not available, skipping", "err", err)
		return nil
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list EIP keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(key)
		if err != nil {
			slog.Warn("reconcile/intent: EIP read failed", "key", key, "err", err)
			continue
		}
		var rec handlers_ec2_eip.EIPRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("reconcile/intent: EIP unmarshal failed", "key", key, "err", err)
			continue
		}
		if !strings.EqualFold(rec.State, "associated") || rec.VpcId == "" || rec.PublicIp == "" || rec.PrivateIp == "" {
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		spec := policy.EIPSpec{
			VPCID:      rec.VpcId,
			ExternalIP: rec.PublicIp,
			LogicalIP:  rec.PrivateIp,
		}
		if rec.ENIId != "" {
			spec.PortName = topology.Port(rec.ENIId)
		}
		// MAC is resolved from the ENI port's port_security in policy.AddEIP,
		// uniformly for every public IP — see that function for the distributed
		// dnat_and_snat shape and the host-reboot recovery it drives.
		out[rec.PrivateIp] = spec
	}
	return nil
}

// loadRouteTables snapshots every route table whose VPC is local. Used by
// loadNATGWs to fan out NATGW SNAT specs over associated subnets — mirrors the
// event-driven publisher in handlers/ec2/routetable.
func loadRouteTables(js nats.JetStreamContext, localVPCs map[string]struct{}) ([]handlers_ec2_routetable.RouteTableRecord, error) {
	kv, err := js.KeyValue(handlers_ec2_routetable.KVBucketRouteTables)
	if err != nil {
		slog.Debug("reconcile/intent: route table bucket not available, skipping", "err", err)
		return nil, nil
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list route table keys: %w", err)
	}
	var out []handlers_ec2_routetable.RouteTableRecord
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(key)
		if err != nil {
			slog.Warn("reconcile/intent: route table read failed", "key", key, "err", err)
			continue
		}
		var rec handlers_ec2_routetable.RouteTableRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("reconcile/intent: route table unmarshal failed", "key", key, "err", err)
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// natgwSpecKey is the intent-map key for NATGWSpec. The reconciler needs one
// spec per (natgwID, subnetCIDR) because a NATGW may serve multiple subnets
// (route-table associations) and OVN stores one snat row per subnet CIDR.
func natgwSpecKey(natgwID, subnetCIDR string) string {
	return natgwID + "|" + subnetCIDR
}

// loadNATGWs emits one NATGWSpec per (NATGW, associated subnet) pair. Mirrors
// publishNatGatewayEventsForAssociation in handlers/ec2/routetable: a NATGW's
// SNAT rule rewrites traffic from the *private* subnets routed through it,
// not from the NATGW's own public subnet.
func loadNATGWs(
	js nats.JetStreamContext,
	localVPCs map[string]struct{},
	subnets map[string]topology.SubnetSpec,
	routeTables []handlers_ec2_routetable.RouteTableRecord,
	out map[string]policy.NATGWSpec,
) error {
	kv, err := js.KeyValue(handlers_ec2_natgw.KVBucketNatGateways)
	if err != nil {
		slog.Debug("reconcile/intent: NAT GW bucket not available, skipping", "err", err)
		return nil
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list NAT GW keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(key)
		if err != nil {
			slog.Warn("reconcile/intent: NAT GW read failed", "key", key, "err", err)
			continue
		}
		var rec handlers_ec2_natgw.NatGatewayRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("reconcile/intent: NAT GW unmarshal failed", "key", key, "err", err)
			continue
		}
		if !strings.EqualFold(rec.State, "available") || rec.VpcId == "" || rec.PublicIp == "" {
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}

		emitted := false
		for _, rt := range routeTables {
			if rt.VpcId != rec.VpcId {
				continue
			}
			hasNatRoute := false
			for _, r := range rt.Routes {
				if r.NatGatewayId == rec.NatGatewayId {
					hasNatRoute = true
					break
				}
			}
			if !hasNatRoute {
				continue
			}
			for _, assoc := range rt.Associations {
				if assoc.SubnetId == "" || assoc.Main {
					continue
				}
				subnet, ok := subnets[assoc.SubnetId]
				if !ok {
					slog.Warn("reconcile/intent: NAT GW associated subnet not in intent, skipping",
						"natgw_id", rec.NatGatewayId, "subnet_id", assoc.SubnetId)
					continue
				}
				cidr := subnet.CIDR.String()
				out[natgwSpecKey(rec.NatGatewayId, cidr)] = policy.NATGWSpec{
					VPCID:        rec.VpcId,
					NATGatewayID: rec.NatGatewayId,
					PublicIP:     rec.PublicIp,
					SubnetCIDR:   cidr,
				}
				emitted = true
			}
		}
		if !emitted {
			slog.Debug("reconcile/intent: NAT GW has no associated private subnets, skipping",
				"natgw_id", rec.NatGatewayId, "vpc_id", rec.VpcId)
		}
	}
	return nil
}

// sgRulesToPolicyRules adapts handler-side SGRule to policy.Rule.
func sgRulesToPolicyRules(in []handlers_ec2_vpc.SGRule) []policy.Rule {
	if len(in) == 0 {
		return nil
	}
	out := make([]policy.Rule, len(in))
	for i, r := range in {
		out[i] = policy.Rule{
			IPProtocol: r.IpProtocol,
			FromPort:   r.FromPort,
			ToPort:     r.ToPort,
			CIDR:       r.CidrIp,
			SourceSG:   r.SourceSG,
		}
	}
	return out
}
