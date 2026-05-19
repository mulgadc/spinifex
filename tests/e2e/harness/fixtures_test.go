//go:build e2e

package harness

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
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
	_ = EnsureSnapshot(t, fx, "vol-0")
	_ = EnsureNATGateway(t, fx, "subnet-0", "")
}
