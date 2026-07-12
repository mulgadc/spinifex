package handlers_eks

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// filteringK3sAMI applies its Filters against each image's tags, mirroring
// AWS server-side tag filtering. fakeK3sAMI ignores Filters and returns every
// image regardless, which cannot distinguish a GPU-tagged AMI from a plain one
// when both are present — needed to verify the worker-launch path actually
// threads the gpu-vendor filter through to DescribeImages.
type filteringK3sAMI struct {
	images []*ec2.Image
}

var _ k3sAMIResolver = (*filteringK3sAMI)(nil)

func (f *filteringK3sAMI) DescribeImages(_ context.Context, input *ec2.DescribeImagesInput, _ string) (*ec2.DescribeImagesOutput, error) {
	out := &ec2.DescribeImagesOutput{}
	for _, img := range f.images {
		if k3sImageMatchesFilters(img, input.Filters) {
			out.Images = append(out.Images, img)
		}
	}
	return out, nil
}

func k3sImageMatchesFilters(img *ec2.Image, filters []*ec2.Filter) bool {
	tagVals := map[string]string{}
	for _, t := range img.Tags {
		tagVals[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	for _, f := range filters {
		name := aws.StringValue(f.Name)
		const prefix = "tag:"
		if len(name) <= len(prefix) || name[:len(prefix)] != prefix {
			continue
		}
		val, ok := tagVals[name[len(prefix):]]
		if !ok {
			return false
		}
		matched := slices.Contains(aws.StringValueSlice(f.Values), val)
		if !matched {
			return false
		}
	}
	return true
}

// fakeWorkerLauncher records RunWorkerInstance / TerminateWorkerInstances calls
// and hands back sequential instance IDs so tests can assert on the IDs a
// nodegroup tracks. It is mutex-guarded so concurrent-reconcile tests exercise
// the production CAS path, not a race in the fake.
type fakeWorkerLauncher struct {
	mu             sync.Mutex
	runCalls       []*ec2.RunInstancesInput
	runNodes       []string
	terminateCalls [][]string

	nextID  int
	runErr  error
	termErr error
}

var _ WorkerLauncher = (*fakeWorkerLauncher)(nil)

func newFakeWorkerLauncher() *fakeWorkerLauncher {
	return &fakeWorkerLauncher{}
}

func (f *fakeWorkerLauncher) RunWorkerInstance(ctx context.Context, input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
	return f.RunWorkerInstanceOnNode(ctx, "", input, accountID)
}

func (f *fakeWorkerLauncher) RunWorkerInstanceOnNode(_ context.Context, nodeID string, input *ec2.RunInstancesInput, _ string) (*ec2.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCalls = append(f.runCalls, input)
	f.runNodes = append(f.runNodes, nodeID)
	if f.runErr != nil {
		return nil, f.runErr
	}
	f.nextID++
	id := "i-worker" + strconv.Itoa(f.nextID)
	return &ec2.Reservation{Instances: []*ec2.Instance{{InstanceId: aws.String(id)}}}, nil
}

func (f *fakeWorkerLauncher) TerminateWorkerInstances(_ context.Context, ids []string, _ string) error {
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

	f.svc.reclaimOrphanedNodegroups(context.Background(), testAccountID, f.kv, cluster)

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
	kv, err := GetOrCreateAccountBucket(js, testAccountID, 1)
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
	// IMDS is served at the host tap, so no in-guest on-link route is emitted.
	assert.NotContains(t, ud, "169.254.169.254")
	assert.NotContains(t, ud, "/etc/local.d/imds-onlink-route.start")
	// Exactly one write_files block (single "write_files:" key).
	assert.Equal(t, 1, strings.Count(ud, "write_files:"))
}

func TestEnsureNodegroupSGRules_AuthorizesExpectedRules(t *testing.T) {
	sg := newFakeSGProvisioner()

	require.NoError(t, EnsureNodegroupSGRules(context.Background(), sg, testAccountID, "c1", "sg-cp", "sg-ng"))

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

	require.NoError(t, EnsureNodegroupSGRules(context.Background(), sg, testAccountID, "c1", "sg-cp", "sg-ng"),
		"duplicate-rule error must be treated as success")
}

func TestCreateNodegroup_HappyPath(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	out, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 2), testAccountID)
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
	_, err = f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 2), testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceInUse)
}

// A GPU instance type must mark the record GPUEnabled with the matching
// vendor, so the worker-launch path resolves the GPU node AMI instead of the
// default eks-node AMI.
func TestCreateNodegroup_GPUInstanceTypeSetsGPUFields(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	in := createNGInput("c1", "ng-gpu", 1)
	in.InstanceTypes = aws.StringSlice([]string{"g5.xlarge"})

	_, err := f.svc.CreateNodegroup(context.Background(), in, testAccountID)
	require.NoError(t, err)

	rec, err := GetNodegroupRecord(f.kv, "c1", "ng-gpu")
	require.NoError(t, err)
	assert.True(t, rec.GPUEnabled)
	assert.Equal(t, "nvidia", rec.GPUVendor)

	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()
}

// A GPU nodegroup create must auto-stage the nvidia-device-plugin addon so
// GPU-tainted nodes get nvidia.com/gpu allocatable without a manual CreateAddon.
func TestCreateNodegroup_GPUNodegroupStagesDevicePluginAddon(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	in := createNGInput("c1", "ng-gpu", 1)
	in.InstanceTypes = aws.StringSlice([]string{"g5.xlarge"})

	_, err := f.svc.CreateNodegroup(context.Background(), in, testAccountID)
	require.NoError(t, err)

	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()

	rec, err := GetAddonRecord(f.kv, "c1", nvidiaDevicePluginAddonName)
	require.NoError(t, err, "nvidia-device-plugin must be staged for a GPU nodegroup")
	assert.Equal(t, "0.17.4", rec.AddonVersion)
}

// A non-GPU nodegroup create must not stage nvidia-device-plugin — it stays
// dormant on non-GPU clusters, and it isn't user-visible in the catalog.
func TestCreateNodegroup_NonGPUNodegroupDoesNotStageDevicePluginAddon(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 1), testAccountID)
	require.NoError(t, err)

	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()

	_, err = GetAddonRecord(f.kv, "c1", nvidiaDevicePluginAddonName)
	require.ErrorIs(t, err, ErrAddonNotFound)
}

// A non-GPU instance type must leave the record's GPU fields unset, so the
// worker-launch path keeps resolving the default eks-node AMI.
func TestCreateNodegroup_NonGPUInstanceTypeClearsGPUFields(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 1), testAccountID)
	require.NoError(t, err)

	rec, err := GetNodegroupRecord(f.kv, "c1", "ng1")
	require.NoError(t, err)
	assert.False(t, rec.GPUEnabled)
	assert.Empty(t, rec.GPUVendor)

	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()
}

// End-to-end: a GPU nodegroup's worker launch must resolve the GPU-tagged AMI
// over a coexisting plain eks-node AMI, not merely set the record's GPU fields.
func TestCreateNodegroup_GPUWorkerLaunchUsesGPUAMI(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	f.svc.deps.Image = &filteringK3sAMI{images: []*ec2.Image{
		{ImageId: aws.String("ami-eks-plain"), CreationDate: aws.String("2026-01-01T00:00:00.000Z"),
			Tags: []*ec2.Tag{{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)}}},
		{ImageId: aws.String("ami-eks-gpu"), CreationDate: aws.String("2026-06-01T00:00:00.000Z"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
				{Key: aws.String(tags.GPUVendorKey), Value: aws.String(tags.GPUVendorNVIDIA)},
			}},
	}}

	in := createNGInput("c1", "ng-gpu", 1)
	in.InstanceTypes = aws.StringSlice([]string{"g5.xlarge"})
	_, err := f.svc.CreateNodegroup(context.Background(), in, testAccountID)
	require.NoError(t, err)

	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()

	rec, err := GetNodegroupRecord(f.kv, "c1", "ng-gpu")
	require.NoError(t, err)
	assert.Equal(t, eks.NodegroupStatusActive, rec.Status)

	require.Len(t, f.worker.runCalls, 1)
	assert.Equal(t, "ami-eks-gpu", aws.StringValue(f.worker.runCalls[0].ImageId))
}

// A GPU nodegroup with no matching GPU AMI must fail, never fall back to the
// plain eks-node AMI (running a GPU workload on a driverless image is worse
// than a clear failure).
func TestCreateNodegroup_GPUWorkerLaunchNoGPUAMINoFallback(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	f.svc.deps.Image = &filteringK3sAMI{images: []*ec2.Image{
		{ImageId: aws.String("ami-eks-plain"), CreationDate: aws.String("2026-01-01T00:00:00.000Z"),
			Tags: []*ec2.Tag{{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)}}},
	}}

	in := createNGInput("c1", "ng-gpu", 1)
	in.InstanceTypes = aws.StringSlice([]string{"g5.xlarge"})
	_, err := f.svc.CreateNodegroup(context.Background(), in, testAccountID)
	require.NoError(t, err)

	f.svc.WaitLaunches()

	rec, err := GetNodegroupRecord(f.kv, "c1", "ng-gpu")
	require.NoError(t, err)
	assert.Equal(t, eks.NodegroupStatusCreateFailed, rec.Status)
	assert.Contains(t, rec.StatusReason, "resolve eks-node AMI")
	assert.Empty(t, f.worker.runCalls, "no worker must launch on the driverless plain AMI")
}

// Workers that launch but never register Ready (no rise in the cluster's Ready-
// node count) must drive the nodegroup to CREATE_FAILED, not a falsely-ACTIVE
// record. Instance IDs are retained so the reclaim path tears the workers down.
func TestCreateNodegroup_WorkersNeverReady_CreateFailed(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")

	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 2), testAccountID)
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
	_, err := f.svc.CreateNodegroup(context.Background(), in, testAccountID)
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

	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 1), testAccountID)
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

	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 1), testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidRequest)
}

func TestCreateNodegroup_UnknownCluster(t *testing.T) {
	f := newEKSServiceFixture(t)
	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("ghost", "ng1", 1), testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestDescribeAndListNodegroups(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 1), testAccountID)
	require.NoError(t, err)
	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()

	desc, err := f.svc.DescribeNodegroup(context.Background(), &eks.DescribeNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "ng1", aws.StringValue(desc.Nodegroup.NodegroupName))
	assert.Equal(t, "c1", aws.StringValue(desc.Nodegroup.ClusterName))

	list, err := f.svc.ListNodegroups(context.Background(), &eks.ListNodegroupsInput{ClusterName: aws.String("c1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"ng1"}, aws.StringValueSlice(list.Nodegroups))

	_, err = f.svc.DescribeNodegroup(context.Background(), &eks.DescribeNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ghost"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestUpdateNodegroupConfig_ScaleUpAndDown(t *testing.T) {
	f := newEKSServiceFixture(t)
	seedActiveClusterWithToken(t, f, "c1")
	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 1), testAccountID)
	require.NoError(t, err)
	markWorkersReady(t, f, "c1", 1)
	f.svc.WaitLaunches()
	require.Len(t, f.worker.runCalls, 1)

	// Scale up 1 → 3: two new workers launched.
	upOut, err := f.svc.UpdateNodegroupConfig(context.Background(), &eks.UpdateNodegroupConfigInput{
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
	_, err = f.svc.UpdateNodegroupConfig(context.Background(), &eks.UpdateNodegroupConfigInput{
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
	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 1), testAccountID)
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
			_, errs[idx] = f.svc.UpdateNodegroupConfig(context.Background(), &eks.UpdateNodegroupConfigInput{
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
		_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 1), testAccountID)
		require.NoError(t, err)
		markWorkersReady(t, f, "c1", 1)
		f.svc.WaitLaunches()

		f.worker.runErr = runErr
		_, err = f.svc.UpdateNodegroupConfig(context.Background(), &eks.UpdateNodegroupConfigInput{
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
	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 2), testAccountID)
	require.NoError(t, err)
	markWorkersReady(t, f, "c1", 2)
	f.svc.WaitLaunches()

	_, err = f.svc.DeleteNodegroup(context.Background(), &eks.DeleteNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, f.worker.terminateCalls, 1)
	assert.Len(t, f.worker.terminateCalls[0], 2)

	_, err = GetNodegroupRecord(f.kv, "c1", "ng1")
	assert.ErrorIs(t, err, ErrNodegroupNotFound)

	// Second delete → ResourceNotFoundException (record already gone).
	_, err = f.svc.DeleteNodegroup(context.Background(), &eks.DeleteNodegroupInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestUpdateNodegroupVersion_NotImplemented(t *testing.T) {
	f := newEKSServiceFixture(t)
	_, err := f.svc.UpdateNodegroupVersion(context.Background(), &eks.UpdateNodegroupVersionInput{
		ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}

// TestSelectWorkerHost_NoSchedulerOrCapacity returns "" so the caller falls back
// to a local launch when no scheduler is wired or no host has free capacity.
func TestSelectWorkerHost_NoSchedulerOrCapacity(t *testing.T) {
	s := &EKSServiceImpl{deps: EKSServiceDeps{}}
	require.Equal(t, "", s.selectWorkerHost(context.Background(), "t3.medium", nil))

	s = &EKSServiceImpl{deps: EKSServiceDeps{Scheduler: &fakeHostScheduler{}}}
	require.Equal(t, "", s.selectWorkerHost(context.Background(), "t3.medium", nil))
}

// mapHostScheduler is a HostScheduler stub with an explicit instance->host map,
// so a test can give every worker a distinct ID (which the fakeHostScheduler's
// "i-<node>" convention cannot, collapsing same-host workers).
type mapHostScheduler struct {
	hosts     []string
	placement map[string]string
}

var _ HostScheduler = (*mapHostScheduler)(nil)

func (m *mapHostScheduler) SchedulableHosts(context.Context, string) []string { return m.hosts }

func (m *mapHostScheduler) InstanceHosts(_ context.Context, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if h, ok := m.placement[id]; ok {
			out[id] = h
		}
	}
	return out
}

// TestSelectWorkerHost_SpreadsThenPacks confirms sequential single-worker
// launches fan out one-per-host before doubling up, the fix for all workers
// landing on a single node.
func TestSelectWorkerHost_SpreadsThenPacks(t *testing.T) {
	hosts := []string{"nodeA", "nodeB", "nodeC"}
	sched := &mapHostScheduler{hosts: hosts, placement: map[string]string{}}
	s := &EKSServiceImpl{deps: EKSServiceDeps{Scheduler: sched}}

	placed := map[string]int{}
	var workerIDs []string
	for i := range 6 {
		host := s.selectWorkerHost(context.Background(), "t3.medium", workerIDs)
		require.Contains(t, hosts, host)
		placed[host]++
		id := "i-w" + strconv.Itoa(i)
		sched.placement[id] = host
		workerIDs = append(workerIDs, id)
	}

	// 6 workers across 3 hosts spread evenly: exactly 2 per host, none starved.
	for _, h := range hosts {
		require.Equal(t, 2, placed[h], "host %s should hold 2 of 6 workers", h)
	}
}
