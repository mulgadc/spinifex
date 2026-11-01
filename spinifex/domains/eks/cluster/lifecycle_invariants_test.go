package cluster

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/elbv2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRLC1_EKSDeleteClusterNotFoundOnAbsent pins AWS DeleteCluster semantics
// (ADR-0003:S3): a cluster already swept from KV returns ResourceNotFound and
// triggers no teardown. Internal callers treat that code as already deleted.
func TestRLC1_EKSDeleteClusterNotFoundOnAbsent(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)

	out, err := f.svc.DeleteCluster(context.Background(), deleteInput("absent"), testAccountID)
	require.EqualErrorf(t, err, awserrors.ErrorEKSResourceNotFound, "ADR-0003:S3: DeleteCluster on an absent cluster must return ResourceNotFoundException, as AWS does")
	assert.Nil(t, out)
	assert.Empty(t, f.inst.terminateCalls, "an absent cluster must trigger no billable teardown")
}

// TestRLC2_EKSBillableTeardownBeforeKVSweep pins billable-before-sweep
// (ADR-0003:S2): no key under the cluster is removed while a billable teardown
// step (OIDC / NLB / VM / EIP) is failing. The request accepts the delete; the
// failed terminate leaves the record DELETING with every ref intact, surfaces
// the failure in DescribeCluster, and a later pass with the fault gone sweeps it.
func TestRLC2_EKSBillableTeardownBeforeKVSweep(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("hypervisor unreachable")
	_, err := f.kv.Put(t.Context(), "clusters/alpha/addons/vpc-cni", []byte(`{}`))
	require.NoError(t, err)

	out, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err, "ADR-0003:S8: the request records the delete and returns; the teardown's outcome is not its answer")
	assert.Equal(t, string(ClusterStatusDeleting), aws.StringValue(out.Cluster.Status))
	f.svc.WaitTeardowns()

	require.Len(t, f.inst.terminated(), 1, "the VM teardown must have been attempted")
	meta, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoErrorf(t, getErr, "ADR-0003:S2 billable-before-sweep: the record must survive a failing billable teardown")
	assert.Equal(t, ClusterStatusDeleting, meta.Status, "a cluster with failed teardown must stay DELETING")
	assert.Equal(t, "i-aaa111", meta.ControlPlaneInstanceID, "the stranded VM's ID must stay recorded for the retry")
	assert.Equal(t, "eipalloc-fake01", meta.EgressEIPAllocationID)
	assert.NotEmpty(t, meta.NLBArn)
	assert.Contains(t, meta.LastDeleteReapError, "hypervisor unreachable")
	_, keyErr := f.kv.Get(t.Context(), "clusters/alpha/addons/vpc-cni")
	require.NoError(t, keyErr, "the KV sweep must not have run: the cluster's child records stay with it")

	desc, err := f.svc.DescribeCluster(context.Background(), &eks.DescribeClusterInput{Name: aws.String("alpha")}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, desc.Cluster.Health, "the failure must surface to the caller, not be swallowed")
	require.Len(t, desc.Cluster.Health.Issues, 1)
	assert.Equal(t, eks.ClusterIssueCodeInternalFailure, aws.StringValue(desc.Cluster.Health.Issues[0].Code))

	f.inst.mu.Lock()
	f.inst.terminateErr = nil
	f.inst.mu.Unlock()
	redrive(t, f.svc)
	_, getErr = GetClusterMeta(t.Context(), f.kv, "alpha")
	require.ErrorIs(t, getErr, ErrClusterNotFound, "once the teardown succeeds the record is swept")
}

// TestRLC3_EKSNLBNoOrphanTargetGroupAfterDelete exercises the historic PROP-LIFECYCLE-006 §6 (behaviour per ADR-0003:S2) NLB
// no-orphan (riding historic PROP-LIFECYCLE-002 cascade composition): after a
// successful DeleteCluster, the eks-{cluster}-cp target group must be gone — an
// orphaned EKS NLB target group would pin itself as ResourceInUse exactly as a
// user target group does.
func TestRLC3_EKSNLBNoOrphanTargetGroupAfterDelete(t *testing.T) {
	t.Parallel()
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

	deleteAndSettle(t, f.svc, "alpha")

	_, getErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.ErrorIs(t, getErr, ErrClusterNotFound, "a fully torn-down cluster must be swept")
	assert.NotContainsf(t, f.nlb.tgByName, tgName,
		"ADR-0003:S2 behaviour; historic PROP-LIFECYCLE-006 §6 mechanism, NLB no-orphan: the eks-{cluster}-cp target group must not survive DeleteCluster (rides historic PROP-LIFECYCLE-002 / mulga-siv-172)")
}
