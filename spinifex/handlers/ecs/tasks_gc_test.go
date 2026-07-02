package handlers_ecs

import (
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
		require.NoError(t, putJSON(kv, TaskKey("web", id), &rec))
	}

	put("stale", TaskStatusStopped, now.Add(-2*time.Hour))   // older than retention -> prune
	put("fresh", TaskStatusStopped, now.Add(-5*time.Minute)) // within retention -> keep
	put("running", TaskStatusRunning, time.Time{})           // not stopped -> keep
	put("nostamp", TaskStatusStopped, time.Time{})           // STOPPED but no timestamp -> keep (defensive)

	pruned, err := svc.sweepStoppedBucket(kv, now, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, pruned)

	exists := func(id string) bool {
		var rec TaskRecord
		found, gerr := getJSON(kv, TaskKey("web", id), &rec)
		require.NoError(t, gerr)
		return found
	}
	assert.False(t, exists("stale"), "stale STOPPED task should be pruned")
	assert.True(t, exists("fresh"), "recently STOPPED task should survive")
	assert.True(t, exists("running"), "RUNNING task should survive")
	assert.True(t, exists("nostamp"), "STOPPED task with no StoppedAt should survive")
}
