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

	// lastDescribeAccount/lastEnsureAccount/lastEnsurePinned capture the last
	// call's account (the third EndpointService parameter, mirroring how
	// ProvisionedEndpointAdapter threads it) and, for Ensure, whether it
	// asked for a pinned endpoint — what the account-aware resolve path
	// tests assert against.
	lastDescribeAccount string
	lastEnsureAccount   string
	lastEnsurePinned    bool

	// beforeDescribe runs inside Describe, so a test can hold every caller at
	// the same point to exercise the concurrent path.
	beforeDescribe func()
}

var _ EndpointService = (*fakeEndpointService)(nil)

func (f *fakeEndpointService) Describe(_ context.Context, _ *DescribeEndpointInput, accountID string) (*DescribeEndpointOutput, error) {
	f.describeCalls.Add(1)
	if f.beforeDescribe != nil {
		f.beforeDescribe()
	}
	f.mu.Lock()
	f.lastDescribeAccount = accountID
	f.mu.Unlock()
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &DescribeEndpointOutput{Endpoint: f.record}, nil
}

// Ensure records the request and moves the fake to STARTING, mirroring the
// daemon: the reply is immediate and the launch outlives it.
func (f *fakeEndpointService) Ensure(_ context.Context, in *EnsureEndpointInput, accountID string) (*EnsureEndpointOutput, error) {
	f.ensureCalls.Add(1)
	f.mu.Lock()
	f.lastEnsureAccount = accountID
	f.lastEnsurePinned = in.Pinned
	f.mu.Unlock()
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

func (f *fakeEndpointService) describeAccount() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastDescribeAccount
}

func (f *fakeEndpointService) ensureAccount() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastEnsureAccount
}

func (f *fakeEndpointService) ensurePinned() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastEnsurePinned
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

// The default is the whole cold-start contract: no wait, no extra describe,
// a retryable answer straight back to the caller.
func TestDynamicEndpointResolver_ColdStartWaitDefaultsToNoWait(t *testing.T) {
	svc := &fakeEndpointService{record: EndpointRecord{ModelID: resolverTestModel, State: StateAbsent}}
	r := NewDynamicEndpointResolver(svc, nil, 0)

	started := time.Now()
	_, ok, err := r.Endpoint(context.Background(), resolverTestModel)

	require.NoError(t, err)
	assert.False(t, ok)
	assert.Less(t, time.Since(started), 100*time.Millisecond, "a cold call must not be held")
	assert.Equal(t, int64(1), svc.describeCalls.Load(), "no polling describe at the default")
}

// With a wait configured, a launch that lands inside the budget is served on
// the original call rather than a retry.
func TestDynamicEndpointResolver_ColdStartWaitResolvesWhenReady(t *testing.T) {
	svc := &fakeEndpointService{record: EndpointRecord{ModelID: resolverTestModel, State: StateAbsent}}
	r := NewDynamicEndpointResolver(svc, nil, 0, WithColdStartWait(5*time.Second))

	go func() {
		time.Sleep(600 * time.Millisecond)
		svc.setRecord(readyRecord("http://10.0.0.20:8000"))
	}()

	baseURL, ok, err := r.Endpoint(context.Background(), resolverTestModel)

	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "http://10.0.0.20:8000", baseURL)
}

// A launch slower than the budget still gets the retryable answer, and the
// budget is what bounds the hold.
func TestDynamicEndpointResolver_ColdStartWaitGivesUpAtTheBudget(t *testing.T) {
	svc := &fakeEndpointService{record: EndpointRecord{ModelID: resolverTestModel, State: StateAbsent}}
	r := NewDynamicEndpointResolver(svc, nil, 0, WithColdStartWait(600*time.Millisecond))

	started := time.Now()
	_, ok, err := r.Endpoint(context.Background(), resolverTestModel)

	require.NoError(t, err)
	assert.False(t, ok)
	assert.GreaterOrEqual(t, time.Since(started), 500*time.Millisecond)
	assert.Less(t, time.Since(started), 3*time.Second)
}

// A client that disconnects mid-wait releases the goroutine without turning
// its own departure into a server error; the launch it triggered carries on.
func TestDynamicEndpointResolver_ColdStartWaitStopsOnClientCancel(t *testing.T) {
	svc := &fakeEndpointService{record: EndpointRecord{ModelID: resolverTestModel, State: StateAbsent}}
	r := NewDynamicEndpointResolver(svc, nil, 0, WithColdStartWait(time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, ok, err := r.Endpoint(ctx, resolverTestModel)

	require.NoError(t, err)
	assert.False(t, ok)
	assert.Less(t, time.Since(started), 5*time.Second)
	assert.Equal(t, int64(1), svc.ensureCalls.Load())
}

// A burst of cold callers shares one wait, not one describe loop each.
func TestDynamicEndpointResolver_ColdStartWaitIsSharedAcrossCallers(t *testing.T) {
	svc := &fakeEndpointService{record: EndpointRecord{ModelID: resolverTestModel, State: StateAbsent}}
	r := NewDynamicEndpointResolver(svc, nil, 0, WithColdStartWait(3*time.Second))

	go func() {
		time.Sleep(600 * time.Millisecond)
		svc.setRecord(readyRecord("http://10.0.0.30:8000"))
	}()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			baseURL, ok, err := r.Endpoint(context.Background(), resolverTestModel)
			assert.NoError(t, err)
			assert.True(t, ok)
			assert.Equal(t, "http://10.0.0.30:8000", baseURL)
		})
	}
	wg.Wait()

	assert.Equal(t, int64(1), svc.ensureCalls.Load(), "one launch, however many callers waited on it")
	assert.Less(t, svc.describeCalls.Load(), int64(8), "the poll is shared, not repeated per caller")
}

// DRAINING has no launch to wait for: nothing relaunches on its own, so the
// answer is immediate even with a wait configured.
func TestDynamicEndpointResolver_ColdStartWaitSkipsDraining(t *testing.T) {
	svc := &fakeEndpointService{record: EndpointRecord{ModelID: resolverTestModel, State: StateDraining}}
	r := NewDynamicEndpointResolver(svc, nil, 0, WithColdStartWait(time.Minute))

	started := time.Now()
	_, ok, err := r.Endpoint(context.Background(), resolverTestModel)

	require.NoError(t, err)
	assert.False(t, ok)
	assert.Less(t, time.Since(started), time.Second)
}

const ptResolverAccount = "000000000001"

// TestDynamicEndpointResolver_EndpointForAccount_KeysOnPassedAccount is the
// account-aware path's whole point: a ready PT endpoint must be described
// under the caller-supplied account, not the GlobalAccountID shorthand
// Endpoint uses.
func TestDynamicEndpointResolver_EndpointForAccount_KeysOnPassedAccount(t *testing.T) {
	svc := &fakeEndpointService{record: readyRecord("http://10.0.0.9:9000")}
	r := NewDynamicEndpointResolver(svc, nil, time.Minute)

	baseURL, ok, err := r.EndpointForAccount(context.Background(), ptResolverAccount, resolverTestModel)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "http://10.0.0.9:9000", baseURL)
	assert.Equal(t, ptResolverAccount, svc.describeAccount())
}

// TestDynamicEndpointResolver_EndpointForAccount_AbsentEnsuresPinned covers
// the launch path: an absent PT endpoint is (re-)requested under the passed
// account with Pinned:true, mirroring how the commitment was created —
// not the GlobalAccountID/Pinned:false shape Endpoint's own Ensure uses.
func TestDynamicEndpointResolver_EndpointForAccount_AbsentEnsuresPinned(t *testing.T) {
	svc := &fakeEndpointService{record: EndpointRecord{ModelID: resolverTestModel, State: StateAbsent}}
	r := NewDynamicEndpointResolver(svc, nil, 0)

	_, ok, err := r.EndpointForAccount(context.Background(), ptResolverAccount, resolverTestModel)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, int64(1), svc.ensureCalls.Load())
	assert.Equal(t, ptResolverAccount, svc.ensureAccount())
	assert.True(t, svc.ensurePinned(), "a PT (re-)ensure must stay pinned")
}

// TestDynamicEndpointResolver_EndpointForAccount_DoesNotShareTheGlobalCache
// guards against the cross-account leak the account-aware path exists to
// avoid: caching its answer under Endpoint's shared, modelID-only cache would
// serve a pinned endpoint's address to an unrelated bare-modelId caller.
func TestDynamicEndpointResolver_EndpointForAccount_DoesNotShareTheGlobalCache(t *testing.T) {
	svc := &fakeEndpointService{record: readyRecord("http://10.0.0.9:9000")}
	r := NewDynamicEndpointResolver(svc, nil, time.Minute)

	_, ok, err := r.EndpointForAccount(context.Background(), ptResolverAccount, resolverTestModel)
	require.NoError(t, err)
	require.True(t, ok)

	baseURL, ok, err := r.Endpoint(context.Background(), resolverTestModel)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "http://10.0.0.9:9000", baseURL)
	assert.Equal(t, int64(2), svc.describeCalls.Load(),
		"the account-scoped resolve must not have pre-populated Endpoint's cache")
}
