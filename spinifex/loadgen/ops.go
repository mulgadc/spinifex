package loadgen

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/sts"
)

// The operations a run may drive. All but one are reads, deliberately: a load
// generator that creates guests would compete with the harness for the very
// capacity the harness sized the run against. CreateTags is the exception
// because a control plane whose reads are fast and whose writes are not is a
// broken control plane, and tagging costs no capacity.
var registry = map[string]Op{
	"DescribeInstances": {
		Name: "DescribeInstances", Kind: KindRead,
		Call: func(ctx context.Context, t *Target) error {
			_, err := t.Clients.EC2.DescribeInstancesWithContext(ctx, &ec2.DescribeInstancesInput{})
			return err
		},
	},
	"DescribeVpcs": {
		Name: "DescribeVpcs", Kind: KindRead,
		Call: func(ctx context.Context, t *Target) error {
			_, err := t.Clients.EC2.DescribeVpcsWithContext(ctx, &ec2.DescribeVpcsInput{})
			return err
		},
	},
	"DescribeSubnets": {
		Name: "DescribeSubnets", Kind: KindRead,
		Call: func(ctx context.Context, t *Target) error {
			_, err := t.Clients.EC2.DescribeSubnetsWithContext(ctx, &ec2.DescribeSubnetsInput{})
			return err
		},
	},
	"DescribeVolumes": {
		Name: "DescribeVolumes", Kind: KindRead,
		Call: func(ctx context.Context, t *Target) error {
			_, err := t.Clients.EC2.DescribeVolumesWithContext(ctx, &ec2.DescribeVolumesInput{})
			return err
		},
	},
	"DescribeSecurityGroups": {
		Name: "DescribeSecurityGroups", Kind: KindRead,
		Call: func(ctx context.Context, t *Target) error {
			_, err := t.Clients.EC2.DescribeSecurityGroupsWithContext(ctx, &ec2.DescribeSecurityGroupsInput{})
			return err
		},
	},
	// The console login path. A cluster whose EC2 API is healthy but whose STS
	// is slow feels broken to every customer signing in, and nothing else in
	// the run would notice.
	"GetSessionToken": {
		Name: "GetSessionToken", Kind: KindRead,
		Call: func(ctx context.Context, t *Target) error {
			_, err := t.Clients.STS.GetSessionTokenWithContext(ctx, &sts.GetSessionTokenInput{})
			return err
		},
	},
	"CreateTags": {
		Name: "CreateTags", Kind: KindWrite,
		Call: func(ctx context.Context, t *Target) error {
			if t.VPCID == "" {
				return fmt.Errorf("CreateTags: no VPC resolved for %s", t.Account)
			}
			_, err := t.Clients.EC2.CreateTagsWithContext(ctx, &ec2.CreateTagsInput{
				Resources: []*string{aws.String(t.VPCID)},
				Tags: []*ec2.Tag{{
					Key:   aws.String("spx:loadgen"),
					Value: aws.String(t.Account),
				}},
			})
			return err
		},
	},
}

// DefaultOps is what a run drives when none are named: the three reads every
// console page and every terraform plan makes, so the number means something
// outside this harness.
var DefaultOps = []string{"DescribeInstances", "DescribeVpcs", "DescribeVolumes"}

// OpNames lists every operation the registry knows, for the usage text.
func OpNames() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolveOps turns names into operations, rejecting an unknown one rather than
// silently running a shorter list than was asked for.
func ResolveOps(names []string) ([]Op, error) {
	ops := make([]Op, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		op, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("loadgen: unknown operation %q (known: %s)",
				name, strings.Join(OpNames(), ", "))
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("loadgen: no operations selected")
	}
	return ops, nil
}

// NeedsVPC reports whether any selected operation acts on a tenant resource,
// so the caller knows whether to pay for discovery before the run starts.
func NeedsVPC(ops []Op) bool {
	for _, op := range ops {
		if op.Name == "CreateTags" {
			return true
		}
	}
	return false
}

// ResolveVPC finds a VPC for the target to act on. Every account has a default
// VPC created with it, so an empty answer means something is wrong with the
// tenant rather than that it has nothing yet.
func ResolveVPC(ctx context.Context, target *Target) error {
	out, err := target.Clients.EC2.DescribeVpcsWithContext(ctx, &ec2.DescribeVpcsInput{})
	if err != nil {
		return fmt.Errorf("loadgen: resolve VPC for %s: %w", target.Account, err)
	}
	for _, vpc := range out.Vpcs {
		if vpc.VpcId != nil && *vpc.VpcId != "" {
			target.VPCID = *vpc.VpcId
			return nil
		}
	}
	return fmt.Errorf("loadgen: account %s has no VPC to act on", target.Account)
}
