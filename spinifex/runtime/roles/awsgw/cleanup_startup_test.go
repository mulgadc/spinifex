package awsgw

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Never called: they only make a cleanup field non-nil.
type (
	stubCompute     struct{ accountlifecycle.ComputeStage }
	stubStorage     struct{ accountlifecycle.StorageStage }
	stubTermination struct{ accountlifecycle.IAMBackend }
)

var errJetStreamTimeout = errors.New("open KV bucket spinifex-spot-requests-terminal: context deadline exceeded")

// fakeStartup is a startup clock: waits and builds advance it, never sleep.
type fakeStartup struct {
	elapsed time.Duration
	waits   []time.Duration
}

func (f *fakeStartup) retry() *startupRetry {
	start := time.Unix(0, 0)
	return newStartupRetry(func() time.Time { return start.Add(f.elapsed) }, func(d time.Duration) {
		f.waits = append(f.waits, d)
		f.elapsed += d
	})
}

// One slow JetStream answer during a coordinated restart must not disable
// account deletion for the life of the process.
func TestCleanupBuildersRetryAStartupFailure(t *testing.T) {
	t.Parallel()

	t.Run("platform", func(t *testing.T) {
		t.Parallel()
		clock := &fakeStartup{}
		calls := 0
		d := withPlatformCleaner(operatoradmin.Deps{}, clock.retry(), func() (accountlifecycle.PlatformCleaner, error) {
			calls++
			if calls == 1 {
				return nil, errJetStreamTimeout
			}
			return fixedCleaner{}, nil
		})
		assert.NotNil(t, d.PlatformCleaner)
		assert.Equal(t, 2, calls)
		assert.Equal(t, []time.Duration{time.Second}, clock.waits)
	})

	t.Run("compute and storage", func(t *testing.T) {
		t.Parallel()
		clock := &fakeStartup{}
		calls := 0
		d := withComputeCleaners(operatoradmin.Deps{}, clock.retry(), func() (accountlifecycle.ComputeStage, accountlifecycle.StorageStage, error) {
			calls++
			if calls == 1 {
				return nil, nil, errJetStreamTimeout
			}
			return stubCompute{}, stubStorage{}, nil
		})
		assert.NotNil(t, d.ComputeCleaner)
		assert.NotNil(t, d.StorageCleaner)
		assert.Equal(t, 2, calls)
		assert.Equal(t, []time.Duration{time.Second}, clock.waits)
	})
}

// Builds that never succeed share one budget: the gateway starts within it,
// both fields stay nil, and a real DeleteAccount is still refused, not half-run.
func TestCleanupBuildersGiveUpWithinTheStartupBudget(t *testing.T) {
	t.Parallel()
	clock := &fakeStartup{}
	retry := clock.retry()

	platformCalls, computeCalls := 0, 0
	d := operatoradmin.Deps{Termination: stubTermination{}}
	d = withPlatformCleaner(d, retry, func() (accountlifecycle.PlatformCleaner, error) {
		platformCalls++
		return nil, errJetStreamTimeout
	})
	d = withComputeCleaners(d, retry, func() (accountlifecycle.ComputeStage, accountlifecycle.StorageStage, error) {
		computeCalls++
		return nil, nil, errJetStreamTimeout
	})

	// Each build fails at once, so each gets the retries that fit before the
	// deadline: the platform build's leave room for the compute build's.
	assert.Equal(t, 4, platformCalls)
	assert.Equal(t, 4, computeCalls)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, time.Second, 2 * time.Second, 4 * time.Second}, clock.waits)
	assert.LessOrEqual(t, clock.elapsed, cleanupRetryBudget)
	assert.Nil(t, d.PlatformCleaner)
	assert.Nil(t, d.ComputeCleaner)
	assert.Nil(t, d.StorageCleaner)

	body, err := json.Marshal(operatorv1.DeleteAccountRequest{
		AccountID: "000000000002", AccountName: "tenant@example.com", ClientToken: "8b9c1d2e3f4a5b6c7d8e9f0a1b2c3d4e",
	})
	require.NoError(t, err)
	_, err = d.RunMethod(t.Context(), "DeleteAccount", dispatch.Actor{}, body)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)
}

// A build that is slow to fail spends the budget too, so a JetStream that
// times out every call cannot hold the gateway's start past it.
func TestCleanupBuildTimeCountsAgainstTheStartupBudget(t *testing.T) {
	t.Parallel()
	clock := &fakeStartup{}
	calls := 0
	d := withPlatformCleaner(operatoradmin.Deps{}, clock.retry(), func() (accountlifecycle.PlatformCleaner, error) {
		calls++
		clock.elapsed += 5 * time.Second
		return nil, errJetStreamTimeout
	})

	assert.Nil(t, d.PlatformCleaner)
	assert.Equal(t, 3, calls)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second}, clock.waits)
	assert.LessOrEqual(t, clock.elapsed, cleanupRetryBudget+5*time.Second, "at most one build in flight past the budget")
}

// Once the budget is spent no further build starts, so a build that ran past
// the deadline is the only one that can.
func TestNoCleanupBuildStartsOnceTheBudgetIsSpent(t *testing.T) {
	t.Parallel()
	clock := &fakeStartup{}
	retry := clock.retry()
	d := withPlatformCleaner(operatoradmin.Deps{}, retry, func() (accountlifecycle.PlatformCleaner, error) {
		clock.elapsed += cleanupRetryBudget + time.Second
		return nil, errJetStreamTimeout
	})
	computeCalls := 0
	d = withComputeCleaners(d, retry, func() (accountlifecycle.ComputeStage, accountlifecycle.StorageStage, error) {
		computeCalls++
		return stubCompute{}, stubStorage{}, nil
	})

	assert.Nil(t, d.PlatformCleaner)
	assert.Zero(t, computeCalls)
	assert.Nil(t, d.ComputeCleaner)
}
