package cluster

import (
	"context"
	"errors"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// ListManagedWorkersInput names the cluster whose managed-nodegroup workers to
// list. It is an internal control-plane request (not an AWS-SDK shape), served
// over NATS for the guest node-gc agent via the internal-workers route.
type ListManagedWorkersInput struct {
	ClusterName string `json:"clusterName"`
}

// ListManagedWorkersOutput carries the instance ID of every worker a managed
// nodegroup of the cluster still owns, sorted.
type ListManagedWorkersOutput struct {
	InstanceIDs []string `json:"instanceIds"`
}

// ListManagedWorkers returns every worker instance ID recorded on the cluster's
// nodegroups. A nodegroup drops an ID only after terminating it (DeleteNodegroup,
// scale-down, reclaim), so the guest deletes a nodegroup Node whose instance is
// absent here, as the AWS cloud controller deletes a Node whose instance is gone.
func (s *EKSServiceImpl) ListManagedWorkers(ctx context.Context, input *ListManagedWorkersInput, accountID string) (*ListManagedWorkersOutput, error) {
	if input == nil || input.ClusterName == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.acctKVForCluster(ctx, accountID, input.ClusterName)
	if err != nil {
		return nil, err
	}
	recs, err := ListNodegroupRecords(ctx, acctKV, input.ClusterName)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for _, rec := range recs {
		ids = append(ids, rec.InstanceIDs...)
	}
	slices.Sort(ids)
	return &ListManagedWorkersOutput{InstanceIDs: slices.Compact(ids)}, nil
}
