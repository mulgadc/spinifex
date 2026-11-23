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

// recordWaits stands in for the sleep between attempts.
func recordWaits(waits *[]time.Duration) func(time.Duration) {
	return func(d time.Duration) { *waits = append(*waits, d) }
}

// One slow JetStream answer during a coordinated restart must not disable
// account deletion for the life of the process.
func TestCleanupBuildersRetryAStartupFailure(t *testing.T) {
	t.Parallel()

	t.Run("platform", func(t *testing.T) {
		t.Parallel()
		var waits []time.Duration
		calls := 0
		d := withPlatformCleaner(operatoradmin.Deps{}, recordWaits(&waits), func() (accountlifecycle.PlatformCleaner, error) {
			calls++
			if calls == 1 {
				return nil, errJetStreamTimeout
			}
			return fixedCleaner{}, nil
		})
		assert.NotNil(t, d.PlatformCleaner)
		assert.Equal(t, 2, calls)
		assert.Equal(t, []time.Duration{time.Second}, waits)
	})

	t.Run("compute and storage", func(t *testing.T) {
		t.Parallel()
		var waits []time.Duration
		calls := 0
		d := withComputeCleaners(operatoradmin.Deps{}, recordWaits(&waits), func() (accountlifecycle.ComputeStage, accountlifecycle.StorageStage, error) {
			calls++
			if calls == 1 {
				return nil, nil, errJetStreamTimeout
			}
			return stubCompute{}, stubStorage{}, nil
		})
		assert.NotNil(t, d.ComputeCleaner)
		assert.NotNil(t, d.StorageCleaner)
		assert.Equal(t, 2, calls)
		assert.Equal(t, []time.Duration{time.Second}, waits)
	})
}

// A build that never succeeds gives up after five attempts and leaves the
// fields nil, so a real DeleteAccount is still refused rather than half-run.
func TestCleanupBuildersGiveUpAfterFiveAttempts(t *testing.T) {
	t.Parallel()
	backoff := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

	var platformWaits, computeWaits []time.Duration
	platformCalls, computeCalls := 0, 0
	d := operatoradmin.Deps{Termination: stubTermination{}}
	d = withPlatformCleaner(d, recordWaits(&platformWaits), func() (accountlifecycle.PlatformCleaner, error) {
		platformCalls++
		return nil, errJetStreamTimeout
	})
	d = withComputeCleaners(d, recordWaits(&computeWaits), func() (accountlifecycle.ComputeStage, accountlifecycle.StorageStage, error) {
		computeCalls++
		return nil, nil, errJetStreamTimeout
	})

	assert.Equal(t, 5, platformCalls)
	assert.Equal(t, 5, computeCalls)
	assert.Equal(t, backoff, platformWaits)
	assert.Equal(t, backoff, computeWaits)
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
