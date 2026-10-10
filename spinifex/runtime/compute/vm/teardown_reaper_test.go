//test:in-package — drives the reaper's unexported per-dependent backoff state
//and reuses the in-package cleaner/state-store doubles.

package vm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTeardownReaperBacksOffARepeatedlyFailingDependent covers the case that
// took the cluster down: a volume whose metadata document cannot be read fails
// every re-drive, and an ungated reaper spent a 20s object-store request on it
// on every sweep. The dependent must stay Failed (never dropped) but must not
// be retried again on the very next sweep.
func TestTeardownReaperBacksOffARepeatedlyFailingDependent(t *testing.T) {
	m, cleaner, store, reaper, v := seedTerminated(t, map[string]string{
		TeardownVolumes: string(TeardownPending),
	})
	_ = m
	cleaner.deleteVolumesErr = errors.New("shard sizes do not match")

	_, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{v.ID}, cleaner.deleteVolumes, "the first sweep must attempt the dependent")
	assert.Equal(t, string(TeardownFailed), v.Teardown[TeardownVolumes],
		"a failed retry must stay Failed, not be dropped")

	_, err = reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{v.ID}, cleaner.deleteVolumes,
		"the next sweep must skip a dependent that is still backed off")
	assert.Equal(t, string(TeardownFailed), v.Teardown[TeardownVolumes],
		"a backed-off dependent stays Failed and its record stays in the store")

	remaining, err := store.ListTerminatedInstances()
	require.NoError(t, err)
	require.Len(t, remaining, 1, "a record with an outstanding dependent must not be purged")
}

// TestTeardownReaperBackoffGrowsAndClearsOnSuccess checks the interval doubles
// per failure and that a dependent which eventually succeeds leaves no backoff
// state behind.
func TestTeardownReaperBackoffGrowsAndClearsOnSuccess(t *testing.T) {
	_, cleaner, _, reaper, v := seedTerminated(t, map[string]string{
		TeardownVolumes: string(TeardownPending),
	})
	cleaner.deleteVolumesErr = errors.New("shard sizes do not match")

	key := v.ID + "/" + TeardownVolumes
	for i := 1; i <= 3; i++ {
		expire(reaper, key)
		_, err := reaper.Sweep(context.Background())
		require.NoError(t, err)
		require.Len(t, cleaner.deleteVolumes, i)

		b := reaper.backoff[key]
		require.NotNil(t, b)
		assert.Equal(t, i, b.failures)
		assert.WithinDuration(t, time.Now().Add(teardownRetryBase<<(i-1)), b.next, time.Minute)
	}

	cleaner.deleteVolumesErr = nil
	expire(reaper, key)
	_, err := reaper.Sweep(context.Background())
	require.NoError(t, err)
	assert.Len(t, cleaner.deleteVolumes, 4)
	assert.Equal(t, string(TeardownDone), v.Teardown[TeardownVolumes])
	assert.Empty(t, reaper.backoff, "a dependent that succeeded must leave no backoff state")
}

// TestTeardownReaperCapsTheBackoffInterval keeps the doubling from running away
// past the cap (or overflowing into a negative duration).
func TestTeardownReaperCapsTheBackoffInterval(t *testing.T) {
	_, _, _, reaper, v := seedTerminated(t, nil)

	for range 80 {
		reaper.recordRetry(v.ID, TeardownVolumes, TeardownFailed)
	}
	b := reaper.backoff[v.ID+"/"+TeardownVolumes]
	require.NotNil(t, b)
	assert.WithinDuration(t, time.Now().Add(teardownRetryMax), b.next, time.Minute)
	assert.True(t, reaper.dueForRetry(v.ID, "some-other-dependent"),
		"backoff is per dependent, not per instance")
}

// expire brings a backoff entry's next-retry time forward so a test can take
// the following sweep without waiting for the real interval.
func expire(r *TerminatedTeardownReaper, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b := r.backoff[key]; b != nil {
		b.next = time.Now().Add(-time.Second)
	}
}
