package accountlifecycle

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	"github.com/nats-io/nats.go"
)

// TemplateConsumerReader answers which launch templates the account's EKS
// nodegroups still name. It reads every nodegroup record whatever its status,
// including DELETING and CREATE_FAILED, because the record is what a worker
// launch reads. Any listing or describe error is an error, never a short answer.
type TemplateConsumerReader struct {
	svc ekscluster.EKSService
}

var _ accountcleanup.TemplateConsumerReader = (*TemplateConsumerReader)(nil)

// NewTemplateConsumerReader reads nodegroups over the EKS service on NATS.
func NewTemplateConsumerReader(nc *nats.Conn) *TemplateConsumerReader {
	return &TemplateConsumerReader{svc: ekscluster.NewNATSEKSService(nc)}
}

func (r *TemplateConsumerReader) AccountTemplateConsumers(ctx context.Context, accountID string) (*accountcleanup.TemplateConsumers, error) {
	clusters, err := listEKSClusterNames(ctx, r.svc, accountID)
	if err != nil {
		return nil, fmt.Errorf("list eks clusters: %w", err)
	}
	out := &accountcleanup.TemplateConsumers{TemplateIDs: map[string]bool{}, TemplateNames: map[string]bool{}}
	for _, cluster := range clusters {
		listed, err := r.svc.ListNodegroups(ctx, &eks.ListNodegroupsInput{ClusterName: aws.String(cluster)}, accountID)
		if err != nil {
			// A cluster deleted between the two calls holds no nodegroup record,
			// because DeleteCluster is refused while any exists.
			if isAlreadyGone(err) {
				continue
			}
			return nil, fmt.Errorf("list nodegroups: %w", err)
		}
		for _, name := range listed.Nodegroups {
			described, err := r.svc.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{
				ClusterName: aws.String(cluster), NodegroupName: name,
			}, accountID)
			if err != nil {
				if isAlreadyGone(err) {
					continue
				}
				return nil, fmt.Errorf("describe nodegroup: %w", err)
			}
			if described == nil || described.Nodegroup == nil {
				return nil, fmt.Errorf("describe nodegroup: empty reply")
			}
			if lt := described.Nodegroup.LaunchTemplate; lt != nil {
				if id := aws.StringValue(lt.Id); id != "" {
					out.TemplateIDs[id] = true
				}
				if template := aws.StringValue(lt.Name); template != "" {
					out.TemplateNames[template] = true
				}
			}
		}
	}
	return out, nil
}
