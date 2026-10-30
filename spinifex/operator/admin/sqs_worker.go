package admin

import (
	"context"
	"errors"
	"slices"
	"time"

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
func EnsureSQSWorker(ctx context.Context, nc *nats.Conn, nodeIDs []string, nodeID, instanceType, accountID string, wait time.Duration) (*workerlaunch.EnsureResponse, error) {
	if nodeID == "" || instanceType == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	subject, err := workerlaunch.EnsureSubject(nodeID)
	if err != nil || (len(nodeIDs) > 0 && !slices.Contains(nodeIDs, nodeID)) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "NodeId is not a node of this cluster")
	}
	out, err := natsmsg.NATSRequest[workerlaunch.EnsureResponse](ctx, nc, subject,
		workerlaunch.EnsureRequest{NodeID: nodeID, InstanceType: instanceType}, wait, accountID)
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "the node is not serving SQS worker launches")
	case errors.Is(err, nats.ErrTimeout):
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable,
			"no reply from the node; the launch may still be running, call again to see its outcome")
	case err != nil:
		return nil, err
	}
	if out.State != workerlaunch.StateBound || out.InstanceID == "" || out.NodeID != nodeID {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return out, nil
}
