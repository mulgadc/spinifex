package handlers_ec2_natgw

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

var _ NatGatewayService = (*NatGatewayServiceImpl)(nil)

// NatGatewayServiceImpl implements NAT Gateway operations with NATS JetStream persistence
type NatGatewayServiceImpl struct {
	natgwKV        nats.KeyValue
	deletedNatgwKV nats.KeyValue
	eipKV          nats.KeyValue
	subnetKV       nats.KeyValue
	vpcKV          nats.KeyValue
	natsConn       *nats.Conn
}

// NewNatGatewayServiceImplWithNATS creates a NAT Gateway service
func NewNatGatewayServiceImplWithNATS(natsConn *nats.Conn) (*NatGatewayServiceImpl, error) {
	js, err := natsConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	natgwKV, err := utils.GetOrCreateKVBucket(js, KVBucketNatGateways, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketNatGateways, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketNatGateways, natgwKV, KVBucketNatGatewaysVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketNatGateways, err)
	}

	// Deleted NAT Gateways bucket with 1-hour TTL — keys auto-expire.
	// Terraform polls DescribeNatGateways after delete and expects state=deleted.
	deletedKV, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:      KVBucketDeletedNatGateways,
		Description: "Deleted NAT Gateways (auto-expire after 1 hour)",
		History:     1,
		TTL:         1 * time.Hour,
	})
	if err != nil {
		deletedKV, err = js.KeyValue(KVBucketDeletedNatGateways)
		if err != nil {
			return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketDeletedNatGateways, err)
		}
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketDeletedNatGateways, deletedKV, KVBucketDeletedNatGatewaysVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketDeletedNatGateways, err)
	}

	eipKV, err := utils.GetOrCreateKVBucket(js, handlers_ec2_eip.KVBucketEIPs, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to get EIP KV bucket: %w", err)
	}

	subnetKV, err := utils.GetOrCreateKVBucket(js, handlers_ec2_vpc.KVBucketSubnets, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to get subnet KV bucket: %w", err)
	}

	vpcKV, err := utils.GetOrCreateKVBucket(js, handlers_ec2_vpc.KVBucketVPCs, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to get VPC KV bucket: %w", err)
	}

	slog.Info("NatGateway service initialized with JetStream KV", "bucket", KVBucketNatGateways)

	return &NatGatewayServiceImpl{
		natgwKV:        natgwKV,
		deletedNatgwKV: deletedKV,
		eipKV:          eipKV,
		subnetKV:       subnetKV,
		vpcKV:          vpcKV,
		natsConn:       natsConn,
	}, nil
}

// natGatewayEvent is published on vpc.add-nat-gateway / vpc.delete-nat-gateway topics.
type natGatewayEvent struct {
	VpcId        string `json:"vpc_id"`
	NatGatewayId string `json:"nat_gateway_id"`
	PublicIp     string `json:"public_ip"`
	SubnetCidr   string `json:"subnet_cidr"` // private subnet CIDR for SNAT rule
}

// CreateNatGateway creates a NAT Gateway in a public subnet with an EIP
func (s *NatGatewayServiceImpl) CreateNatGateway(input *ec2.CreateNatGatewayInput, accountID string) (*ec2.CreateNatGatewayOutput, error) {
	if input.SubnetId == nil || *input.SubnetId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.AllocationId == nil || *input.AllocationId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	subnetID := *input.SubnetId
	allocID := *input.AllocationId

	// Validate subnet exists and get its VPC
	subnetEntry, err := s.subnetKV.Get(utils.AccountKey(accountID, subnetID))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidSubnetIDNotFound)
	}
	var subnetRecord handlers_ec2_vpc.SubnetRecord
	if err := json.Unmarshal(subnetEntry.Value(), &subnetRecord); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Validate EIP exists and is not already associated
	eipEntry, err := s.eipKV.Get(utils.AccountKey(accountID, allocID))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidAllocationIDNotFound)
	}
	var eipRecord handlers_ec2_eip.EIPRecord
	if err := json.Unmarshal(eipEntry.Value(), &eipRecord); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if eipRecord.AssociationId != "" {
		return nil, errors.New(awserrors.ErrorResourceAlreadyAssociated)
	}

	natgwID := utils.GenerateResourceID("nat")
	record := NatGatewayRecord{
		NatGatewayId: natgwID,
		VpcId:        subnetRecord.VpcId,
		SubnetId:     subnetID,
		AllocationId: allocID,
		PublicIp:     eipRecord.PublicIp,
		State:        "available",
		AccountID:    accountID,
		Tags:         make(map[string]string),
		CreatedAt:    time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.natgwKV.Put(utils.AccountKey(accountID, natgwID), data); err != nil {
		slog.Error("Failed to store NAT Gateway", "natGatewayId", natgwID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Mark the EIP associated to this NAT gateway (AWS parity + stops a second
	// NAT GW reusing the same allocation). Linkage is the shared AllocationId;
	// DeleteNatGateway clears it back to allocated. Non-fatal: the NAT GW is
	// already stored, so a failed mark only weakens the reuse guard until then.
	eipRecord.AssociationId = utils.GenerateResourceID("eipassoc")
	eipRecord.VpcId = subnetRecord.VpcId
	eipRecord.State = "associated"
	if eipData, err := json.Marshal(eipRecord); err == nil {
		if _, err := s.eipKV.Update(utils.AccountKey(accountID, allocID), eipData, eipEntry.Revision()); err != nil {
			slog.Warn("CreateNatGateway: failed to mark EIP associated", "natGatewayId", natgwID, "allocationId", allocID, "err", err)
		}
	}

	slog.Info("CreateNatGateway completed", "natGatewayId", natgwID, "subnetId", subnetID,
		"allocationId", allocID, "publicIp", eipRecord.PublicIp, "vpcId", subnetRecord.VpcId, "accountID", accountID)

	return &ec2.CreateNatGatewayOutput{
		NatGateway: recordToEC2(&record),
	}, nil
}

// DeleteNatGateway deletes a NAT Gateway and removes OVN SNAT rules
func (s *NatGatewayServiceImpl) DeleteNatGateway(input *ec2.DeleteNatGatewayInput, accountID string) (*ec2.DeleteNatGatewayOutput, error) {
	if input.NatGatewayId == nil || *input.NatGatewayId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	natgwID := *input.NatGatewayId
	key := utils.AccountKey(accountID, natgwID)

	entry, err := s.natgwKV.Get(key)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			// Idempotent delete: an absent NAT gateway is success so destroy
			// retries converge.
			return &ec2.DeleteNatGatewayOutput{NatGatewayId: aws.String(natgwID)}, nil
		}
		slog.Error("Failed to read NAT Gateway", "natGatewayId", natgwID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var record NatGatewayRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Block while any live route table forwards to this NAT GW (rule #3). The
	// route-delete path tears down the SNAT rule, so a NAT GW only deletes once
	// nothing routes to it.
	if err := s.checkNatGatewayRouteRefs(&record, accountID); err != nil {
		return nil, err
	}

	// Move to deleted bucket (auto-expires via TTL) so DescribeNatGateways can
	// return state=deleted while Terraform polls after deletion.
	record.State = "deleted"
	deleted, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.deletedNatgwKV.Put(key, deleted); err != nil {
		slog.Error("Failed to write deleted NAT Gateway", "natGatewayId", natgwID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Remove from active bucket
	if err := s.natgwKV.Delete(key); err != nil {
		slog.Error("Failed to delete NAT Gateway from active bucket", "natGatewayId", natgwID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Disassociate the EIP, keeping the allocation (AWS parity).
	s.disassociateEIP(&record, accountID)

	slog.Info("DeleteNatGateway completed", "natGatewayId", natgwID, "accountID", accountID)

	return &ec2.DeleteNatGatewayOutput{
		NatGatewayId: aws.String(natgwID),
	}, nil
}

// kvBucketRouteTables is the route-table KV bucket. Duplicated as a literal
// (not imported from the routetable package) because routetable imports this
// package for PublishAddEvent — importing it back would be a cycle.
const kvBucketRouteTables = "spinifex-vpc-route-tables"

// checkNatGatewayRouteRefs returns DependencyViolation if any route table in
// the NAT gateway's VPC has a live route forwarding to it (rule #3).
// Fail-closed on a route-table read error so a transient fault never lets a
// delete blackhole live routes.
func (s *NatGatewayServiceImpl) checkNatGatewayRouteRefs(record *NatGatewayRecord, accountID string) error {
	if s.natsConn == nil {
		return nil
	}
	rtbKV, err := utils.GetOrCreateKVBucket(mustJS(s.natsConn), kvBucketRouteTables, 10)
	if err != nil {
		slog.Error("DeleteNatGateway: route-table scan failed, blocking delete", "natGatewayId", record.NatGatewayId, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	keys, err := rtbKV.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return errors.New(awserrors.ErrorServerInternal)
	}
	prefix := accountID + "."
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := rtbKV.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			return errors.New(awserrors.ErrorServerInternal)
		}
		var rtbData struct {
			VpcId  string `json:"vpc_id"`
			Routes []struct {
				NatGatewayId string `json:"nat_gateway_id"`
			} `json:"routes"`
		}
		if err := json.Unmarshal(entry.Value(), &rtbData); err != nil {
			continue
		}
		if rtbData.VpcId != record.VpcId {
			continue
		}
		for _, r := range rtbData.Routes {
			if r.NatGatewayId == record.NatGatewayId {
				return errors.New(awserrors.ErrorDependencyViolation)
			}
		}
	}
	return nil
}

// disassociateEIP clears the EIP's association to this NAT gateway, keeping the
// allocation (AWS parity). Idempotent and non-fatal: the NAT gateway is already
// gone, so a stale association only delays the EIP's reuse.
func (s *NatGatewayServiceImpl) disassociateEIP(record *NatGatewayRecord, accountID string) {
	if record.AllocationId == "" {
		return
	}
	key := utils.AccountKey(accountID, record.AllocationId)
	entry, err := s.eipKV.Get(key)
	if err != nil {
		if !errors.Is(err, nats.ErrKeyNotFound) {
			slog.Warn("DeleteNatGateway: EIP read failed during disassociate", "allocationId", record.AllocationId, "err", err)
		}
		return
	}
	var eip handlers_ec2_eip.EIPRecord
	if err := json.Unmarshal(entry.Value(), &eip); err != nil {
		return
	}
	if eip.AssociationId == "" && eip.State == "allocated" {
		return
	}
	eip.AssociationId = ""
	eip.ENIId = ""
	eip.InstanceId = ""
	eip.PrivateIp = ""
	eip.MacAddress = ""
	eip.VpcId = ""
	eip.State = "allocated"
	data, err := json.Marshal(eip)
	if err != nil {
		return
	}
	if _, err := s.eipKV.Update(key, data, entry.Revision()); err != nil {
		slog.Warn("DeleteNatGateway: failed to disassociate EIP", "allocationId", record.AllocationId, "err", err)
	}
}

func mustJS(nc *nats.Conn) nats.JetStreamContext {
	js, _ := nc.JetStream()
	return js
}

var describeNatGatewaysValidFilters = map[string]bool{
	"nat-gateway-id": true,
	"subnet-id":      true,
	"vpc-id":         true,
	"state":          true,
}

// DescribeNatGateways lists NAT Gateways, optionally filtered
func (s *NatGatewayServiceImpl) DescribeNatGateways(input *ec2.DescribeNatGatewaysInput, accountID string) (*ec2.DescribeNatGatewaysOutput, error) {
	parsedFilters, err := filterutil.ParseFilters(input.Filter, describeNatGatewaysValidFilters)
	if err != nil {
		slog.Warn("DescribeNatGateways: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	natgwIDs := make(map[string]bool)
	for _, id := range input.NatGatewayIds {
		if id != nil {
			natgwIDs[*id] = true
		}
	}

	prefix := accountID + "."
	keys, err := s.natgwKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var natGateways []*ec2.NatGateway
	foundIDs := make(map[string]bool)

	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.natgwKV.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			slog.Error("Failed to read NAT Gateway", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		var record NatGatewayRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Error("Corrupt NAT Gateway record", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		if len(natgwIDs) > 0 && !natgwIDs[record.NatGatewayId] {
			continue
		}
		if !natgwMatchesFilters(&record, parsedFilters) {
			continue
		}

		natGateways = append(natGateways, recordToEC2(&record))
		foundIDs[record.NatGatewayId] = true
	}

	// Check the deleted bucket for any requested IDs not found in the active bucket.
	for id := range natgwIDs {
		if foundIDs[id] {
			continue
		}
		key := utils.AccountKey(accountID, id)
		entry, err := s.deletedNatgwKV.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				return nil, errors.New(awserrors.ErrorInvalidNatGatewayIDNotFound)
			}
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		var record NatGatewayRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		natGateways = append(natGateways, recordToEC2(&record))
	}

	return &ec2.DescribeNatGatewaysOutput{
		NatGateways: natGateways,
	}, nil
}

// natgwMatchesFilters checks whether a NAT Gateway record matches all parsed filters.
func natgwMatchesFilters(record *NatGatewayRecord, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}
		switch name {
		case "nat-gateway-id":
			if !filterutil.MatchesAny(values, record.NatGatewayId) {
				return false
			}
		case "subnet-id":
			if !filterutil.MatchesAny(values, record.SubnetId) {
				return false
			}
		case "vpc-id":
			if !filterutil.MatchesAny(values, record.VpcId) {
				return false
			}
		case "state":
			if !filterutil.MatchesAny(values, record.State) {
				return false
			}
		default:
			return false
		}
	}
	return filterutil.MatchesTags(filters, record.Tags)
}

// PublishAddEvent publishes a vpc.add-nat-gateway event for vpcd to create the SNAT rule.
// Called by the route table service when CreateRoute targets a NAT GW.
func (s *NatGatewayServiceImpl) PublishAddEvent(vpcId, natGatewayId, publicIp, subnetCidr string) {
	utils.PublishEvent(s.natsConn, "vpc.add-nat-gateway", natGatewayEvent{
		VpcId:        vpcId,
		NatGatewayId: natGatewayId,
		PublicIp:     publicIp,
		SubnetCidr:   subnetCidr,
	})
}

// PublishDeleteEvent publishes a vpc.delete-nat-gateway event for vpcd to remove the SNAT rule.
func (s *NatGatewayServiceImpl) PublishDeleteEvent(vpcId, natGatewayId, publicIp, subnetCidr string) {
	utils.PublishEvent(s.natsConn, "vpc.delete-nat-gateway", natGatewayEvent{
		VpcId:        vpcId,
		NatGatewayId: natGatewayId,
		PublicIp:     publicIp,
		SubnetCidr:   subnetCidr,
	})
}

// GetNatGateway retrieves a NAT Gateway record by ID
func (s *NatGatewayServiceImpl) GetNatGateway(accountID, natgwID string) (*NatGatewayRecord, error) {
	entry, err := s.natgwKV.Get(utils.AccountKey(accountID, natgwID))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorInvalidNatGatewayIDNotFound)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	var record NatGatewayRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &record, nil
}

func recordToEC2(record *NatGatewayRecord) *ec2.NatGateway {
	ngw := &ec2.NatGateway{
		NatGatewayId:     aws.String(record.NatGatewayId),
		VpcId:            aws.String(record.VpcId),
		SubnetId:         aws.String(record.SubnetId),
		State:            aws.String(record.State),
		ConnectivityType: aws.String("public"),
		CreateTime:       aws.Time(record.CreatedAt),
	}

	if record.PublicIp != "" {
		ngw.NatGatewayAddresses = []*ec2.NatGatewayAddress{
			{
				AllocationId: aws.String(record.AllocationId),
				PublicIp:     aws.String(record.PublicIp),
			},
		}
	}

	ngw.Tags = utils.MapToEC2Tags(record.Tags)

	return ngw
}
