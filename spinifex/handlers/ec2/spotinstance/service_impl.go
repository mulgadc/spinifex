package handlers_ec2_spotinstance

import (
	"context"
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
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure SpotInstanceServiceImpl implements SpotInstanceService.
var _ SpotInstanceService = (*SpotInstanceServiceImpl)(nil)

// SpotInstanceServiceImpl persists Spot Instance Requests in NATS JetStream KV.
// Active (open/active) records live in a no-TTL bucket; terminal (closed/cancelled)
// records live in a separate 1-hour-TTL bucket that JetStream auto-purges.
type SpotInstanceServiceImpl struct {
	config     *config.Config
	natsConn   *nats.Conn
	activeKV   nats.KeyValue
	terminalKV nats.KeyValue
}

// NewSpotInstanceServiceImplWithNATS creates a spot instance service with NATS JetStream.
func NewSpotInstanceServiceImplWithNATS(cfg *config.Config, natsConn *nats.Conn) (*SpotInstanceServiceImpl, error) {
	js, err := natsConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	activeKV, err := utils.GetOrCreateKVBucket(js, KVBucketSpotRequests, 10)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketSpotRequests, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketSpotRequests, activeKV, KVBucketSpotRequestsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketSpotRequests, err)
	}

	terminalKV, err := getOrCreateTerminalBucket(js)
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketSpotRequestsTerminal, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketSpotRequestsTerminal, terminalKV, KVBucketSpotRequestsVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", KVBucketSpotRequestsTerminal, err)
	}

	slog.Info("Spot instance service initialized with JetStream KV",
		"active", KVBucketSpotRequests, "terminal", KVBucketSpotRequestsTerminal)

	return &SpotInstanceServiceImpl{
		config:     cfg,
		natsConn:   natsConn,
		activeKV:   activeKV,
		terminalKV: terminalKV,
	}, nil
}

// getOrCreateTerminalBucket creates the terminal bucket with a TTL, mirroring
// the terminated-instances bucket; GetOrCreateKVBucket cannot set a TTL.
func getOrCreateTerminalBucket(js nats.JetStreamContext) (nats.KeyValue, error) {
	kv, err := js.KeyValue(KVBucketSpotRequestsTerminal)
	if err == nil {
		return kv, nil
	}
	if !errors.Is(err, nats.ErrBucketNotFound) {
		return nil, err
	}
	return js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:      KVBucketSpotRequestsTerminal,
		Description: "Terminal Spot Instance Requests (auto-expire after 1 hour)",
		History:     1,
		TTL:         spotTerminalTTL,
	})
}

// PutSpotInstanceRequests stores each request in the active bucket.
func (s *SpotInstanceServiceImpl) PutSpotInstanceRequests(ctx context.Context, input *PutSpotRequestsInput, accountID string) (*PutSpotRequestsOutput, error) {
	for _, req := range input.Requests {
		sirID := aws.StringValue(req.SpotInstanceRequestId)
		if sirID == "" {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		record := SpotRequestRecord{AccountID: accountID, Request: req}
		data, err := json.Marshal(record)
		if err != nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if _, err := s.activeKV.Put(utils.AccountKey(accountID, sirID), data); err != nil {
			slog.ErrorContext(ctx, "PutSpotInstanceRequests: KV put failed", "sirId", sirID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	slog.InfoContext(ctx, "PutSpotInstanceRequests completed", "count", len(input.Requests), "accountID", accountID)
	return &PutSpotRequestsOutput{}, nil
}

var describeSpotRequestsValidFilters = map[string]bool{
	"spot-instance-request-id":   true,
	"state":                      true,
	"instance-id":                true,
	"launch.image-id":            true,
	"launch.instance-type":       true,
	"launch.key-name":            true,
	"type":                       true,
	"launched-availability-zone": true,
	"tag-key":                    true,
}

// DescribeSpotInstanceRequests lists requests from both buckets, merges, and filters.
func (s *SpotInstanceServiceImpl) DescribeSpotInstanceRequests(ctx context.Context, input *ec2.DescribeSpotInstanceRequestsInput, accountID string) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeSpotRequestsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeSpotInstanceRequests: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	idSet := make(map[string]bool)
	for _, id := range input.SpotInstanceRequestIds {
		if id != nil {
			idSet[*id] = true
		}
	}

	active, err := s.listRequests(s.activeKV, accountID)
	if err != nil {
		return nil, err
	}
	terminal, err := s.listRequests(s.terminalKV, accountID)
	if err != nil {
		return nil, err
	}

	found := make(map[string]bool)
	var requests []*ec2.SpotInstanceRequest
	for _, req := range append(active, terminal...) {
		sirID := aws.StringValue(req.SpotInstanceRequestId)
		if len(idSet) > 0 && !idSet[sirID] {
			continue
		}
		if !sirMatchesFilters(req, parsedFilters) {
			continue
		}
		if !filterutil.MatchesTags(parsedFilters, filterutil.EC2TagsToMap(req.Tags)) {
			continue
		}
		found[sirID] = true
		requests = append(requests, req)
	}

	// Requested IDs that don't exist are an error, matching AWS.
	for id := range idSet {
		if !found[id] {
			return nil, errors.New(awserrors.ErrorInvalidSpotInstanceRequestIDNotFound)
		}
	}

	slog.InfoContext(ctx, "DescribeSpotInstanceRequests completed", "count", len(requests), "accountID", accountID)
	return &ec2.DescribeSpotInstanceRequestsOutput{SpotInstanceRequests: requests}, nil
}

// CancelSpotInstanceRequests moves active requests to the terminal bucket as
// cancelled. The instances keep running. Already-terminal/absent IDs are
// idempotent. Returns a cancelled entry for every requested ID.
func (s *SpotInstanceServiceImpl) CancelSpotInstanceRequests(ctx context.Context, input *ec2.CancelSpotInstanceRequestsInput, accountID string) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	if len(input.SpotInstanceRequestIds) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	cancelled := make([]*ec2.CancelledSpotInstanceRequest, 0, len(input.SpotInstanceRequestIds))
	for _, idPtr := range input.SpotInstanceRequestIds {
		sirID := aws.StringValue(idPtr)
		key := utils.AccountKey(accountID, sirID)

		record, err := s.readRecord(s.activeKV, key)
		if err == nil {
			record.Request.State = aws.String(ec2.SpotInstanceStateCancelled)
			setSpotStatus(record.Request, SpotStatusCodeCanceledInstanceRunning,
				"Spot Instance request cancelled; the instance is still running.")
			if err := s.moveToTerminal(key, record); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, nats.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		cancelled = append(cancelled, &ec2.CancelledSpotInstanceRequest{
			SpotInstanceRequestId: aws.String(sirID),
			State:                 aws.String(ec2.CancelSpotInstanceRequestStateCancelled),
		})
	}

	slog.InfoContext(ctx, "CancelSpotInstanceRequests completed", "count", len(cancelled), "accountID", accountID)
	return &ec2.CancelSpotInstanceRequestsOutput{CancelledSpotInstanceRequests: cancelled}, nil
}

// CloseForInstance scans the active bucket for the request fulfilled by
// instanceID and moves it to the terminal bucket as closed. No-op if none match.
// Called in-process by the daemon teardown cleaner when an instance terminates.
func (s *SpotInstanceServiceImpl) CloseForInstance(ctx context.Context, instanceID, accountID string) error {
	if instanceID == "" {
		return nil
	}

	keys, err := s.activeKV.Keys()
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
		record, err := s.readRecord(s.activeKV, key)
		if err != nil {
			continue
		}
		if aws.StringValue(record.Request.InstanceId) != instanceID {
			continue
		}

		record.Request.State = aws.String(ec2.SpotInstanceStateClosed)
		setSpotStatus(record.Request, SpotStatusCodeInstanceTerminatedByUser,
			"Spot Instance request closed; the instance was terminated by the user.")
		if err := s.moveToTerminal(key, record); err != nil {
			return err
		}
		slog.InfoContext(ctx, "CloseForInstance moved SIR to terminal", "instanceId", instanceID,
			"sirId", aws.StringValue(record.Request.SpotInstanceRequestId), "accountID", accountID)
		return nil
	}

	return nil
}

// listRequests reads all account-scoped requests from a bucket.
func (s *SpotInstanceServiceImpl) listRequests(kv nats.KeyValue, accountID string) ([]*ec2.SpotInstanceRequest, error) {
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	prefix := accountID + "."
	var requests []*ec2.SpotInstanceRequest
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		record, err := s.readRecord(kv, key)
		if err != nil {
			slog.Warn("Failed to read spot request record", "key", key, "err", err)
			continue
		}
		requests = append(requests, record.Request)
	}
	return requests, nil
}

// readRecord fetches and unmarshals a single record. The KV Get error (e.g.
// nats.ErrKeyNotFound) is returned unwrapped so callers can match on it.
func (s *SpotInstanceServiceImpl) readRecord(kv nats.KeyValue, key string) (*SpotRequestRecord, error) {
	entry, err := kv.Get(key)
	if err != nil {
		return nil, err
	}
	var record SpotRequestRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &record, nil
}

// moveToTerminal writes the record to the terminal bucket and deletes it from active.
func (s *SpotInstanceServiceImpl) moveToTerminal(key string, record *SpotRequestRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if _, err := s.terminalKV.Put(key, data); err != nil {
		slog.Error("moveToTerminal: terminal put failed", "key", key, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if err := s.activeKV.Delete(key); err != nil {
		slog.Error("moveToTerminal: active delete failed", "key", key, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

// setSpotStatus sets the status code/message and update time on a request.
func setSpotStatus(req *ec2.SpotInstanceRequest, code, message string) {
	req.Status = &ec2.SpotInstanceStatus{
		Code:       aws.String(code),
		Message:    aws.String(message),
		UpdateTime: aws.Time(time.Now().UTC()),
	}
}

// sirMatchesFilters reports whether a request matches all non-tag filters.
// tag:* filters are handled separately via filterutil.MatchesTags.
func sirMatchesFilters(req *ec2.SpotInstanceRequest, filters map[string][]string) bool {
	for name, values := range filters {
		switch {
		case name == "spot-instance-request-id":
			if !filterutil.MatchesAny(values, aws.StringValue(req.SpotInstanceRequestId)) {
				return false
			}
		case name == "state":
			if !filterutil.MatchesAny(values, aws.StringValue(req.State)) {
				return false
			}
		case name == "instance-id":
			if !filterutil.MatchesAny(values, aws.StringValue(req.InstanceId)) {
				return false
			}
		case name == "type":
			if !filterutil.MatchesAny(values, aws.StringValue(req.Type)) {
				return false
			}
		case name == "launched-availability-zone":
			if !filterutil.MatchesAny(values, aws.StringValue(req.LaunchedAvailabilityZone)) {
				return false
			}
		case name == "launch.image-id":
			if !filterutil.MatchesAny(values, aws.StringValue(launchSpec(req).ImageId)) {
				return false
			}
		case name == "launch.instance-type":
			if !filterutil.MatchesAny(values, aws.StringValue(launchSpec(req).InstanceType)) {
				return false
			}
		case name == "launch.key-name":
			if !filterutil.MatchesAny(values, aws.StringValue(launchSpec(req).KeyName)) {
				return false
			}
		case name == "tag-key":
			if !sirMatchesTagKey(req.Tags, values) {
				return false
			}
		case strings.HasPrefix(name, "tag:"):
			// Handled by filterutil.MatchesTags.
		default:
			return false
		}
	}
	return true
}

func sirMatchesTagKey(tags []*ec2.Tag, values []string) bool {
	for _, t := range tags {
		if t.Key != nil && filterutil.MatchesAny(values, *t.Key) {
			return true
		}
	}
	return false
}

// launchSpec returns the request's launch specification, or an empty one when
// absent, so field reads in filter matching need no nil guard.
func launchSpec(req *ec2.SpotInstanceRequest) *ec2.LaunchSpecification {
	if req.LaunchSpecification != nil {
		return req.LaunchSpecification
	}
	return &ec2.LaunchSpecification{}
}
