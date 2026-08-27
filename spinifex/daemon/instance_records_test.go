package daemon_test

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/resource"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRecordManager returns a JetStreamManager over an embedded server with both
// record-carrying buckets open. Each test gets its own server, so tests never
// see each other's keys.
func newRecordManager(t *testing.T) *daemon.JetStreamManager {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())
	require.NoError(t, m.InitTerminatedInstanceBucket())
	return m
}

func testRecord(id string) *vm.InstanceRecord {
	return &vm.InstanceRecord{
		Metadata: resource.Metadata{Name: id, AccountID: "111122223333"},
		Spec:     vm.InstanceSpec{InstanceType: "m7i.large", DesiredState: vm.DesiredStopped},
		Status:   vm.InstanceStatus{Status: vm.StateStopped, LastNode: "node-1"},
	}
}

func TestInstanceRecord_RoundTrips(t *testing.T) {
	m := newRecordManager(t)

	require.NoError(t, m.WriteInstanceRecord("i-1", testRecord("i-1")))

	got, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "i-1", got.Metadata.Name)
	assert.Equal(t, "111122223333", got.Metadata.AccountID)
	assert.Equal(t, vm.DesiredStopped, got.Spec.DesiredState)
	assert.Equal(t, "node-1", got.Status.LastNode)
}

// An instance nobody has heard of is not an error. Every Load accessor on the
// manager answers that way, and the callers that will move onto these are
// written against it.
func TestLoadInstanceRecord_AbsentIsNil(t *testing.T) {
	m := newRecordManager(t)

	got, err := m.LoadInstanceRecord("i-never-written")

	require.NoError(t, err)
	assert.Nil(t, got)
}

// The whole point of a fresh prefix: an old binary reading the keys this
// replaces must not see these records, and this one must not see those. They
// share a bucket, so nothing but the prefix keeps them apart.
func TestInstanceRecords_AreDisjointFromTheKeysTheyReplace(t *testing.T) {
	m := newRecordManager(t)

	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))
	require.NoError(t, m.WriteInstanceRecord("i-1", testRecord("i-1")))

	stopped, err := m.ListStoppedInstances()
	require.NoError(t, err)
	require.Len(t, stopped, 1, "the record must not appear in the stopped listing")
	assert.Equal(t, "t3.nano", stopped[0].InstanceType)

	records, err := m.ListInstanceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1, "the stopped instance must not appear in the record listing")
	assert.Equal(t, "m7i.large", records[0].Spec.InstanceType)

	// And deleting one leaves the other, which is what makes the dual-write
	// release safe to roll back.
	require.NoError(t, m.DeleteInstanceRecord("i-1"))

	stopped, err = m.ListStoppedInstances()
	require.NoError(t, err)
	assert.Len(t, stopped, 1, "deleting the record must not touch the key it will replace")
}

func TestTerminatedInstanceRecords_AreDisjointFromTheKeysTheyReplace(t *testing.T) {
	m := newRecordManager(t)

	require.NoError(t, m.WriteTerminatedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))
	require.NoError(t, m.WriteTerminatedInstanceRecord("i-1", testRecord("i-1")))

	terminated, err := m.ListTerminatedInstances()
	require.NoError(t, err)
	require.Len(t, terminated, 1)
	assert.Equal(t, "t3.nano", terminated[0].InstanceType)

	records, err := m.ListTerminatedInstanceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "m7i.large", records[0].Spec.InstanceType)
}

// The two buckets are separate stores over the same prefix, so a record in one
// must not be visible through the other.
func TestInstanceRecords_TheTwoBucketsDoNotShareRecords(t *testing.T) {
	m := newRecordManager(t)

	require.NoError(t, m.WriteInstanceRecord("i-1", testRecord("i-1")))

	got, err := m.LoadTerminatedInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Nil(t, got, "a live record must not be readable from the terminated bucket")

	terminated, err := m.ListTerminatedInstanceRecords()
	require.NoError(t, err)
	assert.Empty(t, terminated)
}

func TestUpdateInstanceRecord_CommitsTheMutation(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", testRecord("i-1")))

	updated, err := m.UpdateInstanceRecord("i-1", func(r *vm.InstanceRecord) {
		r.Status.Status = vm.StateRunning
		r.Status.LastNode = "node-2"
	})

	require.NoError(t, err)
	assert.Equal(t, vm.StateRunning, updated.Status.Status)

	reread, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Equal(t, "node-2", reread.Status.LastNode, "the mutation must be the committed one, not a local copy")
}

// A mutation racing a delete must fail rather than resurrect what the delete
// removed. This is the guarantee the claim in the next change is built on.
func TestUpdateInstanceRecord_AbsentDoesNotResurrect(t *testing.T) {
	m := newRecordManager(t)

	_, err := m.UpdateInstanceRecord("i-gone", func(r *vm.InstanceRecord) {
		r.Status.LastNode = "node-2"
	})

	assert.ErrorIs(t, err, kvstore.ErrNotFound)

	got, err := m.LoadInstanceRecord("i-gone")
	require.NoError(t, err)
	assert.Nil(t, got, "a failed mutation must leave no record behind")
}

func TestDeleteInstanceRecord_IsIdempotent(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", testRecord("i-1")))

	require.NoError(t, m.DeleteInstanceRecord("i-1"))
	require.NoError(t, m.DeleteInstanceRecord("i-1"), "deleting an absent record is not an error")

	got, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// An empty listing is nil and no error, matching the listings these replace —
// a caller distinguishing "no instances" from "could not read" reads the error.
func TestListInstanceRecords_EmptyIsNotAnError(t *testing.T) {
	m := newRecordManager(t)

	records, err := m.ListInstanceRecords()

	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestListInstanceRecords_ReturnsEveryRecord(t *testing.T) {
	m := newRecordManager(t)
	for _, id := range []string{"i-1", "i-2", "i-3"} {
		require.NoError(t, m.WriteInstanceRecord(id, testRecord(id)))
	}

	records, err := m.ListInstanceRecords()

	require.NoError(t, err)
	names := make([]string, 0, len(records))
	for _, r := range records {
		names = append(names, r.Metadata.Name)
	}
	assert.ElementsMatch(t, []string{"i-1", "i-2", "i-3"}, names)
}

// A write replaces the record wholesale rather than merging into it, so a field
// cleared by the writer stays cleared.
func TestWriteInstanceRecord_ReplacesWholesale(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteInstanceRecord("i-1", testRecord("i-1")))

	replacement := testRecord("i-1")
	replacement.Status.LastNode = ""
	require.NoError(t, m.WriteInstanceRecord("i-1", replacement))

	got, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Empty(t, got.Status.LastNode)
}

// The DeletionTimestamp the envelope carries has to survive the round trip, or
// the teardown path cannot tell a deleting instance from a live one.
func TestInstanceRecord_CarriesDeletionTimestamp(t *testing.T) {
	m := newRecordManager(t)
	record := testRecord("i-1")
	deleted := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	record.Metadata.DeletionTimestamp = &deleted

	require.NoError(t, m.WriteInstanceRecord("i-1", record))

	got, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, got.Metadata.DeletionTimestamp)
	assert.True(t, got.Metadata.MarkedForDeletion())
	assert.Equal(t, deleted, got.Metadata.DeletionTimestamp.UTC())
}

// Every accessor must report an unopened bucket rather than panic on a nil
// store: Tier 1 boot runs before cluster KV exists.
func TestInstanceRecordAccessors_ReportAnUnopenedBucket(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)

	assert.Error(t, m.WriteInstanceRecord("i-1", testRecord("i-1")))
	assert.Error(t, m.DeleteInstanceRecord("i-1"))
	assert.Error(t, m.WriteTerminatedInstanceRecord("i-1", testRecord("i-1")))
	assert.Error(t, m.DeleteTerminatedInstanceRecord("i-1"))

	_, err = m.LoadInstanceRecord("i-1")
	assert.Error(t, err)
	_, err = m.ListInstanceRecords()
	assert.Error(t, err)
	_, err = m.UpdateInstanceRecord("i-1", func(*vm.InstanceRecord) {})
	assert.Error(t, err)
	_, err = m.LoadTerminatedInstanceRecord("i-1")
	assert.Error(t, err)
	_, err = m.ListTerminatedInstanceRecords()
	assert.Error(t, err)
	_, err = m.UpdateTerminatedInstanceRecord("i-1", func(*vm.InstanceRecord) {})
	assert.Error(t, err)
}
