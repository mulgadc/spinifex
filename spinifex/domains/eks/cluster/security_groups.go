package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
)

// sgProvisioner is the narrow VPC surface needed by cluster SG helpers.
type sgProvisioner interface {
	CreateSecurityGroup(ctx context.Context, input *ec2.CreateSecurityGroupInput, accountID string) (*ec2.CreateSecurityGroupOutput, error)
	DescribeSecurityGroups(ctx context.Context, input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error)
	DeleteSecurityGroup(ctx context.Context, input *ec2.DeleteSecurityGroupInput, accountID string) (*ec2.DeleteSecurityGroupOutput, error)
	AuthorizeSecurityGroupIngress(ctx context.Context, input *ec2.AuthorizeSecurityGroupIngressInput, accountID string) (*ec2.AuthorizeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupIngress(ctx context.Context, input *ec2.RevokeSecurityGroupIngressInput, accountID string) (*ec2.RevokeSecurityGroupIngressOutput, error)
	RevokeSecurityGroupEgress(ctx context.Context, input *ec2.RevokeSecurityGroupEgressInput, accountID string) (*ec2.RevokeSecurityGroupEgressOutput, error)
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
	clusterEKSRoleCluster         = "cluster"
	clusterEKSRoleControlPlane    = "control-plane"
	clusterEKSRoleNodegroup       = "nodegroup"
	clusterEKSRolePrivateEndpoint = "private-endpoint"
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

// ClusterNodegroupSGName returns the deterministic SG name used for the
// cluster's nodegroup SG.
func ClusterNodegroupSGName(clusterName string) string {
	return fmt.Sprintf("eks-cluster-%s-nodegroup-sg", clusterName)
}

// clusterSGNamePrefix starts the name of the AWS-style cluster security group,
// eks-cluster-sg-<cluster>-<id>.
const clusterSGNamePrefix = "eks-cluster-sg-"

// EnsureClusterSG creates or reuses the cluster security group in the customer
// VPC: tagged as EKS tags its primary SG, inbound all from itself, outbound all
// (the VPC service's default egress). It is found by tag, so the random name
// suffix never has to be stored. Workers and the private-endpoint ENI carry it.
func EnsureClusterSG(ctx context.Context, sgp sgProvisioner, accountID, clusterName, vpcID string) (string, error) {
	if clusterName == "" {
		return "", errors.New("eks: EnsureClusterSG empty cluster name")
	}
	if vpcID == "" {
		return "", errors.New("eks: EnsureClusterSG empty vpc id")
	}
	id, err := lookupClusterSG(ctx, sgp, accountID, vpcID, clusterName)
	if err != nil {
		return "", err
	}
	if id == "" {
		suffix := make([]byte, 8)
		if _, err := rand.Read(suffix); err != nil {
			return "", fmt.Errorf("generate cluster SG name: %w", err)
		}
		name := clusterSGNamePrefix + clusterName + "-" + hex.EncodeToString(suffix)
		out, err := sgp.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
			GroupName:   aws.String(name),
			Description: aws.String(fmt.Sprintf("EKS created security group applied to ENI that is attached to EKS Control Plane master nodes, as well as any managed workloads (cluster %s)", clusterName)),
			VpcId:       aws.String(vpcID),
			TagSpecifications: []*ec2.TagSpecification{{
				ResourceType: aws.String("security-group"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String(name)},
					{Key: aws.String("kubernetes.io/cluster/" + clusterName), Value: aws.String("owned")},
					{Key: aws.String("aws:eks:cluster-name"), Value: aws.String(clusterName)},
					{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
					{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(clusterName)},
					{Key: aws.String(clusterEKSRoleTagKey), Value: aws.String(clusterEKSRoleCluster)},
				},
			}},
		}, accountID)
		if err != nil {
			return "", fmt.Errorf("create cluster SG %s: %w", name, err)
		}
		if out == nil || aws.StringValue(out.GroupId) == "" {
			return "", fmt.Errorf("eks: CreateSecurityGroup returned empty GroupId for %s", name)
		}
		id = aws.StringValue(out.GroupId)
	}
	_, err = sgp.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(id),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("-1"),
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{
				GroupId:     aws.String(id),
				Description: aws.String("EKS cluster security group, all traffic from itself"),
			}},
		}},
	}, accountID)
	if err != nil && !awserrors.IsErrorCode(err, awserrors.ErrorInvalidPermissionDuplicate) {
		return "", fmt.Errorf("authorize self ingress on cluster SG %s: %w", id, err)
	}
	return id, nil
}

// EnsureWorkerSG returns the SG nodegroup workers launch with. A cluster created
// before the cluster SG existed keeps its nodegroup SG and control-plane rules
// until it is recreated; every other cluster gets the SG recorded on it,
// clusterSGID, or a freshly ensured one when none is recorded yet.
func EnsureWorkerSG(ctx context.Context, sgp sgProvisioner, accountID, clusterName, vpcID, clusterSGID string) (string, error) {
	if vpcID == "" {
		return "", errors.New("eks: EnsureWorkerSG empty vpc id")
	}
	legacy, err := lookupSGByName(ctx, sgp, accountID, vpcID, ClusterNodegroupSGName(clusterName))
	if err != nil {
		return "", err
	}
	if legacy == "" {
		if clusterSGID != "" {
			return clusterSGID, nil
		}
		return EnsureClusterSG(ctx, sgp, accountID, clusterName, vpcID)
	}
	cpSGID, ngSGID, err := EnsureClusterSGs(ctx, sgp, accountID, clusterName, vpcID)
	if err != nil {
		return "", err
	}
	if err := EnsureNodegroupSGRules(ctx, sgp, accountID, clusterName, cpSGID, ngSGID); err != nil {
		return "", err
	}
	return ngSGID, nil
}

// EnsureClusterSGs creates or reuses the control-plane and nodegroup SGs: the
// control-plane SG in the system CP VPC, and both in a pre-cluster-SG customer VPC.
// Idempotent; the two creates are independent (DeleteCluster handles partial teardown).
func EnsureClusterSGs(ctx context.Context, sgp sgProvisioner, accountID, clusterName, vpcID string) (controlPlaneSGID, nodegroupSGID string, err error) {
	if clusterName == "" {
		return "", "", errors.New("eks: EnsureClusterSGs empty cluster name")
	}
	if vpcID == "" {
		return "", "", errors.New("eks: EnsureClusterSGs empty vpc id")
	}

	controlPlaneSGID, err = ensureClusterSG(ctx, sgp, accountID, vpcID, clusterName,
		ClusterControlPlaneSGName(clusterName),
		fmt.Sprintf("EKS control-plane SG for cluster %s", clusterName),
		clusterEKSRoleControlPlane)
	if err != nil {
		return "", "", err
	}

	nodegroupSGID, err = ensureClusterSG(ctx, sgp, accountID, vpcID, clusterName,
		ClusterNodegroupSGName(clusterName),
		fmt.Sprintf("EKS nodegroup SG for cluster %s", clusterName),
		clusterEKSRoleNodegroup)
	if err != nil {
		return "", "", err
	}

	return controlPlaneSGID, nodegroupSGID, nil
}

// DeleteClusterSGs removes the cluster's SGs: the stored cluster SG, the
// control-plane and nodegroup SGs of a cluster created before the cluster SG, and
// the LBC-orphaned SGs. AWS refuses to delete an SG still referenced by another
// SG's rule, in either direction, so before any delete every rule on a
// cluster-owned SG is revoked and, on any other SG in the VPC, only the rules that
// reference a cluster-owned SG. Unrelated customer SGs keep everything else.
// clusterSGID is the id recorded on the cluster; ownership never comes from a
// customer-settable tag. Missing SGs are no-ops; the first error is returned
// after the full sweep.
func DeleteClusterSGs(ctx context.Context, sgp sgProvisioner, accountID, clusterName, vpcID, clusterSGID string) error {
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

	out, err := sgp.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})},
		},
	}, accountID)
	if err != nil {
		slog.WarnContext(ctx, "DeleteClusterSGs: describe SGs failed", "vpc", vpcID, "err", err)
		record(fmt.Errorf("describe SGs in vpc %s: %w", vpcID, err))
	}
	// A failed describe returns a nil out; fall back to an empty set so the
	// sweeps below become no-ops while the targeted deletes still run.
	var sgs []*ec2.SecurityGroup
	if out != nil {
		sgs = out.SecurityGroups
	}

	// owned is every SG this cluster owns, in delete order: LBC-orphaned SGs
	// first (they reference the cluster SG), then the stored cluster SG, then the
	// pre-cluster-SG named ones.
	var owned []string
	isOwned := map[string]bool{}
	own := func(id string) {
		if id != "" && !isOwned[id] {
			isOwned[id] = true
			owned = append(owned, id)
		}
	}
	for _, g := range sgs {
		if g != nil && g.GroupId != nil && aws.StringValue(g.GroupName) != "default" && sgHasLBCClusterTag(g, clusterName) {
			own(*g.GroupId)
		}
	}
	own(clusterSGID)
	// With no stored id (a create that died before recording it) the SG is still
	// ours when it carries the tags only the service can set.
	if clusterSGID == "" {
		for _, g := range sgs {
			if g != nil && g.GroupId != nil && sgIsClusterSG(g, clusterName) {
				own(*g.GroupId)
			}
		}
	}
	for _, name := range []string{ClusterControlPlaneSGName(clusterName), ClusterNodegroupSGName(clusterName)} {
		id, lookupErr := lookupSGByName(ctx, sgp, accountID, vpcID, name)
		if lookupErr != nil {
			slog.WarnContext(ctx, "DeleteClusterSGs: SG lookup failed", "sg", name, "err", lookupErr)
			record(lookupErr)
			continue
		}
		own(id)
	}

	for _, g := range sgs {
		if g == nil || g.GroupId == nil {
			continue
		}
		// The default SG cannot be deleted and does not reference cluster SGs;
		// leave its rules intact.
		if aws.StringValue(g.GroupName) == "default" {
			continue
		}
		var revokeErr error
		if isOwned[*g.GroupId] {
			revokeErr = revokeAllSGRules(ctx, sgp, accountID, g)
		} else {
			revokeErr = revokeRulesReferencing(ctx, sgp, accountID, g, isOwned)
		}
		if revokeErr != nil {
			slog.WarnContext(ctx, "DeleteClusterSGs: revoke rules failed", "sg", aws.StringValue(g.GroupId), "err", revokeErr)
			record(revokeErr)
		}
	}

	for _, id := range owned {
		if delErr := deleteSGAwaitingDetach(ctx, sgp, accountID, id); delErr != nil {
			slog.WarnContext(ctx, "DeleteClusterSGs: SG delete failed", "sg", id, "err", delErr)
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
func deleteSGAwaitingDetach(ctx context.Context, sgp sgProvisioner, accountID, sgID string) error {
	deadline := time.Now().Add(sgDeleteWaitBudget)
	for {
		_, err := sgp.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(sgID)}, accountID)
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
func revokeAllSGRules(ctx context.Context, sgp sgProvisioner, accountID string, g *ec2.SecurityGroup) error {
	if g == nil || g.GroupId == nil {
		return nil
	}
	sgID := aws.StringValue(g.GroupId)
	if len(g.IpPermissions) > 0 {
		if _, err := sgp.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
			GroupId:       g.GroupId,
			IpPermissions: g.IpPermissions,
		}, accountID); err != nil && !awserrors.IsNotFound(err) {
			return fmt.Errorf("revoke ingress on %s: %w", sgID, err)
		}
	}
	if len(g.IpPermissionsEgress) > 0 {
		if _, err := sgp.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
			GroupId:       g.GroupId,
			IpPermissions: g.IpPermissionsEgress,
		}, accountID); err != nil && !awserrors.IsNotFound(err) {
			return fmt.Errorf("revoke egress on %s: %w", sgID, err)
		}
	}
	return nil
}

// revokeRulesReferencing revokes, on a SG the cluster does not own, only the
// rules that reference an owned SG, and only those references. Every other rule
// on the SG is left in place.
func revokeRulesReferencing(ctx context.Context, sgp sgProvisioner, accountID string, g *ec2.SecurityGroup, owned map[string]bool) error {
	sgID := aws.StringValue(g.GroupId)
	if perms := referencingPerms(g.IpPermissions, owned); len(perms) > 0 {
		if _, err := sgp.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
			GroupId:       g.GroupId,
			IpPermissions: perms,
		}, accountID); err != nil && !awserrors.IsNotFound(err) {
			return fmt.Errorf("revoke ingress on %s: %w", sgID, err)
		}
	}
	if perms := referencingPerms(g.IpPermissionsEgress, owned); len(perms) > 0 {
		if _, err := sgp.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
			GroupId:       g.GroupId,
			IpPermissions: perms,
		}, accountID); err != nil && !awserrors.IsNotFound(err) {
			return fmt.Errorf("revoke egress on %s: %w", sgID, err)
		}
	}
	return nil
}

// referencingPerms returns, for each permission that names an owned SG, a copy
// carrying only those group references.
func referencingPerms(perms []*ec2.IpPermission, owned map[string]bool) []*ec2.IpPermission {
	var out []*ec2.IpPermission
	for _, p := range perms {
		if p == nil {
			continue
		}
		var pairs []*ec2.UserIdGroupPair
		for _, pair := range p.UserIdGroupPairs {
			if pair != nil && owned[aws.StringValue(pair.GroupId)] {
				pairs = append(pairs, pair)
			}
		}
		if len(pairs) == 0 {
			continue
		}
		out = append(out, &ec2.IpPermission{
			IpProtocol:       p.IpProtocol,
			FromPort:         p.FromPort,
			ToPort:           p.ToPort,
			UserIdGroupPairs: pairs,
		})
	}
	return out
}

// EnsurePrivateEndpointSG creates (or reuses) the customer-VPC SG for the
// private-endpoint ENI and admits the customer VPC CIDR on :443 (kubectl/SDK) and
// :6443, so in-VPC workers + kubectl reach the apiserver via the Set A NIC. :6443
// carries worker pods to the in-cluster `kubernetes` Endpoints (apiserver
// advertised on :6443). Idempotent: SG lookup-or-create + duplicate-tolerant authorize.
func EnsurePrivateEndpointSG(ctx context.Context, sgp sgProvisioner, accountID, clusterName, vpcID, vpcCIDR string) (string, error) {
	if clusterName == "" {
		return "", errors.New("eks: EnsurePrivateEndpointSG empty cluster name")
	}
	if vpcID == "" {
		return "", errors.New("eks: EnsurePrivateEndpointSG empty vpc id")
	}
	if vpcCIDR == "" {
		return "", errors.New("eks: EnsurePrivateEndpointSG empty vpc cidr")
	}

	sgID, err := ensureClusterSG(ctx, sgp, accountID, vpcID, clusterName,
		ClusterPrivateEndpointSGName(clusterName),
		fmt.Sprintf("EKS private-endpoint SG for cluster %s", clusterName),
		clusterEKSRolePrivateEndpoint)
	if err != nil {
		return "", err
	}

	// :443/:6443 carry kubectl/SDK + the in-cluster `kubernetes` Endpoints; :8132
	// carries the worker konnectivity-agents' tunnel dial to the CP servers.
	for _, port := range []int64{clusterNLBListenPort, k3sAPIServerPort, konnectivityAgentPort} {
		perm := &ec2.IpPermission{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(port),
			ToPort:     aws.Int64(port),
			IpRanges: []*ec2.IpRange{{
				CidrIp:      aws.String(vpcCIDR),
				Description: aws.String("EKS in-VPC clients to private apiserver endpoint"),
			}},
		}
		_, err = sgp.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
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
func DeletePrivateEndpointSG(ctx context.Context, sgp sgProvisioner, accountID, clusterName, vpcID string) error {
	if clusterName == "" {
		return errors.New("eks: DeletePrivateEndpointSG empty cluster name")
	}
	if vpcID == "" {
		return errors.New("eks: DeletePrivateEndpointSG empty vpc id")
	}
	id, err := lookupSGByName(ctx, sgp, accountID, vpcID, ClusterPrivateEndpointSGName(clusterName))
	if err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	if _, err := sgp.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)}, accountID); err != nil && !awserrors.IsNotFound(err) {
		return fmt.Errorf("delete private-endpoint SG %s: %w", id, err)
	}
	return nil
}

func ensureClusterSG(ctx context.Context, sgp sgProvisioner, accountID, vpcID, clusterName, name, description, role string) (string, error) {
	if id, err := lookupSGByName(ctx, sgp, accountID, vpcID, name); err != nil {
		return "", err
	} else if id != "" {
		return id, nil
	}

	out, err := sgp.CreateSecurityGroup(ctx, &ec2.CreateSecurityGroupInput{
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
func EnsureNodegroupSGRules(ctx context.Context, sgp sgProvisioner, accountID, clusterName, cpSGID, ngSGID string) error {
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
		_, err := sgp.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
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

// EnsureControlPlaneIngress admits the NLB→CP hops from the VPC CIDR: the
// apiserver (k3sAPIServerPort), the konnectivity-server agent port
// (konnectivityAgentPort) and the webhook readiness port. Public access is gated separately at the NLB
// front-end. Idempotent.
func EnsureControlPlaneIngress(ctx context.Context, sgp sgProvisioner, accountID, cpSGID, vpcCIDR string) error {
	if cpSGID == "" {
		return errors.New("eks: EnsureControlPlaneIngress empty control-plane SG id")
	}
	if vpcCIDR == "" {
		return errors.New("eks: EnsureControlPlaneIngress empty vpc cidr")
	}

	for _, p := range []struct {
		port int64
		desc string
	}{
		{k3sAPIServerPort, "EKS NLB to apiserver"},
		{konnectivityAgentPort, "EKS NLB to konnectivity-server"},
		{webhookReadyPort, "EKS NLB to token webhook readiness"},
	} {
		perm := &ec2.IpPermission{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(p.port),
			ToPort:     aws.Int64(p.port),
			IpRanges: []*ec2.IpRange{{
				CidrIp:      aws.String(vpcCIDR),
				Description: aws.String(p.desc),
			}},
		}
		_, err := sgp.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId:       aws.String(cpSGID),
			IpPermissions: []*ec2.IpPermission{perm},
		}, accountID)
		if err != nil && !awserrors.IsErrorCode(err, awserrors.ErrorInvalidPermissionDuplicate) {
			return fmt.Errorf("authorize control-plane %s ingress on %s: %w", p.desc, cpSGID, err)
		}
	}
	return nil
}

// EnsureControlPlaneHAIngress authorizes self-referencing CP SG rules for HA etcd peering:
// etcd client+peer (2379-2380/tcp) and kubelet (10250/tcp) within cpSG. Idempotent.
func EnsureControlPlaneHAIngress(ctx context.Context, sgp sgProvisioner, accountID, cpSGID string) error {
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
		_, err := sgp.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
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

// sgHasLBCClusterTag reports whether the SG carries the AWS LoadBalancer
// Controller ownership tag for this cluster.
func sgHasLBCClusterTag(g *ec2.SecurityGroup, clusterName string) bool {
	return slices.ContainsFunc(g.Tags, func(tag *ec2.Tag) bool {
		return tag != nil && aws.StringValue(tag.Key) == lbcClusterOwnershipTagKey && aws.StringValue(tag.Value) == clusterName
	})
}

// sgIsClusterSG reports whether g carries every tag the service stamps on this
// cluster's SG. aws:eks:cluster-name is reserved, so a customer cannot set it.
func sgIsClusterSG(g *ec2.SecurityGroup, clusterName string) bool {
	have := map[string]string{}
	for _, tag := range g.Tags {
		have[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	return have[tags.ManagedByKey] == tags.ManagedByEKS &&
		have["aws:eks:cluster-name"] == clusterName &&
		have[clusterEKSClusterTagKey] == clusterName &&
		have[clusterEKSRoleTagKey] == clusterEKSRoleCluster
}

// lookupClusterSG finds a cluster SG that an earlier, interrupted create made
// before its id was recorded. Only the tags the service alone can set count.
func lookupClusterSG(ctx context.Context, sgp sgProvisioner, accountID, vpcID, clusterName string) (string, error) {
	out, err := sgp.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: aws.StringSlice([]string{vpcID})},
			{Name: aws.String("tag:" + tags.ManagedByKey), Values: aws.StringSlice([]string{tags.ManagedByEKS})},
			{Name: aws.String("tag:aws:eks:cluster-name"), Values: aws.StringSlice([]string{clusterName})},
			{Name: aws.String("tag:" + clusterEKSClusterTagKey), Values: aws.StringSlice([]string{clusterName})},
			{Name: aws.String("tag:" + clusterEKSRoleTagKey), Values: aws.StringSlice([]string{clusterEKSRoleCluster})},
		},
	}, accountID)
	if err != nil {
		return "", fmt.Errorf("describe cluster SG for %s: %w", clusterName, err)
	}
	for _, g := range out.SecurityGroups {
		if g != nil && g.GroupId != nil && sgIsClusterSG(g, clusterName) {
			return *g.GroupId, nil
		}
	}
	return "", nil
}

func lookupSGByName(ctx context.Context, sgp sgProvisioner, accountID, vpcID, name string) (string, error) {
	out, err := sgp.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{
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
