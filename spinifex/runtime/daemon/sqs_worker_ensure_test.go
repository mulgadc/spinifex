package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ensureNode   = "node-1"
	ensureType   = "t3.micro"
	ensureVolume = "vol-0123456789abcdef0"
)

var ensureNodes = []string{"node-1", "node-2"}

type ensureVolumes struct {
	mu       sync.Mutex
	prepared int
}

func (v *ensureVolumes) Prepare(context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.prepared++
	return ensureVolume, nil
}

func (v *ensureVolumes) Encrypted(context.Context, string) (bool, error) { return true, nil }

type ensureRoles struct{}

func (ensureRoles) EnsureWorkerProfile(context.Context) (string, error) {
	return "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":instance-profile/sqsWorkerRole", nil
}

// ensureVMs counts launches; release, when set, holds each launch until closed.
type ensureVMs struct {
	mu       sync.Mutex
	launches int
	release  chan struct{}
	fail     error
}

func (m *ensureVMs) LaunchWorker(ctx context.Context, _ workerlaunch.Spec) (string, error) {
	m.mu.Lock()
	m.launches++
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
	return "i-0123456789abcdef0", nil
}

func (m *ensureVMs) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.launches
}

type ensureFixture struct {
	nc   *nats.Conn
	d    *Daemon
	ids  workerident.Store
	vols *ensureVolumes
	vms  *ensureVMs
	vpc  *ec2vpc.VPCServiceImpl
}

// newEnsureFixture serves node-1's real handler over embedded NATS, with the
// real launcher, identity and first-store records and VPC network.
func newEnsureFixture(t *testing.T, vms *ensureVMs) *ensureFixture {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	vpc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	testutil.StubVpcdSGResponder(t, nc)
	ids, err := workerident.Open(t.Context(), js)
	require.NoError(t, err)
	first, err := firststore.Open(t.Context(), js)
	require.NoError(t, err)

	f := &ensureFixture{nc: nc, ids: ids, vols: &ensureVolumes{}, vms: vms, vpc: vpc}
	l := &workerlaunch.Launcher{
		Identities: ids,
		FirstStore: first,
		Volumes:    f.vols,
		Roles:      ensureRoles{},
		Network:    newSQSWorkerNetwork(vpc, &config.SQSConfig{}),
		Host:       sqsWorkerHost{check: func() (string, error) { return f.d.sqsWorkerHostReady() }},
		VMs:        vms,
		NodeID:     ensureNode,
	}
	f.d = &Daemon{
		node:        ensureNode,
		ctx:         t.Context(),
		config:      &config.Config{Region: sqsTestRegion},
		resourceMgr: &ResourceManager{instanceTypes: map[string]*ec2.InstanceTypeInfo{ensureType: microType()}},
		sqsWorker:   newSQSWorkerEnsurer(func(context.Context) (*workerlaunch.Launcher, error) { return l, nil }),
	}
	ensureHost(t, f.d, true)
	subject, err := workerlaunch.EnsureSubject(ensureNode)
	require.NoError(t, err)
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) { f.d.handleSQSWorkerEnsure(m) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return f
}

// ensureHost gives d a mgmt bridge, a cluster CA and, when installed, a
// complete worker image in a scratch microVM directory.
func ensureHost(t *testing.T, d *Daemon, installed bool) {
	t.Helper()
	orig := microvmDir
	microvmDir = t.TempDir()
	t.Cleanup(func() { microvmDir = orig })
	if installed {
		installWorkerImage(t)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----\n"), 0o600))
	d.config.NATS.CACert = ca
	d.mgmtBridgeIP = "10.97.8.1"
	d.mgmtIPAllocator = sysInstMgmtAllocator(t, "10.97.8.1")
}

func installWorkerImage(t *testing.T) {
	t.Helper()
	require.NoError(t, os.MkdirAll(sqsWorkerImageDir(), 0o755))
	for _, n := range []string{"vmlinuz", "initramfs.cpio.gz"} {
		require.NoError(t, os.WriteFile(filepath.Join(sqsWorkerImageDir(), n), []byte("x"), 0o644))
	}
}

func (f *ensureFixture) call(t *testing.T, node string, wait time.Duration) (*workerlaunch.EnsureResponse, error) {
	t.Helper()
	return admin.EnsureSQSWorker(t.Context(), f.nc, ensureNodes, node, ensureType, awsidentifiers.DefaultAccountID(), wait)
}

func (f *ensureFixture) workerENIs(t *testing.T) int {
	t.Helper()
	out, err := f.vpc.DescribeNetworkInterfaces(t.Context(), &ec2.DescribeNetworkInterfacesInput{}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	return len(out.NetworkInterfaces)
}

func requireAWSCode(t *testing.T, err error, code string) string {
	t.Helper()
	require.Error(t, err)
	got, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "not an AWS error: %v", err)
	require.Equal(t, code, got, "message: %s", message)
	return message
}

// The admin action reaches node-1's handler, which launches and binds one
// worker; a repeat call reports the same bound VM without relaunching.
func TestEnsureSQSWorker_AdminLaunchesAndBindsOnce(t *testing.T) {
	f := newEnsureFixture(t, &ensureVMs{})

	out, err := f.call(t, ensureNode, 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, workerlaunch.EnsureResponse{NodeID: ensureNode, Region: sqsTestRegion,
		InstanceID: "i-0123456789abcdef0", Generation: 1, State: workerlaunch.StateBound}, *out)

	again, err := f.call(t, ensureNode, 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, out, again)
	assert.Equal(t, 1, f.vms.count())
	assert.Equal(t, 1, f.vols.prepared)
	assert.Equal(t, 1, f.workerENIs(t))

	cur, err := f.ids.Current(t.Context(), sqsTestRegion)
	require.NoError(t, err)
	assert.Equal(t, workerident.StateBound, cur.State)
	assert.Equal(t, ensureNode, cur.NodeID)
}

// A reply lost to the caller's timeout does not cancel the launch, and calls
// made meanwhile queue behind it and report its result: one VM, one ENI.
func TestEnsureSQSWorker_LostReplyRetryReportsTheSameLaunch(t *testing.T) {
	vms := &ensureVMs{release: make(chan struct{})}
	f := newEnsureFixture(t, vms)

	_, err := f.call(t, ensureNode, 300*time.Millisecond)
	assert.Contains(t, requireAWSCode(t, err, awserrors.ErrorServiceUnavailable), "may still be running")
	require.Equal(t, 1, vms.count())

	type result struct {
		out *workerlaunch.EnsureResponse
		err error
	}
	results := make(chan result, 3)
	for range 3 {
		go func() {
			out, err := f.call(t, ensureNode, 30*time.Second)
			results <- result{out, err}
		}()
	}
	close(vms.release)
	for range 3 {
		r := <-results
		require.NoError(t, r.err)
		assert.Equal(t, "i-0123456789abcdef0", r.out.InstanceID)
		assert.Equal(t, uint64(1), r.out.Generation)
	}
	assert.Equal(t, 1, vms.count())
	assert.Equal(t, 1, f.vols.prepared)
	assert.Equal(t, 1, f.workerENIs(t))
}

// A dispatched launch that errors is ambiguous: every later call reports it
// with its own code and never resends the launch.
func TestEnsureSQSWorker_AmbiguousLaunchIsNeverResent(t *testing.T) {
	vms := &ensureVMs{fail: errors.New("reply lost")}
	f := newEnsureFixture(t, vms)

	for range 3 {
		_, err := f.call(t, ensureNode, 30*time.Second)
		assert.Contains(t, requireAWSCode(t, err, awserrors.ErrorIncorrectState), "outcome is unknown")
	}
	assert.Equal(t, 1, vms.count())
	cur, err := f.ids.Current(t.Context(), sqsTestRegion)
	require.NoError(t, err)
	assert.Equal(t, workerident.StateLaunching, cur.State)
	assert.True(t, cur.Dispatched)
}

// A node outside the cluster or not safe as a subject token is refused before
// any request; a cluster node that does not serve the subject is reported.
func TestEnsureSQSWorker_WrongNodeIsRefused(t *testing.T) {
	f := newEnsureFixture(t, &ensureVMs{})

	for _, node := range []string{"node-9", "node-1.x", "*", ">"} {
		_, err := f.call(t, node, time.Second)
		requireAWSCode(t, err, awserrors.ErrorInvalidParameterValue)
	}
	_, err := f.call(t, "node-2", time.Second)
	assert.Contains(t, requireAWSCode(t, err, awserrors.ErrorServiceUnavailable), "not serving")
	assert.Zero(t, f.vms.count())
	assert.Zero(t, f.vols.prepared)
}

// The handler re-checks the node the request names against its own.
func TestEnsureSQSWorker_HandlerRefusesAnotherNodesRequest(t *testing.T) {
	f := newEnsureFixture(t, &ensureVMs{})
	body, err := json.Marshal(workerlaunch.EnsureRequest{NodeID: "node-2", InstanceType: ensureType})
	require.NoError(t, err)

	msg, err := f.nc.Request(workerlaunch.EnsureSubjectPrefix+ensureNode, body, 5*time.Second)
	require.NoError(t, err)
	resp, err := awserrors.ValidateErrorPayload(msg.Data)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, aws.StringValue(resp.Code))
	assert.Zero(t, f.vms.count())
}

// An unknown instance type is refused before anything is prepared.
func TestEnsureSQSWorker_UnknownInstanceTypeAllocatesNothing(t *testing.T) {
	f := newEnsureFixture(t, &ensureVMs{})

	_, err := admin.EnsureSQSWorker(t.Context(), f.nc, ensureNodes, ensureNode, "x9.huge",
		awsidentifiers.DefaultAccountID(), 5*time.Second)
	requireAWSCode(t, err, awserrors.ErrorInvalidInstanceType)
	assert.Zero(t, f.vols.prepared)
	assert.Zero(t, f.workerENIs(t))
}

// A Region whose worker is pinned to another node is refused, not moved.
func TestEnsureSQSWorker_PinnedElsewhereIsRefused(t *testing.T) {
	f := newEnsureFixture(t, &ensureVMs{})
	_, err := f.ids.BeginLaunch(t.Context(), sqsTestRegion, workerident.LaunchRequest{
		ClaimID: "claim-0123456789abcdef01234567", NodeID: "node-2",
		DataVolumeID: ensureVolume, DataVolumeEncrypted: true,
	})
	require.NoError(t, err)

	_, err = f.call(t, ensureNode, 5*time.Second)
	assert.Contains(t, requireAWSCode(t, err, awserrors.ErrorInvalidParameterValue), "pinned to another node")
	assert.Zero(t, f.vms.count())
}

// A daemon whose cluster services are not up answers unavailable.
func TestEnsureSQSWorker_MissingServicesAreUnavailable(t *testing.T) {
	f := newEnsureFixture(t, &ensureVMs{})
	f.d.sqsWorker = newSQSWorkerEnsurer((&Daemon{}).buildSQSWorkerLauncher)

	_, err := f.call(t, ensureNode, 5*time.Second)
	requireAWSCode(t, err, awserrors.ErrorServiceUnavailable)
}

// A waiter whose context ends while another launch holds the node gives up.
func TestSQSWorkerEnsurer_CancelledWaiterGivesUp(t *testing.T) {
	vms := &ensureVMs{release: make(chan struct{})}
	f := newEnsureFixture(t, vms)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.d.sqsWorker.ensure(t.Context(), sqsTestRegion, ensureType)
	}()
	require.Eventually(t, func() bool { return vms.count() == 1 }, 5*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := f.d.sqsWorker.ensure(ctx, sqsTestRegion, ensureType)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(vms.release)
	<-done
	assert.Equal(t, 1, vms.count())
}

// The action accepts only a bound reply from the node it addressed.
func TestEnsureSQSWorker_RefusesAnUnboundOrForeignReply(t *testing.T) {
	f := newEnsureFixture(t, &ensureVMs{})
	for _, reply := range []workerlaunch.EnsureResponse{
		{NodeID: "node-2", InstanceID: "i-0123456789abcdef0", State: "launching"},
		{NodeID: "node-1", InstanceID: "i-0123456789abcdef0", State: workerlaunch.StateBound},
		{NodeID: "node-2", State: workerlaunch.StateBound},
	} {
		body, err := json.Marshal(reply)
		require.NoError(t, err)
		sub, err := f.nc.Subscribe(workerlaunch.EnsureSubjectPrefix+"node-2", func(m *nats.Msg) { _ = m.Respond(body) })
		require.NoError(t, err)
		_, err = f.call(t, "node-2", 5*time.Second)
		requireAWSCode(t, err, awserrors.ErrorServerInternal)
		require.NoError(t, sub.Unsubscribe())
	}
}

// A node without a complete worker image refuses before allocating a volume
// or network or recording any intent; once installed, the next call launches.
func TestEnsureSQSWorker_MissingOrIncompleteImageAllocatesNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		breakImage func(t *testing.T)
	}{
		{"missing", func(t *testing.T) {
			require.NoError(t, os.RemoveAll(sqsWorkerImageDir()))
		}},
		{"empty initramfs", func(t *testing.T) {
			require.NoError(t, os.Truncate(filepath.Join(sqsWorkerImageDir(), "initramfs.cpio.gz"), 0))
		}},
		{"no kernel", func(t *testing.T) {
			require.NoError(t, os.Remove(filepath.Join(sqsWorkerImageDir(), "vmlinuz")))
		}},
		{"kernel is a directory", func(t *testing.T) {
			p := filepath.Join(sqsWorkerImageDir(), "vmlinuz")
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Mkdir(p, 0o755))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnsureFixture(t, &ensureVMs{})
			tc.breakImage(t)

			_, err := f.call(t, ensureNode, 10*time.Second)
			msg := requireAWSCode(t, err, awserrors.ErrorServiceUnavailable)
			assert.Contains(t, msg, "sqs worker image")
			assert.Zero(t, f.vols.prepared, "no volume may be prepared")
			assert.Zero(t, f.workerENIs(t), "no network may be allocated")
			assert.Zero(t, f.vms.count())
			_, err = f.ids.Current(t.Context(), sqsTestRegion)
			require.ErrorIs(t, err, jetstream.ErrKeyNotFound, "no intent may be recorded")

			require.NoError(t, os.RemoveAll(sqsWorkerImageDir()))
			installWorkerImage(t)
			resp, err := f.call(t, ensureNode, 10*time.Second)
			require.NoError(t, err)
			assert.Equal(t, workerlaunch.StateBound, resp.State)
			assert.Equal(t, 1, f.vols.prepared)
			assert.Equal(t, 1, f.vms.count())
		})
	}
}

// A replacement after a confirmed stop is also refused before it fences or
// begins, so the stopped record is left exactly as it was.
func TestEnsureSQSWorker_MissingImageLeavesAStoppedWorkerUntouched(t *testing.T) {
	f := newEnsureFixture(t, &ensureVMs{})
	_, err := f.call(t, ensureNode, 10*time.Second)
	require.NoError(t, err)
	cur, err := f.ids.Current(t.Context(), sqsTestRegion)
	require.NoError(t, err)
	intent := workerident.Intent{Region: sqsTestRegion, Generation: cur.Generation, ClaimID: cur.ClaimID}
	require.NoError(t, f.ids.RecordStopped(t.Context(), intent, cur.InstanceID))
	before, err := f.ids.Current(t.Context(), sqsTestRegion)
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(sqsWorkerImageDir()))
	_, err = f.call(t, ensureNode, 10*time.Second)
	requireAWSCode(t, err, awserrors.ErrorServiceUnavailable)
	after, err := f.ids.Current(t.Context(), sqsTestRegion)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, 1, f.vms.count())
}
