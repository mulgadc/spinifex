package handlers_ecs

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPollAssignments_DrainsStopsAndAcks covers the agent poll path: a poll
// returns both the pending assignments and the stop directives for the instance,
// and acking their IDs on a follow-up poll deletes the inbox entries so nothing is
// re-delivered. Empty ack IDs are skipped.
func TestPollAssignments_DrainsStopsAndAcks(t *testing.T) {
	svc, _, kv := serviceTestRig(t)

	require.NoError(t, putJSON(kv, AssignmentKey("web", "i-1", "t-1"),
		&bus.Assign{TaskID: "t-1", ClusterName: "web", InstanceID: "i-1"}))
	require.NoError(t, putJSON(kv, StopKey("web", "i-1", "t-2"),
		&bus.StopDirective{TaskID: "t-2", Reason: "drain"}))

	out, err := svc.PollAssignments(&PollAssignmentsInput{
		Cluster: "web", ContainerInstance: "i-1",
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Assignments, 1)
	assert.Equal(t, "t-1", out.Assignments[0].TaskID)
	require.Len(t, out.Stops, 1)
	assert.Equal(t, "t-2", out.Stops[0].TaskID)
	assert.Equal(t, "drain", out.Stops[0].Reason)

	// Ack both (with a blank ID that must be skipped) → entries removed, next poll
	// drains empty.
	out, err = svc.PollAssignments(&PollAssignmentsInput{
		Cluster: "web", ContainerInstance: "i-1",
		AckTaskIDs: []string{"t-1", ""}, AckStopIDs: []string{"t-2", ""},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Assignments)
	assert.Empty(t, out.Stops)

	found, err := getJSON(kv, StopKey("web", "i-1", "t-2"), &bus.StopDirective{})
	require.NoError(t, err)
	assert.False(t, found)
}
