package accountteardown

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	"github.com/nats-io/nats.go"
)

// ECSReapers returns the ECS-backed reapers.
//
// One reaper, not two. DeleteCluster already force-stops every task, marks
// every service INACTIVE and sweeps the cluster's whole KV subtree, so a
// separate service reaper would duplicate that — and worse, DeleteService
// leaves an INACTIVE record that still lists, so a stage waiting for services
// to disappear would never drain.
func ECSReapers(nc *nats.Conn) []Reaper {
	return []Reaper{&ecsClusterReaper{svc: handlers_ecs.NewNATSECSService(nc)}}
}

// ecsClusterReaper removes the account's ECS clusters.
//
// Its container instances are ordinary EC2 instances in the tenant's account,
// so the instance reaper takes them — but only after this one has stopped the
// tasks riding on them, which is why this is registered first.
type ecsClusterReaper struct {
	svc handlers_ecs.ECSService
}

func (r *ecsClusterReaper) Kind() string { return "ecs-cluster" }
func (r *ecsClusterReaper) Stage() Stage { return StageCompute }

func (r *ecsClusterReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := r.svc.ListClusters(ctx, &ecs.ListClustersInput{}, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, arn := range out.ClusterArns {
		name := ecsClusterName(aws.StringValue(arn))
		if name == "" {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: name})
	}
	return found, nil
}

func (r *ecsClusterReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteCluster(ctx, &ecs.DeleteClusterInput{
		Cluster: aws.String(resource.ID),
	}, accountID)
	return ignoreAlreadyGone(err)
}

// ecsClusterName reduces a cluster ARN to the name the API takes back. The
// name is the cluster's identity in ECS, and it is what an operator reading a
// stuck line needs to see.
func ecsClusterName(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "cluster/"); i >= 0 {
		return ref[i+len("cluster/"):]
	}
	return ref
}
