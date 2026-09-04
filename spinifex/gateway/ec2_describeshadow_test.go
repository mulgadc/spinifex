//test:in-package — drives runDescribeShadow, an unexported GatewayConfig
// method that gates the shadow describe path on DescribeSource, directly.

package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// describeShadowSpyCache counts List/Get calls and closes called the first
// time either happens, so a test can wait for the async shadow path to reach
// the cache without a sleep.
type describeShadowSpyCache struct {
	mu           sync.Mutex
	listN, getN  int
	called       chan struct{}
	closeCalledO sync.Once
}

func newDescribeShadowSpyCache() *describeShadowSpyCache {
	return &describeShadowSpyCache{called: make(chan struct{})}
}

func (s *describeShadowSpyCache) mark() {
	s.closeCalledO.Do(func() { close(s.called) })
}

func (s *describeShadowSpyCache) List(_ context.Context, _ string) ([]*vm.VM, bool) {
	s.mu.Lock()
	s.listN++
	s.mu.Unlock()
	s.mark()
	return nil, true
}

func (s *describeShadowSpyCache) Get(_ context.Context, _ string) (*vm.VM, error) {
	s.mu.Lock()
	s.getN++
	s.mu.Unlock()
	s.mark()
	return nil, nil
}

func (s *describeShadowSpyCache) Degraded() bool { return false }

func (s *describeShadowSpyCache) calls() (list, get int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listN, s.getN
}

// describeShadowBlockingCache never answers, modelling a cache path far
// slower than any request deadline.
type describeShadowBlockingCache struct{ block chan struct{} }

func (b *describeShadowBlockingCache) List(_ context.Context, _ string) ([]*vm.VM, bool) {
	<-b.block
	return nil, true
}

func (b *describeShadowBlockingCache) Get(_ context.Context, _ string) (*vm.VM, error) {
	<-b.block
	return nil, nil
}

func (b *describeShadowBlockingCache) Degraded() bool { return false }

func sampleDescribeOutput() *ec2.DescribeInstancesOutput {
	return &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{
			{InstanceId: aws.String("i-1"), State: &ec2.InstanceState{Name: aws.String("running")}},
		}}},
	}
}

// Under DescribeSourceFanout (the default), the cache path must not run at
// all — not even asynchronously. Because runDescribeShadow's gate returns
// before spawning anything. The call is asynchronous when it does happen
// (see TestRunDescribeShadow_ShadowSource_RunsCachePath), so a bare
// zero-calls check immediately after calling runDescribeShadow would pass
// even with the gate removed — it would just be reading the counters before
// the goroutine had a chance to run. Waiting out a bounded window for the
// spy to fire, and asserting it never does, is what actually distinguishes
// "gated" from "gate removed, but not scheduled yet".
func TestRunDescribeShadow_FanoutSource_NeverTouchesCache(t *testing.T) {
	spy := newDescribeShadowSpyCache()
	gw := &GatewayConfig{
		DescribeSource: DescribeSourceFanout,
		DescribeCache:  spy,
		DescribeShadow: gateway_ec2_instance.NewShadowComparator(time.Minute),
	}

	gw.runDescribeShadow(context.Background(), &ec2.DescribeInstancesInput{}, "acct-1", sampleDescribeOutput())

	select {
	case <-spy.called:
		t.Fatal("cache was reached under fanout")
	case <-time.After(300 * time.Millisecond):
	}

	list, get := spy.calls()
	assert.Zero(t, list, "cache List must never be called under fanout")
	assert.Zero(t, get, "cache Get must never be called under fanout")
}

// Shadow mode with no comparator wired must not panic or touch the cache —
// matching a gateway built without DescribeCache. The cache call, if the
// nil guard were missing, would happen inside a detached goroutine, so — as
// with the fanout gate above — this must wait out a bounded window rather
// than check synchronously; a synchronous check would pass whether or not
// the goroutine was ever spawned.
func TestRunDescribeShadow_ShadowSource_NilComparator_NoOp(t *testing.T) {
	spy := newDescribeShadowSpyCache()
	gw := &GatewayConfig{
		DescribeSource: DescribeSourceShadow,
		DescribeCache:  spy,
		DescribeShadow: nil,
	}

	require.NotPanics(t, func() {
		gw.runDescribeShadow(context.Background(), &ec2.DescribeInstancesInput{}, "acct-1", sampleDescribeOutput())
	})

	select {
	case <-spy.called:
		t.Fatal("cache was reached with no comparator wired")
	case <-time.After(300 * time.Millisecond):
	}
	list, get := spy.calls()
	assert.Zero(t, list)
	assert.Zero(t, get)
}

// Shadow mode with no cache wired must not panic — matching every other
// DescribeSource=cache/shadow guard in this package.
func TestRunDescribeShadow_ShadowSource_NilCache_NoOp(t *testing.T) {
	gw := &GatewayConfig{
		DescribeSource: DescribeSourceShadow,
		DescribeCache:  nil,
		DescribeShadow: gateway_ec2_instance.NewShadowComparator(time.Minute),
	}

	require.NotPanics(t, func() {
		gw.runDescribeShadow(context.Background(), &ec2.DescribeInstancesInput{}, "acct-1", sampleDescribeOutput())
	})
	// No spy to watch here (DescribeCache is nil), so give any wrongly
	// spawned goroutine a window to crash the process before the test exits.
	time.Sleep(300 * time.Millisecond)
}

// Shadow mode, fully wired, must actually run the cache path — proving the
// gate's true branch really reaches ShadowComparator.Run rather than only
// being exercised by unit tests of Run in isolation.
func TestRunDescribeShadow_ShadowSource_RunsCachePath(t *testing.T) {
	spy := newDescribeShadowSpyCache()
	gw := &GatewayConfig{
		DescribeSource: DescribeSourceShadow,
		DescribeCache:  spy,
		DescribeShadow: gateway_ec2_instance.NewShadowComparator(time.Minute),
		AZ:             "us-east-1a",
	}

	gw.runDescribeShadow(context.Background(), &ec2.DescribeInstancesInput{}, "acct-1", sampleDescribeOutput())

	select {
	case <-spy.called:
	case <-time.After(2 * time.Second):
		t.Fatal("shadow comparator never reached the cache")
	}
}

// The synchronous part of runDescribeShadow — everything up to spawning the
// comparison goroutine — must return promptly even when the cache path is
// arbitrarily slow. The comparison itself must never add latency to the
// request it shadows.
func TestRunDescribeShadow_NeverBlocksOnSlowCache(t *testing.T) {
	blocking := &describeShadowBlockingCache{block: make(chan struct{})}
	gw := &GatewayConfig{
		DescribeSource: DescribeSourceShadow,
		DescribeCache:  blocking,
		DescribeShadow: gateway_ec2_instance.NewShadowComparator(time.Minute),
		AZ:             "us-east-1a",
	}

	start := time.Now()
	gw.runDescribeShadow(context.Background(), &ec2.DescribeInstancesInput{}, "acct-1", sampleDescribeOutput())
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 100*time.Millisecond,
		"runDescribeShadow must return without waiting on the cache path")
}
