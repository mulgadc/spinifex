package handlers_eks

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_EKSDeleteClusterIdempotentOnAbsent enforces the Common Resource
// Lifecycle Contract rule #1 (idempotent delete), ADR-0006 §1: DeleteCluster of
// a cluster already swept from KV returns success, not ResourceNotFound, so a
// tofu destroy retry / double-targeted graph node converges instead of failing
// the run. The live-reference teardown is unaffected — only true absence is
// idempotent.
func TestRLC1_EKSDeleteClusterIdempotentOnAbsent(t *testing.T) {
	f := newEKSServiceFixture(t)

	out, err := f.svc.DeleteCluster(deleteInput("absent"), testAccountID)
	require.NoErrorf(t, err, "ADR-0006 §1: DeleteCluster on an absent cluster must return success, not ResourceNotFound (RLC rule #1)")
	require.NotNil(t, out, "ADR-0006 §1: DeleteCluster must return a non-nil output on absent")
	assert.Empty(t, f.inst.terminateCalls, "an absent cluster must trigger no billable teardown")
}

// TestRLC2_EKSBillableTeardownBeforeKVSweep enforces ADR-0006 §6 billable-before-
// sweep ordering: the KV sweep (DeleteClusterPrefix) must NOT run while any
// billable teardown (OIDC / NLB / VM / EIP) returned an error. A failed VM
// terminate must leave the cluster meta in DELETING — its resource ARNs/IDs
// intact for a retry — never erased out from under a still-running billable VM.
// Regression guard for the errors.Join-before-sweep ordering in purgeClusterInfra.
func TestRLC2_EKSBillableTeardownBeforeKVSweep(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("hypervisor unreachable")

	_, err := f.svc.DeleteCluster(deleteInput("alpha"), testAccountID)
	require.Error(t, err, "ADR-0006 §6: a failed billable teardown must surface, not be swallowed")

	require.Len(t, f.inst.terminateCalls, 1, "the VM teardown must have been attempted")
	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoErrorf(t, getErr, "ADR-0006 §6 billable-before-sweep: the KV sweep must NOT run while a billable teardown is failing — the meta must survive for retry")
	assert.Equal(t, ClusterStatusDeleting, meta.Status, "a cluster with failed teardown must stay DELETING")
}

// TestRLC3_EKSNLBNoOrphanTargetGroupAfterDelete enforces ADR-0006 §6 NLB
// no-orphan (riding ADR-0002 / mulga-siv-172 cascade composition): after a
// successful DeleteCluster, the eks-{cluster}-cp target group must be gone — an
// orphaned EKS NLB target group would pin itself as ResourceInUse exactly as a
// user target group does.
func TestRLC3_EKSNLBNoOrphanTargetGroupAfterDelete(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")

	lbName := ClusterNLBName("alpha")
	tgName := ClusterTargetGroupName("alpha")
	f.nlb.lbByName[lbName] = &elbv2.LoadBalancer{
		LoadBalancerArn:  aws.String("arn:lb-alpha"),
		LoadBalancerName: aws.String(lbName),
	}
	f.nlb.tgByName[tgName] = &elbv2.TargetGroup{
		TargetGroupArn:  aws.String("arn:tg-alpha"),
		TargetGroupName: aws.String(tgName),
	}

	_, err := f.svc.DeleteCluster(deleteInput("alpha"), testAccountID)
	require.NoError(t, err)

	_, getErr := GetClusterMeta(f.kv, "alpha")
	require.ErrorIs(t, getErr, ErrClusterNotFound, "a fully torn-down cluster must be swept")
	assert.NotContainsf(t, f.nlb.tgByName, tgName,
		"ADR-0006 §6 NLB no-orphan: the eks-{cluster}-cp target group must not survive DeleteCluster (rides ADR-0002/172)")
}
