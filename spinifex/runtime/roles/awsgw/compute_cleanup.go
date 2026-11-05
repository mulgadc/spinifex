package awsgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountinventory"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	ec2volumeapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/volume"
	ec2metadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	"github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

var (
	errNoTerminateAck = errors.New("terminate was not acknowledged by the owning node")
	errNoWorkerList   = errors.New("managed worker list is not available")
)

// strictInstances is the complete-or-error instance inventory.
type strictInstances struct {
	nc      *nats.Conn
	nodeIDs []string
}

func (s strictInstances) AccountInstances(ctx context.Context, accountID string) (*ec2.DescribeInstancesOutput, error) {
	return ec2instanceapi.DescribeAccountInstancesStrict(ctx, s.nc, s.nodeIDs, accountID)
}

// instanceTerminator sends the existing EC2 terminate and requires the owning
// node to have acknowledged this instance, because that API skips one it could
// not reach and still answers.
type instanceTerminator struct{ nc *nats.Conn }

func (t instanceTerminator) TerminateInstance(ctx context.Context, accountID, instanceID string) error {
	out, err := ec2instanceapi.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []*string{aws.String(instanceID)}}, t.nc, accountID)
	if err != nil {
		return err
	}
	for _, change := range out.TerminatingInstances {
		if aws.StringValue(change.InstanceId) == instanceID {
			return nil
		}
	}
	return errNoTerminateAck
}

// teardownReader reads unfinished instance teardowns through the owner.
type teardownReader struct{ nc *nats.Conn }

func (r teardownReader) AccountTeardownOutstanding(accountID string) ([]ec2instance.OutstandingInstance, error) {
	return ec2instanceapi.AccountTeardownOutstanding(context.Background(), r.nc, accountID)
}

// guardedVolumeDeleter is the customer DeleteVolume path: it refuses unless
// every node answered the live-holder listing, and never deletes from the store.
type guardedVolumeDeleter struct {
	nc      *nats.Conn
	nodeIDs []string
}

func (d guardedVolumeDeleter) DeleteVolume(ctx context.Context, accountID, volumeID string) error {
	_, err := ec2volumeapi.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(volumeID)}, d.nc, d.nodeIDs, accountID)
	return err
}

// eksManagedInstances is every worker a managed nodegroup of the account still
// owns. A cluster that cannot be listed is an error, never an empty set.
type eksManagedInstances struct{ svc ekscluster.EKSService }

func (m eksManagedInstances) AccountManagedInstances(ctx context.Context, accountID string) (map[string]bool, error) {
	if m.svc == nil {
		return nil, errNoWorkerList
	}
	owned := map[string]bool{}
	var token *string
	for {
		page, err := m.svc.ListClusters(ctx, &eks.ListClustersInput{NextToken: token}, accountID)
		if err != nil {
			return nil, fmt.Errorf("list clusters: %w", err)
		}
		for _, name := range page.Clusters {
			workers, err := m.svc.ListManagedWorkers(ctx, &ekscluster.ListManagedWorkersInput{ClusterName: aws.StringValue(name)}, accountID)
			if err != nil {
				return nil, fmt.Errorf("list workers of cluster %s: %w", aws.StringValue(name), err)
			}
			for _, id := range workers.InstanceIDs {
				owned[id] = true
			}
		}
		if aws.StringValue(page.NextToken) == "" {
			return owned, nil
		}
		token = page.NextToken
	}
}

var (
	_ accountcleanup.InstanceInventory     = strictInstances{}
	_ accountcleanup.InstanceTerminator    = instanceTerminator{}
	_ accountcleanup.TeardownReader        = teardownReader{}
	_ accountcleanup.VolumeDeleter         = guardedVolumeDeleter{}
	_ accountcleanup.ManagedInstanceReader = eksManagedInstances{}
)

// computeCleaners builds the EC2 compute and storage cleanup over the owners'
// own services. Any service that cannot be built fails the whole bind, so
// DeleteAccount refuses instead of running with part of the owners missing.
func computeCleaners(ctx context.Context, node *config.Config, nc *nats.Conn, js jetstream.JetStream, nodeIDs []string, objects objectstore.ObjectStore) (accountlifecycle.ComputeStage, accountlifecycle.StorageStage, error) {
	if nc == nil || js == nil || objects == nil || len(nodeIDs) == 0 {
		return nil, nil, errors.New("compute cleanup needs NATS, JetStream, the object store and the node set")
	}
	tagsKV, err := ec2tags.GetOrCreateTagsBucket(ctx, js)
	if err != nil {
		return nil, nil, fmt.Errorf("tags bucket: %w", err)
	}
	tags := ec2tags.NewTagsServiceImpl(node, tagsKV)
	spot, err := ec2spotinstance.NewSpotInstanceServiceImplWithNATS(ctx, node, nc)
	if err != nil {
		return nil, nil, fmt.Errorf("spot instance service: %w", err)
	}
	groups, err := ec2placementgroup.NewPlacementGroupServiceImplWithNATS(ctx, node, nc)
	if err != nil {
		return nil, nil, fmt.Errorf("placement group service: %w", err)
	}
	templates, err := ec2launchtemplate.NewLaunchTemplateServiceImplWithNATS(ctx, node, nc)
	if err != nil {
		return nil, nil, fmt.Errorf("launch template service: %w", err)
	}
	volumes := accountinventory.New(ec2key.NewKeyServiceImpl(node), groups, templates, tags).
		WithCompute(spot, ec2metadata.NewStore(objects, node.Predastore.Bucket))

	instances := strictInstances{nc: nc, nodeIDs: slices.Clone(nodeIDs)}
	teardown := teardownReader{nc: nc}
	compute := accountcleanup.NewCompute(spot, instances, instanceTerminator{nc: nc}, teardown,
		eksManagedInstances{svc: ekscluster.NewNATSEKSService(nc)})
	storage := accountcleanup.NewStorage(volumes, guardedVolumeDeleter{nc: nc, nodeIDs: slices.Clone(nodeIDs)}, instances, teardown, spot, tags)
	return compute, storage, nil
}

// withComputeCleaners binds the compute and storage cleanup every DeleteAccount
// runs. When it cannot be built both fields stay nil, which refuses the request.
func withComputeCleaners(d operatoradmin.Deps, build func() (accountlifecycle.ComputeStage, accountlifecycle.StorageStage, error)) operatoradmin.Deps {
	compute, storage, err := build()
	if err != nil {
		slog.Error("awsgw: EC2 compute and storage cleanup unavailable", "err", err)
		return d
	}
	d.ComputeCleaner, d.StorageCleaner = compute, storage
	return d
}
