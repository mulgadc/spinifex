package awsapi

import (
	"context"
	"errors"

	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// internalWorkersOutput is the body returned to the on-VM node-gc agent: the
// instance ID of every worker the cluster's nodegroups still own.
type internalWorkersOutput struct {
	InstanceIDs []string `json:"instanceIds"`
}

// ListInternalWorkers — GET /clusters/{name}/internal-workers/{acct}.
// Internal control-plane VM route (not an AWS-SDK action): same carve-out as
// ListInternalAddons, gated by AuthorizeInternal, which admits only a CP agent
// serving that account's cluster.
func ListInternalWorkers(ctx context.Context, natsConn *nats.Conn, clusterName, accountID string) (*internalWorkersOutput, error) {
	if natsConn == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if clusterName == "" || accountID == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	out, err := ekscluster.NewNATSEKSService(natsConn).ListManagedWorkers(ctx,
		&ekscluster.ListManagedWorkersInput{ClusterName: clusterName}, accountID)
	if err != nil {
		return nil, err
	}
	return &internalWorkersOutput{InstanceIDs: out.InstanceIDs}, nil
}
