package handlers_eks

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// sgProvisioner is the narrow VPC surface needed by cluster SG helpers.
type sgProvisioner interface {
	CreateSecurityGroup(input *ec2.CreateSecurityGroupInput, accountID string) (*ec2.CreateSecurityGroupOutput, error)
	DescribeSecurityGroups(input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(input *ec2.DeleteSecurityGroupInput, accountID string) (*ec2.DeleteSecurityGroupOutput, error)
	AuthorizeSecurityGroupIngress(input *ec2.AuthorizeSecurityGroupIngressInput, accountID string) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
}

// clusterEKSClusterTagKey groups all cluster-scoped resources for DeleteCluster sweeps.
const clusterEKSClusterTagKey = "spinifex:eks-cluster"

// clusterEKSRoleTagKey distinguishes the control-plane SG from the nodegroup SG.
const clusterEKSRoleTagKey = "spinifex:eks-role"

// clusterEKSNodegroupTagKey is stamped on worker instances to identify their nodegroup.
const clusterEKSNodegroupTagKey = "spinifex:eks-nodegroup"

const (
	clusterEKSRoleControlPlane = "control-plane"
	clusterEKSRoleNodegroup    = "nodegroup"
)

// ClusterControlPlaneSGName returns the deterministic control-plane SG name for a cluster.
func ClusterControlPlaneSGName(clusterName string) string {
	return fmt.Sprintf("eks-cluster-%s-control-plane-sg", clusterName)
}

// ClusterNodegroupSGName returns the deterministic SG name used for the
// cluster's nodegroup SG.
func ClusterNodegroupSGName(clusterName string) string {
	return fmt.Sprintf("eks-cluster-%s-nodegroup-sg", clusterName)
}

// EnsureClusterSGs creates or reuses the control-plane and nodegroup SGs in the customer VPC.
// Idempotent; the two creates are independent (DeleteCluster handles partial teardown).
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

// DeleteClusterSGs removes both cluster SGs. Missing SGs are no-ops; first error is returned.
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

// EnsureNodegroupSGRules authorizes ingress for flannel VXLAN (8472/udp), apiserver
// (6443/tcp), and kubelet (10250/tcp) between CP and nodegroup SGs. Idempotent.
func EnsureNodegroupSGRules(sgp sgProvisioner, accountID, clusterName, cpSGID, ngSGID string) error {
	if cpSGID == "" || ngSGID == "" {
		return errors.New("eks: EnsureNodegroupSGRules empty SG id")
	}

	type rule struct {
		targetSG string
		sourceSG string
		proto    string
		from     int64
		to       int64
		desc     string
	}
	rules := []rule{
		{cpSGID, ngSGID, "tcp", 6443, 6443, "EKS agent to apiserver/supervisor"},
		{cpSGID, ngSGID, "udp", 8472, 8472, "EKS flannel VXLAN node to control-plane"},
		{ngSGID, cpSGID, "udp", 8472, 8472, "EKS flannel VXLAN control-plane to node"},
		{ngSGID, ngSGID, "udp", 8472, 8472, "EKS flannel VXLAN node to node"},
		{ngSGID, cpSGID, "tcp", 10250, 10250, "EKS kubelet control-plane to node"},
		{ngSGID, ngSGID, "-1", -1, -1, "EKS intra-nodegroup all traffic"},
	}

	for _, r := range rules {
		perm := &ec2.IpPermission{
			IpProtocol: aws.String(r.proto),
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{
				GroupId:     aws.String(r.sourceSG),
				Description: aws.String(r.desc),
			}},
		}
		if r.proto != "-1" {
			perm.FromPort = aws.Int64(r.from)
			perm.ToPort = aws.Int64(r.to)
		}
		_, err := sgp.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:       aws.String(r.targetSG),
			IpPermissions: []*ec2.IpPermission{perm},
		}, accountID)
		if err != nil {
			if awserrors.IsErrorCode(err, awserrors.ErrorInvalidPermissionDuplicate) {
				continue
			}
			return fmt.Errorf("authorize %s ingress on %s: %w", r.desc, r.targetSG, err)
		}
	}
	return nil
}

// EnsureControlPlaneIngress admits the NLB→apiserver hop from the VPC CIDR on k3sAPIServerPort.
// Public access is gated separately at the NLB front-end. Idempotent.
func EnsureControlPlaneIngress(sgp sgProvisioner, accountID, cpSGID, vpcCIDR string) error {
	if cpSGID == "" {
		return errors.New("eks: EnsureControlPlaneIngress empty control-plane SG id")
	}
	if vpcCIDR == "" {
		return errors.New("eks: EnsureControlPlaneIngress empty vpc cidr")
	}

	perm := &ec2.IpPermission{
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int64(k3sAPIServerPort),
		ToPort:     aws.Int64(k3sAPIServerPort),
		IpRanges: []*ec2.IpRange{{
			CidrIp:      aws.String(vpcCIDR),
			Description: aws.String("EKS NLB to apiserver"),
		}},
	}
	_, err := sgp.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       aws.String(cpSGID),
		IpPermissions: []*ec2.IpPermission{perm},
	}, accountID)
	if err != nil && !awserrors.IsErrorCode(err, awserrors.ErrorInvalidPermissionDuplicate) {
		return fmt.Errorf("authorize control-plane apiserver ingress on %s: %w", cpSGID, err)
	}
	return nil
}

// EnsureControlPlaneHAIngress authorizes self-referencing CP SG rules for HA etcd peering:
// etcd client+peer (2379-2380/tcp) and kubelet (10250/tcp) within cpSG. Idempotent.
func EnsureControlPlaneHAIngress(sgp sgProvisioner, accountID, cpSGID string) error {
	if cpSGID == "" {
		return errors.New("eks: EnsureControlPlaneHAIngress empty control-plane SG id")
	}

	type rule struct {
		proto string
		from  int64
		to    int64
		desc  string
	}
	rules := []rule{
		{"tcp", 2379, 2380, "EKS etcd client+peer control-plane to control-plane"},
		{"tcp", 10250, 10250, "EKS kubelet control-plane to control-plane"},
	}

	for _, r := range rules {
		perm := &ec2.IpPermission{
			IpProtocol: aws.String(r.proto),
			FromPort:   aws.Int64(r.from),
			ToPort:     aws.Int64(r.to),
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{
				GroupId:     aws.String(cpSGID),
				Description: aws.String(r.desc),
			}},
		}
		_, err := sgp.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:       aws.String(cpSGID),
			IpPermissions: []*ec2.IpPermission{perm},
		}, accountID)
		if err != nil {
			if awserrors.IsErrorCode(err, awserrors.ErrorInvalidPermissionDuplicate) {
				continue
			}
			return fmt.Errorf("authorize %s ingress on %s: %w", r.desc, cpSGID, err)
		}
	}
	return nil
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
