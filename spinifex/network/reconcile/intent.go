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
	VPCs    map[string]topology.VPCSpec
	Subnets map[string]topology.SubnetSpec
	Ports   map[string]topology.PortSpec
	SGs     map[string]policy.SGSpec
	IGWs    map[string]external.IGWSpec // attached only
	EIPs    map[string]policy.EIPSpec   // keyed by logicalIP; associated only
	NATGWs  map[string]policy.NATGWSpec
}

// LoadIntentFromKV assembles IntentState scoped to localAZ. Missing buckets
// (first boot) are treated as empty. AZ filter: `vpc.AZ == "" || vpc.AZ ==
// localAZ`. Children inherit the filter transitively via their parent VPC.
func LoadIntentFromKV(ctx context.Context, js nats.JetStreamContext, localAZ string) (IntentState, error) {
	intent := IntentState{
		VPCs:    make(map[string]topology.VPCSpec),
		Subnets: make(map[string]topology.SubnetSpec),
		Ports:   make(map[string]topology.PortSpec),
		SGs:     make(map[string]policy.SGSpec),
		IGWs:    make(map[string]external.IGWSpec),
		EIPs:    make(map[string]policy.EIPSpec),
		NATGWs:  make(map[string]policy.NATGWSpec),
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
	if err := loadNATGWs(js, localVPCs, intent.Subnets, intent.NATGWs); err != nil {
		return IntentState{}, err
	}

	_ = ctx
	return intent, nil
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
		out[rec.PrivateIp] = spec
	}
	return nil
}

func loadNATGWs(js nats.JetStreamContext, localVPCs map[string]struct{}, subnets map[string]topology.SubnetSpec, out map[string]policy.NATGWSpec) error {
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
		if !strings.EqualFold(rec.State, "available") || rec.VpcId == "" || rec.PublicIp == "" || rec.SubnetId == "" {
			continue
		}
		if _, ok := localVPCs[rec.VpcId]; !ok {
			continue
		}
		subnet, ok := subnets[rec.SubnetId]
		if !ok {
			slog.Warn("reconcile/intent: NAT GW subnet not in intent, skipping", "natgw_id", rec.NatGatewayId, "subnet_id", rec.SubnetId)
			continue
		}
		out[rec.NatGatewayId] = policy.NATGWSpec{
			VPCID:        rec.VpcId,
			NATGatewayID: rec.NatGatewayId,
			PublicIP:     rec.PublicIp,
			SubnetCIDR:   subnet.CIDR.String(),
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
