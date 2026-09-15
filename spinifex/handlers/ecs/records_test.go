package handlers_ecs

//test:in-package — these decode persisted records straight into the unexported
// record types and call their unexported toAWS projection, which is the whole
// point: the assertion is that an older KV entry still reads.

import (
	"encoding/json"
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
