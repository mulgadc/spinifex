package handlers_ec2_igw

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
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure IGWServiceImpl implements IGWService
var _ IGWService = (*IGWServiceImpl)(nil)

const (
	KVBucketIGW        = "spinifex-igw"
	KVBucketIGWVersion = 1
)

// IGWRecord represents a stored Internet Gateway
type IGWRecord struct {
	InternetGatewayId string            `json:"internet_gateway_id"`
	VpcId             string            `json:"vpc_id,omitempty"` // empty when detached
	State             string            `json:"state"`            // "available", "attached", "detached"
	Tags              map[string]string `json:"tags"`
	CreatedAt         time.Time         `json:"created_at"`
}

// IGWServiceImpl implements Internet Gateway operations with NATS JetStream persistence
type IGWServiceImpl struct {
	config   *config.Config
	igwKV    nats.KeyValue
	vpcKV    nats.KeyValue
	natsConn *nats.Conn
}

// NewIGWServiceImplWithNATS creates an Internet Gateway service with NATS JetStream for persistence
func NewIGWServiceImplWithNATS(cfg *config.Config, natsConn *nats.Conn) (*IGWServiceImpl, error) {
	js, err := natsConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	igwKV, err := utils.GetOrCreateKVBucket(js, KVBucketIGW, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketIGW, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketIGW, igwKV, KVBucketIGWVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketIGW, err)
	}

	// Get or create VPC KV bucket for cross-resource ownership validation
	vpcKV, err := utils.GetOrCreateKVBucket(js, handlers_ec2_vpc.KVBucketVPCs, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to get VPC KV bucket: %w", err)
	}

	slog.Info("IGW service initialized with JetStream KV", "bucket", KVBucketIGW)

	return &IGWServiceImpl{
		config:   cfg,
		igwKV:    igwKV,
		vpcKV:    vpcKV,
		natsConn: natsConn,
	}, nil
}

// CreateInternetGateway creates a new Internet Gateway (initially detached)
// CreateInternetGatewayWithID creates an IGW with a pre-determined ID.
// Used by bootstrap to ensure the IGW ID matches [bootstrap] in spinifex.toml.
func (s *IGWServiceImpl) CreateInternetGatewayWithID(input *ec2.CreateInternetGatewayInput, accountID, igwID string) (*ec2.CreateInternetGatewayOutput, error) {
	return s.createIGW(input, accountID, igwID)
}

func (s *IGWServiceImpl) CreateInternetGateway(input *ec2.CreateInternetGatewayInput, accountID string) (*ec2.CreateInternetGatewayOutput, error) {
	igwID := utils.GenerateResourceID("igw")
	return s.createIGW(input, accountID, igwID)
}

func (s *IGWServiceImpl) createIGW(input *ec2.CreateInternetGatewayInput, accountID, igwID string) (*ec2.CreateInternetGatewayOutput, error) {
	record := IGWRecord{
		InternetGatewayId: igwID,
		State:             "available",
		Tags:              utils.ExtractTags(input.TagSpecifications, "internet-gateway"),
		CreatedAt:         time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal IGW record: %w", err)
	}
	if _, err := s.igwKV.Put(utils.AccountKey(accountID, igwID), data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateInternetGateway completed", "internetGatewayId", igwID, "accountID", accountID)

	return &ec2.CreateInternetGatewayOutput{
		InternetGateway: s.recordToEC2(&record),
	}, nil
}

// DeleteInternetGateway deletes an Internet Gateway (must be detached first)
func (s *IGWServiceImpl) DeleteInternetGateway(input *ec2.DeleteInternetGatewayInput, accountID string) (*ec2.DeleteInternetGatewayOutput, error) {
	if input.InternetGatewayId == nil || *input.InternetGatewayId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	igwID := *input.InternetGatewayId
	key := utils.AccountKey(accountID, igwID)

	entry, err := s.igwKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidInternetGatewayIDNotFound)
	}

	var record IGWRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Cannot delete an attached IGW
	if record.VpcId != "" {
		return nil, errors.New(awserrors.ErrorDependencyViolation)
	}

	if err := s.igwKV.Delete(key); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("DeleteInternetGateway completed", "internetGatewayId", igwID, "accountID", accountID)

	return &ec2.DeleteInternetGatewayOutput{}, nil
}

// describeIGWValidFilters defines the set of filter names accepted by DescribeInternetGateways.
var describeIGWValidFilters = map[string]bool{
	"internet-gateway-id": true,
	"attachment.vpc-id":   true,
	"attachment.state":    true,
}

// DescribeInternetGateways lists Internet Gateways, optionally filtered by ID
func (s *IGWServiceImpl) DescribeInternetGateways(input *ec2.DescribeInternetGatewaysInput, accountID string) (*ec2.DescribeInternetGatewaysOutput, error) {
	var igws []*ec2.InternetGateway

	igwIDs := make(map[string]bool)
	for _, id := range input.InternetGatewayIds {
		if id != nil {
			igwIDs[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeIGWValidFilters)
	if err != nil {
		slog.Warn("DescribeInternetGateways: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	prefix := accountID + "."
	keys, err := s.igwKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	foundIDs := make(map[string]bool)

	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.igwKV.Get(key)
		if err != nil {
			slog.Warn("Failed to get IGW record", "key", key, "error", err)
			continue
		}

		var record IGWRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Warn("Failed to unmarshal IGW record", "key", key, "error", err)
			continue
		}

		if len(igwIDs) > 0 && !igwIDs[record.InternetGatewayId] {
			continue
		}

		if len(parsedFilters) > 0 && !igwMatchesFilters(&record, parsedFilters) {
			continue
		}

		igws = append(igws, s.recordToEC2(&record))
		foundIDs[record.InternetGatewayId] = true
	}

	// Return error if specific IDs were requested but not found
	for id := range igwIDs {
		if !foundIDs[id] {
			return nil, errors.New(awserrors.ErrorInvalidInternetGatewayIDNotFound)
		}
	}

	slog.Info("DescribeInternetGateways completed", "count", len(igws), "accountID", accountID)

	return &ec2.DescribeInternetGatewaysOutput{
		InternetGateways: igws,
	}, nil
}

// igwMatchesFilters checks whether an IGWRecord satisfies all parsed filters.
func igwMatchesFilters(record *IGWRecord, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		var field string
		switch name {
		case "internet-gateway-id":
			field = record.InternetGatewayId
		case "attachment.vpc-id":
			field = record.VpcId
		case "attachment.state":
			field = record.State
			if record.VpcId == "" {
				field = "" // no attachment means no state to match
			}
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	return filterutil.MatchesTags(filters, record.Tags)
}

// AttachInternetGateway attaches an IGW to a VPC and publishes a NATS event
// for vpcd to create the OVN external switch, gateway port, and SNAT rules.
func (s *IGWServiceImpl) AttachInternetGateway(input *ec2.AttachInternetGatewayInput, accountID string) (*ec2.AttachInternetGatewayOutput, error) {
	if input.InternetGatewayId == nil || *input.InternetGatewayId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	igwID := *input.InternetGatewayId
	vpcID := *input.VpcId
	key := utils.AccountKey(accountID, igwID)

	entry, err := s.igwKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidInternetGatewayIDNotFound)
	}

	var record IGWRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if record.VpcId != "" {
		return nil, errors.New(awserrors.ErrorResourceAlreadyAssociated)
	}

	// Verify the caller owns the target VPC (fail-closed if KV unavailable)
	if s.vpcKV == nil {
		slog.Error("VPC KV unavailable, cannot verify VPC ownership")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.vpcKV.Get(utils.AccountKey(accountID, vpcID)); err != nil {
		slog.Warn("AttachInternetGateway: VPC not found for account", "vpcId", vpcID, "accountID", accountID)
		return nil, errors.New(awserrors.ErrorInvalidVpcIDNotFound)
	}

	record.VpcId = vpcID
	record.State = "attached"

	data, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.igwKV.Put(key, data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Publish event for vpcd to create OVN external switch + gateway + SNAT
	if s.natsConn != nil {
		event := types.IGWEvent{
			InternetGatewayId: igwID,
			VpcId:             vpcID,
		}
		eventData, err := json.Marshal(event)
		if err != nil {
			slog.Warn("Failed to marshal IGW attach event", "error", err)
		} else if err := s.natsConn.Publish("vpc.igw-attach", eventData); err != nil {
			slog.Warn("Failed to publish IGW attach event", "error", err)
		}
	}

	slog.Info("AttachInternetGateway completed", "internetGatewayId", igwID, "vpcId", vpcID, "accountID", accountID)

	return &ec2.AttachInternetGatewayOutput{}, nil
}

// DetachInternetGateway detaches an IGW from a VPC and publishes a NATS event
// for vpcd to clean up the OVN external switch, gateway port, and NAT rules.
func (s *IGWServiceImpl) DetachInternetGateway(input *ec2.DetachInternetGatewayInput, accountID string) (*ec2.DetachInternetGatewayOutput, error) {
	if input.InternetGatewayId == nil || *input.InternetGatewayId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	igwID := *input.InternetGatewayId
	vpcID := *input.VpcId
	key := utils.AccountKey(accountID, igwID)

	entry, err := s.igwKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidInternetGatewayIDNotFound)
	}

	var record IGWRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if record.VpcId != vpcID {
		return nil, errors.New(awserrors.ErrorGatewayNotAttached)
	}

	record.VpcId = ""
	record.State = "detached"

	data, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.igwKV.Put(key, data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Publish event for vpcd to clean up OVN external switch + gateway + NAT
	if s.natsConn != nil {
		event := types.IGWEvent{
			InternetGatewayId: igwID,
			VpcId:             vpcID,
		}
		eventData, err := json.Marshal(event)
		if err != nil {
			slog.Warn("Failed to marshal IGW detach event", "error", err)
		} else if err := s.natsConn.Publish("vpc.igw-detach", eventData); err != nil {
			slog.Warn("Failed to publish IGW detach event", "error", err)
		}
	}

	slog.Info("DetachInternetGateway completed", "internetGatewayId", igwID, "vpcId", vpcID, "accountID", accountID)

	return &ec2.DetachInternetGatewayOutput{}, nil
}

func (s *IGWServiceImpl) recordToEC2(record *IGWRecord) *ec2.InternetGateway {
	igw := &ec2.InternetGateway{
		InternetGatewayId: aws.String(record.InternetGatewayId),
	}

	if record.VpcId != "" {
		igw.Attachments = []*ec2.InternetGatewayAttachment{
			{
				VpcId: aws.String(record.VpcId),
				State: aws.String(record.State),
			},
		}
	}

	igw.Tags = utils.MapToEC2Tags(record.Tags)

	return igw
}
