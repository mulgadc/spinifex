package handlers_ec2_eigw

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
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure EgressOnlyIGWServiceImpl implements EgressOnlyIGWService
var _ EgressOnlyIGWService = (*EgressOnlyIGWServiceImpl)(nil)

const (
	KVBucketEgressOnlyIGW        = "spinifex-eigw"
	KVBucketEgressOnlyIGWVersion = 1
)

// EgressOnlyIGWRecord represents a stored Egress-only Internet Gateway
type EgressOnlyIGWRecord struct {
	EgressOnlyInternetGatewayId string            `json:"egress_only_internet_gateway_id"`
	VpcId                       string            `json:"vpc_id"`
	State                       string            `json:"state"`
	Tags                        map[string]string `json:"tags"`
	CreatedAt                   time.Time         `json:"created_at"`
}

// EgressOnlyIGWServiceImpl implements Egress-only Internet Gateway operations with NATS JetStream persistence
type EgressOnlyIGWServiceImpl struct {
	config *config.Config
	eigwKV nats.KeyValue
	vpcKV  nats.KeyValue
}

// NewEgressOnlyIGWServiceImplWithNATS creates an Egress-only Internet Gateway service with NATS JetStream for persistence
func NewEgressOnlyIGWServiceImplWithNATS(cfg *config.Config, natsConn *nats.Conn) (*EgressOnlyIGWServiceImpl, error) {
	js, err := natsConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	eigwKV, err := utils.GetOrCreateKVBucket(js, KVBucketEgressOnlyIGW, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketEgressOnlyIGW, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketEgressOnlyIGW, eigwKV, KVBucketEgressOnlyIGWVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketEgressOnlyIGW, err)
	}

	// Get or create VPC KV bucket for cross-resource ownership validation
	vpcKV, err := utils.GetOrCreateKVBucket(js, handlers_ec2_vpc.KVBucketVPCs, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to get VPC KV bucket: %w", err)
	}

	slog.Info("Egress-only IGW service initialized with JetStream KV", "bucket", KVBucketEgressOnlyIGW)

	return &EgressOnlyIGWServiceImpl{
		config: cfg,
		eigwKV: eigwKV,
		vpcKV:  vpcKV,
	}, nil
}

// CreateEgressOnlyInternetGateway creates a new Egress-only Internet Gateway
func (s *EgressOnlyIGWServiceImpl) CreateEgressOnlyInternetGateway(input *ec2.CreateEgressOnlyInternetGatewayInput, accountID string) (*ec2.CreateEgressOnlyInternetGatewayOutput, error) {
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	// Verify the caller owns the target VPC (fail-closed if KV unavailable)
	if s.vpcKV == nil {
		slog.Error("VPC KV unavailable, cannot verify VPC ownership")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.vpcKV.Get(utils.AccountKey(accountID, *input.VpcId)); err != nil {
		slog.Warn("CreateEgressOnlyInternetGateway: VPC not found for account", "vpcId", *input.VpcId, "accountID", accountID)
		return nil, errors.New(awserrors.ErrorInvalidVpcIDNotFound)
	}

	eigwID := utils.GenerateResourceID("eigw")

	record := EgressOnlyIGWRecord{
		EgressOnlyInternetGatewayId: eigwID,
		VpcId:                       *input.VpcId,
		State:                       "attached",
		Tags:                        utils.ExtractTags(input.TagSpecifications, "egress-only-internet-gateway"),
		CreatedAt:                   time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Egress-only IGW record: %w", err)
	}
	if _, err := s.eigwKV.Put(utils.AccountKey(accountID, eigwID), data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateEgressOnlyInternetGateway completed", "egressOnlyInternetGatewayId", eigwID, "vpcId", record.VpcId, "accountID", accountID)

	return &ec2.CreateEgressOnlyInternetGatewayOutput{
		EgressOnlyInternetGateway: s.recordToEC2(&record),
	}, nil
}

// DeleteEgressOnlyInternetGateway deletes an Egress-only Internet Gateway
func (s *EgressOnlyIGWServiceImpl) DeleteEgressOnlyInternetGateway(input *ec2.DeleteEgressOnlyInternetGatewayInput, accountID string) (*ec2.DeleteEgressOnlyInternetGatewayOutput, error) {
	if input.EgressOnlyInternetGatewayId == nil || *input.EgressOnlyInternetGatewayId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	eigwID := *input.EgressOnlyInternetGatewayId
	key := utils.AccountKey(accountID, eigwID)

	// Verify the EIGW exists before deleting
	if _, err := s.eigwKV.Get(key); err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorInvalidEgressOnlyInternetGatewayIdNotFound)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := s.eigwKV.Delete(key); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("DeleteEgressOnlyInternetGateway completed", "egressOnlyInternetGatewayId", eigwID, "accountID", accountID)

	return &ec2.DeleteEgressOnlyInternetGatewayOutput{
		ReturnCode: aws.Bool(true),
	}, nil
}

// describeEIGWValidFilters defines the set of filter names accepted by DescribeEgressOnlyInternetGateways.
var describeEIGWValidFilters = map[string]bool{
	"egress-only-internet-gateway-id": true,
}

// DescribeEgressOnlyInternetGateways describes Egress-only Internet Gateways
func (s *EgressOnlyIGWServiceImpl) DescribeEgressOnlyInternetGateways(input *ec2.DescribeEgressOnlyInternetGatewaysInput, accountID string) (*ec2.DescribeEgressOnlyInternetGatewaysOutput, error) {
	var egressOnlyIGWs []*ec2.EgressOnlyInternetGateway

	eigwIDs := make(map[string]bool)
	for _, id := range input.EgressOnlyInternetGatewayIds {
		if id != nil {
			eigwIDs[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeEIGWValidFilters)
	if err != nil {
		slog.Warn("DescribeEgressOnlyInternetGateways: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	prefix := accountID + "."
	keys, err := s.eigwKV.Keys()
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

		entry, err := s.eigwKV.Get(key)
		if err != nil {
			slog.Warn("Failed to get Egress-only IGW record", "key", key, "error", err)
			continue
		}

		var record EgressOnlyIGWRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Warn("Failed to unmarshal Egress-only IGW record", "key", key, "error", err)
			continue
		}

		if len(eigwIDs) > 0 && !eigwIDs[record.EgressOnlyInternetGatewayId] {
			continue
		}

		if len(parsedFilters) > 0 && !eigwMatchesFilters(&record, parsedFilters) {
			continue
		}

		egressOnlyIGWs = append(egressOnlyIGWs, s.recordToEC2(&record))
	}

	slog.Info("DescribeEgressOnlyInternetGateways completed", "count", len(egressOnlyIGWs), "accountID", accountID)

	return &ec2.DescribeEgressOnlyInternetGatewaysOutput{
		EgressOnlyInternetGateways: egressOnlyIGWs,
	}, nil
}

// eigwMatchesFilters checks whether an EgressOnlyIGWRecord satisfies all parsed filters.
func eigwMatchesFilters(record *EgressOnlyIGWRecord, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		var field string
		switch name {
		case "egress-only-internet-gateway-id":
			field = record.EgressOnlyInternetGatewayId
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	return filterutil.MatchesTags(filters, record.Tags)
}

func (s *EgressOnlyIGWServiceImpl) recordToEC2(record *EgressOnlyIGWRecord) *ec2.EgressOnlyInternetGateway {
	eigw := &ec2.EgressOnlyInternetGateway{
		EgressOnlyInternetGatewayId: aws.String(record.EgressOnlyInternetGatewayId),
		Attachments: []*ec2.InternetGatewayAttachment{
			{
				VpcId: aws.String(record.VpcId),
				State: aws.String(record.State),
			},
		},
	}

	eigw.Tags = utils.MapToEC2Tags(record.Tags)

	return eigw
}
