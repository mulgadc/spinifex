package handlers_bedrock

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const resolverTestModel = "meta.llama3-2-1b-instruct-v1:0"

// fakeEndpointService is an EndpointService returning a scripted record and
// counting the calls, so a resolver test can assert both the answer and how
// many NATS round trips producing it would have cost.
type fakeEndpointService struct {
	mu sync.Mutex

	record      EndpointRecord
	describeErr error
	ensureErr   error

	describeCalls atomic.Int64
	ensureCalls   atomic.Int64

	// beforeDescribe runs inside Describe, so a test can hold every caller at
	// the same point to exercise the concurrent path.
	beforeDescribe func()
}

var _ EndpointService = (*fakeEndpointService)(nil)

func (f *fakeEndpointService) Describe(_ context.Context, _ *DescribeEndpointInput, _ string) (*DescribeEndpointOutput, error) {
	f.describeCalls.Add(1)
	if f.beforeDescribe != nil {
		f.beforeDescribe()
	}
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &DescribeEndpointOutput{Endpoint: f.record}, nil
}

// Ensure records the request and moves the fake to STARTING, mirroring the
// daemon: the reply is immediate and the launch outlives it.
func (f *fakeEndpointService) Ensure(_ context.Context, in *EnsureEndpointInput, _ string) (*EnsureEndpointOutput, error) {
	f.ensureCalls.Add(1)
	if f.ensureErr != nil {
		return nil, f.ensureErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record = EndpointRecord{ModelID: in.ModelID, State: StateStarting}
	return &EnsureEndpointOutput{Endpoint: f.record}, nil
}

func (f *fakeEndpointService) List(context.Context, *ListEndpointsInput, string) (*ListEndpointsOutput, error) {
	return &ListEndpointsOutput{}, nil
}

func (f *fakeEndpointService) Delete(context.Context, *DeleteEndpointInput, string) (*DeleteEndpointOutput, error) {
	return &DeleteEndpointOutput{}, nil
}

func (f *fakeEndpointService) setRecord(rec EndpointRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record = rec
}

func readyRecord(baseURL string) EndpointRecord {
	return EndpointRecord{ModelID: resolverTestModel, State: StateReady, BaseURL: baseURL}
}

// TestDynamicEndpointResolver_StaticWins keeps the escape hatch: a pinned
// endpoint bypasses the lifecycle entirely, which is what dev boxes and the
// gated E2E tier rely on.
func TestDynamicEndpointResolver_StaticWins(t *testing.T) {
	svc := &fakeEndpointService{record: readyRecord("http://10.0.0.9:8000")}
	r := NewDynamicEndpointResolver(svc, map[string]string{resolverTestModel: "http://pinned:8000"}, 0)

	baseURL, ok, err := r.Endpoint(context.Background(), resolverTestModel)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "http://pinned:8000", baseURL)
	assert.Zero(t, svc.describeCalls.Load(), "a pinned endpoint must not consult the registry")
}

func TestDynamicEndpointResolver_ReadyResolves(t *testing.T) {
	svc := &fakeEndpointService{record: readyRecord("http://10.0.0.9:8000")}
	r := NewDynamicEndpointResolver(svc, nil, 0)

	baseURL, ok, err := r.Endpoint(context.Background(), resolverTestModel)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "http://10.0.0.9:8000", baseURL)
	assert.Zero(t, svc.ensureCalls.Load(), "a serving endpoint must not be asked to launch again")
}

// TestDynamicEndpointResolver_ReadyWithoutBaseURLDoesNotResolve guards against
// routing inference at an empty address: READY without a base URL is a record
// we cannot use, not one we can.
func TestDynamicEndpointResolver_ReadyWithoutBaseURLDoesNotResolve(t *testing.T) {
	svc := &fakeEndpointService{record: EndpointRecord{ModelID: resolverTestModel, State: StateReady}}
	r := NewDynamicEndpointResolver(svc, nil, 0)

	baseURL, ok, err := r.Endpoint(context.Background(), resolverTestModel)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, baseURL)
}

// TestDynamicEndpointResolver_AbsentRequestsLaunch is the whole point of the
// change: a cold model asks the daemon for a VM and reports "not yet", which
// the invoke paths turn into a retryable ModelNotReadyException.
func TestDynamicEndpointResolver_AbsentRequestsLaunch(t *testing.T) {
	svc := &fakeEndpointService{record: EndpointRecord{ModelID: resolverTestModel, State: StateAbsent}}
	r := NewDynamicEndpointResolver(svc, nil, 0)

	_, ok, err := r.Endpoint(context.Background(), resolverTestModel)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, int64(1), svc.ensureCalls.Load())
}

// TestDynamicEndpointResolver_InFlightStatesDoNotReEnsure covers the states
// where something is already happening: asking again changes nothing, and a
// draining endpoint must not be resurrected mid-teardown.
func TestDynamicEndpointResolver_InFlightStatesDoNotReEnsure(t *testing.T) {
	for _, state := range []EndpointState{StateStarting, StateDraining} {
		svc := &fakeEndpointService{record: EndpointRecord{ModelID: resolverTestModel, State: state}}
		r := NewDynamicEndpointResolver(svc, nil, 0)

		_, ok, err := r.Endpoint(context.Background(), resolverTestModel)
		require.NoError(t, err, "state %s", state)
		assert.False(t, ok, "state %s", state)
		assert.Zero(t, svc.ensureCalls.Load(), "state %s must not request a launch", state)
	}
}

// TestDynamicEndpointResolver_DescribeErrorPropagates keeps a broken control
// plane distinguishable from a cold model: an unreachable daemon is an error,
// not an indefinite "not ready".
func TestDynamicEndpointResolver_DescribeErrorPropagates(t *testing.T) {
	svc := &fakeEndpointService{describeErr: errors.New("no responders")}
	r := NewDynamicEndpointResolver(svc, nil, 0)

	_, ok, err := r.Endpoint(context.Background(), resolverTestModel)
	require.Error(t, err)
	assert.False(t, ok)
	assert.Zero(t, svc.ensureCalls.Load())
}

// TestDynamicEndpointResolver_EnsureErrorPropagates preserves the daemon's own
// refusal, which carries the real AWS code: no GPU it can admit comes back as
// ModelNotReadyException with the daemon's message rather than a bare one.
func TestDynamicEndpointResolver_EnsureErrorPropagates(t *testing.T) {
	svc := &fakeEndpointService{
		record:    EndpointRecord{ModelID: resolverTestModel, State: StateAbsent},
		ensureErr: errors.New(awserrors.ErrorModelNotReadyException),
	}
	r := NewDynamicEndpointResolver(svc, nil, 0)

	_, ok, err := r.Endpoint(context.Background(), resolverTestModel)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorModelNotReadyException, err.Error())
	assert.False(t, ok)
}

// TestDynamicEndpointResolver_CachesReadyBaseURL keeps a warm model off the
// bus: the steady state is every invoke hitting this path.
func TestDynamicEndpointResolver_CachesReadyBaseURL(t *testing.T) {
	svc := &fakeEndpointService{record: readyRecord("http://10.0.0.9:8000")}
	r := NewDynamicEndpointResolver(svc, nil, time.Minute)

	for range 3 {
		baseURL, ok, err := r.Endpoint(context.Background(), resolverTestModel)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "http://10.0.0.9:8000", baseURL)
	}
	assert.Equal(t, int64(1), svc.describeCalls.Load(), "a cached endpoint must not re-describe")
}

// TestDynamicEndpointResolver_CacheExpires bounds how stale a base URL can
// get, which is what makes a deleted endpoint recoverable without an
// invalidation hook the resolver interface has no room for.
func TestDynamicEndpointResolver_CacheExpires(t *testing.T) {
	svc := &fakeEndpointService{record: readyRecord("http://10.0.0.9:8000")}
	r := NewDynamicEndpointResolver(svc, nil, time.Millisecond)

	_, ok, err := r.Endpoint(context.Background(), resolverTestModel)
	require.NoError(t, err)
	require.True(t, ok)

	svc.setRecord(readyRecord("http://10.0.0.10:8000"))
	time.Sleep(5 * time.Millisecond)

	baseURL, ok, err := r.Endpoint(context.Background(), resolverTestModel)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "http://10.0.0.10:8000", baseURL, "an expired entry must re-resolve")
	assert.Equal(t, int64(2), svc.describeCalls.Load())
}

// TestDynamicEndpointResolver_ConcurrentColdCallsCollapse covers the burst a
// cold model actually sees: many in-flight invokes, one describe and one
// ensure between them.
func TestDynamicEndpointResolver_ConcurrentColdCallsCollapse(t *testing.T) {
	release := make(chan struct{})
	svc := &fakeEndpointService{
		record:         EndpointRecord{ModelID: resolverTestModel, State: StateAbsent},
		beforeDescribe: func() { <-release },
	}
	r := NewDynamicEndpointResolver(svc, nil, 0)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, ok, err := r.Endpoint(context.Background(), resolverTestModel)
			assert.NoError(t, err)
			assert.False(t, ok)
		})
	}

	// Hold the first describe until every caller has had a chance to join it,
	// so the assertion below is about single-flight and not about scheduling.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int64(1), svc.describeCalls.Load())
	assert.Equal(t, int64(1), svc.ensureCalls.Load())
}
