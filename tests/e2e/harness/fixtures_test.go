//go:build e2e

package harness

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
)

// fakeEC2 records create/delete invocations. The embedded ec2iface.EC2API
// satisfies the full interface via nil-method panics — any un-mocked call
// surfaces as a panic so accidental dependencies on new APIs fail loudly.
type fakeEC2 struct {
	ec2iface.EC2API

	createKeyPair atomic.Int64
	deleteKeyPair atomic.Int64

	// holdCreate, when non-nil, blocks CreateKeyPair until closed. Drives
	// the concurrent-callers test deterministically.
	holdCreate chan struct{}

	// Volume teardown state. volState is what DescribeVolumes reports;
	// DetachVolume flips it to "available", modelling a node that releases the
	// volume once the attachment is forced off. deleteVolumeErr, when set, is
	// what DeleteVolume returns — used to drive the leak-reporting path.
	volMu           sync.Mutex
	volState        string
	detachVolume    atomic.Int64
	deleteVolume    atomic.Int64
	deleteVolumeErr error
}

func (f *fakeEC2) CreateVolume(*ec2.CreateVolumeInput) (*ec2.Volume, error) {
	return &ec2.Volume{VolumeId: aws.String("vol-fake")}, nil
}

func (f *fakeEC2) DescribeVolumes(*ec2.DescribeVolumesInput) (*ec2.DescribeVolumesOutput, error) {
	f.volMu.Lock()
	defer f.volMu.Unlock()
	return &ec2.DescribeVolumesOutput{
		Volumes: []*ec2.Volume{{VolumeId: aws.String("vol-fake"), State: aws.String(f.volState)}},
	}, nil
}

func (f *fakeEC2) DetachVolume(*ec2.DetachVolumeInput) (*ec2.VolumeAttachment, error) {
	f.detachVolume.Add(1)
	f.volMu.Lock()
	defer f.volMu.Unlock()
	f.volState = ec2.VolumeStateAvailable
	return &ec2.VolumeAttachment{}, nil
}

// DetachVolumeWithContext is what teardownVolume actually calls (bounded by
// the teardown deadline); it shares DetachVolume's behaviour and ignores ctx.
func (f *fakeEC2) DetachVolumeWithContext(_ aws.Context, in *ec2.DetachVolumeInput, _ ...request.Option) (*ec2.VolumeAttachment, error) {
	return f.DetachVolume(in)
}

func (f *fakeEC2) DeleteVolume(*ec2.DeleteVolumeInput) (*ec2.DeleteVolumeOutput, error) {
	f.deleteVolume.Add(1)
	if f.deleteVolumeErr != nil {
		return nil, f.deleteVolumeErr
	}
	// The refusal the real API gives, and the whole reason teardown has to
	// detach first: an attached volume cannot be deleted.
	f.volMu.Lock()
	defer f.volMu.Unlock()
	if f.volState != ec2.VolumeStateAvailable {
		return nil, awserr.New("VolumeInUse",
			"The specified Amazon EBS volume is attached to an instance.", nil)
	}
	return &ec2.DeleteVolumeOutput{}, nil
}

func (f *fakeEC2) CreateTags(*ec2.CreateTagsInput) (*ec2.CreateTagsOutput, error) {
	return &ec2.CreateTagsOutput{}, nil
}

func (f *fakeEC2) CreateKeyPair(in *ec2.CreateKeyPairInput) (*ec2.CreateKeyPairOutput, error) {
	if f.holdCreate != nil {
		<-f.holdCreate
	}
	f.createKeyPair.Add(1)
	return &ec2.CreateKeyPairOutput{
		KeyName:     in.KeyName,
		KeyMaterial: aws.String("FAKE PEM"),
	}, nil
}

func (f *fakeEC2) DeleteKeyPair(*ec2.DeleteKeyPairInput) (*ec2.DeleteKeyPairOutput, error) {
	f.deleteKeyPair.Add(1)
	return &ec2.DeleteKeyPairOutput{}, nil
}

type fakeELB struct{ elbv2iface.ELBV2API }

// Compile-time check: fakes satisfy the interfaces the Fixture stores.
// Drift in either iface surface surfaces here, not at first test invocation.
var (
	_ ec2iface.EC2API     = (*fakeEC2)(nil)
	_ elbv2iface.ELBV2API = (*fakeELB)(nil)
)

func newFakeFixture(t *testing.T) (*Fixture, *fakeEC2) {
	t.Helper()
	ec2c := &fakeEC2{}
	fx := newFixture(t, ec2c, &fakeELB{})
	return fx, ec2c
}

// TestEnsureKeyPair_FirstCallCreates verifies first invocation calls
// CreateKeyPair once and writes PEM to the artifact dir.
func TestEnsureKeyPair_FirstCallCreates(t *testing.T) {
	fx, ec2c := newFakeFixture(t)
	dir := t.TempDir()

	name, pemPath := EnsureKeyPair(t, fx, dir)
	if name == "" {
		t.Fatalf("EnsureKeyPair returned empty name")
	}
	if got := ec2c.createKeyPair.Load(); got != 1 {
		t.Fatalf("CreateKeyPair calls = %d, want 1", got)
	}
	if filepath.Dir(pemPath) != dir {
		t.Fatalf("pemPath dir = %s, want %s", filepath.Dir(pemPath), dir)
	}
}

// TestEnsureKeyPair_SecondCallCached verifies subsequent invocations
// return the cached name without re-calling CreateKeyPair.
func TestEnsureKeyPair_SecondCallCached(t *testing.T) {
	fx, ec2c := newFakeFixture(t)
	dir := t.TempDir()

	first, _ := EnsureKeyPair(t, fx, dir)
	second, _ := EnsureKeyPair(t, fx, dir)
	if first != second {
		t.Fatalf("second call returned %q, want cached %q", second, first)
	}
	if got := ec2c.createKeyPair.Load(); got != 1 {
		t.Fatalf("CreateKeyPair calls = %d, want 1 (second call should hit cache)", got)
	}
}

// TestEnsureKeyPair_ConcurrentCallers verifies N goroutines see exactly
// one CreateKeyPair via singleflight dedup.
func TestEnsureKeyPair_ConcurrentCallers(t *testing.T) {
	fx, ec2c := newFakeFixture(t)
	ec2c.holdCreate = make(chan struct{})
	dir := t.TempDir()

	const N = 10
	var wg sync.WaitGroup
	names := make([]string, N)
	started := make(chan struct{}, N)
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started <- struct{}{}
			n, _ := EnsureKeyPair(t, fx, dir)
			names[i] = n
		}(i)
	}

	for range N {
		<-started
	}
	close(ec2c.holdCreate)
	wg.Wait()

	if got := ec2c.createKeyPair.Load(); got != 1 {
		t.Fatalf("CreateKeyPair calls = %d, want 1 across %d concurrent callers", got, N)
	}
	for i, n := range names {
		if n != names[0] {
			t.Fatalf("caller %d got name %q, want %q", i, n, names[0])
		}
	}
}

// TestProcessFixture_CloseRunsCleanupsLIFO verifies the process-mode
// cleanup chain fires every registered callback in reverse order, exactly
// once per Close(), and that a second Close() is a no-op.
func TestProcessFixture_CloseRunsCleanupsLIFO(t *testing.T) {
	ec2c := &fakeEC2{}
	fx := &Fixture{
		EC2:      ec2c,
		ELBv2:    &fakeELB{},
		scratch:  "test",
		memo:     map[string]string{},
		cleanups: map[string]struct{}{},
	}

	var order []int
	fx.RegisterCleanup(func() { order = append(order, 1) })
	fx.RegisterCleanup(func() { order = append(order, 2) })
	fx.RegisterCleanup(func() { order = append(order, 3) })

	if err := fx.Close(); err != nil {
		t.Fatalf("Close err = %v, want nil", err)
	}
	want := []int{3, 2, 1}
	if len(order) != len(want) {
		t.Fatalf("cleanup count = %d, want %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("cleanup order = %v, want %v", order, want)
		}
	}

	// Second Close is a no-op — no duplicate firings, no panic.
	if err := fx.Close(); err != nil {
		t.Fatalf("second Close err = %v, want nil", err)
	}
	if len(order) != len(want) {
		t.Fatalf("second Close re-fired callbacks: order=%v", order)
	}
}

// newProcessFakeFixture builds a process-mode fixture (parent == nil) over the
// fake EC2, which is the mode whose teardown failures surface through Close().
func newProcessFakeFixture() (*Fixture, *fakeEC2) {
	ec2c := &fakeEC2{}
	return &Fixture{
		EC2:      ec2c,
		ELBv2:    &fakeELB{},
		scratch:  "test",
		memo:     map[string]string{},
		cleanups: map[string]struct{}{},
	}, ec2c
}

// TestTeardownVolume_DetachesBeforeDeleting covers the ordering gap: fixture
// cleanups drain LIFO, so a volume created after its instance is torn down
// while still attached. Teardown must release it rather than let DeleteVolume
// fail with VolumeInUse and strand it.
func TestTeardownVolume_DetachesBeforeDeleting(t *testing.T) {
	ec2c := &fakeEC2{volState: ec2.VolumeStateInUse}

	if err := teardownVolume(ec2c, "vol-fake"); err != nil {
		t.Fatalf("teardownVolume err = %v, want nil", err)
	}
	if got := ec2c.detachVolume.Load(); got != 1 {
		t.Fatalf("DetachVolume calls = %d, want 1 (attached volume was not released)", got)
	}
	if got := ec2c.deleteVolume.Load(); got != 1 {
		t.Fatalf("DeleteVolume calls = %d, want 1", got)
	}
}

// TestTeardownVolume_AvailableSkipsDetach verifies the common case costs no
// extra API calls: an already-available volume is deleted directly.
func TestTeardownVolume_AvailableSkipsDetach(t *testing.T) {
	ec2c := &fakeEC2{volState: ec2.VolumeStateAvailable}

	if err := teardownVolume(ec2c, "vol-fake"); err != nil {
		t.Fatalf("teardownVolume err = %v, want nil", err)
	}
	if got := ec2c.detachVolume.Load(); got != 0 {
		t.Fatalf("DetachVolume calls = %d, want 0 for an available volume", got)
	}
}

// TestEnsureVolume_CleanupReleasesAttachedVolume is the regression test for the
// leak itself: the volume is created available, then attached, and teardown has
// to cope. This is exactly the shape of a suite that calls EnsureInstance before
// EnsureVolume, since the LIFO chain tears the volume down first.
func TestEnsureVolume_CleanupReleasesAttachedVolume(t *testing.T) {
	fx, ec2c := newProcessFakeFixture()
	ec2c.volState = ec2.VolumeStateAvailable

	_ = EnsureVolume(t, fx, "ap-southeast-2a", 10)
	ec2c.volState = ec2.VolumeStateInUse

	if err := fx.Close(); err != nil {
		t.Fatalf("Close err = %v, want nil (attached volume was not released and is leaked)", err)
	}
	if got := ec2c.detachVolume.Load(); got != 1 {
		t.Fatalf("DetachVolume calls = %d, want 1", got)
	}
}

// TestFixtureClose_ReportsLeak verifies a teardown that fails is reported as a
// leak rather than logged and forgotten. Before this, a failing cleanup left a
// real volume on the node and the run still went green.
func TestFixtureClose_ReportsLeak(t *testing.T) {
	fx, ec2c := newProcessFakeFixture()
	ec2c.volState = ec2.VolumeStateAvailable
	ec2c.deleteVolumeErr = errors.New("boom")

	_ = EnsureVolume(t, fx, "ap-southeast-2a", 10)

	err := fx.Close()
	if err == nil {
		t.Fatalf("Close err = nil, want a leak report for the failed volume delete")
	}
	if !strings.Contains(err.Error(), "vol-fake") {
		t.Fatalf("leak report %q does not name the leaked volume", err)
	}
}

// TestFixtureClose_PanicDoesNotStrandRest verifies one bad teardown cannot
// abort the chain. An aborted chain strands every resource registered before
// the panic, which is how a whole instance has previously been left running.
func TestFixtureClose_PanicDoesNotStrandRest(t *testing.T) {
	fx, _ := newProcessFakeFixture()

	var ran []int
	fx.RegisterCleanup(func() { ran = append(ran, 1) })
	fx.RegisterCleanup(func() { panic("teardown blew up") })
	fx.RegisterCleanup(func() { ran = append(ran, 3) })

	err := fx.Close()
	if len(ran) != 2 {
		t.Fatalf("cleanups run = %v, want both non-panicking callbacks", ran)
	}
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("Close err = %v, want a report naming the panic", err)
	}
}

// TestEnsureKeyPair_CleanupRunsOnce verifies cleanup fires DeleteKeyPair
// exactly once even after multiple Ensure calls memoize to the same ID.
func TestEnsureKeyPair_CleanupRunsOnce(t *testing.T) {
	var ec2c *fakeEC2

	t.Run("inner", func(it *testing.T) {
		var fx *Fixture
		fx, ec2c = newFakeFixture(it)
		dir := it.TempDir()

		EnsureKeyPair(it, fx, dir)
		EnsureKeyPair(it, fx, dir)
		EnsureKeyPair(it, fx, dir)
	})

	if got := ec2c.deleteKeyPair.Load(); got != 1 {
		t.Fatalf("DeleteKeyPair calls = %d, want 1 (3 Ensure calls, 1 cleanup)", got)
	}
}

// TestPollUntil_SuccessFirstAttempt covers the fast path: cond returns
// true immediately, no sleep, no retry.
func TestPollUntil_SuccessFirstAttempt(t *testing.T) {
	calls := 0
	err := pollUntil(t, 100*time.Millisecond, 1*time.Millisecond, func() (bool, error) {
		calls++
		return true, nil
	})
	if err != nil {
		t.Fatalf("pollUntil err = %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("cond invocations = %d, want 1", calls)
	}
}

// TestPollUntil_TimeoutWrapsLastErr verifies the deadline error wraps the
// last cond error so callers can errors.Is against domain sentinels.
func TestPollUntil_TimeoutWrapsLastErr(t *testing.T) {
	sentinel := errors.New("not-ready")
	err := pollUntil(t, 5*time.Millisecond, 1*time.Millisecond, func() (bool, error) {
		return false, sentinel
	})
	if err == nil {
		t.Fatalf("pollUntil err = nil, want timeout")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("pollUntil err = %v, want wrap of %v", err, sentinel)
	}
}

// Compile-time check: every exported Ensure* compiles with the Fixture.
// Never invoked at runtime; pure type assertion against the public API.
//
//nolint:unused,deadcode // exists only to fail compile when an Ensure* signature drifts
func _ensureCompileCheck(t *testing.T, fx *Fixture) {
	_, _ = EnsureKeyPair(t, fx, "/tmp")
	_ = EnsureAMI(t, fx, AMISource{Existing: "ami-0"})
	_ = EnsureDefaultVPC(t, fx)
	_ = EnsureSubnet(t, fx, "vpc-0", "10.0.0.0/24", "ap-southeast-2a")
	_ = EnsureSG(t, fx, "vpc-0", "sg")
	_ = EnsureInstance(t, fx, InstanceSpec{AMIID: "ami-0"})
	_ = EnsureVolume(t, fx, "ap-southeast-2a", 10)
	_ = EnsureSnapshot(t, fx, SnapshotSpec{VolumeID: "vol-0"})
	_ = EnsureNATGateway(t, fx, "subnet-0", "")
}
