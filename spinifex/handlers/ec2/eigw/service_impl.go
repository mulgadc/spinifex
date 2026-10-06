package handlers_ec2_eigw

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/tagmirror"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsfilters "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	awstags "github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Ensure EgressOnlyIGWServiceImpl implements EgressOnlyIGWService.
var _ EgressOnlyIGWService = (*EgressOnlyIGWServiceImpl)(nil)

const (
	KVBucketEgressOnlyIGW        = "spinifex-eigw"
	KVBucketEgressOnlyIGWVersion = 1
)

// EgressOnlyIGWRecord represents a stored Egress-only Internet Gateway.
type EgressOnlyIGWRecord struct {
	EgressOnlyInternetGatewayId string            `json:"egress_only_internet_gateway_id"`
	VpcId                       string            `json:"vpc_id"`
	State                       string            `json:"state"`
	Tags                        map[string]string `json:"tags"`
	CreatedAt                   time.Time         `json:"created_at"`
}

// EgressOnlyIGWServiceImpl implements Egress-only Internet Gateway operations with NATS JetStream persistence.
type EgressOnlyIGWServiceImpl struct {
	config *config.Config
	eigwKV jetstream.KeyValue
	vpcKV  jetstream.KeyValue
}

// NewEgressOnlyIGWServiceImplWithNATS creates an Egress-only Internet Gateway service with NATS JetStream for persistence.
func NewEgressOnlyIGWServiceImplWithNATS(ctx context.Context, cfg *config.Config, natsConn *nats.Conn) (*EgressOnlyIGWServiceImpl, error) {
	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	eigwKV, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketEgressOnlyIGW, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketEgressOnlyIGW, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketEgressOnlyIGW, eigwKV, KVBucketEgressOnlyIGWVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketEgressOnlyIGW, err)
	}

	// Get or create VPC KV bucket for cross-resource ownership validation
	vpcKV, err := kvutil.GetOrCreateBucket(ctx, js, handlers_ec2_vpc.KVBucketVPCs, 10)
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

// CreateEgressOnlyInternetGateway creates a new Egress-only Internet Gateway.
func (s *EgressOnlyIGWServiceImpl) CreateEgressOnlyInternetGateway(ctx context.Context, input *ec2.CreateEgressOnlyInternetGatewayInput, accountID string) (*ec2.CreateEgressOnlyInternetGatewayOutput, error) {
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	// Verify the caller owns the target VPC (fail-closed if KV unavailable)
	if s.vpcKV == nil {
		slog.ErrorContext(ctx, "VPC KV unavailable, cannot verify VPC ownership")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.vpcKV.Get(ctx, kvutil.AccountKey(accountID, *input.VpcId)); err != nil {
		slog.WarnContext(ctx, "CreateEgressOnlyInternetGateway: VPC not found for account", "vpcId", *input.VpcId, "accountID", accountID)
		return nil, awserrors.IDNotFound(awserrors.ErrorInvalidVpcIDNotFound, "vpc", *input.VpcId)
	}

	eigwID := awsidentifiers.GenerateResourceID("eigw")

	record := EgressOnlyIGWRecord{
		EgressOnlyInternetGatewayId: eigwID,
		VpcId:                       *input.VpcId,
		State:                       "attached",
		Tags:                        awstags.Extract(input.TagSpecifications, "egress-only-internet-gateway"),
		CreatedAt:                   time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Egress-only IGW record: %w", err)
	}
	if _, err := s.eigwKV.Put(ctx, kvutil.AccountKey(accountID, eigwID), data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "CreateEgressOnlyInternetGateway completed", "egressOnlyInternetGatewayId", eigwID, "vpcId", record.VpcId, "accountID", accountID)

	return &ec2.CreateEgressOnlyInternetGatewayOutput{
		EgressOnlyInternetGateway: s.recordToEC2(&record),
	}, nil
}

// DeleteEgressOnlyInternetGateway deletes an Egress-only Internet Gateway.
func (s *EgressOnlyIGWServiceImpl) DeleteEgressOnlyInternetGateway(ctx context.Context, input *ec2.DeleteEgressOnlyInternetGatewayInput, accountID string) (*ec2.DeleteEgressOnlyInternetGatewayOutput, error) {
	if input.EgressOnlyInternetGatewayId == nil || *input.EgressOnlyInternetGatewayId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	eigwID := *input.EgressOnlyInternetGatewayId
	key := kvutil.AccountKey(accountID, eigwID)

	// Verify the EIGW exists before deleting
	if _, err := s.eigwKV.Get(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidGatewayIDNotFound, "The eigw ID '%s' does not exist", eigwID)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := s.eigwKV.Delete(ctx, key); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "DeleteEgressOnlyInternetGateway completed", "egressOnlyInternetGatewayId", eigwID, "accountID", accountID)

	return &ec2.DeleteEgressOnlyInternetGatewayOutput{
		ReturnCode: aws.Bool(true),
	}, nil
}

// describeEIGWValidFilters defines the set of filter names accepted by DescribeEgressOnlyInternetGateways.
var describeEIGWValidFilters = map[string]bool{
	"egress-only-internet-gateway-id": true,
	"tag-key":                         true,
	"tag-value":                       true,
}

// DescribeEgressOnlyInternetGateways describes Egress-only Internet Gateways.
func (s *EgressOnlyIGWServiceImpl) DescribeEgressOnlyInternetGateways(ctx context.Context, input *ec2.DescribeEgressOnlyInternetGatewaysInput, accountID string) (*ec2.DescribeEgressOnlyInternetGatewaysOutput, error) {
	var egressOnlyIGWs []*ec2.EgressOnlyInternetGateway

	eigwIDs := make(map[string]bool)
	for _, id := range input.EgressOnlyInternetGatewayIds {
		if id != nil {
			eigwIDs[*id] = true
		}
	}

	parsedFilters, err := awsfilters.ParseFilters(input.Filters, describeEIGWValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeEgressOnlyInternetGateways: invalid filter", "err", err)
		return nil, err
	}
	for _, id := range input.EgressOnlyInternetGatewayIds {
		if id != nil && !eigwIDWellFormed(*id) {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidEgressOnlyInternetGatewayIdMalformed, "The eigw ID %s is malformed", *id)
		}
	}

	prefix := accountID + "."
	keys, err := s.eigwKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, key := range keys {
		if key == kvutil.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.eigwKV.Get(ctx, key)
		if err != nil {
			slog.WarnContext(ctx, "Failed to get Egress-only IGW record", "key", key, "error", err)
			continue
		}

		var record EgressOnlyIGWRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.WarnContext(ctx, "Failed to unmarshal Egress-only IGW record", "key", key, "error", err)
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

	slog.InfoContext(ctx, "DescribeEgressOnlyInternetGateways completed", "count", len(egressOnlyIGWs), "accountID", accountID)

	return &ec2.DescribeEgressOnlyInternetGatewaysOutput{
		EgressOnlyInternetGateways: egressOnlyIGWs,
	}, nil
}

// eigwIDWellFormed reports whether AWS accepts id as an egress-only gateway ID:
// "eigw-" and exactly 17 hex digits of either case. A well-formed unknown ID is
// not an error; the describe returns no gateway for it.
func eigwIDWellFormed(id string) bool {
	suffix, ok := strings.CutPrefix(id, "eigw-")
	if !ok || len(suffix) != 17 {
		return false
	}
	_, err := hex.DecodeString("0" + suffix)
	return err == nil
}

// eigwMatchesFilters checks whether an EgressOnlyIGWRecord satisfies all parsed awsfilters.
func eigwMatchesFilters(record *EgressOnlyIGWRecord, filters map[string][]string) bool {
	for name, values := range filters {
		if awsfilters.IsTagFilter(name) {
			continue
		}

		var field string
		switch name {
		case "egress-only-internet-gateway-id":
			field = record.EgressOnlyInternetGatewayId
		default:
			return false
		}

		if !awsfilters.MatchesAny(values, field) {
			return false
		}
	}

	return awsfilters.MatchesTags(filters, record.Tags)
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

	eigw.Tags = awstags.MapToEC2(record.Tags)

	return eigw
}

// ApplyRecordTags mirrors CreateTags into the owning egress-only IGW KV
// record so tag-filtered describes observe tags added after create. Resource
// ids this service does not own are skipped; absent records are a no-op.
func (s *EgressOnlyIGWServiceImpl) ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return tagmirror.MirrorKVRecordTags(context.Background(), s.eigwKV, accountID, "eigw-", input.Resources,
		func(r *EgressOnlyIGWRecord) *map[string]string { return &r.Tags },
		tagmirror.MergeTagsMut(input))
}

// RemoveRecordTags mirrors DeleteTags into the owning egress-only IGW KV
// record with AWS-faithful delete semantics.
func (s *EgressOnlyIGWServiceImpl) RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return tagmirror.MirrorKVRecordTags(context.Background(), s.eigwKV, accountID, "eigw-", input.Resources,
		func(r *EgressOnlyIGWRecord) *map[string]string { return &r.Tags },
		tagmirror.RemoveTagsMut(input))
}
