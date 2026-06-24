package handlers_eks

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSGProvisioner struct {
	mu               sync.Mutex
	createCalls      []*ec2.CreateSecurityGroupInput
	describeCalls    []*ec2.DescribeSecurityGroupsInput
	deleteCalls      []*ec2.DeleteSecurityGroupInput
	authorizeCalls   []*ec2.AuthorizeSecurityGroupIngressInput
	revokeCalls      []*ec2.RevokeSecurityGroupIngressInput
	revokeEgressCall []*ec2.RevokeSecurityGroupEgressInput

	// existing maps "name|vpcId" → groupId for DescribeSecurityGroups lookup.
	existing map[string]string

	// perms maps groupId → its current ingress rules, served by DescribeSecurityGroups
	// and cleared by RevokeSecurityGroupIngress. permsEgress is the egress equivalent.
	perms       map[string][]*ec2.IpPermission
	permsEgress map[string][]*ec2.IpPermission

	// enforceDepViolation models AWS refusing to delete an SG that still has
	// ingress rules (e.g. a sibling cross-reference), so a test proves the
	// revoke-before-delete ordering.
	enforceDepViolation bool

	// deleteFailsBefore maps groupId → the number of DeleteSecurityGroup calls
	// that return DependencyViolation before succeeding, modelling a worker ENI
	// detaching after a few retries.
	deleteFailsBefore map[string]int

	// nextCreateID is returned by the next CreateSecurityGroup call. Tests can
	// pre-seed a queue via createIDs to differentiate the control-plane vs
	// nodegroup SG IDs.
	createIDs []string

	createErr    error
	describeErr  error
	deleteErr    error
	authorizeErr error
}

var _ sgProvisioner = (*fakeSGProvisioner)(nil)

func newFakeSGProvisioner() *fakeSGProvisioner {
	return &fakeSGProvisioner{existing: map[string]string{}}
}

func (f *fakeSGProvisioner) CreateSecurityGroup(input *ec2.CreateSecurityGroupInput, _ string) (*ec2.CreateSecurityGroupOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, input)
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := "sg-default"
	if len(f.createIDs) > 0 {
		id = f.createIDs[0]
		f.createIDs = f.createIDs[1:]
	}
	if input.GroupName != nil && input.VpcId != nil {
		f.existing[*input.GroupName+"|"+*input.VpcId] = id
	}
	return &ec2.CreateSecurityGroupOutput{GroupId: aws.String(id)}, nil
}

func (f *fakeSGProvisioner) DescribeSecurityGroups(input *ec2.DescribeSecurityGroupsInput, _ string) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describeCalls = append(f.describeCalls, input)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if len(input.GroupIds) > 0 {
		out := &ec2.DescribeSecurityGroupsOutput{}
		for _, gid := range input.GroupIds {
			if gid == nil {
				continue
			}
			out.SecurityGroups = append(out.SecurityGroups, &ec2.SecurityGroup{
				GroupId:             gid,
				IpPermissions:       f.perms[*gid],
				IpPermissionsEgress: f.permsEgress[*gid],
			})
		}
		return out, nil
	}
	var name, vpc string
	for _, filt := range input.Filters {
		if filt == nil || filt.Name == nil || len(filt.Values) == 0 || filt.Values[0] == nil {
			continue
		}
		switch *filt.Name {
		case "group-name":
			name = *filt.Values[0]
		case "vpc-id":
			vpc = *filt.Values[0]
		}
	}
	out := &ec2.DescribeSecurityGroupsOutput{}
	// vpc-id-only filter: enumerate every SG in the VPC (the teardown sweep).
	if name == "" && vpc != "" {
		for key, id := range f.existing {
			parts := strings.SplitN(key, "|", 2)
			if len(parts) != 2 || parts[1] != vpc {
				continue
			}
			out.SecurityGroups = append(out.SecurityGroups, &ec2.SecurityGroup{
				GroupId:             aws.String(id),
				GroupName:           aws.String(parts[0]),
				VpcId:               aws.String(vpc),
				IpPermissions:       f.perms[id],
				IpPermissionsEgress: f.permsEgress[id],
			})
		}
		return out, nil
	}
	if id, ok := f.existing[name+"|"+vpc]; ok {
		out.SecurityGroups = []*ec2.SecurityGroup{{
			GroupId:             aws.String(id),
			GroupName:           aws.String(name),
			VpcId:               aws.String(vpc),
			IpPermissions:       f.perms[id],
			IpPermissionsEgress: f.permsEgress[id],
		}}
	}
	return out, nil
}

func (f *fakeSGProvisioner) DeleteSecurityGroup(input *ec2.DeleteSecurityGroupInput, _ string) (*ec2.DeleteSecurityGroupOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls = append(f.deleteCalls, input)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if input.GroupId != nil && f.deleteFailsBefore[*input.GroupId] > 0 {
		f.deleteFailsBefore[*input.GroupId]--
		return nil, errors.New("DependencyViolation: resource has a dependent object")
	}
	if f.enforceDepViolation && input.GroupId != nil && len(f.perms[*input.GroupId]) > 0 {
		return nil, errors.New("DependencyViolation: resource has a dependent object")
	}
	return &ec2.DeleteSecurityGroupOutput{}, nil
}

func (f *fakeSGProvisioner) RevokeSecurityGroupIngress(input *ec2.RevokeSecurityGroupIngressInput, _ string) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeCalls = append(f.revokeCalls, input)
	if input.GroupId != nil {
		delete(f.perms, *input.GroupId)
	}
	return &ec2.RevokeSecurityGroupIngressOutput{}, nil
}

func (f *fakeSGProvisioner) RevokeSecurityGroupEgress(input *ec2.RevokeSecurityGroupEgressInput, _ string) (*ec2.RevokeSecurityGroupEgressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeEgressCall = append(f.revokeEgressCall, input)
	if input.GroupId != nil {
		delete(f.permsEgress, *input.GroupId)
	}
	return &ec2.RevokeSecurityGroupEgressOutput{}, nil
}

func (f *fakeSGProvisioner) AuthorizeSecurityGroupIngress(input *ec2.AuthorizeSecurityGroupIngressInput, _ string) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authorizeCalls = append(f.authorizeCalls, input)
	if f.authorizeErr != nil {
		return nil, f.authorizeErr
	}
	return &ec2.AuthorizeSecurityGroupIngressOutput{}, nil
}

func TestEnsureControlPlaneOverlaySGRules_AuthorizesVXLANAndKubelet(t *testing.T) {
	sgp := newFakeSGProvisioner()

	require.NoError(t, EnsureControlPlaneOverlaySGRules(sgp, "111122223333", "sg-overlay", "sg-ng"))
	require.Len(t, sgp.authorizeCalls, 4, "VXLAN both ways + kubelet overlay->node + CP mesh self-VXLAN")

	type key struct {
		target string
		source string
		proto  string
		port   int64
	}
	got := map[key]bool{}
	for _, in := range sgp.authorizeCalls {
		require.Len(t, in.IpPermissions, 1)
		p := in.IpPermissions[0]
		require.Len(t, p.UserIdGroupPairs, 1)
		got[key{
			target: aws.StringValue(in.GroupId),
			source: aws.StringValue(p.UserIdGroupPairs[0].GroupId),
			proto:  aws.StringValue(p.IpProtocol),
			port:   aws.Int64Value(p.FromPort),
		}] = true
	}
	assert.True(t, got[key{"sg-ng", "sg-overlay", "udp", 8472}], "CP overlay -> node VXLAN")
	assert.True(t, got[key{"sg-overlay", "sg-ng", "udp", 8472}], "node -> CP overlay VXLAN")
	assert.True(t, got[key{"sg-ng", "sg-overlay", "tcp", 10250}], "apiserver -> kubelet via overlay")
	assert.True(t, got[key{"sg-overlay", "sg-overlay", "udp", 8472}], "CP overlay mesh VXLAN")
}

func TestEnsureControlPlaneOverlaySGRules_EmptyInputsRejected(t *testing.T) {
	sgp := newFakeSGProvisioner()

	require.Error(t, EnsureControlPlaneOverlaySGRules(sgp, "111122223333", "", "sg-ng"))
	require.Error(t, EnsureControlPlaneOverlaySGRules(sgp, "111122223333", "sg-overlay", ""))
	assert.Empty(t, sgp.authorizeCalls)
}

func TestEnsureClusterSGs_EmptyInputsRejected(t *testing.T) {
	sgp := newFakeSGProvisioner()

	_, _, err := EnsureClusterSGs(sgp, "111122223333", "", "vpc-aaa")
	require.Error(t, err)

	_, _, err = EnsureClusterSGs(sgp, "111122223333", "alpha", "")
	require.Error(t, err)

	assert.Empty(t, sgp.createCalls)
	assert.Empty(t, sgp.describeCalls)
}

func TestEnsureClusterSGs_FreshCreatesBoth(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.createIDs = []string{"sg-cp-001", "sg-ng-002"}

	cpID, ngID, err := EnsureClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err)
	assert.Equal(t, "sg-cp-001", cpID)
	assert.Equal(t, "sg-ng-002", ngID)

	require.Len(t, sgp.createCalls, 2)

	assertSGCreateTagged(t, sgp.createCalls[0], "eks-cluster-alpha-control-plane-sg", "vpc-aaa", "alpha", clusterEKSRoleControlPlane)
	assertSGCreateTagged(t, sgp.createCalls[1], "eks-cluster-alpha-nodegroup-sg", "vpc-aaa", "alpha", clusterEKSRoleNodegroup)
}

func TestEnsureClusterSGs_IdempotentReusesExisting(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-existing-cp"
	sgp.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-existing-ng"

	cpID, ngID, err := EnsureClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err)
	assert.Equal(t, "sg-existing-cp", cpID)
	assert.Equal(t, "sg-existing-ng", ngID)
	assert.Empty(t, sgp.createCalls, "no create calls expected when SGs already exist")
}

func TestEnsureClusterSGs_MixedExistenceCreatesMissing(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-existing-cp"
	sgp.createIDs = []string{"sg-new-ng"}

	cpID, ngID, err := EnsureClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err)
	assert.Equal(t, "sg-existing-cp", cpID)
	assert.Equal(t, "sg-new-ng", ngID)

	require.Len(t, sgp.createCalls, 1, "only nodegroup SG should be created")
	assert.Equal(t, "eks-cluster-alpha-nodegroup-sg", aws.StringValue(sgp.createCalls[0].GroupName))
}

func TestEnsureClusterSGs_CreateErrorSurfacedFromControlPlane(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.createErr = errors.New("vpcd unavailable")

	_, _, err := EnsureClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create SG eks-cluster-alpha-control-plane-sg")
}

func TestDeleteClusterSGs_DeletesBothExisting(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-existing-cp"
	sgp.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-existing-ng"

	err := DeleteClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err)

	require.Len(t, sgp.deleteCalls, 2)
	assert.Equal(t, "sg-existing-cp", aws.StringValue(sgp.deleteCalls[0].GroupId))
	assert.Equal(t, "sg-existing-ng", aws.StringValue(sgp.deleteCalls[1].GroupId))
}

func TestDeleteClusterSGs_MissingSGsNoOp(t *testing.T) {
	sgp := newFakeSGProvisioner()

	err := DeleteClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err)
	assert.Empty(t, sgp.deleteCalls, "no delete calls expected when SGs already absent")
}

func TestDeleteClusterSGs_FirstErrorSurfacedSweepContinues(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-existing-cp"
	sgp.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-existing-ng"
	// A non-DependencyViolation error is not retried, so the sweep surfaces it
	// after a single attempt per SG (the retry path is covered separately).
	sgp.deleteErr = errors.New("vpcd unavailable")

	err := DeleteClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sg-existing-cp")
	assert.Len(t, sgp.deleteCalls, 2, "both delete calls should be attempted despite first error")
}

// TestDeleteClusterSGs_RetriesDependencyViolation guards the worker-ENI-detach
// race: a terminating worker's ENI can still reference the nodegroup SG when the
// delete runs. DeleteSecurityGroup must retry the DependencyViolation until the
// instance-terminate cascade releases the ENI (delete succeeds) and surface a
// persistent one past the budget so the SG is never silently leaked — a leaked
// cluster SG pins the customer VPC.
func TestDeleteClusterSGs_RetriesDependencyViolation(t *testing.T) {
	origBudget, origInterval := sgDeleteWaitBudget, sgDeleteWaitInterval
	sgDeleteWaitBudget, sgDeleteWaitInterval = 200*time.Millisecond, time.Millisecond
	defer func() { sgDeleteWaitBudget, sgDeleteWaitInterval = origBudget, origInterval }()

	t.Run("transient then succeeds", func(t *testing.T) {
		sgp := newFakeSGProvisioner()
		sgp.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-ng"
		sgp.deleteFailsBefore = map[string]int{"sg-ng": 2} // 2 DepViolations, then ok

		err := DeleteClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
		require.NoError(t, err, "delete must succeed once the ENI detaches")
		assert.GreaterOrEqual(t, len(sgp.deleteCalls), 3, "retried past the transient violations")
	})

	t.Run("persistent surfaced past budget", func(t *testing.T) {
		sgp := newFakeSGProvisioner()
		sgp.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-ng"
		sgp.deleteErr = errors.New("DependencyViolation: still referenced")

		err := DeleteClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
		require.Error(t, err, "a persistent DependencyViolation must surface, not leak the SG")
		assert.Contains(t, err.Error(), "sg-ng")
	})
}

// TestDeleteClusterSGs_RevokesCrossRefsBeforeDelete guards the no-orphan
// teardown contract: the control-plane and nodegroup SGs reference each other, so
// AWS rejects deleting either while the other's rule points at it. DeleteClusterSGs
// must revoke all ingress on both before deleting, or the SGs leak and pin the VPC.
func TestDeleteClusterSGs_RevokesCrossRefsBeforeDelete(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-cp"
	sgp.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-ng"
	// cp<->ng cross-references, as EnsureNodegroupSGRules creates them.
	sgp.perms = map[string][]*ec2.IpPermission{
		"sg-cp": {groupRefPerm("tcp", 6443, "sg-ng"), groupRefPerm("udp", 8472, "sg-ng")},
		"sg-ng": {groupRefPerm("udp", 8472, "sg-cp"), groupRefPerm("tcp", 10250, "sg-cp")},
	}
	// AWS refuses to delete an SG still referenced by a sibling's rule.
	sgp.enforceDepViolation = true

	err := DeleteClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err, "revoke-then-delete must clear the cp<->ng cycle so both SGs delete")

	require.Len(t, sgp.revokeCalls, 2, "ingress on both SGs must be revoked before delete")
	require.Len(t, sgp.deleteCalls, 2, "both SGs deleted once cross-refs are cleared")
	deleted := map[string]bool{}
	for _, d := range sgp.deleteCalls {
		deleted[aws.StringValue(d.GroupId)] = true
	}
	assert.True(t, deleted["sg-cp"] && deleted["sg-ng"], "both cluster SGs removed")
}

// TestDeleteClusterSGs_RevokesEgressAndNonClusterReferrers guards mulga-siv-410:
// a cluster SG is pinned by ANY referencing rule in the VPC, including an egress
// rule on a non-cluster SG (LBC/ALB, user/TF). The teardown must revoke both
// directions on every non-default SG so the cluster SGs delete, must NOT delete
// the referrer SGs (their owners do), and must leave the default SG untouched.
func TestDeleteClusterSGs_RevokesEgressAndNonClusterReferrers(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-cp"
	sgp.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-ng"
	sgp.existing["alb-controller-sg|vpc-aaa"] = "sg-lbc"
	sgp.existing["default|vpc-aaa"] = "sg-default"
	sgp.perms = map[string][]*ec2.IpPermission{
		"sg-cp":      {groupRefPerm("tcp", 6443, "sg-ng")},
		"sg-default": {groupRefPerm("-1", 0, "sg-default")},
	}
	// The LBC SG references the cluster CP SG via an EGRESS rule: an ingress-only
	// revoke would leave this cross-reference pinning sg-cp on delete.
	sgp.permsEgress = map[string][]*ec2.IpPermission{
		"sg-lbc": {groupRefPerm("tcp", 6443, "sg-cp")},
		"sg-cp":  {groupRefPerm("udp", 8472, "sg-ng")},
	}
	sgp.enforceDepViolation = true

	err := DeleteClusterSGs(sgp, "111122223333", "alpha", "vpc-aaa")
	require.NoError(t, err, "revoking both directions on all non-default SGs must clear every cycle")

	revokedEgress := map[string]bool{}
	for _, r := range sgp.revokeEgressCall {
		revokedEgress[aws.StringValue(r.GroupId)] = true
	}
	assert.True(t, revokedEgress["sg-lbc"], "egress on the LBC referrer SG must be revoked")
	assert.True(t, revokedEgress["sg-cp"], "egress on the cluster CP SG must be revoked")

	// The default SG must never be revoked or deleted.
	for _, r := range sgp.revokeCalls {
		assert.NotEqual(t, "sg-default", aws.StringValue(r.GroupId), "default SG ingress must not be revoked")
	}
	for _, r := range sgp.revokeEgressCall {
		assert.NotEqual(t, "sg-default", aws.StringValue(r.GroupId), "default SG egress must not be revoked")
	}

	deleted := map[string]bool{}
	for _, d := range sgp.deleteCalls {
		deleted[aws.StringValue(d.GroupId)] = true
	}
	assert.True(t, deleted["sg-cp"] && deleted["sg-ng"], "both cluster SGs deleted")
	assert.False(t, deleted["sg-lbc"], "referrer SG must NOT be deleted; its owner handles it")
	assert.False(t, deleted["sg-default"], "default SG must never be deleted")
}

func groupRefPerm(proto string, port int64, refSG string) *ec2.IpPermission {
	return &ec2.IpPermission{
		IpProtocol:       aws.String(proto),
		FromPort:         aws.Int64(port),
		ToPort:           aws.Int64(port),
		UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String(refSG)}},
	}
}

func TestEnsureControlPlaneIngress_AuthorizesAPIServerFromVPCCIDR(t *testing.T) {
	sgp := newFakeSGProvisioner()

	err := EnsureControlPlaneIngress(sgp, "111122223333", "sg-cp-001", "10.0.0.0/16")
	require.NoError(t, err)

	require.Len(t, sgp.authorizeCalls, 1)
	in := sgp.authorizeCalls[0]
	assert.Equal(t, "sg-cp-001", aws.StringValue(in.GroupId))
	require.Len(t, in.IpPermissions, 1)
	perm := in.IpPermissions[0]
	assert.Equal(t, "tcp", aws.StringValue(perm.IpProtocol))
	assert.Equal(t, k3sAPIServerPort, aws.Int64Value(perm.FromPort))
	assert.Equal(t, k3sAPIServerPort, aws.Int64Value(perm.ToPort))
	require.Len(t, perm.IpRanges, 1)
	assert.Equal(t, "10.0.0.0/16", aws.StringValue(perm.IpRanges[0].CidrIp))
}

func TestEnsureControlPlaneIngress_EmptyInputsRejected(t *testing.T) {
	sgp := newFakeSGProvisioner()

	require.Error(t, EnsureControlPlaneIngress(sgp, "111122223333", "", "10.0.0.0/16"))
	require.Error(t, EnsureControlPlaneIngress(sgp, "111122223333", "sg-cp-001", ""))
	assert.Empty(t, sgp.authorizeCalls)
}

func TestEnsureControlPlaneIngress_DuplicateTolerated(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.authorizeErr = errors.New(awserrors.ErrorInvalidPermissionDuplicate)

	err := EnsureControlPlaneIngress(sgp, "111122223333", "sg-cp-001", "10.0.0.0/16")
	require.NoError(t, err, "a duplicate rule on re-run must be treated as success")
}

func TestEnsureControlPlaneIngress_AuthorizeErrorSurfaced(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.authorizeErr = errors.New("vpcd unavailable")

	err := EnsureControlPlaneIngress(sgp, "111122223333", "sg-cp-001", "10.0.0.0/16")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sg-cp-001")
}

func TestEnsureControlPlaneHAIngress_AuthorizesEtcdAndKubeletSelfReferenced(t *testing.T) {
	sgp := newFakeSGProvisioner()

	err := EnsureControlPlaneHAIngress(sgp, "111122223333", "sg-cp-001")
	require.NoError(t, err)

	require.Len(t, sgp.authorizeCalls, 2, "etcd peer/client + kubelet")

	// Collect the authorized (proto, from, to) tuples and assert each rule is
	// self-referencing on the control-plane SG (source == target group).
	type portRange struct {
		proto    string
		from, to int64
	}
	got := map[portRange]bool{}
	for _, in := range sgp.authorizeCalls {
		assert.Equal(t, "sg-cp-001", aws.StringValue(in.GroupId))
		require.Len(t, in.IpPermissions, 1)
		perm := in.IpPermissions[0]
		assert.Empty(t, perm.IpRanges, "HA rules are group-referenced, not CIDR")
		require.Len(t, perm.UserIdGroupPairs, 1)
		assert.Equal(t, "sg-cp-001", aws.StringValue(perm.UserIdGroupPairs[0].GroupId),
			"control-plane SG must reference itself so the rule survives CP churn")
		got[portRange{
			aws.StringValue(perm.IpProtocol),
			aws.Int64Value(perm.FromPort),
			aws.Int64Value(perm.ToPort),
		}] = true
	}

	// 2380 (etcd peer) is the port whose absence silently breaks the join quorum.
	assert.True(t, got[portRange{"tcp", 2379, 2380}], "etcd client+peer 2379-2380 must be opened")
	assert.True(t, got[portRange{"tcp", 10250, 10250}], "kubelet 10250 must be opened")
}

func TestEnsureControlPlaneHAIngress_EmptyInputRejected(t *testing.T) {
	sgp := newFakeSGProvisioner()

	require.Error(t, EnsureControlPlaneHAIngress(sgp, "111122223333", ""))
	assert.Empty(t, sgp.authorizeCalls)
}

func TestEnsureControlPlaneHAIngress_DuplicateTolerated(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.authorizeErr = errors.New(awserrors.ErrorInvalidPermissionDuplicate)

	err := EnsureControlPlaneHAIngress(sgp, "111122223333", "sg-cp-001")
	require.NoError(t, err, "a duplicate rule on re-run must be treated as success")
}

func TestEnsureControlPlaneHAIngress_AuthorizeErrorSurfaced(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.authorizeErr = errors.New("vpcd unavailable")

	err := EnsureControlPlaneHAIngress(sgp, "111122223333", "sg-cp-001")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sg-cp-001")
}

func assertSGCreateTagged(t *testing.T, in *ec2.CreateSecurityGroupInput, name, vpcID, clusterName, role string) {
	t.Helper()
	require.NotNil(t, in)
	assert.Equal(t, name, aws.StringValue(in.GroupName))
	assert.Equal(t, vpcID, aws.StringValue(in.VpcId))
	require.Len(t, in.TagSpecifications, 1)
	spec := in.TagSpecifications[0]
	require.NotNil(t, spec)
	assert.Equal(t, "security-group", aws.StringValue(spec.ResourceType))

	got := map[string]string{}
	for _, tg := range spec.Tags {
		if tg == nil || tg.Key == nil || tg.Value == nil {
			continue
		}
		got[*tg.Key] = *tg.Value
	}
	assert.Equal(t, tags.ManagedByEKS, got[tags.ManagedByKey])
	assert.Equal(t, clusterName, got[clusterEKSClusterTagKey])
	assert.Equal(t, role, got[clusterEKSRoleTagKey])
}
