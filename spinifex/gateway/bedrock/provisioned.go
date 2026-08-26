package gateway_bedrock

import (
	"context"
	"errors"
	"slices"
	"time"
	"uuid"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// EndpointProvisioner is the narrow surface the provisioned-throughput ops
// need from the daemon's endpoint lifecycle (handlers_bedrock.EndpointService):
// launch a pinned endpoint, read its state, and tear it down. It is declared
// here with primitive-typed methods rather than that package's input/output
// structs because handlers_bedrock already imports this package
// (LookupServingSpec), so importing it back here would cycle. See
// handlers_bedrock.ProvisionedEndpointAdapter for the real implementation
// over EndpointService; tests in this package use a stub instead.
type EndpointProvisioner interface {
	// EnsurePinned requests a pinned, account-scoped endpoint for modelID.
	EnsurePinned(ctx context.Context, accountID, modelID string) error
	// EndpointState reports (accountID, modelID)'s current endpoint state:
	// one of the endpointState* constants below, mirroring
	// handlers_bedrock.EndpointState's string values.
	EndpointState(ctx context.Context, accountID, modelID string) (state string, err error)
	// DeletePinned tears down (accountID, modelID)'s pinned endpoint.
	// Idempotent: an already-absent endpoint is a success.
	DeletePinned(ctx context.Context, accountID, modelID string) error
}

// Endpoint state strings EndpointProvisioner.EndpointState returns, mirroring
// handlers_bedrock.EndpointState's string values without importing that
// package back.
const (
	endpointStateStarting = "STARTING"
	endpointStateReady    = "READY"
)

// bedrockProvisionedBucket is the cluster-replicated KV bucket holding
// provisioned-throughput commitment records.
const bedrockProvisionedBucket = "bedrock-provisioned"

// bedrockProvisionedHistory keeps one revision per key: a commitment is
// mutated in place (Update, and Status as observed), not a series.
const bedrockProvisionedHistory = 1

// ProvisionedModelRecord is the gateway control-plane state for one
// commitment. The VM it pins is daemon-owned (handlers_bedrock.EndpointRecord);
// this record is the AWS-shaped metadata layered on top of it.
type ProvisionedModelRecord struct {
	ARN                  string `json:"arn"`
	ProvisionedModelName string `json:"provisioned_model_name"`
	ModelID              string `json:"model_id"`
	AccountID            string `json:"account_id"`
	ModelUnits           int64  `json:"model_units"`
	CommitmentDuration   string `json:"commitment_duration,omitempty"`
	// Status is the value written at Create/Update time (always "Creating"
	// initially). Get derives the live value from the pinned endpoint's
	// current state instead of trusting this field, which List does not.
	Status           string    `json:"status"`
	CreationTime     time.Time `json:"creation_time"`
	LastModifiedTime time.Time `json:"last_modified_time"`
}

// ProvisionedStore persists ProvisionedModelRecords in the bedrock-provisioned
// JetStream KV bucket and drives the daemon endpoint lifecycle underneath
// each commitment, mirroring the LoggingConfigStore/ModelAccessStore
// gateway-direct-KV pattern.
type ProvisionedStore struct {
	store *kvstore.Store[ProvisionedModelRecord]
	// region is baked in at construction (the gateway's own region never
	// changes at runtime) so ARN building/parsing needs no extra parameter
	// threaded through the fixed-arity route table.
	region   string
	endpoint EndpointProvisioner
}

// NewProvisionedStore constructs a ProvisionedStore over js, replicated
// across replicas nodes, driving endpoint launches/teardowns through
// endpoint.
func NewProvisionedStore(js jetstream.JetStream, replicas int, region string, endpoint EndpointProvisioner) *ProvisionedStore {
	return &ProvisionedStore{
		store: kvstore.New[ProvisionedModelRecord](js, kvstore.Config{
			Name:     bedrockProvisionedBucket,
			History:  bedrockProvisionedHistory,
			Replicas: replicas,
			Missing:  "bedrock: provisioned store has no JetStream client configured",
		}),
		region:   region,
		endpoint: endpoint,
	}
}

// provisionedKey scopes every stored record to its owning account, so List's
// prefix scan is the only thing that ever needs to see across ids, and a
// foreign account's raw-id guess can never collide with another tenant's key.
func provisionedKey(accountID, id string) string {
	return accountID + "/" + id
}

// get reads accountID's record for id, or (zero, false, nil) if it does not
// exist.
func (s *ProvisionedStore) get(ctx context.Context, accountID, id string) (ProvisionedModelRecord, bool, error) {
	rec, _, found, err := s.getRevision(ctx, provisionedKey(accountID, id))
	return rec, found, err
}

// getRevision is get with the KV revision surfaced too, for Update's CAS
// write.
func (s *ProvisionedStore) getRevision(ctx context.Context, key string) (ProvisionedModelRecord, uint64, bool, error) {
	rec, rev, err := s.store.Get(ctx, key)
	if errors.Is(err, kvstore.ErrNotFound) {
		return ProvisionedModelRecord{}, 0, false, nil
	}
	if err != nil {
		return ProvisionedModelRecord{}, 0, false, err
	}
	return *rec, rev, true, nil
}

// update CAS-writes rec back over rev, the revision it was read at. A lost
// race is reported as a retryable ConflictException rather than silently
// clobbering the winner's write.
func (s *ProvisionedStore) update(ctx context.Context, key string, rec ProvisionedModelRecord, rev uint64) error {
	err := s.store.CompareAndSet(ctx, key, &rec, rev)
	if errors.Is(err, kvstore.ErrConflict) {
		return awserrors.Errorf(awserrors.ErrorConflictException,
			"bedrock: provisioned throughput %s was modified concurrently; retry the request", rec.ARN)
	}
	return err
}

// committedModelUnits sums ModelUnits across every commitment servingAccountID
// holds for modelID. Every such commitment pins the same daemon endpoint
// (EnsurePinned is keyed on (accountID, modelID) alone), so their capacity
// stacks onto one shared serving VM rather than being read independently.
// An empty servingAccountID (the shared ON_DEMAND path, which has no
// commitment to read) returns 0 without touching the store.
func committedModelUnits(ctx context.Context, store *ProvisionedStore, servingAccountID, modelID string) (int64, error) {
	if servingAccountID == "" {
		return 0, nil
	}
	recs, err := store.store.List(ctx, servingAccountID+"/")
	if err != nil {
		return 0, err
	}

	var total int64
	for _, rec := range recs {
		if rec.ModelID == modelID {
			total += rec.ModelUnits
		}
	}
	return total, nil
}

// deriveStatus reports the AWS-shaped status for (accountID, modelID)'s
// pinned endpoint. Anything other than STARTING/READY — including an absent
// endpoint, DRAINING (only ever transient during this package's own Delete),
// or a launch that reverted to ABSENT on failure — reads as Failed: there is
// no persisted FAILED state in the daemon's own state machine to distinguish
// "never launched" from "launch failed", and both are equally not-serving.
func deriveStatus(ctx context.Context, endpoint EndpointProvisioner, accountID, modelID string) (string, error) {
	state, err := endpoint.EndpointState(ctx, accountID, modelID)
	if err != nil {
		return "", err
	}
	switch state {
	case endpointStateStarting:
		return bedrock.ProvisionedModelStatusCreating, nil
	case endpointStateReady:
		return bedrock.ProvisionedModelStatusInService, nil
	default:
		return bedrock.ProvisionedModelStatusFailed, nil
	}
}

// nonEmptyStringPtr returns nil for an empty string, so an optional
// AWS-shaped field is omitted rather than rendered as an empty string.
func nonEmptyStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

// CreateProvisionedModelThroughput commits a pinned, account-scoped endpoint
// for input.ModelId. Self-host only: a provider-hosted model has nothing to
// provision capacity for. Creation is async, mirroring AWS: the record is
// written Status:Creating and a caller polls Get for InService.
//
// The launch request and the KV write are not atomic — a crash between them
// leaves a pinned endpoint with no commitment record — but the daemon's own
// Ensure is idempotent, so a retried Create converges on the same endpoint
// rather than doubling up.
func CreateProvisionedModelThroughput(ctx context.Context, accountID string, store *ProvisionedStore, input *bedrock.CreateProvisionedModelThroughputInput) (*bedrock.CreateProvisionedModelThroughputOutput, error) {
	if input == nil || aws.StringValue(input.ModelId) == "" || aws.StringValue(input.ProvisionedModelName) == "" || input.ModelUnits == nil {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	modelID := aws.StringValue(input.ModelId)

	if _, found, selfHost := LookupServingSpec(modelID); !found || !selfHost {
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}

	// Resolve the bucket before pinning an endpoint, so an unreachable KV
	// fails without launching a VM whose commitment record can never be
	// written.
	if _, err := store.store.KV(ctx); err != nil {
		return nil, err
	}

	id := uuid.NewV4().String()
	arn := FormatProvisionedModelARN(store.region, accountID, id)

	if err := store.endpoint.EnsurePinned(ctx, accountID, modelID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	rec := ProvisionedModelRecord{
		ARN:                  arn,
		ProvisionedModelName: aws.StringValue(input.ProvisionedModelName),
		ModelID:              modelID,
		AccountID:            accountID,
		ModelUnits:           aws.Int64Value(input.ModelUnits),
		CommitmentDuration:   aws.StringValue(input.CommitmentDuration),
		Status:               bedrock.ProvisionedModelStatusCreating,
		CreationTime:         now,
		LastModifiedTime:     now,
	}
	if err := store.store.Set(ctx, provisionedKey(accountID, id), &rec); err != nil {
		return nil, err
	}

	return &bedrock.CreateProvisionedModelThroughputOutput{ProvisionedModelArn: aws.String(arn)}, nil
}

// GetProvisionedModelThroughput returns input.ProvisionedModelId's commitment,
// with Status reflecting the pinned endpoint's live state rather than the
// value last written to the record.
func GetProvisionedModelThroughput(ctx context.Context, accountID string, store *ProvisionedStore, input *bedrock.GetProvisionedModelThroughputInput) (*bedrock.GetProvisionedModelThroughputOutput, error) {
	if input == nil || aws.StringValue(input.ProvisionedModelId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id, err := resolveProvisionedModelID(aws.StringValue(input.ProvisionedModelId), store.region, accountID)
	if err != nil {
		return nil, err
	}

	rec, found, err := store.get(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}

	status, err := deriveStatus(ctx, store.endpoint, accountID, rec.ModelID)
	if err != nil {
		return nil, err
	}

	fmArn := modelARN(rec.ModelID)
	return &bedrock.GetProvisionedModelThroughputOutput{
		CommitmentDuration:   nonEmptyStringPtr(rec.CommitmentDuration),
		CreationTime:         aws.Time(rec.CreationTime),
		DesiredModelArn:      aws.String(fmArn),
		DesiredModelUnits:    aws.Int64(rec.ModelUnits),
		FoundationModelArn:   aws.String(fmArn),
		LastModifiedTime:     aws.Time(rec.LastModifiedTime),
		ModelArn:             aws.String(fmArn),
		ModelUnits:           aws.Int64(rec.ModelUnits),
		ProvisionedModelArn:  aws.String(rec.ARN),
		ProvisionedModelName: aws.String(rec.ProvisionedModelName),
		Status:               aws.String(status),
	}, nil
}

// ListProvisionedModelThroughputs returns accountID's own commitments only,
// sorted by creation time, so one tenant never sees another's. Status is the
// last value written to each record (Create's Creating, unchanged by
// Update), not live-derived — unlike Get, a list scan does not pay for one
// endpoint describe per commitment.
func ListProvisionedModelThroughputs(ctx context.Context, accountID string, store *ProvisionedStore, _ *bedrock.ListProvisionedModelThroughputsInput) (*bedrock.ListProvisionedModelThroughputsOutput, error) {
	recs, err := store.store.List(ctx, accountID+"/")
	if err != nil {
		return nil, err
	}

	slices.SortFunc(recs, func(a, b ProvisionedModelRecord) int {
		return a.CreationTime.Compare(b.CreationTime)
	})

	summaries := make([]*bedrock.ProvisionedModelSummary, 0, len(recs))
	for _, rec := range recs {
		fmArn := modelARN(rec.ModelID)
		summaries = append(summaries, &bedrock.ProvisionedModelSummary{
			CommitmentDuration:   nonEmptyStringPtr(rec.CommitmentDuration),
			CreationTime:         aws.Time(rec.CreationTime),
			DesiredModelArn:      aws.String(fmArn),
			DesiredModelUnits:    aws.Int64(rec.ModelUnits),
			FoundationModelArn:   aws.String(fmArn),
			LastModifiedTime:     aws.Time(rec.LastModifiedTime),
			ModelArn:             aws.String(fmArn),
			ModelUnits:           aws.Int64(rec.ModelUnits),
			ProvisionedModelArn:  aws.String(rec.ARN),
			ProvisionedModelName: aws.String(rec.ProvisionedModelName),
			Status:               aws.String(rec.Status),
		})
	}
	return &bedrock.ListProvisionedModelThroughputsOutput{ProvisionedModelSummaries: summaries}, nil
}

// UpdateProvisionedModelThroughput honours name changes only. AWS allows a
// limited desired-model swap within a custom model's family; this platform
// has no custom models, so any attempt to change the served model — as
// opposed to re-stating the model it already serves — is rejected outright.
func UpdateProvisionedModelThroughput(ctx context.Context, accountID string, store *ProvisionedStore, input *bedrock.UpdateProvisionedModelThroughputInput) (*bedrock.UpdateProvisionedModelThroughputOutput, error) {
	if input == nil || aws.StringValue(input.ProvisionedModelId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id, err := resolveProvisionedModelID(aws.StringValue(input.ProvisionedModelId), store.region, accountID)
	if err != nil {
		return nil, err
	}

	key := provisionedKey(accountID, id)
	rec, rev, found, err := store.getRevision(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorResourceNotFoundException)
	}

	if desired := aws.StringValue(input.DesiredModelId); desired != "" && desired != rec.ModelID && desired != modelARN(rec.ModelID) {
		return nil, awserrors.Errorf(awserrors.ErrorValidationException,
			"bedrock: provisioned throughput %s cannot change its served model", id)
	}
	if name := aws.StringValue(input.DesiredProvisionedModelName); name != "" {
		rec.ProvisionedModelName = name
	}
	rec.LastModifiedTime = time.Now().UTC()

	if err := store.update(ctx, key, rec, rev); err != nil {
		return nil, err
	}
	return &bedrock.UpdateProvisionedModelThroughputOutput{}, nil
}

// DeleteProvisionedModelThroughput tears down the pinned endpoint for
// input.ProvisionedModelId's model, then removes the record. An already-absent
// commitment is a no-op success, matching handlers_bedrock.Service.Delete's
// own idempotence.
func DeleteProvisionedModelThroughput(ctx context.Context, accountID string, store *ProvisionedStore, input *bedrock.DeleteProvisionedModelThroughputInput) (*bedrock.DeleteProvisionedModelThroughputOutput, error) {
	if input == nil || aws.StringValue(input.ProvisionedModelId) == "" {
		return nil, errors.New(awserrors.ErrorValidationException)
	}
	id, err := resolveProvisionedModelID(aws.StringValue(input.ProvisionedModelId), store.region, accountID)
	if err != nil {
		return nil, err
	}

	rec, found, err := store.get(ctx, accountID, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return &bedrock.DeleteProvisionedModelThroughputOutput{}, nil
	}

	if err := store.endpoint.DeletePinned(ctx, accountID, rec.ModelID); err != nil {
		return nil, err
	}
	if err := store.store.Delete(ctx, provisionedKey(accountID, id)); err != nil {
		return nil, err
	}
	return &bedrock.DeleteProvisionedModelThroughputOutput{}, nil
}
