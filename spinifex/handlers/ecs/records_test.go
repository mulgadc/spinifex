package handlers_ecs

//test:in-package — these decode persisted records straight into the unexported
// record types and call their unexported toAWS projection, which is the whole
// point: the assertion is that an older KV entry still reads.

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTaskDefRecord_DecodesPreExistingJSON verifies that a TaskDefRecord
// persisted before portMappings[].name and runtimePlatform existed still
// decodes cleanly: every KV entry written by an older daemon build must stay
// readable without a migration.
func TestTaskDefRecord_DecodesPreExistingJSON(t *testing.T) {
	legacy := `{
		"family": "app",
		"revision": 1,
		"arn": "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/app:1",
		"status": "ACTIVE",
		"containers": [{
			"name": "app",
			"image": "registry/app:1",
			"essential": true,
			"portMappings": [{"containerPort": 80, "hostPort": 8080, "protocol": "tcp"}]
		}]
	}`
	var rec TaskDefRecord
	require.NoError(t, json.Unmarshal([]byte(legacy), &rec))
	assert.Nil(t, rec.RuntimePlatform)
	require.Len(t, rec.Containers, 1)
	require.Len(t, rec.Containers[0].PortMappings, 1)
	assert.Empty(t, rec.Containers[0].PortMappings[0].Name)

	// toAWS must not panic or invent a RuntimePlatform for the missing field.
	td := rec.toAWS()
	assert.Nil(t, td.RuntimePlatform)
	require.Len(t, td.ContainerDefinitions, 1)
	assert.Nil(t, td.ContainerDefinitions[0].PortMappings[0].Name)
}

// TestServiceRecord_DecodesPreExistingJSON verifies that a ServiceRecord
// persisted before enableEcsManagedTags existed still decodes cleanly, and
// defaults the new field to false rather than failing to unmarshal.
func TestServiceRecord_DecodesPreExistingJSON(t *testing.T) {
	legacy := `{
		"name": "web",
		"arn": "arn:aws:ecs:ap-southeast-2:123456789012:service/web/web",
		"cluster": "web",
		"status": "ACTIVE",
		"schedulingStrategy": "REPLICA",
		"deploymentId": "d-1"
	}`
	var rec ServiceRecord
	require.NoError(t, json.Unmarshal([]byte(legacy), &rec))
	assert.False(t, rec.EnableECSManagedTags)
	assert.Empty(t, rec.Subnets)
}

// TestAppendServiceEvent_NewestFirst verifies each append lands at index 0, so
// a caller (and the DescribeServices projection) can read the ring newest
// first without a separate sort or reverse step.
func TestAppendServiceEvent_NewestFirst(t *testing.T) {
	var rec ServiceRecord
	appendServiceEvent(&rec, "first")
	appendServiceEvent(&rec, "second")
	appendServiceEvent(&rec, "third")
	require.Len(t, rec.Events, 3)
	assert.Equal(t, "third", rec.Events[0].Message)
	assert.Equal(t, "second", rec.Events[1].Message)
	assert.Equal(t, "first", rec.Events[2].Message)
}

// TestAppendServiceEvent_CapsRingAndDropsOldest guards clause C of the plan:
// the ring is capped at serviceEventRingCap, discarding the oldest entries
// first, mirroring AWS's own roughly-100-event retention.
func TestAppendServiceEvent_CapsRingAndDropsOldest(t *testing.T) {
	var rec ServiceRecord
	for i := range serviceEventRingCap + 5 {
		appendServiceEvent(&rec, fmt.Sprintf("event %d", i))
	}
	require.Len(t, rec.Events, serviceEventRingCap)
	// Newest is the last one appended; oldest retained is the 5th (events 0-4
	// were pushed out once the ring exceeded its cap).
	assert.Equal(t, fmt.Sprintf("event %d", serviceEventRingCap+4), rec.Events[0].Message)
	assert.Equal(t, "event 5", rec.Events[serviceEventRingCap-1].Message)
}
