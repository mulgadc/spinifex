package handlers_eks

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWorkerLauncher records RunWorkerInstance / TerminateWorkerInstances calls
// and hands back sequential instance IDs so tests can assert on the IDs a
// nodegroup tracks. It is mutex-guarded so concurrent-reconcile tests exercise
// the production CAS path, not a race in the fake.
type fakeWorkerLauncher struct {
	mu             sync.Mutex
	runCalls       []*ec2.RunInstancesInput
	terminateCalls [][]string

	nextID  int
	runErr  error
	termErr error
}

var _ WorkerLauncher = (*fakeWorkerLauncher)(nil)

func newFakeWorkerLauncher() *fakeWorkerLauncher {
	return &fakeWorkerLauncher{}
}

func (f *fakeWorkerLauncher) RunWorkerInstance(input *ec2.RunInstancesInput, _ string) (*ec2.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls = append(f.runCalls, input)
	if f.runErr != nil {
		return nil, f.runErr
	}
	f.nextID++
	id := "i-worker" + strconv.Itoa(f.nextID)
	return &ec2.Reservation{Instances: []*ec2.Instance{{InstanceId: aws.String(id)}}}, nil
}

func (f *fakeWorkerLauncher) TerminateWorkerInstances(ids []string, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminateCalls = append(f.terminateCalls, ids)
	return f.termErr
}

// runCount / terminatedCount return the recorded call totals under the lock so
// concurrent tests can read them safely after joining.
func (f *fakeWorkerLauncher) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runCalls)
}

func (f *fakeWorkerLauncher) terminatedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for _, ids := range f.terminateCalls {
		n += len(ids)
	}
	return n
}

// seedActiveClusterWithToken lays down an ACTIVE cluster meta with the control-
// plane ENI IP + VPC populated and an encrypted K3s join token, so the
// nodegroup path can run end-to-end.
func seedActiveClusterWithToken(t *testing.T, f *eksServiceFixture, cluster string) {
	t.Helper()
	meta := &ClusterMeta{
		Name:              cluster,
		Status:            ClusterStatusActive,
		Version:           "1.32",
		ControlPlaneENIIP: "10.0.1.42",
		Endpoint:          "https://10.254.0.9:443",
		ResourcesVpcConfig: &ClusterVpcConfig{
			SubnetIds: []string{"subnet-aaa"},
			VpcId:     "vpc-aaa",
		},
	}
	require.NoError(t, PutClusterMeta(f.kv, meta))

	ct, err := handlers_iam.EncryptSecret("K10node-join-token::server:abc", bootstrapTestMasterKey)
	require.NoError(t, err)
	_, err = f.kv.Put(NodeTokenKey(cluster), []byte(ct))
	require.NoError(t, err)
}

// markWorkersReady simulates the cluster reconciler observing n Ready nodes from
// the CP state report, so launchNodegroupInfra's Ready-gate (waitWorkersReady)
// can resolve. Seed clusters start at NodeCount 0, so n is the create baseline
// (0) plus the worker count the test expects Ready.
func markWorkersReady(t *testing.T, f *eksServiceFixture, cluster string, n int) {
	t.Helper()
	require.NoError(t, SetClusterHealthState(f.kv, cluster, "", n))
}

func createNGInput(cluster, ng string, desired int64) *eks.CreateNodegroupInput {
	return &eks.CreateNodegroupInput{
		ClusterName:   aws.String(cluster),
		NodegroupName: aws.String(ng),
		NodeRole:      aws.String("arn:aws:iam::111122223333:role/eks-node"),
		Subnets:       aws.StringSlice([]string{"subnet-aaa"}),
		ScalingConfig: &eks.NodegroupScalingConfig{
			MinSize:     aws.Int64(1),
			MaxSize:     aws.Int64(3),
			DesiredSize: aws.Int64(desired),
		},
	}
}

func TestReclaimOrphanedNodegroups_TerminatesStrandedWorkers(t *testing.T) {
	f := newEKSServiceFixture(t)
	const cluster = "toc"
	seedActiveClusterWithToken(t, f, cluster)

	// CREATING left behind by a crashed launcher, workers already recorded.
	require.NoError(t, PutNodegroupRecord(f.kv, &NodegroupRecord{
		ClusterName: cluster, Name: "ng-stuck",
		Status:      eks.NodegroupStatusCreating,
		InstanceIDs: []string{"i-worker1", "i-worker2"},
	}))
	// CREATE_FAILED that still holds a partially-launched worker.
	require.NoError(t, PutNodegroupRecord(f.kv, &NodegroupRecord{
		ClusterName: cluster, Name: "ng-failed",
		Status:      eks.NodegroupStatusCreateFailed,
		InstanceIDs: []string{"i-worker3"},
	}))
	// ACTIVE must be left untouched.
	require.NoError(t, PutNodegroupRecord(f.kv, &NodegroupRecord{
		ClusterName: cluster, Name: "ng-active",
		Status:      eks.NodegroupStatusActive,
		InstanceIDs: []string{"i-worker9"},
	}))

	f.svc.reclaimOrphanedNodegroups(testAccountID, f.kv, cluster)

	var terminated []string
	for _, call := range f.worker.terminateCalls {
		terminated = append(terminated, call...)
	}
	assert.ElementsMatch(t, []string{"i-worker1", "i-worker2", "i-worker3"}, terminated)

	for _, ng := range []string{"ng-stuck", "ng-failed"} {
		got, err := GetNodegroupRecord(f.kv, cluster, ng)
		require.NoError(t, err)
		assert.Equal(t, eks.NodegroupStatusCreateFailed, got.Status, ng)
		assert.Empty(t, got.InstanceIDs, ng)
	}

	got, err := GetNodegroupRecord(f.kv, cluster, "ng-active")
	require.NoError(t, err)
	assert.Equal(t, eks.NodegroupStatusActive, got.Status)
	assert.Equal(t, []string{"i-worker9"}, got.InstanceIDs)
}

func TestNodegroupRecord_CRUDRoundTrip(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)

	rec := &NodegroupRecord{
		ClusterName:    "c1",
		Name:           "ng1",
		Arn:            "arn:aws:eks:us-east-1:111122223333:nodegroup/c1/ng1/abc",
		Status:         eks.NodegroupStatusActive,
		Subnets:        []string{"subnet-aaa"},
		InstanceTypes:  []string{"t3.medium"},
		ScalingMin:     1,
		ScalingMax:     3,
		ScalingDesired: 2,
		InstanceIDs:    []string{"i-1", "i-2"},
	}
	require.NoError(t, PutNodegroupRecord(kv, rec))

	got, err := GetNodegroupRecord(kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, rec.InstanceIDs, got.InstanceIDs)
	assert.Equal(t, int64(2), got.ScalingDesired)

	list, err := ListNodegroupRecords(kv, "c1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "ng1", list[0].Name)

	require.NoError(t, DeleteNodegroupRecord(kv, "c1", "ng1"))
	_, err = GetNodegroupRecord(kv, "c1", "ng1")
	assert.ErrorIs(t, err, ErrNodegroupNotFound)

	// Delete of an absent record is idempotent.
	require.NoError(t, DeleteNodegroupRecord(kv, "c1", "ng1"))
}

func TestBuildAgentUserData_Shape(t *testing.T) {
	ud := buildAgentUserData(agentUserDataInput{
		ClusterName:   "c1",
		NodegroupName: "ng1",
		ServerURL:     "https://10.254.0.9:443",
		JoinToken:     "K10secret::server:xyz",
		NodeName:      "c1-ng1-abc123de",
	})

	assert.True(t, strings.HasPrefix(ud, "#cloud-config\n"))
	assert.Contains(t, ud, "SPINIFEX_K3S_ROLE=agent")
	assert.Contains(t, ud, "K3S_URL=https://10.254.0.9:443")
	assert.Contains(t, ud, "K3S_TOKEN=K10secret::server:xyz")
	assert.Contains(t, ud, "K3S_NODE_NAME=c1-ng1-abc123de")
	assert.Contains(t, ud, "eks.amazonaws.com/nodegroup=ng1")
	assert.Contains(t, ud, agentEnvPath)
	// IMDS on-link route line present.
	assert.Contains(t, ud, "ip route replace "+imdsServerIP+"/32")
	// Route script is run directly, not via `rc-service local start` — that would
	// deadlock against cloud-final and stall the boot ~50s on the OpenRC timeout.
	assert.Contains(t, ud, "[ /etc/local.d/imds-onlink-route.start ]")
	assert.NotContains(t, ud, "rc-service, local, start")
	// Exactly one write_files block (single "write_files:" key).
	assert.Equal(t, 1, strings.Count(ud, "write_files:"))
}

func TestEnsureNodegroupSGRules_AuthorizesExpectedRules(t *testing.T) {
	sg := newFakeSGProvisioner()

	require.NoError(t, EnsureNodegroupSGRules(sg, testAccountID, "c1", "sg-cp", "sg-ng"))

	require.Len(t, sg.authorizeCalls, 6)

	// Verify the apiserver rule: 6443/tcp on cpSG sourced from ngSG.
	var found6443 bool
	for _, call := range sg.authorizeCalls {
		require.Len(t, call.IpPermissions, 1)
		perm := call.IpPermissions[0]
		if aws.StringValue(call.GroupId) == "sg-cp" &&
			aws.StringValue(perm.IpProtocol) == "tcp" &&
			aws.Int64Value(perm.FromPort) == 6443 {
			require.Len(t, perm.UserIdGroupPairs, 1)
			assert.Equal(t, "sg-ng", aws.StringValue(perm.UserIdGroupPairs[0].GroupId))
			assert.Empty(t, perm.IpRanges, "rules must be group-to-group, not CIDR")
			found6443 = true
		}
	}
	assert.True(t, found6443, "expected agent→apiserver 6443/tcp rule")
}

func TestEnsureNodegroupSGRules_DuplicateRuleTolerated(t *testing.T) {
	sg := newFakeSGProvisioner()
	sg.authorizeErr = errors.New(awserrors.ErrorInvalidPermissionDuplicate)

	require.NoError(t, EnsureNodegroupSGRules(sg, testAccountID, "c1", "sg-cp", "sg-ng"),
		"duplicate-rule error must be treated as success")
}

func TestCreateNodegroup_HappyPath(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	out, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 2), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Nodegroup)
	// The create accepts the request as CREATING; worker launch runs async.
	assert.Equal(t, eks.NodegroupStatusCreating, aws.StringValue(out.Nodegroup.Status))
	assert.Equal(t, int64(2), aws.Int64Value(out.Nodegroup.ScalingConfig.DesiredSize))
	assert.Contains(t, aws.StringValue(out.Nodegroup.NodegroupArn), ":nodegroup/c1/ng1/")

	// Both workers register Ready → the Ready-gate lets the nodegroup go ACTIVE.
	markWorkersReady(t, f, "c1", 2)
	f.svc.WaitLaunches()

	// The async launch transitions the record to ACTIVE once workers run.
	active, err := GetNodegroupRecord(f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, eks.NodegroupStatusActive, active.Status)

	// Two workers launched and tracked.
	assert.Len(t, f.worker.runCalls, 2)
	rec, err := GetNodegroupRecord(f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Len(t, rec.InstanceIDs, 2)

	// Workers go through the customer path: no ManagedBy tag, SG = nodegroup SG.
	for _, call := range f.worker.runCalls {
		require.NotEmpty(t, call.SecurityGroupIds)
		assert.Equal(t, "subnet-aaa", aws.StringValue(call.SubnetId))
		for _, ts := range call.TagSpecifications {
			for _, tg := range ts.Tags {
				assert.NotEqual(t, "spinifex:managed-by", aws.StringValue(tg.Key))
			}
		}
	}

	// SG ingress rules were authorized.
	assert.NotEmpty(t, f.sg.authorizeCalls)

	// Duplicate create → ResourceInUseException.
	_, err = f.svc.CreateNodegroup(createNGInput("c1", "ng1", 2), testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceInUse)
}

// Workers that launch but never register Ready (no rise in the cluster's Ready-
// node count) must drive the nodegroup to CREATE_FAILED, not a falsely-ACTIVE
// record. Instance IDs are retained so the reclaim path tears the workers down.
func TestCreateNodegroup_WorkersNeverReady_CreateFailed(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 2), testAccountID)
	require.NoError(t, err)

	// No markWorkersReady → NodeCount stays at the baseline; the Ready-gate times out.
	f.svc.WaitLaunches()

	rec, err := GetNodegroupRecord(f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Equal(t, eks.NodegroupStatusCreateFailed, rec.Status)
	assert.Contains(t, rec.StatusReason, "did not become Ready")
	assert.Len(t, rec.InstanceIDs, 2, "launched workers retained for the reclaim path")
}

func TestCreateNodegroup_DiskSizePropagatesToBlockDeviceMapping(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	in := createNGInput("c1", "ng1", 1)
	in.DiskSize = aws.Int64(30)
	_, err := f.svc.CreateNodegroup(in, testAccountID)
	require.NoError(t, err)

	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()

	require.Len(t, f.worker.runCalls, 1)
	bdm := f.worker.runCalls[0].BlockDeviceMappings
	require.Len(t, bdm, 1)
	require.NotNil(t, bdm[0].Ebs)
	assert.Equal(t, int64(30), aws.Int64Value(bdm[0].Ebs.VolumeSize))
}

func TestCreateNodegroup_NoDiskSizeOmitsBlockDeviceMapping(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 1), testAccountID)
	require.NoError(t, err)

	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()

	require.Len(t, f.worker.runCalls, 1)
	// No DiskSize requested → leave the launch path on its default sizing.
	assert.Empty(t, f.worker.runCalls[0].BlockDeviceMappings)
}

func TestCreateNodegroup_ClusterNotActive(t *testing.T) {
	f := newEKSServiceFixture(t)
	require.NoError(t, PutClusterMeta(f.kv, &ClusterMeta{
		Name:               "c1",
		Status:             ClusterStatusCreating,
		ControlPlaneENIIP:  "10.0.1.42",
		ResourcesVpcConfig: &ClusterVpcConfig{VpcId: "vpc-aaa"},
	}))

	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 1), testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidRequest)
}

func TestCreateNodegroup_UnknownCluster(t *testing.T) {
	f := newEKSServiceFixture(t)
	_, err := f.svc.CreateNodegroup(createNGInput("ghost", "ng1", 1), testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestDescribeAndListNodegroups(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 1), testAccountID)
	require.NoError(t, err)
	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()

	desc, err := f.svc.DescribeNodegroup(&eks.DescribeNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "ng1", aws.StringValue(desc.Nodegroup.NodegroupName))
	assert.Equal(t, "c1", aws.StringValue(desc.Nodegroup.ClusterName))

	list, err := f.svc.ListNodegroups(&eks.ListNodegroupsInput{ClusterName: aws.String("c1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"ng1"}, aws.StringValueSlice(list.Nodegroups))

	_, err = f.svc.DescribeNodegroup(&eks.DescribeNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ghost"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestUpdateNodegroupConfig_ScaleUpAndDown(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 1), testAccountID)
	require.NoError(t, err)
	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()
	require.Len(t, f.worker.runCalls, 1)

	// Scale up 1 → 3: two new workers launched.
	upOut, err := f.svc.UpdateNodegroupConfig(&eks.UpdateNodegroupConfigInput{
		ClusterName:   aws.String("c1"),
		NodegroupName: aws.String("ng1"),
		ScalingConfig: &eks.NodegroupScalingConfig{DesiredSize: aws.Int64(3)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, eks.UpdateStatusSuccessful, aws.StringValue(upOut.Update.Status))
	assert.Len(t, f.worker.runCalls, 3)
	rec, err := GetNodegroupRecord(f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Len(t, rec.InstanceIDs, 3)

	// Scale down 3 → 1: surplus (last two) terminated.
	_, err = f.svc.UpdateNodegroupConfig(&eks.UpdateNodegroupConfigInput{
		ClusterName:   aws.String("c1"),
		NodegroupName: aws.String("ng1"),
		ScalingConfig: &eks.NodegroupScalingConfig{DesiredSize: aws.Int64(1)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, f.worker.terminateCalls, 1)
	assert.Len(t, f.worker.terminateCalls[0], 2)
	rec, err = GetNodegroupRecord(f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Len(t, rec.InstanceIDs, 1)
}

// TestUpdateNodegroupConfig_ConcurrentScaleUpConverges proves the CAS reconcile
// fixes the lost-update overshoot: two overlapping scale-up requests (UI +
// terraform retry) must converge to exactly desired, not double-launch. Pre-fix
// each read current=1 and launched desired-current=2, yielding 5 live workers.
func TestUpdateNodegroupConfig_ConcurrentScaleUpConverges(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 1), testAccountID)
	require.NoError(t, err)
	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()
	require.Len(t, f.worker.runCalls, 1)

	const goroutines = 2
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, errs[idx] = f.svc.UpdateNodegroupConfig(&eks.UpdateNodegroupConfigInput{
				ClusterName:   aws.String("c1"),
				NodegroupName: aws.String("ng1"),
				ScalingConfig: &eks.NodegroupScalingConfig{DesiredSize: aws.Int64(3)},
			}, testAccountID)
		}(i)
	}
	close(start)
	wg.Wait()
	for _, e := range errs {
		require.NoError(t, e)
	}

	rec, err := GetNodegroupRecord(f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.Len(t, rec.InstanceIDs, 3, "record must converge to desired=3, not overshoot")

	// Net live workers (Run − Terminate) must equal desired: a surplus VM launched
	// under the exact-boundary race is terminated, never left running.
	assert.Equal(t, 3, f.worker.runCount()-f.worker.terminatedCount(),
		"net live workers must equal desired=3")
}

// TestUpdateNodegroupConfig_CapacityErrorSurfacesCode proves an out-of-capacity
// scale returns the bare InsufficientInstanceCapacity code (gateway maps to 400),
// not a wrapped string the gateway sanitizes to 500. A generic launcher failure
// still wraps, staying an opaque 500.
func TestUpdateNodegroupConfig_CapacityErrorSurfacesCode(t *testing.T) {
	scaleUpWithRunErr := func(t *testing.T, runErr error) error {
		f := newEKSServiceFixture(t)
		seedActiveClusterWithToken(t, f, "c1")
		_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 1), testAccountID)
		require.NoError(t, err)
		markWorkersReady(t, f, "c1", 1)
		f.svc.WaitLaunches()

		f.worker.runErr = runErr
		_, err = f.svc.UpdateNodegroupConfig(&eks.UpdateNodegroupConfigInput{
			ClusterName:   aws.String("c1"),
			NodegroupName: aws.String("ng1"),
			ScalingConfig: &eks.NodegroupScalingConfig{DesiredSize: aws.Int64(3)},
		}, testAccountID)
		return err
	}

	t.Run("capacity code preserved", func(t *testing.T) {
		err := scaleUpWithRunErr(t, errors.New(awserrors.ErrorInsufficientInstanceCapacity))
		require.EqualError(t, err, awserrors.ErrorInsufficientInstanceCapacity)
		assert.True(t, awserrors.HasErrorCode(err.Error()))
	})

	t.Run("opaque error wrapped", func(t *testing.T) {
		err := scaleUpWithRunErr(t, errors.New("kvm: out of host memory"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "launch workers:")
		assert.False(t, awserrors.HasErrorCode(err.Error()))
	})
}

func TestDeleteNodegroup_TerminatesAndIsIdempotent(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	_, err := f.svc.CreateNodegroup(createNGInput("c1", "ng1", 2), testAccountID)
	require.NoError(t, err)
	markWorkersReady(t, f, "c1", 2)
	f.svc.WaitLaunches()

	_, err = f.svc.DeleteNodegroup(&eks.DeleteNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, f.worker.terminateCalls, 1)
	assert.Len(t, f.worker.terminateCalls[0], 2)

	_, err = GetNodegroupRecord(f.kv, "c1", "ng1")
	assert.ErrorIs(t, err, ErrNodegroupNotFound)

	// Second delete → ResourceNotFoundException (record already gone).
	_, err = f.svc.DeleteNodegroup(&eks.DeleteNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestUpdateNodegroupVersion_NotImplemented(t *testing.T) {
	f := newEKSServiceFixture(t)
	_, err := f.svc.UpdateNodegroupVersion(&eks.UpdateNodegroupVersionInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}
