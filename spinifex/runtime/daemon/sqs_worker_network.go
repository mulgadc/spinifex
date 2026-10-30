package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/network/systemvpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
)

// The SQS system VPC has its own tag keys and address space, so no other
// component's teardown or orphan reaper can reach it.
const (
	sqsSystemVPCTagKey     = "spinifex:sqs-system-vpc"
	sqsSystemVPCRoleTagKey = "spinifex:sqs-role"
	sqsSystemVPCRolePrefix = "sqs"
	sqsWorkerClaimTagKey   = "spinifex:sqs-worker-claim"
)

// sqsSystemVPCSpec builds the region's private SQS system VPC spec; an unset
// supernet falls back to the default.
func sqsSystemVPCSpec(cfg *config.SQSConfig, region string) systemvpc.Spec {
	supernet := config.SQSDefaultSystemVPCSupernet
	if cfg != nil && cfg.SystemVPCSupernet != "" {
		supernet = cfg.SystemVPCSupernet
	}
	return systemvpc.Spec{
		Owner: systemvpc.Owner{
			Name:        "sqs-system-" + region,
			ManagedBy:   tags.ManagedBySQS,
			OwnerTagKey: sqsSystemVPCTagKey,
			RoleTagKey:  sqsSystemVPCRoleTagKey,
		},
		Region:         region,
		RolePrefix:     sqsSystemVPCRolePrefix,
		Supernet:       supernet,
		PrivateSubnets: 1,
	}
}

// sqsWorkerNetworkVPC is the VPC, subnet and ENI surface the worker's network
// needs; the daemon's VPC service implements it.
type sqsWorkerNetworkVPC interface {
	systemvpc.VPCProvisioner
	CreateNetworkInterface(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error)
	DescribeNetworkInterfaces(ctx context.Context, input *ec2.DescribeNetworkInterfacesInput, accountID string) (*ec2.DescribeNetworkInterfacesOutput, error)
}

// sqsWorkerNetwork places the worker's primary ENI in the private SQS system
// VPC, one ENI per launch claim. Allocations are serialised in this process so
// two resumes of one claim cannot both miss the describe and create two ENIs.
type sqsWorkerNetwork struct {
	vpc  sqsWorkerNetworkVPC
	cfg  *config.SQSConfig
	sem  chan struct{}
	desc string
}

var _ workerlaunch.NetworkAllocator = (*sqsWorkerNetwork)(nil)

func newSQSWorkerNetwork(vpc sqsWorkerNetworkVPC, cfg *config.SQSConfig) *sqsWorkerNetwork {
	return &sqsWorkerNetwork{vpc: vpc, cfg: cfg, sem: make(chan struct{}, 1), desc: "SQS worker interface for "}
}

// newSQSStoreNetwork places the store's ENI in the same private subnet as the
// worker, found by its own claim.
func newSQSStoreNetwork(vpc sqsWorkerNetworkVPC, cfg *config.SQSConfig) *sqsWorkerNetwork {
	return &sqsWorkerNetwork{vpc: vpc, cfg: cfg, sem: make(chan struct{}, 1), desc: "SQS store interface for "}
}

// FindWorkerNetwork returns the claim's visible ENI, or nil when there is none.
func (n *sqsWorkerNetwork) FindWorkerNetwork(ctx context.Context, region, claimID string) (*workerlaunch.Network, error) {
	return n.allocate(ctx, region, claimID, false)
}

// CreateWorkerNetwork reuses the claim's visible ENI or creates it.
func (n *sqsWorkerNetwork) CreateWorkerNetwork(ctx context.Context, region, claimID string) (*workerlaunch.Network, error) {
	return n.allocate(ctx, region, claimID, true)
}

func (n *sqsWorkerNetwork) allocate(ctx context.Context, region, claimID string, create bool) (*workerlaunch.Network, error) {
	if n == nil || n.vpc == nil || n.sem == nil {
		return nil, errors.New("sqs worker: network allocator is not configured")
	}
	if region == "" || claimID == "" {
		return nil, errors.New("sqs worker: network needs a region and claim ID")
	}
	select {
	case n.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-n.sem }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	refs, err := systemvpc.EnsurePrivate(ctx, n.vpc, sqsSystemVPCSpec(n.cfg, region), awsidentifiers.GlobalAccountID)
	if err != nil {
		return nil, fmt.Errorf("sqs worker: ensure system VPC: %w", err)
	}
	subnet, err := netip.ParsePrefix(refs.SubnetCIDR)
	if err != nil {
		return nil, fmt.Errorf("sqs worker: subnet CIDR %q: %w", refs.SubnetCIDR, err)
	}
	eni, err := n.claimENI(ctx, refs.SubnetID, claimID, create)
	if err != nil || eni == nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(aws.StringValue(eni.PrivateIpAddress))
	if err != nil || !subnet.Contains(ip) {
		return nil, fmt.Errorf("sqs worker: ENI %s address is not in subnet %s", aws.StringValue(eni.NetworkInterfaceId), refs.SubnetCIDR)
	}
	gateway, bits, err := topology.SubnetGatewayCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("sqs worker: subnet gateway: %w", err)
	}
	return &workerlaunch.Network{
		SubnetID: refs.SubnetID,
		ENIID:    aws.StringValue(eni.NetworkInterfaceId),
		ENIMac:   aws.StringValue(eni.MacAddress),
		ENIIP:    ip.String(),
		CIDR:     fmt.Sprintf("%s/%d", ip, bits),
		Gateway:  gateway,
	}, nil
}

// claimENI returns the claim's ENI, creating it on a miss with the VPC default
// security group only when create is set. More than one complete match is
// refused, not chosen from.
func (n *sqsWorkerNetwork) claimENI(ctx context.Context, subnetID, claimID string, create bool) (*ec2.NetworkInterface, error) {
	acct := awsidentifiers.GlobalAccountID
	if n.desc == "" {
		return nil, errors.New("sqs worker: network allocator is not configured")
	}
	desc := n.desc + claimID
	out, err := n.vpc.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{Filters: []*ec2.Filter{
		{Name: aws.String("subnet-id"), Values: aws.StringSlice([]string{subnetID})},
		{Name: aws.String("description"), Values: aws.StringSlice([]string{desc})},
	}}, acct)
	if err != nil {
		return nil, fmt.Errorf("sqs worker: describe ENI: %w", err)
	}
	var found []*ec2.NetworkInterface
	if out != nil {
		for _, e := range out.NetworkInterfaces {
			if completeENI(e) {
				found = append(found, e)
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		if !create {
			return nil, nil
		}
	default:
		return nil, fmt.Errorf("sqs worker: %d ENIs carry claim %s; refusing to choose", len(found), claimID)
	}
	created, err := n.vpc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String(desc),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("network-interface"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedBySQS)},
				{Key: aws.String(sqsWorkerClaimTagKey), Value: aws.String(claimID)},
			},
		}},
	}, acct)
	if err != nil {
		return nil, fmt.Errorf("sqs worker: create ENI: %w", err)
	}
	if created == nil || !completeENI(created.NetworkInterface) {
		return nil, errors.New("sqs worker: create ENI returned an incomplete interface")
	}
	return created.NetworkInterface, nil
}

func completeENI(e *ec2.NetworkInterface) bool {
	return e != nil && aws.StringValue(e.NetworkInterfaceId) != "" &&
		aws.StringValue(e.MacAddress) != "" && aws.StringValue(e.PrivateIpAddress) != ""
}
