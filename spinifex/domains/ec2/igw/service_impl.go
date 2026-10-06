package igw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/tagmirror"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsfilters "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/paging"
	awstags "github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Ensure IGWServiceImpl implements IGWService.
var _ IGWService = (*IGWServiceImpl)(nil)

const (
	KVBucketIGW        = "spinifex-igw"
	KVBucketIGWVersion = 1
)

// IGWRecord represents a stored Internet Gateway.
type IGWRecord struct {
	InternetGatewayId string            `json:"internet_gateway_id"`
	VpcId             string            `json:"vpc_id,omitempty"` // empty when detached
	State             string            `json:"state"`            // "available" — AWS attachment.state is "available" when attached
	AttachState       string            `json:"attach_state,omitempty"`
	Tags              map[string]string `json:"tags"`
	CreatedAt         time.Time         `json:"created_at"`
}

// Observed attachment state, distinct from State. State stays the AWS-facing
// attachment.state and doubles as the reconciler's intent signal; AttachState
// records whether vpcd has actually brought the OVN gateway up.
const (
	// AttachStatePending is exported for the reconciler, which decides from the
	// record whether an attachment still needs confirming.
	AttachStatePending  = "pending"
	attachStateAttached = "attached"
)

// attachmentVisible reports whether the record has an attachment AWS would
// return. A pending attach has been requested but not yet confirmed in OVN; an
// empty AttachState is a record predating attach tracking.
func (r *IGWRecord) attachmentVisible() bool {
	return r.VpcId != "" && r.AttachState != AttachStatePending
}

// GatePublisher recomputes per-subnet egress gate/ungate decisions for a VPC.
// Wired by the daemon so IGW attach/detach triggers immediate OVN policy updates
// rather than waiting for the reconciler's drift tick.
type GatePublisher interface {
	PublishGateDecisionsForVPC(accountID, vpcID, destCidr string)
}

// IGWServiceImpl implements Internet Gateway operations with NATS JetStream persistence.
type IGWServiceImpl struct {
	config        *config.Config
	igwKV         jetstream.KeyValue
	vpcKV         jetstream.KeyValue
	natsConn      *nats.Conn
	gatePublisher GatePublisher
}

// SetGatePublisher installs the cross-handler fan-out hook. Called by the
// daemon after the RouteTable service is constructed.
func (s *IGWServiceImpl) SetGatePublisher(p GatePublisher) {
	s.gatePublisher = p
}

// NewIGWServiceImplWithNATS creates an Internet Gateway service with NATS JetStream for persistence.
func NewIGWServiceImplWithNATS(ctx context.Context, cfg *config.Config, natsConn *nats.Conn) (*IGWServiceImpl, error) {
	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	igwKV, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketIGW, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketIGW, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketIGW, igwKV, KVBucketIGWVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketIGW, err)
	}

	// Get or create VPC KV bucket for cross-resource ownership validation
	vpcKV, err := kvutil.GetOrCreateBucket(ctx, js, ec2vpc.KVBucketVPCs, 10)
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

// CreateInternetGateway creates a new Internet Gateway (initially detached).
func (s *IGWServiceImpl) CreateInternetGateway(ctx context.Context, input *ec2.CreateInternetGatewayInput, accountID string) (*ec2.CreateInternetGatewayOutput, error) {
	igwID := awsidentifiers.GenerateResourceID("igw")
	return s.createIGW(ctx, input, accountID, igwID)
}

func (s *IGWServiceImpl) createIGW(ctx context.Context, input *ec2.CreateInternetGatewayInput, accountID, igwID string) (*ec2.CreateInternetGatewayOutput, error) {
	record := IGWRecord{
		InternetGatewayId: igwID,
		State:             "available",
		Tags:              awstags.Extract(input.TagSpecifications, "internet-gateway"),
		CreatedAt:         time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal IGW record: %w", err)
	}
	if _, err := s.igwKV.Put(ctx, kvutil.AccountKey(accountID, igwID), data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "CreateInternetGateway completed", "internetGatewayId", igwID, "accountID", accountID)

	return &ec2.CreateInternetGatewayOutput{
		InternetGateway: s.recordToEC2(&record, accountID),
	}, nil
}

// DeleteInternetGateway deletes an Internet Gateway (must be detached first).
func (s *IGWServiceImpl) DeleteInternetGateway(ctx context.Context, input *ec2.DeleteInternetGatewayInput, accountID string) (*ec2.DeleteInternetGatewayOutput, error) {
	if input.InternetGatewayId == nil || *input.InternetGatewayId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	igwID := *input.InternetGatewayId
	key := kvutil.AccountKey(accountID, igwID)

	entry, err := s.igwKV.Get(ctx, key)
	if err != nil {
		// AWS-faithful: an absent internet gateway is NotFound (provider
		// tolerates it on destroy); destroy orchestration tolerates it too.
		// A transient read error stays a server error.
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, igwNotFoundError(igwID)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var record IGWRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Cannot delete an attached IGW. AWS's message does not name the VPC, and
	// a pending attachment is hidden from describes, so the log names it.
	if record.VpcId != "" {
		slog.WarnContext(ctx, "DeleteInternetGateway: gateway is attached", "internetGatewayId", igwID, "vpcId", record.VpcId)
		return nil, awserrors.HasDependencies("internetGateway", igwID)
	}

	if err := s.igwKV.Delete(ctx, key); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "DeleteInternetGateway completed", "internetGatewayId", igwID, "accountID", accountID)

	return &ec2.DeleteInternetGatewayOutput{}, nil
}

var describeIGWPaging = paging.EC2{
	MaxResults: 1000,
	TooLarge:   "Value ( %d ) for parameter MaxResults is invalid. Expecting a value smaller than or equal to 1000.",
	TooSmall:   "Value ( %d ) for parameter MaxResults is invalid. Expecting a value greater than or equal to 5.",
	WithIDs:    "The parameter InternetGatewayIds cannot be used with the parameter MaxResults",
}

// describeIGWValidFilters defines the set of filter names accepted by DescribeInternetGateways.
var describeIGWValidFilters = map[string]bool{
	"internet-gateway-id": true,
	"attachment.vpc-id":   true,
	"attachment.state":    true,
	"tag-key":             true,
	"tag-value":           true,
}

// DescribeInternetGateways lists Internet Gateways, optionally filtered by ID.
func (s *IGWServiceImpl) DescribeInternetGateways(ctx context.Context, input *ec2.DescribeInternetGatewaysInput, accountID string) (*ec2.DescribeInternetGatewaysOutput, error) {
	var igws []*ec2.InternetGateway

	// Validated but not paged: AWS's paging of DescribeInternetGateways is unobserved.
	if _, err := describeIGWPaging.Parse(input.MaxResults, input.NextToken, len(input.InternetGatewayIds)); err != nil {
		return nil, err
	}

	igwIDs := make(map[string]bool)
	for _, id := range input.InternetGatewayIds {
		if id != nil {
			igwIDs[*id] = true
		}
	}

	parsedFilters, err := awsfilters.ParseFilters(input.Filters, describeIGWValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeInternetGateways: invalid filter", "err", err)
		return nil, err
	}

	prefix := accountID + "."
	keys, err := s.igwKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	foundIDs := make(map[string]bool)

	for _, key := range keys {
		if key == kvutil.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.igwKV.Get(ctx, key)
		if err != nil {
			slog.WarnContext(ctx, "Failed to get IGW record", "key", key, "error", err)
			continue
		}

		var record IGWRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.WarnContext(ctx, "Failed to unmarshal IGW record", "key", key, "error", err)
			continue
		}

		if len(igwIDs) > 0 && !igwIDs[record.InternetGatewayId] {
			continue
		}

		if len(parsedFilters) > 0 && !igwMatchesFilters(&record, parsedFilters) {
			continue
		}

		igws = append(igws, s.recordToEC2(&record, accountID))
		foundIDs[record.InternetGatewayId] = true
	}

	// Return error if specific IDs were requested but not found
	for _, id := range input.InternetGatewayIds {
		if id != nil && !foundIDs[*id] {
			return nil, igwNotFoundError(*id)
		}
	}

	slog.InfoContext(ctx, "DescribeInternetGateways completed", "count", len(igws), "accountID", accountID)

	return &ec2.DescribeInternetGatewaysOutput{
		InternetGateways: igws,
	}, nil
}

// igwMatchesFilters checks whether an IGWRecord satisfies all parsed awsfilters.
func igwMatchesFilters(record *IGWRecord, filters map[string][]string) bool {
	for name, values := range filters {
		if awsfilters.IsTagFilter(name) {
			continue
		}

		var field string
		switch name {
		case "internet-gateway-id":
			field = record.InternetGatewayId
		case "attachment.vpc-id":
			field = record.VpcId
			if !record.attachmentVisible() {
				field = "" // no reportable attachment means no vpc to match
			}
		case "attachment.state":
			field = record.State
			if !record.attachmentVisible() {
				field = "" // no attachment means no state to match
			}
		default:
			return false
		}

		if !awsfilters.MatchesAny(values, field) {
			return false
		}
	}

	return awsfilters.MatchesTags(filters, record.Tags)
}

// AttachInternetGateway attaches an IGW to a VPC and publishes a NATS event
// for vpcd to create the OVN external switch, gateway port, and SNAT rules.
func (s *IGWServiceImpl) AttachInternetGateway(ctx context.Context, input *ec2.AttachInternetGatewayInput, accountID string) (*ec2.AttachInternetGatewayOutput, error) {
	if input.InternetGatewayId == nil || *input.InternetGatewayId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	igwID := *input.InternetGatewayId
	vpcID := *input.VpcId
	key := kvutil.AccountKey(accountID, igwID)

	entry, err := s.igwKV.Get(ctx, key)
	if err != nil {
		return nil, igwNotFoundError(igwID)
	}

	var record IGWRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if record.VpcId != "" {
		return nil, awserrors.Errorf(awserrors.ErrorResourceAlreadyAssociated, "resource %s is already attached to network %s", igwID, record.VpcId)
	}

	// Verify the caller owns the target VPC (fail-closed if KV unavailable)
	if s.vpcKV == nil {
		slog.ErrorContext(ctx, "VPC KV unavailable, cannot verify VPC ownership")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.vpcKV.Get(ctx, kvutil.AccountKey(accountID, vpcID)); err != nil {
		slog.WarnContext(ctx, "AttachInternetGateway: VPC not found for account", "vpcId", vpcID, "accountID", accountID)
		return nil, awserrors.IDNotFound(awserrors.ErrorInvalidVpcIDNotFound, "vpc", vpcID)
	}

	record.VpcId = vpcID
	record.State = "available"
	// vpcd attaches the OVN gateway asynchronously, so the attachment is not
	// reported until a reconcile pass confirms it.
	record.AttachState = AttachStatePending

	data, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.igwKV.Put(ctx, key, data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	s.publishAttach(ctx, igwID, vpcID)

	// Gate fan-out is intentionally skipped on attach to avoid a race with
	// the bootstrap CreateRoute path. Detach triggers gate fan-out directly.

	slog.InfoContext(ctx, "AttachInternetGateway completed", "internetGatewayId", igwID, "vpcId", vpcID, "accountID", accountID)

	return &ec2.AttachInternetGatewayOutput{}, nil
}

// DetachInternetGateway detaches an IGW from a VPC and publishes a NATS event
// for vpcd to clean up the OVN external switch, gateway port, and NAT rules.
func (s *IGWServiceImpl) DetachInternetGateway(ctx context.Context, input *ec2.DetachInternetGatewayInput, accountID string) (*ec2.DetachInternetGatewayOutput, error) {
	if input.InternetGatewayId == nil || *input.InternetGatewayId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	igwID := *input.InternetGatewayId
	vpcID := *input.VpcId
	key := kvutil.AccountKey(accountID, igwID)

	entry, err := s.igwKV.Get(ctx, key)
	if err != nil {
		return nil, igwNotFoundError(igwID)
	}

	var record IGWRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if record.VpcId != vpcID {
		return nil, errors.New(awserrors.ErrorGatewayNotAttached)
	}

	record.VpcId = ""
	record.State = "available"
	record.AttachState = ""

	data, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.igwKV.Put(ctx, key, data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Publish event for vpcd to clean up OVN external switch + gateway + NAT
	if s.natsConn != nil {
		event := ec2v1.InternetGatewayEvent{
			InternetGatewayId: igwID,
			VpcId:             vpcID,
		}
		eventData, err := json.Marshal(event)
		if err != nil {
			slog.WarnContext(ctx, "Failed to marshal IGW detach event", "error", err)
		} else if err := s.natsConn.Publish(ec2v1.InternetGatewayDetachSubject, eventData); err != nil {
			slog.WarnContext(ctx, "Failed to publish IGW detach event", "error", err)
		}
	}

	// After detach the VPC's LR external gateway and router-wide default
	// route are removed; any subnet whose effective RT still points at the
	// (now-blackholed) IGW must surface the ungate→gate transition so the
	// reconciler drops the previous ungate policy in favour of a DROP.
	if s.gatePublisher != nil {
		s.gatePublisher.PublishGateDecisionsForVPC(accountID, vpcID, "0.0.0.0/0")
	}

	slog.InfoContext(ctx, "DetachInternetGateway completed", "internetGatewayId", igwID, "vpcId", vpcID, "accountID", accountID)

	return &ec2.DetachInternetGatewayOutput{}, nil
}

// publishAttach asks vpcd to create the OVN external switch, gateway and SNAT.
func (s *IGWServiceImpl) publishAttach(ctx context.Context, igwID, vpcID string) {
	if s.natsConn == nil {
		return
	}
	eventData, err := json.Marshal(ec2v1.InternetGatewayEvent{InternetGatewayId: igwID, VpcId: vpcID})
	if err != nil {
		slog.WarnContext(ctx, "Failed to marshal IGW attach event", "error", err)
	} else if err := s.natsConn.Publish(ec2v1.InternetGatewayAttachSubject, eventData); err != nil {
		slog.WarnContext(ctx, "Failed to publish IGW attach event", "error", err)
	}
}

// CreateAttachedInternetGateway creates igwID already attached to vpcID, for a
// default VPC whose gateway ID every node agreed in advance. It never overwrites,
// so racing callers build one gateway; created is false when another got there first.
func (s *IGWServiceImpl) CreateAttachedInternetGateway(ctx context.Context, accountID, igwID, vpcID string) (created bool, err error) {
	if s.vpcKV == nil {
		return false, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.vpcKV.Get(ctx, kvutil.AccountKey(accountID, vpcID)); err != nil {
		return false, awserrors.IDNotFound(awserrors.ErrorInvalidVpcIDNotFound, "vpc", vpcID)
	}

	key := kvutil.AccountKey(accountID, igwID)
	data, err := json.Marshal(IGWRecord{
		InternetGatewayId: igwID,
		VpcId:             vpcID,
		State:             "available",
		AttachState:       AttachStatePending,
		Tags:              map[string]string{},
		CreatedAt:         time.Now(),
	})
	if err != nil {
		return false, errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.igwKV.Create(ctx, key, data); err != nil {
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return false, errors.New(awserrors.ErrorServerInternal)
		}
		entry, err := s.igwKV.Get(ctx, key)
		if err != nil {
			return false, errors.New(awserrors.ErrorServerInternal)
		}
		var existing IGWRecord
		if err := json.Unmarshal(entry.Value(), &existing); err != nil {
			return false, errors.New(awserrors.ErrorServerInternal)
		}
		if existing.VpcId == vpcID {
			return false, nil
		}
		// Present but detached: attach it rather than failing the default VPC.
		_, err = s.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
			InternetGatewayId: &igwID,
			VpcId:             &vpcID,
		}, accountID)
		return false, err
	}

	s.publishAttach(ctx, igwID, vpcID)
	slog.InfoContext(ctx, "Created attached internet gateway", "internetGatewayId", igwID, "vpcId", vpcID, "accountID", accountID)
	return true, nil
}

// AttachmentIntent returns the IGW whose record names vpcID, or nil if none
// does. Unlike DescribeInternetGateways it reports an attachment that has been
// requested but not yet confirmed, because a caller provisioning or tearing
// down a VPC asks what was asked for, not what OVN has caught up with. The AWS
// surface must not answer that question, so this is the in-process seam for it.
func (s *IGWServiceImpl) AttachmentIntent(ctx context.Context, accountID, vpcID string) (*ec2.InternetGateway, error) {
	if vpcID == "" {
		return nil, nil
	}

	prefix := accountID + "."
	keys, err := s.igwKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		slog.ErrorContext(ctx, "AttachmentIntent: IGW key listing failed", "accountID", accountID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, key := range keys {
		if key == kvutil.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.igwKV.Get(ctx, key)
		if err != nil {
			// Fail closed: callers read nil as "no gateway" and build one, so a
			// record this could not read must not read as absent.
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			slog.ErrorContext(ctx, "AttachmentIntent: IGW read failed", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		var record IGWRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.ErrorContext(ctx, "AttachmentIntent: IGW unmarshal failed", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if record.VpcId != vpcID {
			continue
		}
		// Built directly rather than through recordToEC2, which hides a pending
		// attachment: this view exists to show one.
		return &ec2.InternetGateway{
			InternetGatewayId: aws.String(record.InternetGatewayId),
			Attachments: []*ec2.InternetGatewayAttachment{
				{VpcId: aws.String(record.VpcId), State: aws.String(record.State)},
			},
			Tags: awstags.MapToEC2(record.Tags),
		}, nil
	}

	return nil, nil
}

// MarkAttached records that vpcd has brought the OVN gateway up for vpcID on the
// IGW at recordKey. vpcID must match: a record key survives detach and re-attach,
// so a pass that converged the previous VPC would otherwise confirm the new one.
// Writing only on the pending transition keeps a pass from waking the drift loop.
func MarkAttached(ctx context.Context, kv jetstream.KeyValue, recordKey, vpcID string) error {
	confirmed := false
	record, err := kvutil.Update(ctx, kv, recordKey, kvutil.CASConfig{}, func(r *IGWRecord) (bool, error) {
		// Reset per attempt: a retried CAS may see a record another writer has
		// already moved out of pending.
		confirmed = r.VpcId == vpcID && r.AttachState == AttachStatePending
		if !confirmed {
			return false, nil
		}
		r.AttachState = attachStateAttached
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("confirm IGW attachment %s: %w", recordKey, err)
	}

	if confirmed {
		slog.InfoContext(ctx, "IGW attachment confirmed", "internetGatewayId", record.InternetGatewayId, "vpcId", record.VpcId)
	}

	return nil
}

func (s *IGWServiceImpl) recordToEC2(record *IGWRecord, accountID string) *ec2.InternetGateway {
	igw := &ec2.InternetGateway{
		InternetGatewayId: aws.String(record.InternetGatewayId),
		OwnerId:           aws.String(accountID),
	}

	// AWS returns no attachment at all unless one exists, so a requested but
	// unconfirmed attach must not be reported as available.
	if record.attachmentVisible() {
		igw.Attachments = []*ec2.InternetGatewayAttachment{
			{
				VpcId: aws.String(record.VpcId),
				State: aws.String(record.State),
			},
		}
	}

	igw.Tags = awstags.MapToEC2(record.Tags)

	return igw
}

// ApplyRecordTags mirrors CreateTags into the owning IGW KV record so
// tag-filtered describes observe tags added after create. Resource ids this
// service does not own are skipped; absent records are a no-op.
func (s *IGWServiceImpl) ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return tagmirror.MirrorKVRecordTags(context.Background(), s.igwKV, accountID, "igw-", input.Resources,
		func(r *IGWRecord) *map[string]string { return &r.Tags },
		tagmirror.MergeTagsMut(input))
}

// RemoveRecordTags mirrors DeleteTags into the owning IGW KV record with
// AWS-faithful delete semantics.
func (s *IGWServiceImpl) RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return tagmirror.MirrorKVRecordTags(context.Background(), s.igwKV, accountID, "igw-", input.Resources,
		func(r *IGWRecord) *map[string]string { return &r.Tags },
		tagmirror.RemoveTagsMut(input))
}

func igwNotFoundError(id string) error {
	return awserrors.IDNotFound(awserrors.ErrorInvalidInternetGatewayIDNotFound, "internetGateway", id)
}
