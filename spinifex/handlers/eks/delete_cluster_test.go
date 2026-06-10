package handlers_eks

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSubnetResolver struct{}

var _ SubnetVPCResolver = (*fakeSubnetResolver)(nil)

func (fakeSubnetResolver) GetSubnetVPC(_, _ string) (string, error) { return "vpc-aaa", nil }
func (fakeSubnetResolver) GetVPCCIDR(_, _ string) (string, error)   { return "10.0.0.0/16", nil }

func deleteInput(name string) *eks.DeleteClusterInput {
	return &eks.DeleteClusterInput{Name: aws.String(name)}
}

// eksServiceFixture stands up an EKSServiceImpl with fake orchestration deps
// and an embedded JetStream KV. Tests poke the fakes' *Err fields to force
// failures at specific lifecycle steps.
type eksServiceFixture struct {
	svc    *EKSServiceImpl
	kv     nats.KeyValue
	nlb    *fakeNLBProvisioner
	inst   *fakeK3sInst
	vpc    *fakeK3sVPC
	ami    *fakeK3sAMI
	eip    *fakeEIPProvisioner
	sg     *fakeSGProvisioner
	worker *fakeWorkerLauncher
}

func newEKSServiceFixture(t *testing.T) *eksServiceFixture {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)

	nlb := newFakeNLBProvisioner()
	inst := &fakeK3sInst{}
	vpc := &fakeK3sVPC{}
	ami := &fakeK3sAMI{}
	eip := newFakeEIPProvisioner()
	sg := newFakeSGProvisioner()
	worker := newFakeWorkerLauncher()

	svc, err := NewEKSServiceImpl(EKSServiceDeps{
		NATSConn:         nc,
		MasterKey:        bootstrapTestMasterKey,
		GatewayBaseURL:   "https://gw.local:9999",
		Region:           "us-east-1",
		HolderID:         "node-1",
		SystemGatewayURL: "https://gw.local:9999",
		SystemAccessKey:  "AKIAEXAMPLE",
		SystemSecretKey:  "s3cr3t-key",
		GatewayCACert:    "-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n",
		VPCSG:            sg,
		VPCK3s:           vpc,
		VPCSubnet:        fakeSubnetResolver{},
		NLB:              nlb,
		Instance:         inst,
		Image:            ami,
		EIP:              eip,
		Worker:           worker,
		PlacementGroup:   &fakePlacer{},
		Scheduler:        &fakeHostScheduler{},
	})
	require.NoError(t, err)
	t.Cleanup(svc.Shutdown)

	return &eksServiceFixture{svc: svc, kv: kv, nlb: nlb, inst: inst, vpc: vpc, ami: ami, eip: eip, sg: sg, worker: worker}
}

// deleteClusterFixture is an eksServiceFixture pre-seeded with a CREATING
// cluster meta whose NLB/VM/SG resource references are all populated so
// DeleteCluster exercises every teardown branch.
type deleteClusterFixture struct {
	svc  *EKSServiceImpl
	kv   nats.KeyValue
	nlb  *fakeNLBProvisioner
	inst *fakeK3sInst
	vpc  *fakeK3sVPC
	eip  *fakeEIPProvisioner
}

func newDeleteClusterFixture(t *testing.T, clusterName string) *deleteClusterFixture {
	t.Helper()
	f := newEKSServiceFixture(t)
	kv := f.kv

	meta := sampleClusterMeta(clusterName)
	meta.NLBArn = "arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/net/" + ClusterNLBName(clusterName) + "/lb-001"
	meta.NLBTargetGroupArn = "arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/" + ClusterTargetGroupName(clusterName) + "/tg-001"
	meta.ControlPlaneENIIP = "10.0.1.42"
	meta.ControlPlaneInstanceID = "i-aaa111"
	meta.ControlPlaneENIID = "eni-aaa111"
	meta.ResourcesVpcConfig.VpcId = "vpc-aaa"
	meta.EgressEIPPublicIP = "203.0.113.50"
	meta.EgressEIPAllocationID = "eipalloc-fake01"
	require.NoError(t, PutClusterMeta(kv, meta))

	// Real OIDC key so ZeroizeClusterOIDCKey has material to wipe.
	_, _, err := GenerateClusterOIDCKeypair(kv, clusterName, bootstrapTestMasterKey)
	require.NoError(t, err)

	return &deleteClusterFixture{svc: f.svc, kv: kv, nlb: f.nlb, inst: f.inst, vpc: f.vpc, eip: f.eip}
}

func TestDeleteCluster_AllTeardownSucceedsSweepsKV(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")

	out, err := f.svc.DeleteCluster(deleteInput("alpha"), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	_, err = GetClusterMeta(f.kv, "alpha")
	assert.ErrorIs(t, err, ErrClusterNotFound, "meta must be swept after successful teardown")
	assert.Len(t, f.inst.terminateCalls, 1)
	assert.Len(t, f.vpc.deleteCalls, 1)
}

func TestDeleteCluster_NLBFailureLeavesMetaAndDELETING(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")

	// Seed the LB so delete is attempted, then force it to fail.
	lbName := ClusterNLBName("alpha")
	f.nlb.lbByName[lbName] = &elbv2.LoadBalancer{
		LoadBalancerArn:  aws.String("arn:lb"),
		LoadBalancerName: aws.String(lbName),
	}
	f.nlb.deleteLBErr = errors.New("elbv2 unavailable")

	_, err := f.svc.DeleteCluster(deleteInput("alpha"), testAccountID)
	require.Error(t, err, "teardown failure must surface, not be swallowed")

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr, "meta must survive so a retry can find the resource ARNs")
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.NotEmpty(t, meta.NLBArn, "NLB ARN must remain recorded for retry")
}

func TestDeleteCluster_VMTerminateFailureLeavesMeta(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("hypervisor unreachable")

	_, err := f.svc.DeleteCluster(deleteInput("alpha"), testAccountID)
	require.Error(t, err)

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
}

// A prior retry (or the egress-delete cascade) already released the allocation,
// so ReleaseAddress returns InvalidAllocationID.NotFound. That is idempotent
// success — teardown must complete and sweep the KV, not wedge in DELETING.
func TestDeleteCluster_EgressEIPAlreadyReleasedSweepsKV(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	f.eip.releaseErr = errors.New(awserrors.ErrorInvalidAllocationIDNotFound)

	out, err := f.svc.DeleteCluster(deleteInput("alpha"), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	require.Len(t, f.eip.releaseCalls, 1, "release must still be attempted")
	_, getErr := GetClusterMeta(f.kv, "alpha")
	assert.ErrorIs(t, getErr, ErrClusterNotFound, "NotFound release must not block the KV sweep")
}

// A genuine release failure (not a NotFound) is retryable and must still wedge
// the cluster in DELETING so the billable EIP is not orphaned.
func TestDeleteCluster_EgressEIPReleaseRealErrorLeavesMeta(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	f.eip.releaseErr = errors.New("AddressLimitExceeded")

	_, err := f.svc.DeleteCluster(deleteInput("alpha"), testAccountID)
	require.Error(t, err)

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.NotEmpty(t, meta.EgressEIPAllocationID, "alloc ID must remain for retry")
}
