package daemon_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedFrozenKey writes raw bytes at one of the key spaces the record replaced.
// Nothing does this any more, which is the point: it stages what a cluster that
// crossed the cutover still has lying around.
func seedFrozenKey(t *testing.T, nc *nats.Conn, bucket, key string) {
	t.Helper()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(context.Background(), bucket)
	require.NoError(t, err)
	_, err = kv.Put(context.Background(), key, []byte(`{"id":"i-1"}`))
	require.NoError(t, err)
}

// frozenKeyExists reports whether a key in one of the replaced key spaces is
// still there.
func frozenKeyExists(t *testing.T, nc *nats.Conn, bucket, key string) bool {
	t.Helper()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(context.Background(), bucket)
	require.NoError(t, err)
	_, err = kv.Get(context.Background(), key)
	return err == nil
}

func TestUpdateStoppedInstance_CommitsToTheRecord(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	_, err := m.UpdateStoppedInstance("i-1", func(v *vm.VM) { v.LastNode = "node-2" })
	require.NoError(t, err)

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, "node-2", record.Status.LastNode, "the record must carry what the mutation committed")
}

// A mutation that finds nothing to mutate must not create it. The record would
// then hold an instance nothing ever stopped.
func TestUpdateStoppedInstance_AbsentCreatesNothing(t *testing.T) {
	m := newRecordManager(t)

	_, err := m.UpdateStoppedInstance("i-gone", func(v *vm.VM) { v.LastNode = "node-2" })
	require.Error(t, err)

	record, err := m.LoadInstanceRecord("i-gone")
	require.NoError(t, err)
	assert.Nil(t, record)
}

// The delete drains the key the record replaced as well as the record. The
// frozen space is kept so the cutover can be rolled back, but an instance that
// is gone should not be recoverable from it, and draining on this path is what
// stops it growing without bound.
func TestDeleteStoppedInstance_DrainsTheFrozenKey(t *testing.T) {
	m, nc := newRecordManagerConn(t)
	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))
	seedFrozenKey(t, nc, daemon.InstanceStateBucket, daemon.StoppedInstancePrefix+"i-1")

	require.NoError(t, m.DeleteStoppedInstance("i-1"))

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Nil(t, record)

	stopped, err := m.ListStoppedInstances()
	require.NoError(t, err)
	assert.Empty(t, stopped)

	assert.False(t, frozenKeyExists(t, nc, daemon.InstanceStateBucket, daemon.StoppedInstancePrefix+"i-1"),
		"the key the record replaced must not outlive the instance")
}

// Exclusivity is a compare-and-set on the record now rather than a delete of
// it. One key holds an instance for its whole life, so a claim that deleted
// would be deleting the instance it is about to start.
func TestClaimStoppedInstance_OnlyOneCallerWins(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	claimed, err := m.ClaimStoppedInstance("i-1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, "t3.nano", claimed.InstanceType)

	_, err = m.ClaimStoppedInstance("i-1")
	assert.ErrorIs(t, err, vm.ErrStoppedInstanceClaimed, "only one caller may win a claim")
}

// What the winner changes is the intent, not the key. Clearing DesiredStopped
// is what makes the record no longer claimable and no longer stopped, and it is
// the same edit that stops it reading back as an instance nobody started.
func TestClaimStoppedInstance_KeepsTheRecordAndClearsTheStopIntent(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	_, err := m.ClaimStoppedInstance("i-1")
	require.NoError(t, err)

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record, "the claim moves the record between sets, it does not delete it")
	assert.Equal(t, vm.DesiredRunning, record.Spec.DesiredState)

	stopped, err := m.ListStoppedInstances()
	require.NoError(t, err)
	assert.Empty(t, stopped, "a claimed instance must not keep reading back as stopped")

	got, err := m.LoadStoppedInstance("i-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// LastNode is left alone deliberately. The claimant has not run the instance
// yet, and the node that last did is the one that should recover it if the
// launch that follows this claim never happens.
func TestClaimStoppedInstance_LeavesOwnershipWithTheLastNodeToRunIt(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano", LastNode: "node-2"}))

	_, err := m.ClaimStoppedInstance("i-1")
	require.NoError(t, err)

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, "node-2", record.Status.LastNode)
}

func TestClaimStoppedInstance_AbsentIsNotClaimable(t *testing.T) {
	m := newRecordManager(t)

	_, err := m.ClaimStoppedInstance("i-never-existed")
	assert.ErrorIs(t, err, vm.ErrStoppedInstanceClaimed)
}

func TestWriteTerminatedInstance_WritesTheRecord(t *testing.T) {
	m := newRecordManager(t)

	require.NoError(t, m.WriteTerminatedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	record, err := m.LoadTerminatedInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, "t3.nano", record.Spec.InstanceType)
}

func TestUpdateTerminatedInstance_CommitsToTheRecord(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteTerminatedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	_, err := m.UpdateTerminatedInstance("i-1", func(v *vm.VM) {
		v.Teardown = map[string]string{"eni": "done"}
	})
	require.NoError(t, err)

	record, err := m.LoadTerminatedInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, map[string]string{"eni": "done"}, record.Status.Teardown)
}

func TestDeleteTerminatedInstance_DrainsTheFrozenKey(t *testing.T) {
	m, nc := newRecordManagerConn(t)
	require.NoError(t, m.InitTerminatedInstanceBucket())
	require.NoError(t, m.WriteTerminatedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))
	seedFrozenKey(t, nc, daemon.TerminatedInstanceBucket, daemon.TerminatedInstancePrefix+"i-1")

	require.NoError(t, m.DeleteTerminatedInstance("i-1"))

	record, err := m.LoadTerminatedInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Nil(t, record)

	terminated, err := m.ListTerminatedInstances()
	require.NoError(t, err)
	assert.Empty(t, terminated)

	assert.False(t, frozenKeyExists(t, nc, daemon.TerminatedInstanceBucket, daemon.TerminatedInstancePrefix+"i-1"))
}

// seedLegacyBucket creates bucket at schema version 1 holding instance at the
// key the migration copies from, as a cluster that has never run the
// per-resource key space would leave it.
func seedLegacyBucket(t *testing.T, nc *nats.Conn, bucket, prefix string, instance *vm.VM) jetstream.KeyValue {
	t.Helper()
	ctx := context.Background()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, History: 1})
	require.NoError(t, err)

	data, err := json.Marshal(instance)
	require.NoError(t, err)
	_, err = kv.Put(ctx, prefix+instance.ID, data)
	require.NoError(t, err)

	require.NoError(t, kvutil.WriteVersion(ctx, kv, 1))
	return kv
}

func TestInstanceStateMigration_CopiesStoppedInstancesForward(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	seedLegacyBucket(t, nc, daemon.InstanceStateBucket, daemon.StoppedInstancePrefix,
		&vm.VM{ID: "i-1", InstanceType: "t3.nano"})

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record, "opening the bucket must copy the old keys forward")
	assert.Equal(t, "t3.nano", record.Spec.InstanceType)
}

// A bucket old enough to need both steps gets both, and the node blob is split
// by the second rather than swept up by the first. The blob's own members are
// covered by TestInstanceStateMigration_SplitsTheNodeBlob; what this pins is
// that an empty one does not make the stopped copy-forward skip or duplicate.
func TestInstanceStateMigration_RunsEveryStepFromTheOldestVersion(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := seedLegacyBucket(t, nc, daemon.InstanceStateBucket, daemon.StoppedInstancePrefix,
		&vm.VM{ID: "i-1", InstanceType: "t3.nano"})

	blob, err := json.Marshal(daemon.LocalState{SchemaVersion: daemon.LocalStateSchemaVersion})
	require.NoError(t, err)
	_, err = kv.Put(context.Background(), daemon.InstanceStatePrefix+"node-1", blob)
	require.NoError(t, err)

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())

	records, err := m.ListInstanceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "i-1", records[0].Metadata.Name)
}

// A node that has already upgraded can be writing the destination while the
// scan runs, and the version stamp is a read-then-write rather than a CAS, so
// two nodes can run this at once. Neither may lose a fresher record.
func TestInstanceStateMigration_DoesNotOverwriteAFresherRecord(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := seedLegacyBucket(t, nc, daemon.InstanceStateBucket, daemon.StoppedInstancePrefix,
		&vm.VM{ID: "i-1", InstanceType: "t3.nano"})

	fresher, err := json.Marshal((&vm.VM{ID: "i-1", InstanceType: "m7i.large"}).Record())
	require.NoError(t, err)
	_, err = kv.Put(context.Background(), daemon.InstanceRecordPrefix+"i-1", fresher)
	require.NoError(t, err)

	// A second instance nobody has copied yet, so the run this test measures is
	// one that does work rather than one that skips everything.
	untouched, err := json.Marshal(&vm.VM{ID: "i-2", InstanceType: "t3.micro"})
	require.NoError(t, err)
	_, err = kv.Put(context.Background(), daemon.StoppedInstancePrefix+"i-2", untouched)
	require.NoError(t, err)

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, "m7i.large", record.Spec.InstanceType)

	copied, err := m.LoadInstanceRecord("i-2")
	require.NoError(t, err)
	require.NotNil(t, copied, "skipping an existing destination must not stop the scan")
	assert.Equal(t, "t3.micro", copied.Spec.InstanceType)
}

func TestInstanceStateMigration_IsSafeToRunTwice(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := seedLegacyBucket(t, nc, daemon.InstanceStateBucket, daemon.StoppedInstancePrefix,
		&vm.VM{ID: "i-1", InstanceType: "t3.nano"})

	first, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, first.InitKVBucket())

	require.NoError(t, kvutil.WriteVersion(context.Background(), kv, 1))
	second, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, second.InitKVBucket())

	records, err := second.ListInstanceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, "t3.nano", records[0].Spec.InstanceType)
}

func TestTerminatedInstanceMigration_CopiesTerminatedInstancesForward(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	seedLegacyBucket(t, nc, daemon.TerminatedInstanceBucket, daemon.TerminatedInstancePrefix,
		&vm.VM{ID: "i-1", InstanceType: "t3.nano"})

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitTerminatedInstanceBucket())

	record, err := m.LoadTerminatedInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, "t3.nano", record.Spec.InstanceType)
}
