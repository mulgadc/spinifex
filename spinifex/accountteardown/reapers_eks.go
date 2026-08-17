package accountteardown

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// EKSReapers returns the EKS-backed reapers, nodegroups before clusters.
//
// DeleteCluster tears down the NLB, the control-plane VMs, the security groups
// and the managed control-plane VPC, but it does not touch nodegroups. Deleting
// the cluster first would strand every worker node with nothing left to
// address it by.
func EKSReapers(nc *nats.Conn) []Reaper {
	svc := handlers_eks.NewNATSEKSService(nc)
	return []Reaper{
		&eksNodegroupReaper{svc: svc},
		&eksClusterReaper{svc: svc},
	}
}

// eksNodegroupSeparator joins a nodegroup's cluster to its name. A nodegroup
// name is only unique within its cluster, so the pair is the identity.
const eksNodegroupSeparator = "/"

type eksNodegroupReaper struct {
	svc handlers_eks.EKSService
}

func (r *eksNodegroupReaper) Kind() string { return "eks-nodegroup" }
func (r *eksNodegroupReaper) Stage() Stage { return StageCompute }

func (r *eksNodegroupReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	clusters, err := listEKSClusterNames(ctx, r.svc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, cluster := range clusters {
		out, err := r.svc.ListNodegroups(ctx, &eks.ListNodegroupsInput{
			ClusterName: aws.String(cluster),
		}, accountID)
		if err != nil {
			// The cluster went between the two calls, which is the ordinary
			// result of a concurrent delete rather than a reason to fail the
			// whole listing.
			if isAlreadyGone(err) {
				continue
			}
			return nil, err
		}
		for _, name := range out.Nodegroups {
			if aws.StringValue(name) == "" {
				continue
			}
			found = append(found, Resource{
				Kind: r.Kind(),
				ID:   cluster + eksNodegroupSeparator + aws.StringValue(name),
			})
		}
	}
	return found, nil
}

func (r *eksNodegroupReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	cluster, nodegroup, ok := strings.Cut(resource.ID, eksNodegroupSeparator)
	if !ok {
		return nil
	}
	_, err := r.svc.DeleteNodegroup(ctx, &eks.DeleteNodegroupInput{
		ClusterName:   aws.String(cluster),
		NodegroupName: aws.String(nodegroup),
	}, accountID)
	return ignoreAlreadyGone(err)
}

type eksClusterReaper struct {
	svc handlers_eks.EKSService
}

func (r *eksClusterReaper) Kind() string { return "eks-cluster" }
func (r *eksClusterReaper) Stage() Stage { return StageCompute }

func (r *eksClusterReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	names, err := listEKSClusterNames(ctx, r.svc, accountID)
	if err != nil {
		return nil, err
	}

	found := make([]Resource, 0, len(names))
	for _, name := range names {
		found = append(found, Resource{Kind: r.Kind(), ID: name})
	}
	return found, nil
}

func (r *eksClusterReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteCluster(ctx, &eks.DeleteClusterInput{
		Name: aws.String(resource.ID),
	}, accountID)
	return ignoreAlreadyGone(err)
}

// eksListPageLimit bounds a runaway pagination loop. A tenant is nowhere near
// this many clusters; the cap exists so a NextToken that never advances cannot
// spin the drain loop forever.
const eksListPageLimit = 100

// listEKSClusterNames pages through ListClusters. The listing is capped at 100
// per page, so taking only the first page would silently leave every cluster
// past it behind — and a teardown that misses one never converges.
func listEKSClusterNames(ctx context.Context, svc handlers_eks.EKSService, accountID string) ([]string, error) {
	var names []string
	var token *string

	for range eksListPageLimit {
		out, err := svc.ListClusters(ctx, &eks.ListClustersInput{NextToken: token}, accountID)
		if err != nil {
			return nil, err
		}
		for _, name := range out.Clusters {
			if aws.StringValue(name) != "" {
				names = append(names, aws.StringValue(name))
			}
		}
		if out.NextToken == nil || aws.StringValue(out.NextToken) == "" {
			break
		}
		token = out.NextToken
	}
	return names, nil
}
