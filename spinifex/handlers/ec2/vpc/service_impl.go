package handlers_ec2_vpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure VPCServiceImpl implements VPCService
var _ VPCService = (*VPCServiceImpl)(nil)

const (
	KVBucketVPCs       = "spinifex-vpc-vpcs"
	KVBucketSubnets    = "spinifex-vpc-subnets"
	KVBucketVNICounter = "spinifex-vpc-vni-counter"
	vniCounterKey      = "counter"
	vniStart           = 100 // Starting VNI value (avoid 0 and low numbers)

	KVBucketVPCsVersion       = 1
	KVBucketSubnetsVersion    = 1
	KVBucketVNICounterVersion = 1
)

// VPCRecord represents a stored VPC. AZ is stamped at create time; empty AZ
// is a legacy value that matches every AZ in the reconciler.
type VPCRecord struct {
	VpcId                            string            `json:"vpc_id"`
	CidrBlock                        string            `json:"cidr_block"`
	State                            string            `json:"state"`
	IsDefault                        bool              `json:"is_default"`
	VNI                              int64             `json:"vni"`
	AZ                               string            `json:"az,omitempty"`
	EnableDnsHostnames               bool              `json:"enable_dns_hostnames"`
	EnableDnsSupport                 bool              `json:"enable_dns_support"`
	EnableNetworkAddressUsageMetrics bool              `json:"enable_network_address_usage_metrics"`
	Tags                             map[string]string `json:"tags"`
	CreatedAt                        time.Time         `json:"created_at"`
}

// SubnetRecord represents a stored Subnet
type SubnetRecord struct {
	SubnetId            string            `json:"subnet_id"`
	VpcId               string            `json:"vpc_id"`
	CidrBlock           string            `json:"cidr_block"`
	AvailabilityZone    string            `json:"availability_zone"`
	State               string            `json:"state"`
	IsDefault           bool              `json:"is_default"`
	MapPublicIpOnLaunch bool              `json:"map_public_ip_on_launch"`
	Tags                map[string]string `json:"tags"`
	CreatedAt           time.Time         `json:"created_at"`
}

// VPCServiceImpl implements VPC, Subnet, and ENI operations with NATS JetStream persistence
type VPCServiceImpl struct {
	config   *config.Config
	natsConn *nats.Conn
	vpcKV    nats.KeyValue
	subnetKV nats.KeyValue
	vniKV    nats.KeyValue
	eniKV    nats.KeyValue
	sgKV     nats.KeyValue
	rtbKV    nats.KeyValue // route table bucket for auto-creating main route table
	ipam     *IPAM

	// Optional: injected after construction for public IP cleanup in DeleteNetworkInterface.
	externalIPAM *ExternalIPAM
	eipKV        nats.KeyValue

	// Optional: injected after construction. Maintains the eni-by-vpc-ip
	// reverse index the IMDS handler reads. Nil-safe — focused tests and
	// IMDS-less deployments leave it unset.
	eniIndex *ENIByIPIndex
}

// SetExternalIPAM injects external IPAM and EIP KV store references so that
// DeleteNetworkInterface can release auto-assigned public IPs and NAT rules.
func (s *VPCServiceImpl) SetExternalIPAM(ipam *ExternalIPAM, eipKV nats.KeyValue) {
	s.externalIPAM = ipam
	s.eipKV = eipKV
}

// SetENIByIPIndex injects the eni-by-vpc-ip reverse-index writer so
// CreateNetworkInterface / DeleteNetworkInterface keep the IMDS source-IP→ENI
// lookup index in sync. Optional: when unset, the index is simply not written.
func (s *VPCServiceImpl) SetENIByIPIndex(index *ENIByIPIndex) {
	s.eniIndex = index
}

// localAZ returns the node's local availability zone, sourced from
// s.config.AZ. Returns "" when no config is wired (mostly test paths),
// which the reconciler treats as a legacy record matching every AZ.
func (s *VPCServiceImpl) localAZ() string {
	if s.config == nil {
		return ""
	}
	return s.config.AZ
}

// NewVPCServiceImplWithNATS creates a VPC service with NATS JetStream for persistence
func NewVPCServiceImplWithNATS(cfg *config.Config, natsConn *nats.Conn) (*VPCServiceImpl, error) {
	js, err := natsConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	vpcKV, err := utils.GetOrCreateKVBucket(js, KVBucketVPCs, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketVPCs, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketVPCs, vpcKV, KVBucketVPCsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketVPCs, err)
	}

	subnetKV, err := utils.GetOrCreateKVBucket(js, KVBucketSubnets, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketSubnets, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketSubnets, subnetKV, KVBucketSubnetsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketSubnets, err)
	}

	vniKV, err := utils.GetOrCreateKVBucket(js, KVBucketVNICounter, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketVNICounter, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketVNICounter, vniKV, KVBucketVNICounterVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketVNICounter, err)
	}

	eniKV, err := utils.GetOrCreateKVBucket(js, KVBucketENIs, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketENIs, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketENIs, eniKV, KVBucketENIsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketENIs, err)
	}

	sgKV, err := utils.GetOrCreateKVBucket(js, KVBucketSecurityGroups, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketSecurityGroups, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketSecurityGroups, sgKV, KVBucketSecurityGroupsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketSecurityGroups, err)
	}

	rtbKV, err := utils.GetOrCreateKVBucket(js, "spinifex-vpc-route-tables", 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket spinifex-vpc-route-tables: %w", err)
	}

	ipam, err := NewIPAM(js)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize IPAM: %w", err)
	}

	slog.Info("VPC service initialized with JetStream KV",
		"vpcBucket", KVBucketVPCs,
		"subnetBucket", KVBucketSubnets,
		"vniBucket", KVBucketVNICounter,
		"eniBucket", KVBucketENIs,
		"sgBucket", KVBucketSecurityGroups)

	return &VPCServiceImpl{
		config:   cfg,
		natsConn: natsConn,
		vpcKV:    vpcKV,
		subnetKV: subnetKV,
		vniKV:    vniKV,
		eniKV:    eniKV,
		sgKV:     sgKV,
		rtbKV:    rtbKV,
		ipam:     ipam,
	}, nil
}

// nextVNI allocates the next VNI using atomic increment on the NATS KV counter
func (s *VPCServiceImpl) nextVNI() (int64, error) {
	entry, err := s.vniKV.Get(vniCounterKey)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			// First VNI allocation — initialize counter
			vni := int64(vniStart)
			data, marshalErr := json.Marshal(vni + 1)
			if marshalErr != nil {
				return 0, fmt.Errorf("failed to marshal VNI counter: %w", marshalErr)
			}
			if _, err := s.vniKV.Create(vniCounterKey, data); err != nil {
				return 0, fmt.Errorf("failed to initialize VNI counter: %w", err)
			}
			return vni, nil
		}
		return 0, fmt.Errorf("failed to get VNI counter: %w", err)
	}

	var current int64
	if err := json.Unmarshal(entry.Value(), &current); err != nil {
		return 0, fmt.Errorf("failed to unmarshal VNI counter: %w", err)
	}

	next := current + 1
	data, marshalErr := json.Marshal(next)
	if marshalErr != nil {
		return 0, fmt.Errorf("failed to marshal VNI counter: %w", marshalErr)
	}
	if _, err := s.vniKV.Update(vniCounterKey, data, entry.Revision()); err != nil {
		return 0, fmt.Errorf("failed to update VNI counter (CAS conflict): %w", err)
	}

	return current, nil
}

// CreateVpc creates a new VPC
func (s *VPCServiceImpl) CreateVpc(input *ec2.CreateVpcInput, accountID string) (*ec2.CreateVpcOutput, error) {
	if input.CidrBlock == nil || *input.CidrBlock == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	// Validate CIDR block
	_, ipNet, err := net.ParseCIDR(*input.CidrBlock)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidVpcRange)
	}

	// AWS allows /16 to /28 for VPC CIDR blocks
	ones, _ := ipNet.Mask.Size()
	if ones < 16 || ones > 28 {
		return nil, errors.New(awserrors.ErrorInvalidVpcRange)
	}

	// Allocate VNI for overlay network
	vni, err := s.nextVNI()
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	vpcID := utils.GenerateResourceID("vpc")

	record := VPCRecord{
		VpcId:              vpcID,
		CidrBlock:          ipNet.String(), // Normalize CIDR
		State:              "available",
		IsDefault:          false,
		VNI:                vni,
		AZ:                 s.localAZ(),
		EnableDnsSupport:   true,  // AWS default
		EnableDnsHostnames: false, // AWS default
		Tags:               utils.ExtractTags(input.TagSpecifications, "vpc"),
		CreatedAt:          time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal VPC record: %w", err)
	}
	if _, err := s.vpcKV.Put(utils.AccountKey(accountID, vpcID), data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateVpc completed", "vpcId", vpcID, "cidrBlock", record.CidrBlock, "vni", vni, "accountID", accountID)

	// Publish vpc.create event for vpcd topology translation
	s.publishVPCEvent("vpc.create", record.VpcId, record.CidrBlock, record.VNI)

	// Auto-create main route table with local route (matches AWS behavior)
	if s.rtbKV != nil {
		if err := s.createMainRouteTable(accountID, vpcID, record.CidrBlock); err != nil {
			slog.Error("Failed to create main route table for VPC", "vpcId", vpcID, "err", err)
		}
	}

	// Provision the per-VPC default SG synchronously. On failure the VPC
	// record persists; user must DeleteVpc and retry.
	if _, err := s.createDefaultSecurityGroupInternal(accountID, vpcID); err != nil {
		slog.Error("Failed to create default security group for VPC", "vpcId", vpcID, "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	return &ec2.CreateVpcOutput{
		Vpc: s.vpcRecordToEC2(&record, accountID),
	}, nil
}

// requireVPCExists returns InvalidVpcID.NotFound if the VPC doesn't exist for
// this account.
func (s *VPCServiceImpl) requireVPCExists(accountID, vpcId string) error {
	if _, err := s.vpcKV.Get(utils.AccountKey(accountID, vpcId)); err != nil {
		return errors.New(awserrors.ErrorInvalidVpcIDNotFound)
	}
	return nil
}

// DeleteVpc deletes a VPC
func (s *VPCServiceImpl) DeleteVpc(input *ec2.DeleteVpcInput, accountID string) (*ec2.DeleteVpcOutput, error) {
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	vpcID := *input.VpcId
	key := utils.AccountKey(accountID, vpcID)

	if _, err := s.vpcKV.Get(key); err != nil {
		// AWS-faithful: an absent VPC is NotFound (the tofu/SDK provider
		// tolerates it on destroy); destroy orchestration tolerates it too.
		// A transient read error stays a server error.
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorInvalidVpcIDNotFound)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Check for dependent subnets owned by this account
	prefix := accountID + "."
	subnetKeys, err := s.subnetKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	for _, k := range subnetKeys {
		if k == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.subnetKV.Get(k)
		if err != nil {
			// ErrKeyNotFound means the subnet was deleted between Keys() and
			// Get() — fine to skip. Any other error is fail-closed: a
			// transient read error must not let DeleteVpc bypass a subnet
			// dependency it can't see.
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			slog.Warn("DeleteVpc: subnet read failed", "key", k, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		var subnet SubnetRecord
		if err := json.Unmarshal(entry.Value(), &subnet); err != nil {
			slog.Warn("DeleteVpc: subnet unmarshal failed", "key", k, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if subnet.VpcId == vpcID {
			return nil, errors.New(awserrors.ErrorDependencyViolation)
		}
	}

	// Reject if any non-default SG remains in this VPC; the cascade only
	// auto-deletes the default SG (matches AWS DeleteVpc semantics).
	defaultSGId := ""
	sgKeys, err := s.sgKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	for _, k := range sgKeys {
		if k == utils.VersionKey || !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.sgKV.Get(k)
		if err != nil {
			// ErrKeyNotFound means the SG was deleted between Keys() and
			// Get() — fine to skip. Any other error is fail-closed: a
			// transient read error must not let DeleteVpc orphan a
			// non-default SG it can't see.
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			slog.Warn("DeleteVpc: SG read failed", "key", k, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		var sg SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &sg); err != nil {
			slog.Warn("DeleteVpc: SG unmarshal failed", "key", k, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if sg.VpcId != vpcID {
			continue
		}
		if !sg.IsDefault {
			return nil, errors.New(awserrors.ErrorDependencyViolation)
		}
		defaultSGId = sg.GroupId
	}

	// Cascade-delete the default SG before removing the VPC record so a vpcd
	// failure surfaces to the caller and leaves both records intact for retry.
	if defaultSGId != "" {
		if err := s.deleteSecurityGroupInternal(accountID, defaultSGId); err != nil {
			slog.Error("DeleteVpc: cascade-delete of default SG failed", "vpcId", vpcID, "groupId", defaultSGId, "err", err)
			return nil, err
		}
	}

	if err := s.vpcKV.Delete(key); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("DeleteVpc completed", "vpcId", vpcID, "accountID", accountID)

	// Publish vpc.delete event for vpcd topology cleanup
	s.publishVPCEvent("vpc.delete", vpcID, "", 0)

	return &ec2.DeleteVpcOutput{}, nil
}

// describeVpcsValidFilters defines the set of filter names accepted by DescribeVpcs.
var describeVpcsValidFilters = map[string]bool{
	"vpc-id":     true,
	"state":      true,
	"cidr-block": true,
	"is-default": true,
	"owner-id":   true,
}

// DescribeVpcs describes VPCs
func (s *VPCServiceImpl) DescribeVpcs(input *ec2.DescribeVpcsInput, accountID string) (*ec2.DescribeVpcsOutput, error) {
	var vpcs []*ec2.Vpc

	vpcIDs := make(map[string]bool)
	for _, id := range input.VpcIds {
		if id != nil {
			vpcIDs[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeVpcsValidFilters)
	if err != nil {
		slog.Warn("DescribeVpcs: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	prefix := accountID + "."
	keys, err := s.vpcKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.vpcKV.Get(key)
		if err != nil {
			slog.Warn("Failed to get VPC record", "key", key, "error", err)
			continue
		}

		var record VPCRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Warn("Failed to unmarshal VPC record", "key", key, "error", err)
			continue
		}

		if len(vpcIDs) > 0 && !vpcIDs[record.VpcId] {
			continue
		}

		if len(parsedFilters) > 0 && !vpcMatchesFilters(&record, accountID, parsedFilters) {
			continue
		}

		vpcs = append(vpcs, s.vpcRecordToEC2(&record, accountID))
	}

	// If specific VPC IDs were requested but not found, return error
	if len(vpcIDs) > 0 {
		found := make(map[string]bool)
		for _, vpc := range vpcs {
			if vpc.VpcId != nil {
				found[*vpc.VpcId] = true
			}
		}
		for id := range vpcIDs {
			if !found[id] {
				return nil, errors.New(awserrors.ErrorInvalidVpcIDNotFound)
			}
		}
	}

	slog.Info("DescribeVpcs completed", "count", len(vpcs), "accountID", accountID)

	return &ec2.DescribeVpcsOutput{
		Vpcs: vpcs,
	}, nil
}

// CreateSubnet creates a new subnet within a VPC
func (s *VPCServiceImpl) CreateSubnet(input *ec2.CreateSubnetInput, accountID string) (*ec2.CreateSubnetOutput, error) {
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.CidrBlock == nil || *input.CidrBlock == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	vpcID := *input.VpcId

	// Verify VPC exists and belongs to this account
	vpcEntry, err := s.vpcKV.Get(utils.AccountKey(accountID, vpcID))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidVpcIDNotFound)
	}

	var vpcRecord VPCRecord
	if err := json.Unmarshal(vpcEntry.Value(), &vpcRecord); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	// Validate subnet CIDR
	_, subnetNet, err := net.ParseCIDR(*input.CidrBlock)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidSubnetRange)
	}

	// AWS allows /16 to /28 for subnet CIDR blocks
	ones, _ := subnetNet.Mask.Size()
	if ones < 16 || ones > 28 {
		return nil, errors.New(awserrors.ErrorInvalidSubnetRange)
	}

	// Verify subnet CIDR is within VPC CIDR
	_, vpcNet, err := net.ParseCIDR(vpcRecord.CidrBlock)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if !vpcNet.Contains(subnetNet.IP) {
		return nil, errors.New(awserrors.ErrorInvalidSubnetRange)
	}

	// Check for CIDR conflicts with existing subnets in this VPC (same account)
	prefix := accountID + "."
	subnetKeys, err := s.subnetKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, k := range subnetKeys {
		if k == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.subnetKV.Get(k)
		if err != nil {
			continue
		}
		var existing SubnetRecord
		if err := json.Unmarshal(entry.Value(), &existing); err != nil {
			continue
		}
		if existing.VpcId != vpcID {
			continue
		}
		_, existingNet, err := net.ParseCIDR(existing.CidrBlock)
		if err != nil {
			continue
		}
		if existingNet.Contains(subnetNet.IP) || subnetNet.Contains(existingNet.IP) {
			return nil, errors.New(awserrors.ErrorInvalidSubnetConflict)
		}
	}

	// Determine AZ
	az := ""
	if input.AvailabilityZone != nil {
		az = *input.AvailabilityZone
	} else if s.config != nil {
		az = s.config.AZ
	}

	subnetID := utils.GenerateResourceID("subnet")

	// Calculate available IPs (total hosts minus AWS reserved: network, router, DNS, future, broadcast)
	// ones is validated to be 16-28 above, so (32-ones) is always 4-16 and safe for uint conversion
	totalHosts := max((1<<(32-ones))-5, 0) //#nosec G115 - ones validated 16-28

	record := SubnetRecord{
		SubnetId:         subnetID,
		VpcId:            vpcID,
		CidrBlock:        subnetNet.String(),
		AvailabilityZone: az,
		State:            "available",
		IsDefault:        false,
		Tags:             utils.ExtractTags(input.TagSpecifications, "subnet"),
		CreatedAt:        time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal subnet record: %w", err)
	}
	if _, err := s.subnetKV.Put(utils.AccountKey(accountID, subnetID), data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateSubnet completed", "subnetId", subnetID, "vpcId", vpcID, "cidrBlock", record.CidrBlock, "accountID", accountID)

	// Publish vpc.create-subnet event for vpcd topology translation
	s.publishSubnetEvent("vpc.create-subnet", record.SubnetId, record.VpcId, record.CidrBlock)

	return &ec2.CreateSubnetOutput{
		Subnet: s.subnetRecordToEC2(&record, totalHosts, accountID),
	}, nil
}

// DeleteSubnet deletes a subnet
func (s *VPCServiceImpl) DeleteSubnet(input *ec2.DeleteSubnetInput, accountID string) (*ec2.DeleteSubnetOutput, error) {
	if input.SubnetId == nil || *input.SubnetId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	subnetID := *input.SubnetId
	key := utils.AccountKey(accountID, subnetID)

	// Read subnet record before deletion (needed for vpcd event)
	subnetEntry, err := s.subnetKV.Get(key)
	if err != nil {
		// AWS-faithful: an absent subnet is NotFound (provider tolerates it on
		// destroy); destroy orchestration tolerates it too. A transient read
		// error stays a server error.
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorInvalidSubnetIDNotFound)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	var subnetRecord SubnetRecord
	_ = json.Unmarshal(subnetEntry.Value(), &subnetRecord)

	// Block while a live ENI attachment (hence a resident instance) remains in
	// the subnet (rule #3). Orphan/available ENIs do not pin it: tofu deletes
	// them first and the GC backstop reaps leftovers.
	if err := s.checkSubnetResidents(accountID, subnetID); err != nil {
		return nil, err
	}

	if err := s.subnetKV.Delete(key); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("DeleteSubnet completed", "subnetId", subnetID, "accountID", accountID)

	// Publish vpc.delete-subnet event for vpcd topology cleanup
	s.publishSubnetEvent("vpc.delete-subnet", subnetID, subnetRecord.VpcId, subnetRecord.CidrBlock)

	return &ec2.DeleteSubnetOutput{}, nil
}

// checkSubnetResidents returns DependencyViolation if any ENI residing in the
// subnet is a live attachment (rule #3). Fail-closed on a KV read error so a
// transient fault never lets a delete orphan a port.
func (s *VPCServiceImpl) checkSubnetResidents(accountID, subnetID string) error {
	keys, err := s.eniKV.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		slog.Error("DeleteSubnet: ENI scan failed, blocking delete to avoid orphaning ports", "subnetId", subnetID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	prefix := accountID + "."
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.eniKV.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			return errors.New(awserrors.ErrorServerInternal)
		}
		var record ENIRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			continue
		}
		if record.SubnetId == subnetID && eniIsLiveAttachment(&record) {
			return errors.New(awserrors.ErrorDependencyViolation)
		}
	}
	return nil
}

// DescribeSubnets describes subnets
func (s *VPCServiceImpl) DescribeSubnets(input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error) {
	var subnets []*ec2.Subnet

	subnetIDs := make(map[string]bool)
	for _, id := range input.SubnetIds {
		if id != nil {
			subnetIDs[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeSubnetsValidFilters)
	if err != nil {
		slog.Warn("DescribeSubnets: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	prefix := accountID + "."
	keys, err := s.subnetKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.subnetKV.Get(key)
		if err != nil {
			slog.Warn("Failed to get subnet record", "key", key, "error", err)
			continue
		}

		var record SubnetRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Warn("Failed to unmarshal subnet record", "key", key, "error", err)
			continue
		}

		if len(subnetIDs) > 0 && !subnetIDs[record.SubnetId] {
			continue
		}

		if len(parsedFilters) > 0 && !subnetMatchesFilters(&record, parsedFilters) {
			continue
		}

		// Calculate available IPs
		_, subnetNet, err := net.ParseCIDR(record.CidrBlock)
		availableIPs := 0
		if err == nil {
			ones, _ := subnetNet.Mask.Size()
			availableIPs = max((1<<(32-ones))-5, 0) //#nosec G115 - ones from validated CIDR
		}

		subnets = append(subnets, s.subnetRecordToEC2(&record, availableIPs, accountID))
	}

	// If specific subnet IDs were requested but not found, return error
	if len(subnetIDs) > 0 {
		found := make(map[string]bool)
		for _, subnet := range subnets {
			if subnet.SubnetId != nil {
				found[*subnet.SubnetId] = true
			}
		}
		for id := range subnetIDs {
			if !found[id] {
				return nil, errors.New(awserrors.ErrorInvalidSubnetIDNotFound)
			}
		}
	}

	slog.Info("DescribeSubnets completed", "count", len(subnets), "accountID", accountID)

	return &ec2.DescribeSubnetsOutput{
		Subnets: subnets,
	}, nil
}

// vpcMatchesFilters checks whether a VPCRecord satisfies all parsed filters.
func vpcMatchesFilters(record *VPCRecord, accountID string, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		var field string
		switch name {
		case "vpc-id":
			field = record.VpcId
		case "state":
			field = record.State
		case "cidr-block":
			field = record.CidrBlock
		case "is-default":
			field = strconv.FormatBool(record.IsDefault)
		case "owner-id":
			field = accountID
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	return filterutil.MatchesTags(filters, record.Tags)
}

// describeSubnetsValidFilters defines the set of filter names accepted by DescribeSubnets.
var describeSubnetsValidFilters = map[string]bool{
	"subnet-id":         true,
	"vpc-id":            true,
	"availability-zone": true,
	"cidr-block":        true,
	"state":             true,
	"default-for-az":    true,
}

// subnetMatchesFilters checks whether a SubnetRecord satisfies all parsed filters.
func subnetMatchesFilters(record *SubnetRecord, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		var field string
		switch name {
		case "subnet-id":
			field = record.SubnetId
		case "vpc-id":
			field = record.VpcId
		case "availability-zone":
			field = record.AvailabilityZone
		case "cidr-block":
			field = record.CidrBlock
		case "state":
			field = record.State
		case "default-for-az":
			field = strconv.FormatBool(record.IsDefault)
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	return filterutil.MatchesTags(filters, record.Tags)
}

func (s *VPCServiceImpl) vpcRecordToEC2(record *VPCRecord, accountID string) *ec2.Vpc {
	vpc := &ec2.Vpc{
		VpcId:     aws.String(record.VpcId),
		CidrBlock: aws.String(record.CidrBlock),
		State:     aws.String(record.State),
		IsDefault: aws.Bool(record.IsDefault),
		OwnerId:   aws.String(accountID),
		CidrBlockAssociationSet: []*ec2.VpcCidrBlockAssociation{
			{
				CidrBlock: aws.String(record.CidrBlock),
				CidrBlockState: &ec2.VpcCidrBlockState{
					State: aws.String("associated"),
				},
				AssociationId: aws.String(fmt.Sprintf("vpc-cidr-assoc-%s", record.VpcId[4:])),
			},
		},
		DhcpOptionsId:   aws.String("dopt-default"),
		InstanceTenancy: aws.String("default"),
	}

	vpc.Tags = utils.MapToEC2Tags(record.Tags)

	return vpc
}

func (s *VPCServiceImpl) subnetRecordToEC2(record *SubnetRecord, availableIPs int, accountID string) *ec2.Subnet {
	subnet := &ec2.Subnet{
		SubnetId:                aws.String(record.SubnetId),
		VpcId:                   aws.String(record.VpcId),
		CidrBlock:               aws.String(record.CidrBlock),
		AvailabilityZone:        aws.String(record.AvailabilityZone),
		State:                   aws.String(record.State),
		DefaultForAz:            aws.Bool(record.IsDefault),
		AvailableIpAddressCount: aws.Int64(int64(availableIPs)),
		OwnerId:                 aws.String(accountID),
		MapPublicIpOnLaunch:     aws.Bool(record.MapPublicIpOnLaunch),
	}

	subnet.Tags = utils.MapToEC2Tags(record.Tags)

	return subnet
}

// ModifySubnetAttribute modifies a subnet's attributes (e.g. MapPublicIpOnLaunch).
func (s *VPCServiceImpl) ModifySubnetAttribute(input *ec2.ModifySubnetAttributeInput, accountID string) (*ec2.ModifySubnetAttributeOutput, error) {
	if input.SubnetId == nil || *input.SubnetId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	subnetID := *input.SubnetId
	key := utils.AccountKey(accountID, subnetID)

	entry, err := s.subnetKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidSubnetIDNotFound)
	}

	var record SubnetRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if input.MapPublicIpOnLaunch != nil && input.MapPublicIpOnLaunch.Value != nil {
		record.MapPublicIpOnLaunch = *input.MapPublicIpOnLaunch.Value
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal subnet record: %w", err)
	}
	if _, err := s.subnetKV.Update(key, data, entry.Revision()); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("ModifySubnetAttribute completed", "subnetId", subnetID, "mapPublicIpOnLaunch", record.MapPublicIpOnLaunch, "accountID", accountID)

	return &ec2.ModifySubnetAttributeOutput{}, nil
}

// ModifyVpcAttribute modifies a VPC's DNS attributes.
func (s *VPCServiceImpl) ModifyVpcAttribute(input *ec2.ModifyVpcAttributeInput, accountID string) (*ec2.ModifyVpcAttributeOutput, error) {
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.EnableDnsHostnames == nil && input.EnableDnsSupport == nil && input.EnableNetworkAddressUsageMetrics == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	vpcID := *input.VpcId
	key := utils.AccountKey(accountID, vpcID)

	entry, err := s.vpcKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidVpcIDNotFound)
	}

	var record VPCRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		slog.Error("ModifyVpcAttribute: corrupted VPC record", "vpcId", vpcID, "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if input.EnableDnsHostnames != nil && input.EnableDnsHostnames.Value != nil {
		record.EnableDnsHostnames = *input.EnableDnsHostnames.Value
	}
	if input.EnableDnsSupport != nil && input.EnableDnsSupport.Value != nil {
		record.EnableDnsSupport = *input.EnableDnsSupport.Value
	}
	if input.EnableNetworkAddressUsageMetrics != nil && input.EnableNetworkAddressUsageMetrics.Value != nil {
		record.EnableNetworkAddressUsageMetrics = *input.EnableNetworkAddressUsageMetrics.Value
	}

	data, err := json.Marshal(record)
	if err != nil {
		slog.Error("ModifyVpcAttribute: failed to marshal VPC record", "vpcId", vpcID, "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.vpcKV.Update(key, data, entry.Revision()); err != nil {
		slog.Error("ModifyVpcAttribute: KV update failed", "vpcId", vpcID, "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("ModifyVpcAttribute completed", "vpcId", vpcID, "accountID", accountID)

	return &ec2.ModifyVpcAttributeOutput{}, nil
}

// DescribeVpcAttribute returns a single VPC attribute per call (AWS behavior).
func (s *VPCServiceImpl) DescribeVpcAttribute(input *ec2.DescribeVpcAttributeInput, accountID string) (*ec2.DescribeVpcAttributeOutput, error) {
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.Attribute == nil || *input.Attribute == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	vpcID := *input.VpcId
	key := utils.AccountKey(accountID, vpcID)

	entry, err := s.vpcKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidVpcIDNotFound)
	}

	var record VPCRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		slog.Error("DescribeVpcAttribute: corrupted VPC record", "vpcId", vpcID, "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	output := &ec2.DescribeVpcAttributeOutput{
		VpcId: aws.String(vpcID),
	}

	switch *input.Attribute {
	case ec2.VpcAttributeNameEnableDnsHostnames:
		output.EnableDnsHostnames = &ec2.AttributeBooleanValue{Value: aws.Bool(record.EnableDnsHostnames)}
	case ec2.VpcAttributeNameEnableDnsSupport:
		output.EnableDnsSupport = &ec2.AttributeBooleanValue{Value: aws.Bool(record.EnableDnsSupport)}
	case ec2.VpcAttributeNameEnableNetworkAddressUsageMetrics:
		output.EnableNetworkAddressUsageMetrics = &ec2.AttributeBooleanValue{Value: aws.Bool(record.EnableNetworkAddressUsageMetrics)}
	default:
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return output, nil
}

// Default VPC constants matching AWS defaults.
const (
	DefaultVPCCidr    = "172.31.0.0/16"
	DefaultSubnetCidr = "172.31.0.0/20"
)

// DefaultVPCInfo holds the IDs of the default VPC and subnet for bootstrap config.
type DefaultVPCInfo struct {
	VpcId      string
	SubnetId   string
	Cidr       string
	SubnetCidr string
}

// BootstrapIDs holds pre-generated resource IDs from the [bootstrap] config.
// When provided, EnsureDefaultVPC uses these IDs instead of generating random ones,
// ensuring consistency between admin init, daemon, and vpcd.
type BootstrapIDs struct {
	VpcId    string
	SubnetId string
}

// EnsureDefaultVPC creates a default VPC and subnet if none exists for the
// account. Safe to call multiple times — no-ops if already present.
func (s *VPCServiceImpl) EnsureDefaultVPC(accountID string, bootstrap ...BootstrapIDs) (*DefaultVPCInfo, error) {
	if s.vpcKV == nil {
		return nil, nil // No persistence, skip
	}

	// Check if a default VPC already exists for this account
	prefix := accountID + "."
	keys, err := s.vpcKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, fmt.Errorf("list VPCs: %w", err)
	}

	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.vpcKV.Get(key)
		if err != nil {
			continue
		}
		var record VPCRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			continue
		}
		if record.IsDefault {
			slog.Debug("Default VPC already exists", "vpcId", record.VpcId, "accountID", accountID)
			// Look up the default subnet to return full info
			defaultSubnet, _ := s.GetDefaultSubnet(accountID)
			info := &DefaultVPCInfo{VpcId: record.VpcId, Cidr: record.CidrBlock}
			if defaultSubnet != nil {
				info.SubnetId = defaultSubnet.SubnetId
				info.SubnetCidr = defaultSubnet.CidrBlock
			}
			return info, nil
		}
	}

	// Create default VPC — use bootstrap IDs if provided for consistency
	vni, err := s.nextVNI()
	if err != nil {
		return nil, fmt.Errorf("allocate VNI for default VPC: %w", err)
	}

	vpcID := utils.GenerateResourceID("vpc")
	if len(bootstrap) > 0 && bootstrap[0].VpcId != "" {
		vpcID = bootstrap[0].VpcId
	}
	vpcRecord := VPCRecord{
		VpcId:              vpcID,
		CidrBlock:          DefaultVPCCidr,
		State:              "available",
		IsDefault:          true,
		VNI:                vni,
		AZ:                 s.localAZ(),
		EnableDnsSupport:   true, // AWS default
		EnableDnsHostnames: true, // AWS default for default VPC
		Tags:               map[string]string{"Name": "default"},
		CreatedAt:          time.Now(),
	}

	data, err := json.Marshal(vpcRecord)
	if err != nil {
		return nil, fmt.Errorf("marshal default VPC: %w", err)
	}
	if _, err := s.vpcKV.Put(utils.AccountKey(accountID, vpcID), data); err != nil {
		return nil, fmt.Errorf("store default VPC: %w", err)
	}

	s.publishVPCEvent("vpc.create", vpcID, DefaultVPCCidr, vni)

	// Determine AZ
	az := "us-east-1a"
	if s.config != nil && s.config.AZ != "" {
		az = s.config.AZ
	}

	// Create default subnet (public — matches AWS default VPC behavior)
	subnetID := utils.GenerateResourceID("subnet")
	if len(bootstrap) > 0 && bootstrap[0].SubnetId != "" {
		subnetID = bootstrap[0].SubnetId
	}
	subnetRecord := SubnetRecord{
		SubnetId:            subnetID,
		VpcId:               vpcID,
		CidrBlock:           DefaultSubnetCidr,
		AvailabilityZone:    az,
		State:               "available",
		IsDefault:           true,
		MapPublicIpOnLaunch: true, // AWS default subnets auto-assign public IPs
		Tags:                map[string]string{"Name": "default"},
		CreatedAt:           time.Now(),
	}

	data, err = json.Marshal(subnetRecord)
	if err != nil {
		return nil, fmt.Errorf("marshal default subnet: %w", err)
	}
	if _, err := s.subnetKV.Put(utils.AccountKey(accountID, subnetID), data); err != nil {
		return nil, fmt.Errorf("store default subnet: %w", err)
	}

	s.publishSubnetEvent("vpc.create-subnet", subnetID, vpcID, DefaultSubnetCidr)

	// Create main route table with local route (written directly to KV to avoid circular import)
	if s.rtbKV != nil {
		if err := s.createMainRouteTable(accountID, vpcID, DefaultVPCCidr); err != nil {
			slog.Error("Failed to create main route table for VPC", "vpcId", vpcID, "err", err)
		}
	}

	// Best-effort default SG provisioning. Bootstrap runs during daemon Start()
	// before vpcd has subscribed to vpc.create-sg, so the synchronous round-trip
	// will time out on first boot. The SG record is already in KV; vpcd's
	// reconcile-sgs loop creates the OVN port group on its first scan.
	if _, err := s.createDefaultSecurityGroupInternal(accountID, vpcID); err != nil {
		slog.Warn("Default security group bootstrap deferred to vpcd reconciler",
			"vpcId", vpcID, "accountID", accountID, "err", err)
	}

	slog.Info("Created default VPC and subnet",
		"vpcId", vpcID,
		"vpcCidr", DefaultVPCCidr,
		"subnetId", subnetID,
		"subnetCidr", DefaultSubnetCidr,
		"az", az,
		"accountID", accountID,
	)
	return &DefaultVPCInfo{
		VpcId:      vpcID,
		SubnetId:   subnetID,
		Cidr:       DefaultVPCCidr,
		SubnetCidr: DefaultSubnetCidr,
	}, nil
}

// createMainRouteTable writes a main route table record directly to the route
// table KV bucket. Avoids a circular import; idempotent via findMainRouteTableID
// so concurrent EnsureDefaultVPC calls do not mint duplicate main RTs.
func (s *VPCServiceImpl) createMainRouteTable(accountID, vpcID, vpcCidr string) error {
	if existing, err := s.findMainRouteTableID(accountID, vpcID); err != nil {
		return fmt.Errorf("check existing main route table: %w", err)
	} else if existing != "" {
		slog.Info("Main route table already exists for VPC, skipping creation",
			"routeTableId", existing, "vpcId", vpcID, "accountID", accountID)
		return nil
	}
	type rtbRecord struct {
		RouteTableId string `json:"route_table_id"`
		VpcId        string `json:"vpc_id"`
		AccountID    string `json:"account_id"`
		IsMain       bool   `json:"is_main"`
		Routes       []struct {
			DestinationCidrBlock string `json:"destination_cidr_block"`
			GatewayId            string `json:"gateway_id,omitempty"`
			State                string `json:"state"`
			Origin               string `json:"origin"`
		} `json:"routes"`
		Associations []struct {
			AssociationId string `json:"association_id"`
			Main          bool   `json:"main"`
		} `json:"associations"`
		Tags      map[string]string `json:"tags"`
		CreatedAt time.Time         `json:"created_at"`
	}

	rtbID := utils.GenerateResourceID("rtb")
	record := rtbRecord{
		RouteTableId: rtbID,
		VpcId:        vpcID,
		AccountID:    accountID,
		IsMain:       true,
		Routes: []struct {
			DestinationCidrBlock string `json:"destination_cidr_block"`
			GatewayId            string `json:"gateway_id,omitempty"`
			State                string `json:"state"`
			Origin               string `json:"origin"`
		}{
			{DestinationCidrBlock: vpcCidr, GatewayId: "local", State: "active", Origin: "CreateRouteTable"},
		},
		Associations: []struct {
			AssociationId string `json:"association_id"`
			Main          bool   `json:"main"`
		}{
			{AssociationId: utils.GenerateResourceID("rtbassoc"), Main: true},
		},
		Tags:      make(map[string]string),
		CreatedAt: time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal main route table: %w", err)
	}
	if _, err := s.rtbKV.Put(utils.AccountKey(accountID, rtbID), data); err != nil {
		return fmt.Errorf("store main route table: %w", err)
	}

	slog.Info("Created main route table for VPC", "routeTableId", rtbID, "vpcId", vpcID, "accountID", accountID)
	return nil
}

// findMainRouteTableID returns the rtb-ID of the main route table for vpcID
// in accountID, or "" if none exists. Partial unmarshal — only the fields
// needed to identify a main RT, so this stays cheap on hot KV scans.
func (s *VPCServiceImpl) findMainRouteTableID(accountID, vpcID string) (string, error) {
	prefix := accountID + "."
	keys, err := s.rtbKV.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return "", nil
		}
		return "", fmt.Errorf("list rtb keys: %w", err)
	}
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.rtbKV.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			return "", fmt.Errorf("read rtb %s: %w", key, err)
		}
		var rt struct {
			RouteTableId string `json:"route_table_id"`
			VpcId        string `json:"vpc_id"`
			IsMain       bool   `json:"is_main"`
		}
		if err := json.Unmarshal(entry.Value(), &rt); err != nil {
			continue
		}
		if rt.VpcId == vpcID && rt.IsMain {
			return rt.RouteTableId, nil
		}
	}
	return "", nil
}

// GetDefaultSubnet returns the default subnet for RunInstances when no SubnetId is specified.
func (s *VPCServiceImpl) GetDefaultSubnet(accountID string) (*SubnetRecord, error) {
	prefix := accountID + "."
	keys, err := s.subnetKV.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, fmt.Errorf("no default subnet found")
		}
		return nil, fmt.Errorf("list subnets: %w", err)
	}

	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.subnetKV.Get(key)
		if err != nil {
			continue
		}
		var record SubnetRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			continue
		}
		if record.IsDefault {
			return &record, nil
		}
	}

	return nil, fmt.Errorf("no default subnet found")
}

// GetSubnet looks up a SubnetRecord by ID.
func (s *VPCServiceImpl) GetSubnet(accountID, subnetId string) (*SubnetRecord, error) {
	key := utils.AccountKey(accountID, subnetId)
	entry, err := s.subnetKV.Get(key)
	if err != nil {
		return nil, fmt.Errorf("subnet %s not found: %w", subnetId, err)
	}
	var record SubnetRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, fmt.Errorf("unmarshal subnet record: %w", err)
	}
	return &record, nil
}

// publishVPCEvent publishes a VPC lifecycle event to NATS for vpcd consumption.
// This is fire-and-forget; errors are logged but do not fail the API response.
func (s *VPCServiceImpl) publishVPCEvent(topic, vpcId, cidrBlock string, vni int64) {
	utils.PublishEvent(s.natsConn, topic, struct {
		VpcId     string `json:"vpc_id"`
		CidrBlock string `json:"cidr_block"`
		VNI       int64  `json:"vni"`
	}{VpcId: vpcId, CidrBlock: cidrBlock, VNI: vni})
}

// publishSubnetEvent publishes a subnet lifecycle event to NATS for vpcd consumption.
func (s *VPCServiceImpl) publishSubnetEvent(topic, subnetId, vpcId, cidrBlock string) {
	utils.PublishEvent(s.natsConn, topic, struct {
		SubnetId  string `json:"subnet_id"`
		VpcId     string `json:"vpc_id"`
		CidrBlock string `json:"cidr_block"`
	}{SubnetId: subnetId, VpcId: vpcId, CidrBlock: cidrBlock})
}
