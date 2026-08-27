package vm

import (
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A record written before DesiredState existed carries the operator stop in the
// command it was stamped with. Losing that on upgrade would relaunch an
// instance the operator deliberately stopped.
func TestVMUnmarshal_LegacyOperatorStopFolds(t *testing.T) {
	var v VM
	require.NoError(t, json.Unmarshal([]byte(
		`{"id":"i-1","status":"stopped","attributes":{"stop_instance":true}}`), &v))

	assert.Equal(t, DesiredStopped, v.DesiredState)
	assert.Nil(t, v.LegacyAttributes, "the legacy field is read once and dropped")
}

// A drain stop never stamped StopInstance, so a legacy drain-stopped record
// must stay desired-running and be relaunched.
func TestVMUnmarshal_LegacyDrainStopStaysRunning(t *testing.T) {
	var v VM
	require.NoError(t, json.Unmarshal([]byte(
		`{"id":"i-1","status":"stopped","attributes":{"stop_instance":false}}`), &v))

	assert.Equal(t, DesiredRunning, v.DesiredState)
}

// Only StopInstance folds. TerminateInstance was a launch-race latch, not a
// persisted intent, and a terminated record is already settled by Status.
func TestVMUnmarshal_LegacyTerminateDoesNotFold(t *testing.T) {
	var v VM
	require.NoError(t, json.Unmarshal([]byte(
		`{"id":"i-1","status":"terminated","attributes":{"delete_instance":true}}`), &v))

	assert.Equal(t, DesiredRunning, v.DesiredState)
	assert.Nil(t, v.DeletionTimestamp)
}

// A record carrying both must take the new field: the legacy one is only a
// fallback for records written before it existed.
func TestVMUnmarshal_ExplicitDesiredStateWins(t *testing.T) {
	var v VM
	require.NoError(t, json.Unmarshal([]byte(
		`{"id":"i-1","desired_state":"stopped","attributes":{"stop_instance":false}}`), &v))

	assert.Equal(t, DesiredStopped, v.DesiredState)
}

// The point of the change is that the command stops being persisted at all.
// Round-tripping a legacy record must emit desired_state and no attributes.
func TestVMMarshal_LegacyAttributesNotWrittenBack(t *testing.T) {
	var v VM
	require.NoError(t, json.Unmarshal([]byte(
		`{"id":"i-1","status":"stopped","attributes":{"stop_instance":true}}`), &v))

	out, err := json.Marshal(&v)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &raw))
	assert.NotContains(t, raw, "attributes", "the command must not be written back")
	assert.Contains(t, raw, "desired_state")
}

// The other twelve command booleans had nowhere to go because nothing read them
// back. A record must carry none of them, so they are gone rather than unread.
func TestVMMarshal_CommandBooleansAreNotPersisted(t *testing.T) {
	legacy, err := json.Marshal(map[string]any{
		"id": "i-1", "status": StateRunning,
		"attributes": types.EC2CommandAttributes{
			AttachVolume:                true,
			DetachVolume:                true,
			DrainVolume:                 true,
			StartInstance:               true,
			RebootInstance:              true,
			AttachENI:                   true,
			DetachENI:                   true,
			AssociateIamInstanceProfile: true,
			SetSpotLineage:              true,
			SetInstanceTags:             true,
			RemoveInstanceTags:          true,
			SetInstanceMonitoring:       true,
		},
	})
	require.NoError(t, err)

	var v VM
	require.NoError(t, json.Unmarshal(legacy, &v))
	require.Nil(t, v.LegacyAttributes)

	out, err := json.Marshal(&v)
	require.NoError(t, err)

	for _, field := range []string{
		"attach_volume", "detach_volume", "drain_volume", "start_instance",
		"reboot_instance", "attach_eni", "detach_eni",
		"associate_iam_instance_profile", "set_spot_lineage", "set_instance_tags",
		"remove_instance_tags", "set_instance_monitoring",
	} {
		assert.NotContains(t, string(out), field,
			"command boolean %q must not reach the persisted record", field)
	}
}
