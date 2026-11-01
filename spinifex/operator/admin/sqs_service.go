package admin

import (
	"context"
	"errors"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/servicestate"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storelaunch"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// SQSStatus separates what the operator asked for from what is running.
// Ready needs a bound store with a recorded server key and a polling worker.
type SQSStatus struct {
	Region          string `json:"Region"`
	Desired         string `json:"Desired"`
	NodeID          string `json:"NodeId,omitempty"`
	Generation      uint64 `json:"Generation"`
	Store           string `json:"Store"`
	StoreInstanceID string `json:"StoreInstanceId,omitempty"`
	WorkerPolling   bool   `json:"WorkerPolling"`
	Ready           bool   `json:"Ready"`
}

// Store progress names in SQSStatus.
const (
	sqsStoreAbsent    = "absent"
	sqsStoreLaunching = "launching"
	sqsStoreUnpinned  = "bound-awaiting-server-key"
	sqsStoreReady     = "bound"
	sqsStoreUnknown   = "unknown"
)

// DescribeSQS reads the desired state and the store and worker evidence.
// A store record that cannot be read is reported unknown, never absent.
func DescribeSQS(ctx context.Context, js jetstream.JetStream, region string, polling func() bool) (*SQSStatus, error) {
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
	out := &SQSStatus{Region: region, Desired: string(rec.Desired), NodeID: rec.Node, Generation: rec.Generation, Store: sqsStoreUnknown}
	if polling != nil {
		out.WorkerPolling = polling()
	}
	records, err := storelaunch.Bind(ctx, js)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		out.Store = sqsStoreAbsent
	} else if err == nil {
		cur, err := records.Current(ctx, region)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			out.Store = sqsStoreAbsent
		case err != nil:
		case cur.State == storelaunch.StateLaunching:
			out.Store, out.StoreInstanceID = sqsStoreLaunching, cur.InstanceID
		case cur.State == storelaunch.StateBound && cur.ServerKeyPin == "":
			out.Store, out.StoreInstanceID = sqsStoreUnpinned, cur.InstanceID
		case cur.State == storelaunch.StateBound:
			out.Store, out.StoreInstanceID = sqsStoreReady, cur.InstanceID
		}
	}
	out.Ready = rec.Desired == servicestate.Enabled && out.Store == sqsStoreReady && out.WorkerPolling
	return out, nil
}

// SQSEnableInput is EnableSQS's request.
type SQSEnableInput struct {
	Region, NodeID                        string
	WorkerInstanceType, StoreInstanceType string
	VolumeSizeGiB                         string
	AccountID                             string
}

// EnableSQS records enabled for the Region first, so a retry resumes an
// interrupted enable, then ensures the worker and the store on the one node.
// It refuses any cluster that is not exactly one node.
func EnableSQS(ctx context.Context, nc *nats.Conn, js jetstream.JetStream, nodeIDs []string, expectedNodes int,
	in SQSEnableInput, polling func() bool, wait time.Duration,
) (*SQSStatus, error) {
	if expectedNodes != 1 || len(nodeIDs) != 1 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "SQS can be enabled only on a single-node cluster")
	}
	if in.NodeID != nodeIDs[0] {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "NodeId must be this cluster's node")
	}
	if in.WorkerInstanceType == "" || in.StoreInstanceType == "" || in.VolumeSizeGiB == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
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
	if _, err := EnsureSQSWorker(ctx, nc, nodeIDs, in.NodeID, in.WorkerInstanceType, in.AccountID, wait); err != nil {
		return nil, err
	}
	if _, err := EnsureSQSStore(ctx, nc, nodeIDs, in.NodeID, in.StoreInstanceType, in.VolumeSizeGiB, in.AccountID, wait); err != nil {
		return nil, err
	}
	return DescribeSQS(ctx, js, in.Region, polling)
}
