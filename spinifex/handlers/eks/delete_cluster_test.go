package handlers_eks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
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

func (fakeSubnetResolver) GetSubnetVPC(_ context.Context, _, _ string) (string, error) {
	return "vpc-aaa", nil
}
func (fakeSubnetResolver) GetVPCCIDR(_ context.Context, _, _ string) (string, error) {
	return "10.0.0.0/16", nil
}
func (fakeSubnetResolver) GetSubnetAZ(_ context.Context, _, _ string) (string, error) {
	return "spinifexz1", nil
}

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
	vpcMgr *fakeVPCProvisioner
	igw    *fakeIGWProvisioner
	ngw    *fakeNatGatewayProvisioner
	rt     *fakeRouteTableProvisioner
}

func newEKSServiceFixture(t *testing.T) *eksServiceFixture {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := GetOrCreateAccountBucket(js, testAccountID, 1)
	require.NoError(t, err)

	nlb := newFakeNLBProvisioner()
	inst := &fakeK3sInst{}
	vpc := &fakeK3sVPC{}
	ami := &fakeK3sAMI{}
	eip := newFakeEIPProvisioner()
	sg := newFakeSGProvisioner()
	worker := newFakeWorkerLauncher()
	vpcMgr := newFakeVPCProvisioner()
	igw := newFakeIGWProvisioner()
	ngw := newFakeNatGatewayProvisioner()
	rt := newFakeRouteTableProvisioner()

	svc, err := NewEKSServiceImpl(EKSServiceDeps{
		NATSConn:            nc,
		MasterKey:           bootstrapTestMasterKey,
		GatewayBaseURL:      "https://gw.local:9999",
		Region:              "us-east-1",
		HolderID:            "node-1",
		SystemGatewayURL:    "https://gw.local:9999",
		SystemAccessKey:     "AKIAEXAMPLE",
		SystemSecretKey:     "s3cr3t-key",
		SystemPredastoreURL: "https://10.15.8.1:8443",
		GatewayCACert:       "-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n",
		VPCSG:               sg,
		VPCK3s:              vpc,
		VPCSubnet:           fakeSubnetResolver{},
		IAM:                 newFakeEnsurer(),
		NLB:                 nlb,
		Instance:            inst,
		Image:               ami,
		EIP:                 eip,
		Worker:              worker,
		VPCMgr:              vpcMgr,
		IGW:                 igw,
		NATGW:               ngw,
		RouteTable:          rt,
		PlacementGroup:      &fakePlacer{},
		Scheduler:           &fakeHostScheduler{},
	})
	require.NoError(t, err)
	// Tight Ready-gating knobs so nodegroup launches resolve in tests without the
	// production 10m timeout; tests drive meta.NodeCount to simulate the reconciler.
	svc.nodegroupReadyTimeout = 1 * time.Second
	svc.nodegroupReadyPoll = 2 * time.Millisecond
	t.Cleanup(svc.Shutdown)

	return &eksServiceFixture{svc: svc, kv: kv, nlb: nlb, inst: inst, vpc: vpc, ami: ami, eip: eip, sg: sg, worker: worker, vpcMgr: vpcMgr, igw: igw, ngw: ngw, rt: rt}
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
	sg   *fakeSGProvisioner
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

	return &deleteClusterFixture{svc: f.svc, kv: kv, nlb: f.nlb, inst: f.inst, vpc: f.vpc, eip: f.eip, sg: f.sg}
}

// TestDeleteCluster_LeakedSGKeepsClusterDeleting guards the customer-VPC SG
// no-orphan contract: the cluster SGs live in the customer VPC,
// so a DeleteClusterSGs failure must surface and leave the cluster DELETING — its
// meta intact for a retry — never complete deletion with an SG stranded that pins
// the VPC on DependencyViolation. All other teardown steps succeed, isolating the
// SG failure as the sole cause.
func TestDeleteCluster_LeakedSGKeepsClusterDeleting(t *testing.T) {
	origBudget, origInterval := sgDeleteWaitBudget, sgDeleteWaitInterval
	sgDeleteWaitBudget, sgDeleteWaitInterval = 50*time.Millisecond, time.Millisecond
	defer func() { sgDeleteWaitBudget, sgDeleteWaitInterval = origBudget, origInterval }()

	f := newDeleteClusterFixture(t, "alpha")
	f.sg.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-cp"
	f.sg.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-ng"
	// A worker ENI never detaches — the SG delete keeps returning DependencyViolation.
	f.sg.deleteErr = errors.New("DependencyViolation: resource has a dependent object")

	_, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.Error(t, err, "a leaked customer-VPC SG must surface, not be swallowed")

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr, "the KV sweep must NOT run while an SG is still leaked — meta must survive for retry")
	assert.Equal(t, ClusterStatusDeleting, meta.Status, "a cluster with a leaked SG must stay DELETING")
}

// TestRLC5_DeleteClusterCPVPCReleasesNATGWEIPAfterRoutes locks the
// teardown order: route tables must be torn down before the NAT gateway, because
// the NAT GW delete is guarded against live route forwards (rule #3). Deleting it
// while the private route table still routes to it fails with DependencyViolation
// and strands the billable NAT-GW EIP.
func TestRLC5_DeleteClusterCPVPCReleasesNATGWEIPAfterRoutes(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.ngw.routeGuard = f.rt
	f.ngw.gws = []*fakeCPNatGateway{{
		id:    "nat-cp",
		tags:  map[string]string{clusterEKSClusterTagKey: "alpha", clusterEKSRoleTagKey: clusterEKSRoleCPNatGW},
		state: "available",
		addrs: []*ec2.NatGatewayAddress{{AllocationId: aws.String("eipalloc-cp")}},
	}}
	f.rt.tables = []*fakeCPRouteTable{{
		id:   "rtb-cp",
		vpc:  "vpc-cp",
		tags: map[string]string{clusterEKSClusterTagKey: "alpha", clusterEKSRoleTagKey: clusterEKSRolePrivateRT},
	}}

	require.NoError(t, DeleteClusterCPVPC(context.Background(), f.svc.cpVPCDeps(), testAccountID, "alpha"))

	require.Len(t, f.ngw.gws, 1)
	assert.Equal(t, "deleted", f.ngw.gws[0].state, "NAT GW must delete after routes are cleared (rule #3 guard not tripped)")
	require.Len(t, f.eip.releaseCalls, 1, "the NAT-GW EIP must be released, not stranded")
	assert.Equal(t, "eipalloc-cp", aws.StringValue(f.eip.releaseCalls[0].AllocationId))
}

// TestRLC1_DeleteClusterCPVPCToleratesAbsentVPC enforces the destroy-side half
// of the Common Resource Lifecycle Contract rule #1 (AWS-faithful delete): the
// EC2 DeleteVpc API now returns InvalidVpcID.NotFound for an absent VPC, so the
// teardown orchestrator must converge by tolerating that NotFound (a concurrent
// GC or a retried delete-cluster already removed it) — while a real failure
// still surfaces so the backstop retries.
func TestRLC1_DeleteClusterCPVPCToleratesAbsentVPC(t *testing.T) {
	cpVPC := []*fakeCPVPC{{
		id:   "vpc-cp",
		cidr: "10.0.0.0/16",
		tags: map[string]string{clusterEKSClusterTagKey: "alpha", clusterEKSRoleTagKey: clusterEKSRoleCPVPC},
	}}

	t.Run("absent VPC (NotFound) converges to success", func(t *testing.T) {
		f := newEKSServiceFixture(t)
		f.vpcMgr.vpcs = cpVPC
		f.vpcMgr.deleteVpcErr = errors.New(awserrors.ErrorInvalidVpcIDNotFound)

		require.NoErrorf(t, DeleteClusterCPVPC(context.Background(), f.svc.cpVPCDeps(), testAccountID, "alpha"),
			"RLC rule #1: teardown must tolerate DeleteVpc NotFound (resource already gone), not abort")
	})

	t.Run("a real DeleteVpc failure still surfaces", func(t *testing.T) {
		f := newEKSServiceFixture(t)
		f.vpcMgr.vpcs = cpVPC
		f.vpcMgr.deleteVpcErr = errors.New(awserrors.ErrorDependencyViolation)

		err := DeleteClusterCPVPC(context.Background(), f.svc.cpVPCDeps(), testAccountID, "alpha")
		require.Error(t, err, "a non-NotFound DeleteVpc failure must surface so the teardown backstop retries")
		assert.Contains(t, err.Error(), awserrors.ErrorDependencyViolation)
	})
}

func TestDeleteCluster_AllTeardownSucceedsSweepsKV(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")

	out, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)

	_, err = GetClusterMeta(f.kv, "alpha")
	assert.ErrorIs(t, err, ErrClusterNotFound, "meta must be swept after successful teardown")
	assert.Len(t, f.inst.terminateCalls, 1)
	assert.Len(t, f.vpc.deleteCalls, 1)
}

// TestDeleteCluster_DetachesPrivateEndpointENIBeforeDelete locks the
// teardown fix: the Set A private-endpoint ENI is an extra NIC on the cluster NLB's
// LB VM, not that VM's primary ENI, so the instance-terminate cascade never reclaims
// it. purgeClusterInfra must detach (store-clear) the stale attachment before
// deleting, or DeleteNetworkInterface loops on InvalidNetworkInterface.InUse and the
// cluster wedges in DELETING. The fake models that in-use-until-detached semantics,
// so the teardown only completes if detach precedes delete.
func TestDeleteCluster_DetachesPrivateEndpointENIBeforeDelete(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")

	meta, err := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, err)
	meta.PrivateEndpointENIID = "eni-pe-001"
	require.NoError(t, PutClusterMeta(f.kv, meta))
	f.vpc.inUseUntilDetached = map[string]bool{"eni-pe-001": true}

	out, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err, "teardown must complete: the private-endpoint ENI is detached before delete")
	require.NotNil(t, out)

	assert.Contains(t, f.vpc.detachCalls, "eni-pe-001", "private-endpoint ENI must be detached before delete")
	_, getErr := GetClusterMeta(f.kv, "alpha")
	assert.ErrorIs(t, getErr, ErrClusterNotFound, "successful teardown must sweep the meta")
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

	_, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.Error(t, err, "teardown failure must surface, not be swallowed")

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr, "meta must survive so a retry can find the resource ARNs")
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.NotEmpty(t, meta.NLBArn, "NLB ARN must remain recorded for retry")
}

func TestDeleteCluster_VMTerminateFailureLeavesMeta(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("hypervisor unreachable")

	_, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
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

	out, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
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

	_, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.Error(t, err)

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr)
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
	assert.NotEmpty(t, meta.EgressEIPAllocationID, "alloc ID must remain for retry")
}

// TestDeleteClusterSingleFlightLoserSkipsTeardown locks the contract: when a
// concurrent handler already holds the per-cluster teardown lease (an SDK retry
// fanned to another worker), DeleteCluster must return the cluster as DELETING
// without running purgeClusterInfra — no duplicate ENI/NLB/EIP teardown — and
// must leave the meta for the lease holder to sweep.
func TestDeleteClusterSingleFlightLoserSkipsTeardown(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")

	// Stand in for the winning handler: hold the teardown lease.
	_, err := f.svc.leaderKV.Create(teardownLeaderKey(testAccountID, "alpha"), []byte("node-2"))
	require.NoError(t, err)

	out, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Cluster)
	assert.Equal(t, string(ClusterStatusDeleting), aws.StringValue(out.Cluster.Status),
		"the loser must report the cluster as DELETING (AWS-async delete semantics)")

	assert.Empty(t, f.inst.terminateCalls, "the loser must not terminate the CP VM — the lease holder owns the teardown")
	assert.Empty(t, f.eip.releaseCalls, "the loser must not release the billable EIP")

	meta, getErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, getErr, "the loser must leave the meta for the lease holder to sweep")
	assert.Equal(t, ClusterStatusCreating, meta.Status, "the loser must not mutate KV state; the winner owns the DELETING transition")
}

// TestDeleteClusterWinnerReleasesTeardownLease: a successful synchronous delete
// must release the teardown lease so a later delete or the backstop reaper can
// re-acquire it (a leaked lease would wedge every future teardown until TTL).
func TestDeleteClusterWinnerReleasesTeardownLease(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")

	_, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)

	_, getErr := f.svc.leaderKV.Get(teardownLeaderKey(testAccountID, "alpha"))
	assert.ErrorIs(t, getErr, nats.ErrKeyNotFound, "the teardown lease must be released after a successful delete")
}

// TestDeleteCluster_ManagedCPVPCReclaimsCustomerVPCSGs guards the contract: in
// the managed control-plane VPC topology the nodegroup's worker-side security
// groups are created by launchNodegroupInfra in the CUSTOMER VPC
// (ResourcesVpcConfig.VpcId), not the managed CP VPC. Teardown must reclaim them
// there too, or they orphan — cross-referencing each other — and pin the
// customer VPC with DependencyViolation, hanging tofu destroy of the VPC.
func TestDeleteCluster_ManagedCPVPCReclaimsCustomerVPCSGs(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")

	meta, err := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, err)
	meta.ManagedCPVPC = &ManagedCPVPC{VpcId: "vpc-cp"}
	require.NoError(t, PutClusterMeta(f.kv, meta))

	// Worker-side SGs created in the customer VPC (vpc-aaa) by launchNodegroupInfra.
	f.sg.existing["eks-cluster-alpha-control-plane-sg|vpc-aaa"] = "sg-cust-cp"
	f.sg.existing["eks-cluster-alpha-nodegroup-sg|vpc-aaa"] = "sg-cust-ng"

	_, err = f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.NoError(t, err)

	var deleted []string
	for _, c := range f.sg.deleteCalls {
		deleted = append(deleted, aws.StringValue(c.GroupId))
	}
	assert.Contains(t, deleted, "sg-cust-cp", "customer-VPC control-plane SG must be reclaimed on teardown")
	assert.Contains(t, deleted, "sg-cust-ng", "customer-VPC nodegroup SG must be reclaimed on teardown")
}
