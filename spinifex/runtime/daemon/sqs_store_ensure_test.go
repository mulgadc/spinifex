package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storelaunch"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	storeType     = "t3.small"
	otherType     = "t3.large"
	storeSize     = "8"
	storeWorkerID = "i-0aaaaaaaaaaaaaaaa"
)

type storeVolumes struct {
	mu         sync.Mutex
	sizes      []int64
	encrypted  bool
	prepareErr error
}

func (v *storeVolumes) Prepare(_ context.Context, size int64) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.prepareErr != nil {
		return "", v.prepareErr
	}
	v.sizes = append(v.sizes, size)
	return fmt.Sprintf("vol-%017x", len(v.sizes)), nil
}

func (v *storeVolumes) Encrypted(context.Context, string) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.encrypted, nil
}

func (v *storeVolumes) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.sizes)
}

// storeVMs records each launch spec; release, when set, holds each launch.
type storeVMs struct {
	mu      sync.Mutex
	specs   []storelaunch.Spec
	release chan struct{}
	fail    error
}

func (m *storeVMs) LaunchStore(ctx context.Context, spec storelaunch.Spec) (string, error) {
	m.mu.Lock()
	m.specs = append(m.specs, spec)
	release, fail := m.release, m.fail
	m.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if fail != nil {
		return "", fail
	}
	return "i-0bbbbbbbbbbbbbbbb", nil
}

func (m *storeVMs) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.specs)
}

type storeFixture struct {
	nc       *nats.Conn
	d        *Daemon
	ids      workerident.Store
	first    firststore.Store
	records  storelaunch.Records
	vols     *storeVolumes
	vms      *storeVMs
	vpc      *ec2vpc.VPCServiceImpl
	hostErr  error
	pub      string
	seed     string
	workerNw string
}

// newStoreFixture serves node-1's real store handler over embedded NATS with
// the real launcher, launch, identity and first-store records and VPC network.
func newStoreFixture(t *testing.T, vms *storeVMs) *storeFixture {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	vpc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	testutil.StubVpcdSGResponder(t, nc)
	f := &storeFixture{nc: nc, vols: &storeVolumes{encrypted: true}, vms: vms, vpc: vpc}
	f.ids, err = workerident.Open(t.Context(), js)
	require.NoError(t, err)
	f.first, err = firststore.Open(t.Context(), js)
	require.NoError(t, err)
	f.records, err = storelaunch.Open(t.Context(), js)
	require.NoError(t, err)
	cfg := &config.SQSConfig{}
	l := &storelaunch.Launcher{
		Records:        f.records,
		Workers:        f.ids,
		FirstStore:     f.first,
		Volumes:        f.vols,
		Network:        sqsStoreNetwork{n: newSQSStoreNetwork(vpc, cfg)},
		Host:           sqsStoreHost{check: func() error { return f.hostErr }},
		VMs:            vms,
		NodeID:         ensureNode,
		RetainedVolume: retainedStoreVolume,
	}
	f.d = &Daemon{
		node:   ensureNode,
		ctx:    t.Context(),
		config: &config.Config{Region: sqsTestRegion},
		resourceMgr: &ResourceManager{instanceTypes: map[string]*ec2.InstanceTypeInfo{
			storeType: {InstanceType: aws.String(storeType), MemoryInfo: &ec2.MemoryInfo{SizeInMiB: aws.Int64(2048)}},
			"t3.nano": {InstanceType: aws.String("t3.nano"), MemoryInfo: &ec2.MemoryInfo{SizeInMiB: aws.Int64(512)}},
			otherType: {InstanceType: aws.String(otherType), MemoryInfo: &ec2.MemoryInfo{SizeInMiB: aws.Int64(4096)}},
		}},
		sqsStore: newSQSStoreEnsurer(func(context.Context) (*storelaunch.Launcher, error) { return l, nil }),
	}
	subject, err := storelaunch.EnsureSubject(ensureNode)
	require.NoError(t, err)
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) { f.d.handleSQSStoreEnsure(m) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// The worker's own ENI, so the store's can be checked to share its subnet.
	w, err := newSQSWorkerNetwork(vpc, cfg).CreateWorkerNetwork(t.Context(), sqsTestRegion, "claim-worker-1")
	require.NoError(t, err)
	f.workerNw = w.SubnetID
	return f
}

// bindWorker binds the Region's worker on nodeID and, when record is set,
// records its public key as the worker's boot does.
func (f *storeFixture) bindWorker(t *testing.T, nodeID string, record bool) {
	t.Helper()
	ctx := t.Context()
	intent, err := f.ids.BeginLaunch(ctx, sqsTestRegion, workerident.LaunchRequest{InstanceType: "t3.small", ClaimID: "claim-worker-1",
		NodeID: nodeID, DataVolumeID: ensureVolume, DataVolumeEncrypted: true})
	require.NoError(t, err)
	require.NoError(t, f.ids.MarkDispatched(ctx, intent))
	require.NoError(t, f.ids.BindInstance(ctx, intent, storeWorkerID))
	if !record {
		return
	}
	kp, err := nkeys.CreateUser()
	require.NoError(t, err)
	f.pub, _ = kp.PublicKey()
	seed, _ := kp.Seed()
	f.seed = string(seed)
	id := firststore.Identity{InstanceID: storeWorkerID, ClaimID: "claim-worker-1", Generation: intent.Generation}
	d, err := f.first.Claim(ctx, sqsTestRegion, id)
	require.NoError(t, err)
	require.True(t, d.CreateKey)
	require.NoError(t, f.first.RecordPublicKey(ctx, sqsTestRegion, id, f.pub))
}

func (f *storeFixture) call(t *testing.T, node, itype string, wait time.Duration) (*storelaunch.EnsureResponse, error) {
	t.Helper()
	return admin.EnsureSQSStore(t.Context(), f.nc, ensureNodes, node, itype, storeSize, "", wait)
}

func (f *storeFixture) storeENIs(t *testing.T) []*ec2.NetworkInterface {
	t.Helper()
	out, err := f.vpc.DescribeNetworkInterfaces(t.Context(), &ec2.DescribeNetworkInterfacesInput{}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	var store []*ec2.NetworkInterface
	for _, eni := range out.NetworkInterfaces {
		if strings.HasPrefix(aws.StringValue(eni.Description), "SQS store") {
			store = append(store, eni)
		}
	}
	return store
}

func (f *storeFixture) nothingAllocated(t *testing.T) {
	t.Helper()
	assert.Equal(t, 0, f.vols.count())
	assert.Equal(t, 0, f.vms.count())
	assert.Empty(t, f.storeENIs(t))
	_, err := f.records.Current(t.Context(), sqsTestRegion)
	assert.Error(t, err, "a refused ensure must record nothing")
}

// Through the admin action, the store launches once with the worker's
// recorded public key, an encrypted volume of the operator's size and one ENI
// in the worker's subnet; a repeat call returns the same VM.
func TestEnsureSQSStore_AdminLaunchesWithTheRecordedKeyOnce(t *testing.T) {
	f := newStoreFixture(t, &storeVMs{})
	f.bindWorker(t, ensureNode, true)

	out, err := f.call(t, ensureNode, storeType, 30*time.Second)
	require.NoError(t, err)
	enis := f.storeENIs(t)
	require.Len(t, enis, 1)
	assert.Equal(t, storelaunch.EnsureResponse{NodeID: ensureNode, Region: sqsTestRegion,
		InstanceID: "i-0bbbbbbbbbbbbbbbb", Address: aws.StringValue(enis[0].PrivateIpAddress),
		InstanceType: storeType, State: storelaunch.StateBoundResponse}, *out)

	spec := f.vms.specs[0]
	assert.Equal(t, f.pub, spec.ClientNKey)
	assert.True(t, nkeys.IsValidPublicUserKey(spec.ClientNKey))
	assert.Equal(t, storeType, spec.InstanceType)
	assert.Equal(t, f.workerNw, spec.Network.SubnetID, "the store must share the worker's subnet")
	assert.Equal(t, f.workerNw, aws.StringValue(enis[0].SubnetId))
	assert.Equal(t, []int64{8}, f.vols.sizes)

	again, err := f.call(t, ensureNode, storeType, 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, out, again)
	assert.Equal(t, 1, f.vms.count())
	assert.Equal(t, 1, f.vols.count())
	assert.Len(t, f.storeENIs(t), 1)

	rec, err := f.records.Current(t.Context(), sqsTestRegion)
	require.NoError(t, err)
	raw, _ := json.Marshal(rec)
	assert.NotContains(t, string(raw), f.seed)
}

// A reply lost to the caller's timeout does not cancel the launch; calls made
// meanwhile queue behind it and report its one VM.
func TestEnsureSQSStore_LostReplyRetryReportsTheSameLaunch(t *testing.T) {
	vms := &storeVMs{release: make(chan struct{})}
	f := newStoreFixture(t, vms)
	f.bindWorker(t, ensureNode, true)

	_, err := f.call(t, ensureNode, storeType, 300*time.Millisecond)
	assert.Contains(t, requireAWSCode(t, err, awserrors.ErrorServiceUnavailable), "may still be running")

	results := make(chan error, 3)
	for range 3 {
		go func() {
			out, err := f.call(t, ensureNode, storeType, 30*time.Second)
			if err == nil && out.InstanceID != "i-0bbbbbbbbbbbbbbbb" {
				err = errors.New("unexpected instance " + out.InstanceID)
			}
			results <- err
		}()
	}
	close(vms.release)
	for range 3 {
		require.NoError(t, <-results)
	}
	assert.Equal(t, 1, vms.count())
	assert.Equal(t, 1, f.vols.count())
	assert.Len(t, f.storeENIs(t), 1)
}

// A dispatched launch that errors is reported as unknown and never resent.
func TestEnsureSQSStore_AmbiguousLaunchIsNeverResent(t *testing.T) {
	f := newStoreFixture(t, &storeVMs{fail: errors.New("reply lost")})
	f.bindWorker(t, ensureNode, true)
	for range 3 {
		_, err := f.call(t, ensureNode, storeType, 30*time.Second)
		assert.Contains(t, requireAWSCode(t, err, awserrors.ErrorIncorrectState), "outcome is unknown")
	}
	assert.Equal(t, 1, f.vms.count())
	assert.Equal(t, 1, f.vols.count())
}

// A volume Prepare that sent no create (here a missing mkfs.ext4) is
// retryable: the caller gets ServiceUnavailable, and once the host is fixed the
// next call launches once.
func TestEnsureSQSStore_VolumePrepareThatCreatedNothingIsRetryable(t *testing.T) {
	f := newStoreFixture(t, &storeVMs{})
	f.bindWorker(t, ensureNode, true)
	f.vols.prepareErr = fmt.Errorf("%w: mkfs.ext4 is not installed", storevolume.ErrNotCreated)
	for range 2 {
		_, err := f.call(t, ensureNode, storeType, 30*time.Second)
		assert.Contains(t, requireAWSCode(t, err, awserrors.ErrorServiceUnavailable), "mkfs.ext4")
	}
	f.vols.prepareErr = nil
	out, err := f.call(t, ensureNode, storeType, 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, storeType, out.InstanceType)
	assert.Equal(t, 1, f.vols.count())
	assert.Equal(t, 1, f.vms.count())
	assert.Len(t, f.storeENIs(t), 1)
}

// The instance type is fixed by the first request: a retry before dispatch
// and a request after binding that ask for another type are both refused.
func TestEnsureSQSStore_RefusesAnotherInstanceType(t *testing.T) {
	f := newStoreFixture(t, &storeVMs{})
	f.bindWorker(t, ensureNode, true)
	f.vols.prepareErr = fmt.Errorf("%w: scratch dir is full", storevolume.ErrNotCreated)
	_, err := f.call(t, ensureNode, storeType, 30*time.Second)
	requireAWSCode(t, err, awserrors.ErrorServiceUnavailable)
	_, err = f.call(t, ensureNode, otherType, 30*time.Second)
	assert.Contains(t, requireAWSCode(t, err, awserrors.ErrorIncorrectState), storeType)

	f.vols.prepareErr = nil
	_, err = f.call(t, ensureNode, storeType, 30*time.Second)
	require.NoError(t, err)
	_, err = f.call(t, ensureNode, otherType, 30*time.Second)
	assert.Contains(t, requireAWSCode(t, err, awserrors.ErrorIncorrectState), storeType)
	assert.Equal(t, 1, f.vms.count())
	assert.Equal(t, storeType, f.vms.specs[0].InstanceType)
}

// A volume whose own record is not encrypted is refused with its ID kept,
// and no ENI or VM follows.
func TestEnsureSQSStore_UnencryptedVolumeIsRefusedAndKept(t *testing.T) {
	f := newStoreFixture(t, &storeVMs{})
	f.vols.encrypted = false
	f.bindWorker(t, ensureNode, true)
	for range 2 {
		_, err := f.call(t, ensureNode, storeType, 30*time.Second)
		msg := requireAWSCode(t, err, awserrors.ErrorUnsupportedOperation)
		assert.Contains(t, msg, "vol-00000000000000001")
	}
	assert.Equal(t, 1, f.vols.count())
	assert.Equal(t, 0, f.vms.count())
	assert.Empty(t, f.storeENIs(t))
}

// Without a bound worker on this node that has recorded its key, or on a node
// that cannot boot the store, nothing is recorded or allocated.
func TestEnsureSQSStore_RefusesBeforeAllocating(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T, *storeFixture)
		code  string
	}{
		{"no worker", func(*testing.T, *storeFixture) {}, awserrors.ErrorIncorrectState},
		{"worker on another node", func(t *testing.T, f *storeFixture) { f.bindWorker(t, "node-2", true) }, awserrors.ErrorIncorrectState},
		{"key not recorded", func(t *testing.T, f *storeFixture) { f.bindWorker(t, ensureNode, false) }, awserrors.ErrorIncorrectState},
		{"host not ready", func(t *testing.T, f *storeFixture) {
			f.bindWorker(t, ensureNode, true)
			f.hostErr = errors.New("sqs store image is not installed")
		}, awserrors.ErrorServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStoreFixture(t, &storeVMs{})
			tc.setup(t, f)
			_, err := f.call(t, ensureNode, storeType, 30*time.Second)
			requireAWSCode(t, err, tc.code)
			f.nothingAllocated(t)
		})
	}
}

// An obviously undersized or unknown guest is refused before anything else;
// sizing beyond that floor is not validated here.
func TestEnsureSQSStore_RefusesUndersizedOrUnknownTypes(t *testing.T) {
	f := newStoreFixture(t, &storeVMs{})
	f.bindWorker(t, ensureNode, true)
	_, err := f.call(t, ensureNode, "t3.nano", 30*time.Second)
	requireAWSCode(t, err, awserrors.ErrorInvalidParameterValue)
	_, err = f.call(t, ensureNode, "t3.unknown", 30*time.Second)
	requireAWSCode(t, err, awserrors.ErrorInvalidInstanceType)
	f.nothingAllocated(t)
}

// The admin action refuses bad sizes and nodes before any request, and the
// handler refuses a body that names another node.
func TestEnsureSQSStore_RefusesBadParameters(t *testing.T) {
	f := newStoreFixture(t, &storeVMs{})
	f.bindWorker(t, ensureNode, true)
	for _, size := range []string{"", "0", "-1", "16385", "x"} {
		_, err := admin.EnsureSQSStore(t.Context(), f.nc, ensureNodes, ensureNode, storeType, size, "", time.Second)
		require.Error(t, err, size)
	}
	_, err := admin.EnsureSQSStore(t.Context(), f.nc, ensureNodes, "node-9", storeType, storeSize, "", time.Second)
	requireAWSCode(t, err, awserrors.ErrorInvalidParameterValue)

	body, _ := json.Marshal(storelaunch.EnsureRequest{NodeID: "node-2", InstanceType: storeType, VolumeSizeGiB: 8})
	subject, _ := storelaunch.EnsureSubject(ensureNode)
	reply, err := f.nc.Request(subject, body, 5*time.Second)
	require.NoError(t, err)
	assert.Contains(t, string(reply.Data), awserrors.ErrorInvalidParameterValue)
	f.nothingAllocated(t)
}

// The action accepts only a bound reply from the node it addressed that
// names the instance type it asked for.
func TestEnsureSQSStore_RefusesAMismatchedReply(t *testing.T) {
	f := newStoreFixture(t, &storeVMs{})
	ok := storelaunch.EnsureResponse{NodeID: "node-2", InstanceID: "i-0123456789abcdef0", Address: "10.240.0.9",
		InstanceType: storeType, State: storelaunch.StateBoundResponse}
	for _, mutate := range []func(*storelaunch.EnsureResponse){
		func(r *storelaunch.EnsureResponse) { r.InstanceType = otherType },
		func(r *storelaunch.EnsureResponse) { r.InstanceType = "" },
		func(r *storelaunch.EnsureResponse) { r.NodeID = "node-1" },
		func(r *storelaunch.EnsureResponse) { r.State = "launching" },
	} {
		reply := ok
		mutate(&reply)
		body, err := json.Marshal(reply)
		require.NoError(t, err)
		sub, err := f.nc.Subscribe(storelaunch.EnsureSubjectPrefix+"node-2", func(m *nats.Msg) { _ = m.Respond(body) })
		require.NoError(t, err)
		_, err = f.call(t, "node-2", storeType, 5*time.Second)
		requireAWSCode(t, err, awserrors.ErrorServerInternal)
		require.NoError(t, sub.Unsubscribe())
	}
}

// A request for another volume size after the store is bound is refused
// through the admin path, and no second store is launched.
func TestEnsureSQSStore_RefusesAnotherSizeOnceBound(t *testing.T) {
	f := newStoreFixture(t, &storeVMs{})
	f.bindWorker(t, ensureNode, true)
	_, err := f.call(t, ensureNode, storeType, 30*time.Second)
	require.NoError(t, err)
	_, err = admin.EnsureSQSStore(t.Context(), f.nc, ensureNodes, ensureNode, storeType, "9", "", 30*time.Second)
	assert.Contains(t, requireAWSCode(t, err, awserrors.ErrorIncorrectState), "8 GiB")
	assert.Equal(t, 1, f.vols.count())
	assert.Equal(t, 1, f.vms.count())
}
