package handlers_ecs

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSweepStoppedBucket_PrunesOnlyStale(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	now := time.Now().UTC()

	put := func(id, status string, stoppedAt time.Time) {
		rec := TaskRecord{
			TaskID: id, Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", id),
			LastStatus: status, DesiredStatus: status, StoppedAt: stoppedAt,
		}
		require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", id), &rec))
	}

	put("stale", TaskStatusStopped, now.Add(-2*time.Hour))   // older than retention -> prune
	put("fresh", TaskStatusStopped, now.Add(-5*time.Minute)) // within retention -> keep
	put("running", TaskStatusRunning, time.Time{})           // not stopped -> keep
	put("nostamp", TaskStatusStopped, time.Time{})           // STOPPED but no timestamp -> keep (defensive)

	pruned, due, err := svc.sweepStoppedBucket(t.Context(), kv, testAccountID, now, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, pruned)
	// "fresh" is the only survivor with a clock on it, so the sweep must come back
	// when its retention expires, not on the resync.
	assert.Equal(t, 55*time.Minute, due)

	exists := func(id string) bool {
		var rec TaskRecord
		found, gerr := getJSON(t.Context(), kv, TaskKey("web", id), &rec)
		require.NoError(t, gerr)
		return found
	}
	assert.False(t, exists("stale"), "stale STOPPED task should be pruned")
	assert.True(t, exists("fresh"), "recently STOPPED task should survive")
	assert.True(t, exists("running"), "RUNNING task should survive")
	assert.True(t, exists("nostamp"), "STOPPED task with no StoppedAt should survive")
}

func TestSweepStoppedBucket_RetriesOwedENI_Succeeds(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	eni := &stubENI{}
	svc.eni = eni
	now := time.Now().UTC()

	rec := TaskRecord{
		TaskID: "t-1", Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", "t-1"),
		LastStatus: TaskStatusStopped, DesiredStatus: TaskStatusStopped,
		NetworkMode: NetworkModeAwsvpc, ENIID: "eni-1", ENIAttachmentID: "att-1",
		StoppedAt: now.Add(-2 * time.Hour), // well past retention
	}
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "t-1"), &rec))

	pruned, due, err := svc.sweepStoppedBucket(t.Context(), kv, testAccountID, now, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, pruned, "an owed ENI is never pruned, however stale")
	assert.Zero(t, due)
	assert.Equal(t, 1, eni.releaseCalls)

	var got TaskRecord
	found, gerr := getJSON(t.Context(), kv, TaskKey("web", "t-1"), &got)
	require.NoError(t, gerr)
	require.True(t, found)
	assert.Equal(t, "eni-1", got.ENIID, "the identity survives release as the forensic record")
	assert.True(t, got.ENIReleased, "a successful retry marks the task released")
	assert.Equal(t, 0, got.ENIReleaseAttempts)
	assert.True(t, got.ENIReleaseNextTry.IsZero())
}

func TestSweepStoppedBucket_RetryFails_BacksOff(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	eni := &stubENI{releaseErr: errors.New("nats timeout")}
	svc.eni = eni
	now := time.Now().UTC()

	rec := TaskRecord{
		TaskID: "t-1", Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", "t-1"),
		LastStatus: TaskStatusStopped, DesiredStatus: TaskStatusStopped,
		NetworkMode: NetworkModeAwsvpc, ENIID: "eni-1",
		StoppedAt: now.Add(-2 * time.Hour),
	}
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "t-1"), &rec))

	pruned, due, err := svc.sweepStoppedBucket(t.Context(), kv, testAccountID, now, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, pruned)
	assert.Equal(t, eniReleaseBackoffBase, due)

	var got TaskRecord
	found, gerr := getJSON(t.Context(), kv, TaskKey("web", "t-1"), &got)
	require.NoError(t, gerr)
	require.True(t, found)
	assert.Equal(t, "eni-1", got.ENIID, "a failed retry is still owed")
	assert.Equal(t, 1, got.ENIReleaseAttempts)
	assert.WithinDuration(t, now.Add(eniReleaseBackoffBase), got.ENIReleaseNextTry, time.Second)
}

func TestSweepStoppedBucket_RetryNotDueYet_Skipped(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	eni := &stubENI{}
	svc.eni = eni
	now := time.Now().UTC()

	rec := TaskRecord{
		TaskID: "t-1", Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", "t-1"),
		LastStatus: TaskStatusStopped, DesiredStatus: TaskStatusStopped,
		NetworkMode:       NetworkModeAwsvpc,
		ENIID:             "eni-1",
		StoppedAt:         now.Add(-2 * time.Hour),
		ENIReleaseNextTry: now.Add(10 * time.Minute),
	}
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "t-1"), &rec))

	pruned, due, err := svc.sweepStoppedBucket(t.Context(), kv, testAccountID, now, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, pruned)
	assert.Equal(t, 0, eni.releaseCalls, "the next try has not arrived yet")
	assert.Equal(t, 10*time.Minute, due)
}

func TestSweepStoppedBucket_HonoursPerPassCap(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	eni := &stubENI{releaseErr: errors.New("nats timeout")}
	svc.eni = eni
	now := time.Now().UTC()

	total := eniReleaseRetriesPerPass + 2
	for i := range total {
		id := fmt.Sprintf("t-%d", i)
		rec := TaskRecord{
			TaskID: id, Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", id),
			LastStatus: TaskStatusStopped, DesiredStatus: TaskStatusStopped,
			NetworkMode: NetworkModeAwsvpc, ENIID: "eni-" + id,
			StoppedAt: now.Add(-2 * time.Hour),
		}
		require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", id), &rec))
	}

	_, due, err := svc.sweepStoppedBucket(t.Context(), kv, testAccountID, now, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, eniReleaseRetriesPerPass, eni.releaseCalls,
		"a bucket full of due retries must not stall the sweep past its per-pass cap")
	// The retried records back off to eniReleaseBackoffBase, which beats the
	// sweepInterval fallback the capped-out overflow records ask for; either
	// way the sweep must learn a deadline rather than falling through to none.
	assert.Equal(t, eniReleaseBackoffBase, due)
}

// TestSweepStoppedBucket_CappedOverflow_AsksForSweepInterval isolates the
// capped-out branch: every retry this pass succeeds (so none re-arms its own
// backoff), leaving the overflow records as the only source of a deadline.
func TestSweepStoppedBucket_CappedOverflow_AsksForSweepInterval(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	eni := &stubENI{}
	svc.eni = eni
	now := time.Now().UTC()

	total := eniReleaseRetriesPerPass + 2
	for i := range total {
		id := fmt.Sprintf("t-%d", i)
		rec := TaskRecord{
			TaskID: id, Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", id),
			LastStatus: TaskStatusStopped, DesiredStatus: TaskStatusStopped,
			NetworkMode: NetworkModeAwsvpc, ENIID: "eni-" + id,
			StoppedAt: now.Add(-2 * time.Hour),
		}
		require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", id), &rec))
	}

	_, due, err := svc.sweepStoppedBucket(t.Context(), kv, testAccountID, now, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, eniReleaseRetriesPerPass, eni.releaseCalls)
	assert.Equal(t, sweepInterval, due,
		"the 2 records left over from the cap must still bring the sweep back soon")
}

// TestSweepStoppedBucket_PrunesReleased_KeepsOwed pins the post-fix contract: a
// released ENI is no longer "owed" and is pruned like any other stale STOPPED
// record, while an unreleased one is refused however far past retention it is.
func TestSweepStoppedBucket_PrunesReleased_KeepsOwed(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	now := time.Now().UTC()

	released := TaskRecord{
		TaskID: "released", Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", "released"),
		LastStatus: TaskStatusStopped, DesiredStatus: TaskStatusStopped,
		NetworkMode: NetworkModeAwsvpc, ENIID: "eni-released", ENIReleased: true,
		StoppedAt: now.Add(-2 * time.Hour), // well past retention
	}
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "released"), &released))

	owed := TaskRecord{
		TaskID: "owed", Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", "owed"),
		LastStatus: TaskStatusStopped, DesiredStatus: TaskStatusStopped,
		NetworkMode: NetworkModeAwsvpc, ENIID: "eni-owed", ENIReleased: false,
		StoppedAt: now.Add(-2 * time.Hour), // also well past retention, but still owed
	}
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "owed"), &owed))

	pruned, _, err := svc.sweepStoppedBucket(t.Context(), kv, testAccountID, now, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, pruned)

	exists := func(id string) bool {
		var rec TaskRecord
		found, gerr := getJSON(t.Context(), kv, TaskKey("web", id), &rec)
		require.NoError(t, gerr)
		return found
	}
	assert.False(t, exists("released"), "a released ENI is no longer owed, so retention prunes it")
	assert.True(t, exists("owed"), "an unreleased ENI is never pruned, however stale")
}

func TestEniReleaseBackoff_DoublesThenCaps(t *testing.T) {
	assert.Equal(t, eniReleaseBackoffBase, eniReleaseBackoff(1))
	assert.Equal(t, 2*eniReleaseBackoffBase, eniReleaseBackoff(2))
	assert.Equal(t, 4*eniReleaseBackoffBase, eniReleaseBackoff(3))
	assert.Equal(t, eniReleaseBackoffMax, eniReleaseBackoff(100), "backoff must never exceed the cap")
}
