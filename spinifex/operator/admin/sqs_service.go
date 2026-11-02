package admin

import (
	"context"
	"errors"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/servicestate"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// SQSStatus separates what the operator asked for from what the SQS VM was
// last observed doing. Ready also needs the tenant API registered here.
type SQSStatus struct {
	Region     string `json:"Region"`
	Desired    string `json:"Desired"`
	NodeID     string `json:"NodeId,omitempty"`
	Generation uint64 `json:"Generation"`
	// Instance is the SQS VM's launch progress; ReceiptKey its receipt key
	// initialization, which is recorded only by the booted guest.
	Instance      string `json:"Instance"`
	InstanceID    string `json:"InstanceId,omitempty"`
	ReceiptKey    string `json:"ReceiptKey,omitempty"`
	WorkerPolling bool   `json:"WorkerPolling"`
	TenantAPI     string `json:"TenantAPI"`
	// Unsupported names an SQS VM whose launch contract blocks enable.
	Unsupported string `json:"Unsupported,omitempty"`
	Ready       bool   `json:"Ready"`
}

// SQSObservers are this gateway's own observations. WorkerPolling is true
// only while a worker whose local store is up has polled recently, and
// TenantAPIRegistered only when the composed gateway serves the SQS API.
type SQSObservers struct {
	WorkerPolling       func() bool
	TenantAPIRegistered func() bool
}

const (
	sqsInstanceAbsent  = "absent"
	sqsInstanceUnknown = "unknown"

	sqsTenantAPIRegistered    = "registered"
	sqsTenantAPINotRegistered = "not-registered"
)

// DescribeSQS reads the desired state and the SQS VM's evidence. A record
// that cannot be read is reported unknown, never absent.
func DescribeSQS(ctx context.Context, js jetstream.JetStream, region string, obs SQSObservers) (*SQSStatus, error) {
	st, err := servicestate.Bind(ctx, js)
	var rec servicestate.Record
	switch {
	case errors.Is(err, jetstream.ErrBucketNotFound):
		rec = servicestate.Record{Region: region, Desired: servicestate.Disabled}
	case err != nil:
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "SQS service state unavailable")
	default:
		if rec, err = st.Get(ctx, region); err != nil {
			return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "SQS service state unavailable")
		}
	}
	out := &SQSStatus{Region: region, Desired: string(rec.Desired), NodeID: rec.Node, Generation: rec.Generation,
		Instance: sqsInstanceUnknown}
	if obs.WorkerPolling != nil {
		out.WorkerPolling = obs.WorkerPolling()
	}
	registered := obs.TenantAPIRegistered != nil && obs.TenantAPIRegistered()
	out.TenantAPI = sqsTenantAPINotRegistered
	if registered {
		out.TenantAPI = sqsTenantAPIRegistered
	}
	cur, found, err := currentWorker(ctx, js, region)
	legacy, lerr := legacyStorePresent(ctx, js, region)
	switch {
	case err != nil || lerr != nil:
		out.Unsupported = "unknown: SQS records unreadable"
	case !found:
		out.Instance = sqsInstanceAbsent
	default:
		out.Instance, out.InstanceID, out.ReceiptKey = string(cur.State), cur.InstanceID, string(cur.ReceiptInit)
		if cur.UnsupportedContract() {
			out.Unsupported = sqsUnsupportedReason(cur.InstanceID)
		}
	}
	if legacy && out.Unsupported == "" {
		out.Unsupported = sqsLegacyStoreReason
	}
	out.Ready = registered && rec.Desired == servicestate.Enabled && out.Instance == string(workerident.StateBound) &&
		out.ReceiptKey == string(workerident.ReceiptRecorded) && out.WorkerPolling && out.Unsupported == ""
	return out, nil
}

// currentWorker reads the Region's SQS VM record without creating anything.
func currentWorker(ctx context.Context, js jetstream.JetStream, region string) (workerident.Record, bool, error) {
	workers, err := workerident.Bind(ctx, js)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return workerident.Record{}, false, nil
	}
	if err != nil {
		return workerident.Record{}, false, err
	}
	cur, err := workers.Current(ctx, region)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return workerident.Record{}, false, nil
	}
	return cur, err == nil, err
}

// legacyStoreBucket held the separate SQS store VM's launch records, which this
// release does not run. It is only ever bound, never created or changed.
const legacyStoreBucket = "spinifex-sqs-store-launch"

// legacyStorePresent reports whether the Region has a separate store record.
func legacyStorePresent(ctx context.Context, js jetstream.JetStream, region string) (bool, error) {
	kv, err := js.KeyValue(ctx, legacyStoreBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = kv.Get(ctx, "region."+region)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	return err == nil, err
}

const sqsLegacyStoreReason = "a separate SQS store VM record exists, which this release does not run; enable refused; any existing volume is kept"

func sqsUnsupportedReason(instanceID string) string {
	if instanceID == "" {
		instanceID = "(not launched)"
	}
	return "SQS instance " + instanceID + " has a missing or unsupported launch contract; enable refused; any existing volume is kept"
}

// SQSEnableInput is EnableSQS's request.
type SQSEnableInput struct {
	Region, NodeID string
	InstanceType   string
	VolumeSizeGiB  int64
	AccountID      string
}

// EnableSQS records enabled for the Region first, so a retry resumes an
// interrupted enable, then ensures the one SQS VM on the one node. It
// refuses any cluster that is not exactly one node.
func EnableSQS(ctx context.Context, nc *nats.Conn, js jetstream.JetStream, nodeIDs []string, expectedNodes int,
	in SQSEnableInput, obs SQSObservers, wait time.Duration,
) (*SQSStatus, error) {
	if expectedNodes != 1 || len(nodeIDs) != 1 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "SQS can be enabled only on a single-node cluster")
	}
	if in.NodeID != nodeIDs[0] {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "NodeId must be this cluster's node")
	}
	if in.InstanceType == "" || in.VolumeSizeGiB == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	// Refused before any write or allocation; nothing is deleted or replaced.
	cur, found, err := currentWorker(ctx, js, in.Region)
	if err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "SQS records unavailable")
	}
	if found && cur.UnsupportedContract() {
		return nil, awserrors.Errorf(awserrors.ErrorIncorrectState, "%s", sqsUnsupportedReason(cur.InstanceID))
	}
	switch legacy, err := legacyStorePresent(ctx, js, in.Region); {
	case err != nil:
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "SQS records unavailable")
	case legacy:
		return nil, awserrors.Errorf(awserrors.ErrorIncorrectState, "%s", sqsLegacyStoreReason)
	}
	st, err := servicestate.Open(ctx, js)
	if err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "SQS service state unavailable")
	}
	if _, err := st.Set(ctx, in.Region, servicestate.Enabled, in.NodeID, time.Now()); err != nil {
		if errors.Is(err, servicestate.ErrNodeConflict) {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "SQS is already enabled on another node")
		}
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "SQS service state unavailable")
	}
	if _, err := EnsureSQSWorker(ctx, nc, nodeIDs, in.NodeID, in.InstanceType, in.VolumeSizeGiB, in.AccountID, wait); err != nil {
		return nil, err
	}
	return DescribeSQS(ctx, js, in.Region, obs)
}
