package resource_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/resource"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type instanceSpec struct {
	InstanceType string `json:"instance_type"`
	DesiredState string `json:"desired_state,omitempty"`
}

type instanceStatus struct {
	State  string `json:"state"`
	NodeID string `json:"node_id,omitempty"`
}

type instance = resource.Object[instanceSpec, instanceStatus]

func TestMutateSpec_BumpsGeneration(t *testing.T) {
	var o instance
	require.False(t, o.NeedsSync(), "a record nobody has changed is already in sync")

	o.MutateSpec(func(s *instanceSpec) { s.InstanceType = "t3.micro" })

	assert.Equal(t, "t3.micro", o.Spec.InstanceType)
	assert.EqualValues(t, 1, o.Metadata.Generation)
	assert.True(t, o.NeedsSync(), "a spec change the node has not acted on is out of sync")
}

// The node records the generation it acted on, not the current one: a spec
// change that lands mid-reconcile must leave the record out of sync rather than
// being marked done by a reconcile that never saw it.
func TestObserveGeneration_RecordsWhatTheNodeActedOn(t *testing.T) {
	var o instance
	o.MutateSpec(func(s *instanceSpec) { s.InstanceType = "t3.micro" })
	acted := o.Metadata.Generation

	o.MutateSpec(func(s *instanceSpec) { s.InstanceType = "t3.large" })
	o.ObserveGeneration(acted)

	assert.True(t, o.NeedsSync(), "the node acted on generation 1, the spec is at 2")

	o.ObserveGeneration(o.Metadata.Generation)
	assert.False(t, o.NeedsSync())
}

// Status is the node's half, so writing it must not make the record look like
// it has an unhandled spec change.
func TestStatusWrite_DoesNotBumpGeneration(t *testing.T) {
	var o instance
	o.MutateSpec(func(s *instanceSpec) { s.InstanceType = "t3.micro" })
	o.ObserveGeneration(o.Metadata.Generation)

	o.Status = instanceStatus{State: "running", NodeID: "node-1"}

	assert.False(t, o.NeedsSync(), "an observation is not a request")
}

func TestMarkForDeletion(t *testing.T) {
	var m resource.Metadata
	assert.False(t, m.MarkedForDeletion())

	first := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	require.True(t, m.MarkForDeletion(first))
	assert.True(t, m.MarkedForDeletion())

	assert.False(t, m.MarkForDeletion(first.Add(time.Hour)),
		"a second delete is accepted but changes nothing")
	assert.Equal(t, first, *m.DeletionTimestamp,
		"finalizer timeouts are measured from the first mark, so it must not move")
}

func TestMarkForDeletion_NormalisesToUTC(t *testing.T) {
	var m resource.Metadata
	zone := time.FixedZone("AEST", 10*60*60)
	local := time.Date(2026, 8, 28, 20, 0, 0, 0, zone)

	require.True(t, m.MarkForDeletion(local))
	assert.Equal(t, time.UTC, m.DeletionTimestamp.Location())
	assert.True(t, m.DeletionTimestamp.Equal(local))
}

func TestFinalizers(t *testing.T) {
	var m resource.Metadata

	assert.False(t, m.HasFinalizer("eni"))
	assert.False(t, m.RemoveFinalizer("eni"), "removing one that was never added reports nothing was done")

	require.True(t, m.AddFinalizer("eni"))
	assert.False(t, m.AddFinalizer("eni"), "registering twice must not queue two clears")
	require.True(t, m.AddFinalizer("volume"))
	assert.True(t, m.HasFinalizer("eni"))

	require.True(t, m.RemoveFinalizer("eni"))
	assert.False(t, m.HasFinalizer("eni"))
	assert.Equal(t, []string{"volume"}, m.Finalizers, "removing one must not disturb the others")

	require.True(t, m.RemoveFinalizer("volume"))
	assert.Nil(t, m.Finalizers, "an empty list is dropped so it does not serialise")
}

// A finalizer registered after the delete was accepted would hold open a
// deletion that had already been evaluated without it.
func TestAddFinalizer_RefusedAfterMarkedForDeletion(t *testing.T) {
	var m resource.Metadata
	require.True(t, m.MarkForDeletion(time.Now()))

	assert.False(t, m.AddFinalizer("eni"))
	assert.Empty(t, m.Finalizers)
}

func TestCollectable(t *testing.T) {
	var m resource.Metadata
	require.True(t, m.AddFinalizer("eni"))
	assert.False(t, m.Collectable(), "an unmarked resource is never collectable")

	require.True(t, m.MarkForDeletion(time.Now()))
	assert.False(t, m.Collectable(), "a marked resource waits for its finalizers")

	require.True(t, m.RemoveFinalizer("eni"))
	assert.True(t, m.Collectable())
}

func TestOwnerRefs(t *testing.T) {
	var m resource.Metadata

	_, ok := m.OwnerOfKind("instance")
	assert.False(t, ok)

	owner := resource.OwnerRef{Kind: "instance", Name: "i-1", UID: "uid-1"}
	require.True(t, m.AddOwnerRef(owner))

	got, ok := m.OwnerOfKind("instance")
	require.True(t, ok)
	assert.Equal(t, owner, got)

	assert.False(t, m.AddOwnerRef(resource.OwnerRef{Kind: "instance", Name: "i-2"}),
		"reparenting is not something a second add should do quietly")
	assert.Len(t, m.OwnerRefs, 1)

	require.True(t, m.AddOwnerRef(resource.OwnerRef{Kind: "cluster", Name: "c-1"}))
	assert.Len(t, m.OwnerRefs, 2, "a resource can be owned by one of each kind")
}

// UID is what stops an OwnerRef being satisfied by a replacement that reused
// the name.
func TestOwnerRef_UIDDistinguishesARecreatedOwner(t *testing.T) {
	var m resource.Metadata
	require.True(t, m.AddOwnerRef(resource.OwnerRef{Kind: "instance", Name: "i-1", UID: "uid-1"}))

	ref, ok := m.OwnerOfKind("instance")
	require.True(t, ok)
	live := resource.Metadata{Name: "i-1", UID: "uid-2"}

	assert.Equal(t, live.Name, ref.Name)
	assert.NotEqual(t, live.UID, ref.UID, "same name, different resource — the ref is stale")
}

// The envelope is persisted through kvstore, so an empty record must round-trip
// without inventing keys a reader would then have to tolerate.
func TestObjectJSON_OmitsEmptyBookkeeping(t *testing.T) {
	out, err := json.Marshal(instance{Metadata: resource.Metadata{Name: "i-1"}})
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &raw))
	assert.Equal(t, []string{"metadata", "spec", "status"}, sortedKeys(raw))

	var meta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["metadata"], &meta))
	assert.Equal(t, []string{"name"}, sortedKeys(meta))
}

func TestObjectJSON_RoundTrips(t *testing.T) {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	want := instance{
		Metadata: resource.Metadata{
			Name: "i-1", UID: "uid-1", AccountID: "111122223333", Region: "ap-southeast-2",
			Generation: 3, ObservedGeneration: 2,
			OwnerRefs:         []resource.OwnerRef{{Kind: "cluster", Name: "c-1", UID: "uid-c"}},
			Finalizers:        []string{"eni"},
			Tags:              map[string]string{"Name": "web"},
			DeletionTimestamp: &now,
		},
		Spec:   instanceSpec{InstanceType: "t3.micro", DesiredState: "stopped"},
		Status: instanceStatus{State: "stopping", NodeID: "node-1"},
	}

	out, err := json.Marshal(want)
	require.NoError(t, err)

	var got instance
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, want, got)
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
