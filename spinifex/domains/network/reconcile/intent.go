package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"

	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	ec2igw "github.com/mulgadc/spinifex/spinifex/domains/ec2/igw"
	ec2natgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/natgw"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// IntentState is the desired OVN state derived from the JetStream KV
// snapshot, scoped to the local AZ. Empty maps are valid.
type IntentState struct {
	PeeringIntent
	TransitGatewayIntent

	VPCs        map[string]topology.VPCSpec
	Subnets     map[string]topology.SubnetSpec
	Ports       map[string]topology.PortSpec
	SGs         map[string]policy.SGSpec
	IGWs        map[string]external.IGWSpec // attached only
	EIPs        map[string]policy.EIPSpec   // keyed by eipIntentKey; associated only
	NATGWs      map[string]policy.NATGWSpec
	IGWRoutes   map[string]SubnetEgressIntent // per (subnet, dest) IGW egress reroute
	NATGWRoutes map[string]SubnetEgressIntent // per (subnet, dest) NATGW egress reroute
	DropGates   map[string]SubnetEgressIntent // per (subnet, dest=0.0.0.0/0) drop policy
	// IdlePorts maps a public-IP guest LSP to its instance when that instance is
	// stopped or terminated. Its port has no tap, so the datapath probe skips it.
	IdlePorts map[string]string
	// TopologyIncomplete is non-nil when a topology record was skipped because
	// it did not decode or a field did not parse. Intent then lacks a resource
	// that exists, so no path may remove or replace live state against it.
	TopologyIncomplete error
}

// skipLog collects the keys of records a topology loader could not use.
type skipLog struct {
	count int
	keys  []string
}

// maxSkipKeys bounds the keys named in TopologyIncomplete, so a bucket of bad
// records still makes one short log line.
const maxSkipKeys = 3

func (l *skipLog) add(key string) {
	l.count++
	if len(l.keys) < maxSkipKeys {
		l.keys = append(l.keys, key)
	}
}

func (l *skipLog) err() error {
	if l.count == 0 {
		return nil
	}
	return fmt.Errorf("%d topology record(s) did not decode or parse, first %s", l.count, strings.Join(l.keys, ", "))
}

// The instance record space the daemon owns. Duplicated rather than imported
// because daemon imports this package; a daemon test pins the two together.
const (
	InstanceRecordBucket = "spinifex-instance-state"
	InstanceRecordPrefix = "i."
)

// SubnetEgressIntent is a per-subnet default-route policy entry for the VPC LR,
// keyed by (subnet, destCIDR) pointing at an IGW or NAT gateway.
type SubnetEgressIntent struct {
	VPCID    string
	SubnetID string
	DestCIDR netip.Prefix
}

// eipIntentKey is the EIPs map key. Private IPs repeat across VPCs (every
// default VPC is 172.31.0.0/16), so the VPC is part of the identity.
func eipIntentKey(vpcID, privateIP string) string {
	return vpcID + "|" + privateIP
}

// subnetEgressKey is the IGWRoutes/NATGWRoutes map key.
func subnetEgressKey(subnetID string, prefix netip.Prefix) string {
	return subnetID + "|" + prefix.String()
}

// LoadIntentFromKV assembles IntentState for localAZ. Missing buckets are empty.
// AZ filter: `vpc.AZ == "" || vpc.AZ == localAZ`; children inherit it transitively.
func LoadIntentFromKV(ctx context.Context, js jetstream.JetStream, localAZ string) (IntentState, error) {
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
		IdlePorts:   make(map[string]string),
	}

	skips := &skipLog{}
	localVPCs, vpcIDs, err := loadVPCs(ctx, js, localAZ, intent.VPCs, skips)
	if err != nil {
		return IntentState{}, err
	}
	subnetAZs := map[string]string{}
	if err := loadSubnets(ctx, js, localVPCs, intent.Subnets, subnetAZs, skips); err != nil {
		return IntentState{}, err
	}
	if err := loadSGs(ctx, js, localVPCs, intent.SGs, skips); err != nil {
		return IntentState{}, err
	}
	portInstances := make(map[string]string)
	if err := loadPorts(ctx, js, localVPCs, intent.Ports, portInstances, skips); err != nil {
		return IntentState{}, err
	}
	if err := loadIGWs(ctx, js, localVPCs, intent.IGWs, skips); err != nil {
		return IntentState{}, err
	}
	if err := loadEIPs(ctx, js, localVPCs, intent.EIPs, skips); err != nil {
		return IntentState{}, err
	}
	loadIdlePorts(ctx, js, intent, portInstances)
	routeTables, err := loadRouteTables(ctx, js, localVPCs, skips)
	if err != nil {
		return IntentState{}, err
	}
	if err := loadNATGWs(ctx, js, localVPCs, intent.Subnets, routeTables, intent.NATGWs, skips); err != nil {
		return IntentState{}, err
	}
	loadSubnetEgressRoutes(localVPCs, intent.Subnets, routeTables, intent.IGWRoutes, intent.NATGWRoutes, skips)
	intent.PeeringIntent = loadPeering(ctx, js, localVPCs, vpcIDs)
	intent.PeeringRoutes = loadPeeringRoutes(intent.VPCs, intent.Subnets, routeTables)
	intent.TransitGatewayIntent = loadTransitGateways(ctx, js, intent.VPCs, vpcIDs, subnetAZs)
	intent.TGWRoutes = loadTGWRoutes(intent.VPCs, intent.Subnets, routeTables)
	loadSubnetDropGates(localVPCs, intent.Subnets, intent.IGWs, intent.IGWRoutes, intent.NATGWRoutes, intent.DropGates)
	if intent.TopologyIncomplete = skips.err(); intent.TopologyIncomplete != nil {
		slog.Warn("reconcile/intent: intent is incomplete; no live state will be removed", "err", intent.TopologyIncomplete)
	}

	return intent, nil
}

// loadSubnetEgressRoutes fans IGW/NATGW routes over associated subnets (explicit
// non-main + implicit main-RT subnets). Mirrors the runtime event-publisher fan-out.
func loadSubnetEgressRoutes(
	localVPCs map[string]struct{},
	subnets map[string]topology.SubnetSpec,
	routeTables []ec2routetable.RouteTableRecord,
	igwOut, natgwOut map[string]SubnetEgressIntent,
	skips *skipLog,
) {
	byTable := routeTableSubnets(subnets, routeTables)
	for _, rt := range routeTables {
		if _, ok := localVPCs[rt.VpcId]; !ok {
			continue
		}
		targets := byTable[rt.RouteTableId]
		for _, r := range rt.Routes {
			if r.State != "" && !strings.EqualFold(r.State, "active") {
				continue
			}
			prefix, err := netip.ParsePrefix(r.DestinationCidrBlock)
			if err != nil {
				slog.Debug("reconcile/intent: route CIDR parse failed",
					"route_table_id", rt.RouteTableId, "cidr", r.DestinationCidrBlock, "err", err)
				skips.add(rt.RouteTableId)
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

// loadSubnetDropGates emits a drop intent for each subnet whose VPC has an IGW
// but lacks a 0.0.0.0/0 reroute. VPCs without an IGW need no drop policy.
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

func keyIsVersion(key string) bool { return key == kvstore.VersionKey }

func validAddr(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

// openBucket opens a topology bucket. Only a bucket that does not exist reads
// as empty; any other failure fails the read, since an empty answer would let
// the sweeps remove everything the bucket holds.
func openBucket(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.KeyValue, bool, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		slog.Debug("reconcile/intent: bucket not available, skipping", "bucket", bucket)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open bucket %s: %w", bucket, err)
	}
	return kv, true, nil
}

// loadVPCs fills out with this AZ's VPCs and returns their IDs, and the ID of
// every VPC with a key in any AZ, readable or not; nil if the bucket is absent.
func loadVPCs(ctx context.Context, js jetstream.JetStream, localAZ string, out map[string]topology.VPCSpec, skips *skipLog) (map[string]struct{}, map[string]struct{}, error) {
	localVPCs := make(map[string]struct{})

	kv, ok, err := openBucket(ctx, js, ec2vpc.KVBucketVPCs)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return localVPCs, nil, nil
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return localVPCs, map[string]struct{}{}, nil
		}
		return nil, nil, fmt.Errorf("list VPC keys: %w", err)
	}
	vpcIDs := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if _, id, ok := strings.Cut(key, "."); ok && strings.HasPrefix(id, "vpc-") {
			vpcIDs[id] = struct{}{}
		}
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		// Fail the pass; skipping narrows localVPCs, which drops every ENI in the
		// VPC, which the port sweep reads as permission to delete every live
		// guest's port. Unmarshal failures below stay skips — poison record, not
		// an unhealthy store.
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return nil, nil, fmt.Errorf("read VPC %s: %w", key, err)
		}
		var rec ec2vpc.VPCRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Debug("reconcile/intent: VPC unmarshal failed", "key", key, "err", err)
			skips.add(key)
			continue
		}
		if !matchesLocalAZ(rec.AZ, localAZ) {
			continue
		}
		prefix, err := netip.ParsePrefix(rec.CidrBlock)
		if err != nil {
			slog.Debug("reconcile/intent: VPC CIDR parse failed", "vpc_id", rec.VpcId, "cidr", rec.CidrBlock, "err", err)
			skips.add(key)
			continue
		}
		out[rec.VpcId] = topology.VPCSpec{
			VPCID: rec.VpcId,
			CIDR:  prefix,
			VNI:   rec.VNI,
		}
		localVPCs[rec.VpcId] = struct{}{}
	}
	return localVPCs, vpcIDs, nil
}

func loadSubnets(ctx context.Context, js jetstream.JetStream, localVPCs map[string]struct{}, out map[string]topology.SubnetSpec, azs map[string]string, skips *skipLog) error {
	kv, ok, err := openBucket(ctx, js, ec2vpc.KVBucketSubnets)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list subnet keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("read subnet %s: %w", key, err)
		}
		var rec ec2vpc.SubnetRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Debug("reconcile/intent: subnet unmarshal failed", "key", key, "err", err)
			skips.add(key)
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		prefix, err := netip.ParsePrefix(rec.CidrBlock)
		if err != nil {
			slog.Debug("reconcile/intent: subnet CIDR parse failed", "subnet_id", rec.SubnetId, "cidr", rec.CidrBlock, "err", err)
			skips.add(key)
			continue
		}
		out[rec.SubnetId] = topology.SubnetSpec{
			SubnetID: rec.SubnetId,
			VPCID:    rec.VpcId,
			CIDR:     prefix,
		}
		azs[rec.SubnetId] = rec.AvailabilityZone
	}
	return nil
}

func loadSGs(ctx context.Context, js jetstream.JetStream, localVPCs map[string]struct{}, out map[string]policy.SGSpec, skips *skipLog) error {
	kv, ok, err := openBucket(ctx, js, ec2vpc.KVBucketSecurityGroups)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list SG keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("read security group %s: %w", key, err)
		}
		var rec ec2vpc.SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Debug("reconcile/intent: SG unmarshal failed", "key", key, "err", err)
			skips.add(key)
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		out[rec.GroupId] = policy.SGSpec{
			GroupID:      rec.GroupId,
			VPCID:        rec.VpcId,
			IngressRules: SGRulesToPolicyRules(rec.IngressRules),
			EgressRules:  SGRulesToPolicyRules(rec.EgressRules),
		}
	}
	return nil
}

func loadPorts(ctx context.Context, js jetstream.JetStream, localVPCs map[string]struct{}, out map[string]topology.PortSpec, instances map[string]string, skips *skipLog) error {
	kv, ok, err := openBucket(ctx, js, ec2vpc.KVBucketENIs)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list ENI keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("read ENI %s: %w", key, err)
		}
		var rec ec2vpc.ENIRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Debug("reconcile/intent: ENI unmarshal failed", "key", key, "err", err)
			skips.add(key)
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		addr, err := netip.ParseAddr(rec.PrivateIpAddress)
		if err != nil {
			slog.Debug("reconcile/intent: ENI IP parse failed", "eni", rec.NetworkInterfaceId, "ip", rec.PrivateIpAddress, "err", err)
			skips.add(key)
			continue
		}
		mac, err := net.ParseMAC(rec.MacAddress)
		if err != nil {
			slog.Debug("reconcile/intent: ENI MAC parse failed", "eni", rec.NetworkInterfaceId, "mac", rec.MacAddress, "err", err)
			skips.add(key)
			continue
		}
		// PublicIP (auto-assigned or ELB) marks the ENI for drop-gate exemption;
		// zero when absent. User EIPs come from the EIP bucket instead. A bad
		// address keeps the ENI, so its private port is still repaired.
		var publicIP netip.Addr
		if rec.PublicIpAddress != "" {
			if publicIP, err = netip.ParseAddr(rec.PublicIpAddress); err != nil {
				slog.Debug("reconcile/intent: ENI public IP parse failed", "eni", rec.NetworkInterfaceId, "ip", rec.PublicIpAddress, "err", err)
				skips.add(key)
			}
		}
		out[rec.NetworkInterfaceId] = topology.PortSpec{
			PortID:       rec.NetworkInterfaceId,
			SubnetID:     rec.SubnetId,
			VPCID:        rec.VpcId,
			PrivateIP:    addr,
			MAC:          mac,
			SGIDs:        append([]string(nil), rec.SecurityGroupIds...),
			PublicIP:     publicIP,
			SuppressDHCP: rec.SuppressDHCP,
		}
		if rec.InstanceId != "" {
			instances[rec.NetworkInterfaceId] = rec.InstanceId
		}
	}
	return nil
}

// loadIdlePorts fills intent.IdlePorts for the ports the EIP datapath probe would
// visit. Anything it cannot read is left out, so the port is probed as before.
func loadIdlePorts(ctx context.Context, js jetstream.JetStream, intent IntentState, instances map[string]string) {
	candidates := make(map[string]struct{}, len(intent.EIPs))
	for id, p := range intent.Ports {
		if p.PublicIP.IsValid() {
			candidates[topology.Port(id)] = struct{}{}
		}
	}
	for _, e := range intent.EIPs {
		if e.PortName != "" {
			candidates[e.PortName] = struct{}{}
		}
	}
	if len(candidates) == 0 {
		return
	}

	kv, err := js.KeyValue(ctx, InstanceRecordBucket)
	if err != nil {
		slog.Debug("reconcile/intent: instance record bucket not available, probing every guest port", "err", err)
		return
	}
	for lsp := range candidates {
		instanceID := instances[eniIDFromPort(lsp)]
		if instanceID == "" {
			continue
		}
		entry, err := kv.Get(ctx, InstanceRecordPrefix+instanceID)
		if err != nil {
			if !errors.Is(err, jetstream.ErrKeyNotFound) {
				slog.Warn("reconcile/intent: instance record read failed", "instance_id", instanceID, "err", err)
			}
			continue
		}
		var rec vm.InstanceRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("reconcile/intent: instance record unmarshal failed", "instance_id", instanceID, "err", err)
			continue
		}
		if instanceHasNoTap(&rec) {
			intent.IdlePorts[lsp] = instanceID
		}
	}
}

// instanceHasNoTap reports an instance nothing will relaunch. A DRAIN also leaves
// StateStopped, but restore relaunches those, so desired state has to agree.
func instanceHasNoTap(rec *vm.InstanceRecord) bool {
	switch rec.Status.Status {
	case vm.StateTerminated:
		return true
	case vm.StateStopped:
		return rec.Spec.DesiredState == vm.DesiredStopped
	case vm.StateProvisioning, vm.StatePending, vm.StateRunning, vm.StateStopping, vm.StateShuttingDown, vm.StateError:
		// Treated as having a tap, so the reconciler keeps their network state.
	}
	return false
}

func loadIGWs(ctx context.Context, js jetstream.JetStream, localVPCs map[string]struct{}, out map[string]external.IGWSpec, skips *skipLog) error {
	kv, ok, err := openBucket(ctx, js, ec2igw.KVBucketIGW)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list IGW keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("read IGW %s: %w", key, err)
		}
		var rec ec2igw.IGWRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Debug("reconcile/intent: IGW unmarshal failed", "key", key, "err", err)
			skips.add(key)
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
			RecordKey:         key,
			AttachPending:     rec.AttachState == ec2igw.AttachStatePending,
		}
	}
	return nil
}

func loadEIPs(ctx context.Context, js jetstream.JetStream, localVPCs map[string]struct{}, out map[string]policy.EIPSpec, skips *skipLog) error {
	kv, ok, err := openBucket(ctx, js, ec2eip.KVBucketEIPs)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list EIP keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("read EIP %s: %w", key, err)
		}
		var rec ec2eip.EIPRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Debug("reconcile/intent: EIP unmarshal failed", "key", key, "err", err)
			skips.add(key)
			continue
		}
		if !strings.EqualFold(rec.State, "associated") || rec.VpcId == "" || rec.PublicIp == "" || rec.PrivateIp == "" {
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		if !validAddr(rec.PublicIp) || !validAddr(rec.PrivateIp) {
			slog.Debug("reconcile/intent: EIP address parse failed", "key", key, "public_ip", rec.PublicIp, "private_ip", rec.PrivateIp)
			skips.add(key)
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
		// MAC drives distributed dnat_and_snat; empty falls back to centralised.
		spec.MAC = rec.MacAddress
		out[eipIntentKey(rec.VpcId, rec.PrivateIp)] = spec
	}
	return nil
}

// loadRouteTables snapshots every local-VPC route table.
func loadRouteTables(ctx context.Context, js jetstream.JetStream, localVPCs map[string]struct{}, skips *skipLog) ([]ec2routetable.RouteTableRecord, error) {
	kv, ok, err := openBucket(ctx, js, ec2routetable.KVBucketRouteTables)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list route table keys: %w", err)
	}
	var out []ec2routetable.RouteTableRecord
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("read route table %s: %w", key, err)
		}
		var rec ec2routetable.RouteTableRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Debug("reconcile/intent: route table unmarshal failed", "key", key, "err", err)
			skips.add(key)
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// natgwSpecKey is the intent-map key for NATGWSpec: one entry per (natgwID, subnetCIDR).
func natgwSpecKey(natgwID, subnetCIDR string) string {
	return natgwID + "|" + subnetCIDR
}

// loadNATGWs emits one NATGWSpec per (NATGW, associated private subnet) pair.
// SNAT rewrites traffic from the private subnets routed through the NATGW, not its home subnet.
func loadNATGWs(
	ctx context.Context,
	js jetstream.JetStream,
	localVPCs map[string]struct{},
	subnets map[string]topology.SubnetSpec,
	routeTables []ec2routetable.RouteTableRecord,
	out map[string]policy.NATGWSpec,
	skips *skipLog,
) error {
	kv, ok, err := openBucket(ctx, js, ec2natgw.KVBucketNatGateways)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list NAT GW keys: %w", err)
	}
	for _, key := range keys {
		if keyIsVersion(key) {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("read NAT gateway %s: %w", key, err)
		}
		var rec ec2natgw.NatGatewayRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Debug("reconcile/intent: NAT GW unmarshal failed", "key", key, "err", err)
			skips.add(key)
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
			if !slices.ContainsFunc(rt.Routes, func(r ec2routetable.RouteRecord) bool {
				return r.NatGatewayId == rec.NatGatewayId
			}) {
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

// SGRulesToPolicyRules adapts stored SGRules to policy.Rule for the ACL builder
// and the endpoint proxy's egress check alike, dropping any rule with no IPv4
// source, which would reach them as an unspecified rule, not a narrower one.
func SGRulesToPolicyRules(in []ec2vpc.SGRule) []policy.Rule {
	if len(in) == 0 {
		return nil
	}
	out := make([]policy.Rule, 0, len(in))
	for _, r := range in {
		if r.CidrIp == "" && r.SourceSG == "" && r.PrefixListId == "" {
			continue
		}
		out = append(out, policy.Rule{
			IPProtocol:   r.IpProtocol,
			FromPort:     r.FromPort,
			ToPort:       r.ToPort,
			CIDR:         r.CidrIp,
			SourceSG:     r.SourceSG,
			PrefixListID: r.PrefixListId,
		})
	}
	return out
}
