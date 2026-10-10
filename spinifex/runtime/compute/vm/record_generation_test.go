package vm_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/resource"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bookkeptRecord sets every resource.Metadata field, so a conversion that
// drops one shows up as a zero on the far side.
func bookkeptRecord() *vm.InstanceRecord {
	deleted := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	return &vm.InstanceRecord{
		Metadata: resource.Metadata{
			Name:               "i-0123456789abcdef0",
			UID:                "uid-1",
			AccountID:          "111122223333",
			Region:             "ap-southeast-2",
			Generation:         7,
			ObservedGeneration: 6,
			OwnerRefs:          []resource.OwnerRef{{Kind: "cluster", Name: "c-1"}},
			Finalizers:         []string{"eni"},
			Tags:               map[string]string{"Name": "web"},
			DeletionTimestamp:  &deleted,
		},
		Spec:   vm.InstanceSpec{InstanceType: "t3.micro", DesiredState: vm.DesiredRunning},
		Status: vm.InstanceStatus{Status: vm.StateRunning, LastNode: "node-1"},
	}
}

// Characterises current behaviour, not the ADR-0007 target: vm.VM has no
// generation or bookkeeping fields, so every path that rebuilds a record from a
// VM writes them back as zero.
func TestCurrentBehaviour_RecordThroughVMLosesGenerationAndBookkeeping(t *testing.T) {
	original := bookkeptRecord()

	rebuilt := vm.VMFromRecord(original).Record()

	// Preserved: the three metadata fields VM carries.
	assert.Equal(t, original.Metadata.Name, rebuilt.Metadata.Name)
	assert.Equal(t, original.Metadata.AccountID, rebuilt.Metadata.AccountID)
	assert.Equal(t, original.Metadata.DeletionTimestamp, rebuilt.Metadata.DeletionTimestamp)

	// Lost: everything else in the envelope.
	assert.Zero(t, rebuilt.Metadata.Generation, "LOST: Generation does not survive VM")
	assert.Zero(t, rebuilt.Metadata.ObservedGeneration, "LOST: ObservedGeneration does not survive VM")
	assert.Empty(t, rebuilt.Metadata.UID, "LOST: UID")
	assert.Empty(t, rebuilt.Metadata.Region, "LOST: Region")
	assert.Nil(t, rebuilt.Metadata.OwnerRefs, "LOST: OwnerRefs")
	assert.Nil(t, rebuilt.Metadata.Finalizers, "LOST: Finalizers")
	assert.Nil(t, rebuilt.Metadata.Tags, "LOST: Metadata.Tags")
}

// Characterises current behaviour: VM.Record() never stamps a generation, so
// the wire form of any record built from a VM omits both keys.
func TestCurrentBehaviour_RecordFromVMHasNoGenerationKeys(t *testing.T) {
	out, err := json.Marshal((&vm.VM{ID: "i-1", Status: vm.StateRunning}).Record())
	require.NoError(t, err)

	var raw struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(out, &raw))
	assert.NotContains(t, raw.Metadata, "generation")
	assert.NotContains(t, raw.Metadata, "observed_generation")
}

// Characterises current behaviour: the typed record decode ignores JSON it does
// not know, so decode-then-encode by this binary silently drops a field a newer
// binary added, at every nesting level.
func TestCurrentBehaviour_TypedRecordRoundTripDropsUnknownFields(t *testing.T) {
	newer := []byte(`{"metadata":{"name":"i-1","generation":4,"assignment_fence":"f-9"},` +
		`"spec":{"instance_type":"t3.micro","desired_generation":4},` +
		`"status":{"status":"running","last_node":"node-1","observed_assignment":3},` +
		`"assignment":{"node":"node-1","generation":3}}`)

	var decoded vm.InstanceRecord
	require.NoError(t, json.Unmarshal(newer, &decoded), "unknown fields are accepted, not rejected")

	reencoded, err := json.Marshal(&decoded)
	require.NoError(t, err)

	var raw map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(reencoded, &raw))
	assert.NotContains(t, raw, "assignment", "LOST: unknown top-level object")
	assert.NotContains(t, raw["metadata"], "assignment_fence", "LOST: unknown metadata field")
	assert.NotContains(t, raw["spec"], "desired_generation", "LOST: unknown spec field")
	assert.NotContains(t, raw["status"], "observed_assignment", "LOST: unknown status field")
	assert.JSONEq(t, `4`, string(raw["metadata"]["generation"]), "a known field survives the typed round trip")
}
