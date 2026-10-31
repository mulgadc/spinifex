package admin

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storelaunch"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// EnsureSQSStore asks the node the Region's worker is pinned to to ensure the
// SQS store guest. The reply names the bound VM and its address; it does not
// mean the store is ready, which only the worker's own check establishes.
func EnsureSQSStore(ctx context.Context, nc *nats.Conn, nodeIDs []string, nodeID, instanceType, sizeGiB, accountID string, wait time.Duration) (*storelaunch.EnsureResponse, error) {
	if nodeID == "" || instanceType == "" || sizeGiB == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	size, err := strconv.ParseInt(sizeGiB, 10, 64)
	if err != nil || size < 1 || size > storelaunch.MaxSizeGiB {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "VolumeSizeGiB must be between 1 and 16384")
	}
	subject, err := storelaunch.EnsureSubject(nodeID)
	if err != nil || (len(nodeIDs) > 0 && !slices.Contains(nodeIDs, nodeID)) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "NodeId is not a node of this cluster")
	}
	out, err := natsmsg.NATSRequest[storelaunch.EnsureResponse](ctx, nc, subject,
		storelaunch.EnsureRequest{NodeID: nodeID, InstanceType: instanceType, VolumeSizeGiB: size}, wait, accountID)
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "the node is not serving SQS store launches")
	case errors.Is(err, nats.ErrTimeout):
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable,
			"no reply from the node; the launch may still be running, call again to see its outcome")
	case err != nil:
		return nil, err
	}
	if out.State != storelaunch.StateBoundResponse || out.InstanceID == "" || out.Address == "" || out.NodeID != nodeID ||
		out.InstanceType != instanceType {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return out, nil
}
