package handlers_eks

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// sgProvisioner is the subset of handlers_ec2_vpc.VPCService that the cluster
// SG helpers need. Narrow so tests can fake with hand-rolled recorders without
// implementing the full VPCService surface.
type sgProvisioner interface {
	CreateSecurityGroup(input *ec2.CreateSecurityGroupInput, accountID string) (*ec2.CreateSecurityGroupOutput, error)
	DescribeSecurityGroups(input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(input *ec2.DeleteSecurityGroupInput, accountID string) (*ec2.DeleteSecurityGroupOutput, error)
}

// clusterEKSClusterTagKey is stamped on every cluster-scoped resource so the
// DeleteCluster sweep + the UI hide-from-customer filter can group them.
const clusterEKSClusterTagKey = "spinifex:eks-cluster"

// clusterEKSRoleTagKey distinguishes the control-plane SG from the nodegroup
// SG when both live in the same VPC.
const clusterEKSRoleTagKey = "spinifex:eks-role"

const (
	clusterEKSRoleControlPlane = "control-plane"
	clusterEKSRoleNodegroup    = "nodegroup"
)

// ClusterControlPlaneSGName returns the deterministic SG name used for the
// cluster's control-plane SG. Stable so Stage-6 DeleteCluster can locate it
// from clusterName alone.
func ClusterControlPlaneSGName(clusterName string) string {
	return fmt.Sprintf("eks-cluster-%s-control-plane-sg", clusterName)
}

// ClusterNodegroupSGName returns the deterministic SG name used for the
// cluster's nodegroup SG.
func ClusterNodegroupSGName(clusterName string) string {
	return fmt.Sprintf("eks-cluster-%s-nodegroup-sg", clusterName)
}

// EnsureClusterSGs creates (or returns existing) control-plane + nodegroup
// security groups in the customer VPC. Both are tagged
// spinifex:managed-by=eks + spinifex:eks-cluster=<name>. Idempotent: an SG
// whose group-name + vpc-id match the expected pair is reused without a
// second create call. The two creates are independent — a failure on the
// nodegroup SG does not unwind the control-plane SG (DeleteCluster cleans up).
func EnsureClusterSGs(sgp sgProvisioner, accountID, clusterName, vpcID string) (controlPlaneSGID, nodegroupSGID string, err error) {
	if clusterName == "" {
		return "", "", errors.New("eks: EnsureClusterSGs empty cluster name")
	}
	if vpcID == "" {
		return "", "", errors.New("eks: EnsureClusterSGs empty vpc id")
	}

	controlPlaneSGID, err = ensureClusterSG(sgp, accountID, vpcID, clusterName,
		ClusterControlPlaneSGName(clusterName),
		fmt.Sprintf("EKS control-plane SG for cluster %s", clusterName),
		clusterEKSRoleControlPlane)
	if err != nil {
		return "", "", err
	}

	nodegroupSGID, err = ensureClusterSG(sgp, accountID, vpcID, clusterName,
		ClusterNodegroupSGName(clusterName),
		fmt.Sprintf("EKS nodegroup SG for cluster %s", clusterName),
		clusterEKSRoleNodegroup)
	if err != nil {
		return "", "", err
	}

	return controlPlaneSGID, nodegroupSGID, nil
}

// DeleteClusterSGs removes both cluster SGs in the customer VPC. Missing SGs
// are no-ops so DeleteCluster can retry safely. Delete errors are logged and
// the sweep continues; the first error is returned so the caller surfaces it
// without halting the broader teardown.
func DeleteClusterSGs(sgp sgProvisioner, accountID, clusterName, vpcID string) error {
	if clusterName == "" {
		return errors.New("eks: DeleteClusterSGs empty cluster name")
	}
	if vpcID == "" {
		return errors.New("eks: DeleteClusterSGs empty vpc id")
	}

	var firstErr error
	for _, name := range []string{ClusterControlPlaneSGName(clusterName), ClusterNodegroupSGName(clusterName)} {
		id, err := lookupSGByName(sgp, accountID, vpcID, name)
		if err != nil {
			slog.Warn("DeleteClusterSGs: SG lookup failed", "sg", name, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if id == "" {
			continue
		}
		if _, err := sgp.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)}, accountID); err != nil {
			slog.Warn("DeleteClusterSGs: SG delete failed", "sg", id, "name", name, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("delete SG %s: %w", id, err)
			}
		}
	}
	return firstErr
}

func ensureClusterSG(sgp sgProvisioner, accountID, vpcID, clusterName, name, description, role string) (string, error) {
	if id, err := lookupSGByName(sgp, accountID, vpcID, name); err != nil {
		return "", err
	} else if id != "" {
		return id, nil
	}

	out, err := sgp.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(name),
		Description: aws.String(description),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("security-group"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
				{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(clusterName)},
				{Key: aws.String(clusterEKSRoleTagKey), Value: aws.String(role)},
			},
		}},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("create SG %s: %w", name, err)
	}
	if out == nil || out.GroupId == nil || *out.GroupId == "" {
		return "", fmt.Errorf("eks: CreateSecurityGroup returned empty GroupId for %s", name)
	}
	return *out.GroupId, nil
}

func lookupSGByName(sgp sgProvisioner, accountID, vpcID, name string) (string, error) {
	out, err := sgp.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-name"), Values: aws.StringSlice([]string{name})},
			{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})},
		},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("describe SG %s: %w", name, err)
	}
	for _, g := range out.SecurityGroups {
		if g == nil || g.GroupId == nil {
			continue
		}
		return *g.GroupId, nil
	}
	return "", nil
}
