package handlers_eks

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

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
	RevokeSecurityGroupIngress(input *ec2.RevokeSecurityGroupIngressInput, accountID string) (*ec2.RevokeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupEgress(input *ec2.RevokeSecurityGroupEgressInput, accountID string) (*ec2.RevokeSecurityGroupEgressOutput, error)
}

// clusterEKSClusterTagKey groups all cluster-scoped resources for DeleteCluster sweeps.
const clusterEKSClusterTagKey = "spinifex:eks-cluster"

// clusterEKSRoleTagKey distinguishes the control-plane SG from the nodegroup SG.
const clusterEKSRoleTagKey = "spinifex:eks-role"

// clusterEKSAccountTagKey records the customer account that owns the cluster
// meta. The control-plane VM/ENI run under the system account, so this tag is
// the only link from a running CP ENI back to the KV bucket holding its cluster
// meta — the billable GC reaper needs it to decide orphan-hood.
const clusterEKSAccountTagKey = "spinifex:eks-cluster-account"

// clusterEKSNodegroupTagKey is stamped on worker instances to identify their nodegroup.
const clusterEKSNodegroupTagKey = "spinifex:eks-nodegroup"

const (
	clusterEKSRoleControlPlane        = "control-plane"
	clusterEKSRoleNodegroup           = "nodegroup"
	clusterEKSRolePrivateEndpoint     = "private-endpoint"
	clusterEKSRoleControlPlaneOverlay = "control-plane-overlay"
)

// ClusterPrivateEndpointSGName returns the deterministic customer-VPC SG name for
// the cluster's private-endpoint ENI (the Set A NIC on the cluster NLB's LB VM).
func ClusterPrivateEndpointSGName(clusterName string) string {
	return fmt.Sprintf("eks-cluster-%s-private-endpoint-sg", clusterName)
}

// ClusterControlPlaneSGName returns the deterministic control-plane SG name for a cluster.
func ClusterControlPlaneSGName(clusterName string) string {
	return fmt.Sprintf("eks-cluster-%s-control-plane-sg", clusterName)
}

// ClusterControlPlaneOverlaySGName returns the deterministic worker-VPC SG name for
// the cluster's per-CP-server overlay ENIs (the flannel re-home NICs).
func ClusterControlPlaneOverlaySGName(clusterName string) string {
	return fmt.Sprintf("eks-cluster-%s-control-plane-overlay-sg", clusterName)
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

// DeleteClusterSGs removes the cluster control-plane and nodegroup SGs. AWS
// refuses to delete a security group still referenced by ANY other SG's rule —
// not just the cp<->ng cross-reference (EnsureNodegroupSGRules) but the VPC's
// LBC/ALB SG, the private-endpoint SG, and user/TF SGs, in either direction
// (ingress or egress). To break every cycle order-independently, all ingress and
// egress rules are revoked on every non-default SG in the VPC first, then the two
// cluster-owned SGs are deleted. Referrer SGs are owned and deleted by their own
// teardown — only the cluster SGs are deleted here. Missing SGs are no-ops; the
// first error is returned after the full sweep.
func DeleteClusterSGs(sgp sgProvisioner, accountID, clusterName, vpcID string) error {
	if clusterName == "" {
		return errors.New("eks: DeleteClusterSGs empty cluster name")
	}
	if vpcID == "" {
		return errors.New("eks: DeleteClusterSGs empty vpc id")
	}

	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Break every cross-reference in the VPC before deleting any SG. Revoking
	// both directions on every non-default SG guarantees no rule still points at
	// a cluster SG, so the deletes below succeed regardless of order.
	out, err := sgp.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})},
		},
	}, accountID)
	if err != nil {
		slog.Warn("DeleteClusterSGs: describe SGs failed", "vpc", vpcID, "err", err)
		record(fmt.Errorf("describe SGs in vpc %s: %w", vpcID, err))
	}
	for _, g := range out.SecurityGroups {
		if g == nil || g.GroupId == nil {
			continue
		}
		// The default SG cannot be deleted and does not reference cluster SGs;
		// leave its rules intact.
		if aws.StringValue(g.GroupName) == "default" {
			continue
		}
		if revokeErr := revokeAllSGRules(sgp, accountID, g); revokeErr != nil {
			slog.Warn("DeleteClusterSGs: revoke rules failed", "sg", aws.StringValue(g.GroupId), "err", revokeErr)
			record(revokeErr)
		}
	}

	// Delete only the cluster-owned SGs.
	for _, name := range []string{ClusterControlPlaneSGName(clusterName), ClusterNodegroupSGName(clusterName)} {
		id, lookupErr := lookupSGByName(sgp, accountID, vpcID, name)
		if lookupErr != nil {
			slog.Warn("DeleteClusterSGs: SG lookup failed", "sg", name, "err", lookupErr)
			record(lookupErr)
			continue
		}
		if id == "" {
			continue
		}
		if delErr := deleteSGAwaitingDetach(sgp, accountID, id); delErr != nil {
			slog.Warn("DeleteClusterSGs: SG delete failed", "sg", id, "err", delErr)
			record(fmt.Errorf("delete SG %s: %w", id, delErr))
		}
	}
	return firstErr
}

var (
	// sgDeleteWaitBudget bounds how long deleteSGAwaitingDetach retries a
	// DeleteSecurityGroup returning DependencyViolation while a terminating
	// worker's ENI still references the nodegroup SG. The instance-terminate
	// cascade releases the ENI asynchronously, so the immediate delete can lose
	// the race. Vars (not consts) so tests can shrink them.
	sgDeleteWaitBudget   = 45 * time.Second
	sgDeleteWaitInterval = 1 * time.Second
)

// deleteSGAwaitingDetach deletes a cluster SG, tolerating the DependencyViolation
// window while the async instance-terminate cascade releases the last worker ENI
// still referencing it. A missing SG is idempotent success. A persistent
// DependencyViolation past the budget is surfaced so the teardown backstop retries
// rather than leaking the SG — a leaked cluster SG sits in the customer VPC and
// pins it on DependencyViolation.
func deleteSGAwaitingDetach(sgp sgProvisioner, accountID, sgID string) error {
	deadline := time.Now().Add(sgDeleteWaitBudget)
	for {
		_, err := sgp.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String(sgID)}, accountID)
		switch {
		case err == nil, awserrors.IsNotFound(err):
			return nil
		case awserrors.IsErrorCode(err, awserrors.ErrorDependencyViolation):
			if time.Now().After(deadline) {
				return err
			}
			time.Sleep(sgDeleteWaitInterval)
		default:
			return err
		}
	}
}

// revokeAllSGRules clears every ingress AND egress rule on the given SG so no
// rule (in either direction) still references another SG, letting the cluster
// SGs delete in any order. An egress cross-reference pins an SG on
// DependencyViolation just as an ingress one does, so both must go. Empty rule
// sets are no-ops; a raced-away SG (NotFound) is tolerated.
func revokeAllSGRules(sgp sgProvisioner, accountID string, g *ec2.SecurityGroup) error {
	if g == nil || g.GroupId == nil {
		return nil
	}
	sgID := aws.StringValue(g.GroupId)
	if len(g.IpPermissions) > 0 {
		if _, err := sgp.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
			GroupId:       g.GroupId,
			IpPermissions: g.IpPermissions,
		}, accountID); err != nil && !awserrors.IsNotFound(err) {
			return fmt.Errorf("revoke ingress on %s: %w", sgID, err)
		}
	}
	if len(g.IpPermissionsEgress) > 0 {
		if _, err := sgp.RevokeSecurityGroupEgress(&ec2.RevokeSecurityGroupEgressInput{
			GroupId:       g.GroupId,
			IpPermissions: g.IpPermissionsEgress,
		}, accountID); err != nil && !awserrors.IsNotFound(err) {
			return fmt.Errorf("revoke egress on %s: %w", sgID, err)
		}
	}
	return nil
}

// EnsurePrivateEndpointSG creates (or reuses) the customer-VPC SG for the
// private-endpoint ENI and admits the customer VPC CIDR on :443 (kubectl/SDK) and
// :6443, so in-VPC workers + kubectl reach the apiserver via the Set A NIC. :6443
// carries worker pods to the in-cluster `kubernetes` Endpoints (apiserver
// advertised on :6443). Idempotent: SG lookup-or-create + duplicate-tolerant authorize.
func EnsurePrivateEndpointSG(sgp sgProvisioner, accountID, clusterName, vpcID, vpcCIDR string) (string, error) {
	if clusterName == "" {
		return "", errors.New("eks: EnsurePrivateEndpointSG empty cluster name")
	}
	if vpcID == "" {
		return "", errors.New("eks: EnsurePrivateEndpointSG empty vpc id")
	}
	if vpcCIDR == "" {
		return "", errors.New("eks: EnsurePrivateEndpointSG empty vpc cidr")
	}

	sgID, err := ensureClusterSG(sgp, accountID, vpcID, clusterName,
		ClusterPrivateEndpointSGName(clusterName),
		fmt.Sprintf("EKS private-endpoint SG for cluster %s", clusterName),
		clusterEKSRolePrivateEndpoint)
	if err != nil {
		return "", err
	}

	for _, port := range []int64{clusterNLBListenPort, k3sAPIServerPort} {
		perm := &ec2.IpPermission{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(port),
			ToPort:     aws.Int64(port),
			IpRanges: []*ec2.IpRange{{
				CidrIp:      aws.String(vpcCIDR),
				Description: aws.String("EKS in-VPC clients to private apiserver endpoint"),
			}},
		}
		_, err = sgp.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:       aws.String(sgID),
			IpPermissions: []*ec2.IpPermission{perm},
		}, accountID)
		if err != nil && !awserrors.IsErrorCode(err, awserrors.ErrorInvalidPermissionDuplicate) {
			return "", fmt.Errorf("authorize private-endpoint :%d ingress on %s: %w", port, sgID, err)
		}
	}
	return sgID, nil
}

// DeletePrivateEndpointSG removes the cluster's private-endpoint SG. Missing SG
// is a no-op. The private-endpoint ENI must be deleted first (it references this SG).
func DeletePrivateEndpointSG(sgp sgProvisioner, accountID, clusterName, vpcID string) error {
	if clusterName == "" {
		return errors.New("eks: DeletePrivateEndpointSG empty cluster name")
	}
	if vpcID == "" {
		return errors.New("eks: DeletePrivateEndpointSG empty vpc id")
	}
	id, err := lookupSGByName(sgp, accountID, vpcID, ClusterPrivateEndpointSGName(clusterName))
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	if _, err := sgp.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)}, accountID); err != nil && !awserrors.IsNotFound(err) {
		return fmt.Errorf("delete private-endpoint SG %s: %w", id, err)
	}
	return nil
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

// EnsureControlPlaneOverlaySG creates (or reuses) the worker-VPC SG for the CP
// servers' overlay ENIs (the flannel re-home NICs). Rules are authorized against
// the nodegroup SG by EnsureControlPlaneOverlaySGRules once that SG exists.
// Idempotent: lookup-or-create by deterministic name.
func EnsureControlPlaneOverlaySG(sgp sgProvisioner, accountID, clusterName, vpcID string) (string, error) {
	if clusterName == "" {
		return "", errors.New("eks: EnsureControlPlaneOverlaySG empty cluster name")
	}
	if vpcID == "" {
		return "", errors.New("eks: EnsureControlPlaneOverlaySG empty vpc id")
	}
	sgID, err := ensureClusterSG(sgp, accountID, vpcID, clusterName,
		ClusterControlPlaneOverlaySGName(clusterName),
		fmt.Sprintf("EKS control-plane overlay SG for cluster %s", clusterName),
		clusterEKSRoleControlPlaneOverlay)
	if err != nil {
		return "", err
	}

	// The 3 CP servers' mutual flannel VXLAN rides this overlay subnet (flannel
	// re-home), so the SG must admit itself on 8472/udp. This is authorized at SG
	// creation (cluster launch) — not with the nodegroup rules — because the CP
	// flannel mesh must form for the CP nodes to go Ready, which gates the cluster
	// reaching ACTIVE, which in turn gates nodegroup creation.
	perm := &ec2.IpPermission{
		IpProtocol: aws.String("udp"),
		FromPort:   aws.Int64(8472),
		ToPort:     aws.Int64(8472),
		UserIdGroupPairs: []*ec2.UserIdGroupPair{{
			GroupId:     aws.String(sgID),
			Description: aws.String("EKS flannel VXLAN control-plane overlay mesh"),
		}},
	}
	_, err = sgp.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{perm},
	}, accountID)
	if err != nil && !awserrors.IsErrorCode(err, awserrors.ErrorInvalidPermissionDuplicate) {
		return "", fmt.Errorf("authorize overlay mesh VXLAN ingress on %s: %w", sgID, err)
	}
	return sgID, nil
}

// EnsureControlPlaneOverlaySGRules authorizes the flannel re-home datapath between
// the CP overlay SG and the nodegroup SG (both in the worker VPC): VXLAN 8472/udp
// both ways (apiserver<->pod overlay) and kubelet 10250/tcp overlay->node
// (egress-selector-mode disabled dials kubelet directly). Idempotent.
func EnsureControlPlaneOverlaySGRules(sgp sgProvisioner, accountID, overlaySGID, ngSGID string) error {
	if overlaySGID == "" || ngSGID == "" {
		return errors.New("eks: EnsureControlPlaneOverlaySGRules empty SG id")
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
		{ngSGID, overlaySGID, "udp", 8472, 8472, "EKS flannel VXLAN control-plane overlay to node"},
		{overlaySGID, ngSGID, "udp", 8472, 8472, "EKS flannel VXLAN node to control-plane overlay"},
		{ngSGID, overlaySGID, "tcp", 10250, 10250, "EKS kubelet control-plane overlay to node"},
	}

	for _, r := range rules {
		perm := &ec2.IpPermission{
			IpProtocol: aws.String(r.proto),
			FromPort:   aws.Int64(r.from),
			ToPort:     aws.Int64(r.to),
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{
				GroupId:     aws.String(r.sourceSG),
				Description: aws.String(r.desc),
			}},
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

// DeleteControlPlaneOverlaySG removes the cluster's worker-VPC overlay SG. Missing
// SG is a no-op. The overlay ENIs must be deleted first (they reference this SG).
func DeleteControlPlaneOverlaySG(sgp sgProvisioner, accountID, clusterName, vpcID string) error {
	if clusterName == "" {
		return errors.New("eks: DeleteControlPlaneOverlaySG empty cluster name")
	}
	if vpcID == "" {
		return errors.New("eks: DeleteControlPlaneOverlaySG empty vpc id")
	}
	id, err := lookupSGByName(sgp, accountID, vpcID, ClusterControlPlaneOverlaySGName(clusterName))
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	if _, err := sgp.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)}, accountID); err != nil && !awserrors.IsNotFound(err) {
		return fmt.Errorf("delete control-plane overlay SG %s: %w", id, err)
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
