package admin

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// sqsWorkerEnsureWait bounds how long the action waits for the node. A launch
// outlives it; calling again reports the bound worker or an unknown outcome.
const sqsWorkerEnsureWait = 10 * time.Minute

// EnsureSQSWorker asks one node to ensure its Region's shared SQS worker. The
// reply names the bound VM; it does not mean the worker or SQS is ready.
func EnsureSQSWorker(ctx context.Context, nc *nats.Conn, nodeIDs []string, nodeID, instanceType string, sizeGiB int64, accountID string, wait time.Duration) (*workerlaunch.EnsureResponse, error) {
	if nodeID == "" || instanceType == "" || sizeGiB == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	subject, err := workerlaunch.EnsureSubject(nodeID)
	if err != nil || (len(nodeIDs) > 0 && !slices.Contains(nodeIDs, nodeID)) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "NodeId is not a node of this cluster")
	}
	out, err := natsmsg.NATSRequest[workerlaunch.EnsureResponse](ctx, nc, subject,
		workerlaunch.EnsureRequest{NodeID: nodeID, InstanceType: instanceType, VolumeSizeGiB: sizeGiB}, wait, accountID)
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "the node is not serving SQS worker launches")
	case errors.Is(err, nats.ErrTimeout):
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable,
			"no reply from the node; the launch may still be running, call again to see its outcome")
	case err != nil:
		return nil, err
	}
	if out.State != workerlaunch.StateBound || out.InstanceID == "" || out.NodeID != nodeID ||
		out.InstanceType != instanceType {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return out, nil
}

// sqsVolumeSize parses the SQS data volume size argument. An absent value is
// zero, which the callers refuse as a missing parameter.
func sqsVolumeSize(arg string) (int64, error) {
	if arg == "" {
		return 0, nil
	}
	size, err := strconv.ParseInt(arg, 10, 64)
	if err != nil || size < 1 || size > workerident.MaxSizeGiB {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "VolumeSizeGiB must be between 1 and 16384")
	}
	return size, nil
}
