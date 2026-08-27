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

// legacyOnly leaves an instance at the key i/<id> replaces and nowhere else,
// which is the state a node that predates the per-resource key space leaves
// behind on every write it makes.
func legacyOnly(t *testing.T, m *daemon.JetStreamManager, instance *vm.VM) {
	t.Helper()
	require.NoError(t, m.WriteStoppedInstance(instance.ID, instance))
	require.NoError(t, m.DeleteInstanceRecord(instance.ID))
}

func TestLoadStoppedInstance_PrefersTheRecordKey(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	// Only reachable by writing the record key directly; the accessors keep the
	// two in step. It stands in for a mirror written after the key it mirrors.
	fresher := testRecord("i-1")
	require.NoError(t, m.WriteInstanceRecord("i-1", fresher))

	got, err := m.LoadStoppedInstance("i-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "m7i.large", got.InstanceType, "the record key must win over the key it replaces")
}

func TestLoadStoppedInstance_FallsBackToTheKeyItReplaces(t *testing.T) {
	m := newRecordManager(t)
	legacyOnly(t, m, &vm.VM{ID: "i-1", InstanceType: "t3.nano"})

	got, err := m.LoadStoppedInstance("i-1")

	require.NoError(t, err)
	require.NotNil(t, got, "an instance only the old key holds must still be found")
	assert.Equal(t, "t3.nano", got.InstanceType)
}

// Neither key space is a superset of the other during the transition, so a
// listing that read either alone would drop instances.
func TestListStoppedInstances_MergesBothKeySpaces(t *testing.T) {
	m := newRecordManager(t)

	legacyOnly(t, m, &vm.VM{ID: "i-old", InstanceType: "t3.nano"})
	require.NoError(t, m.WriteInstanceRecord("i-new", testRecord("i-new")))
	require.NoError(t, m.WriteStoppedInstance("i-both", &vm.VM{ID: "i-both", InstanceType: "t3.micro"}))

	stopped, err := m.ListStoppedInstances()

	require.NoError(t, err)
	ids := make([]string, 0, len(stopped))
	for _, instance := range stopped {
		ids = append(ids, instance.ID)
	}
	assert.ElementsMatch(t, []string{"i-old", "i-new", "i-both"}, ids)
}

func TestUpdateStoppedInstance_MirrorsTheCommittedValue(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	_, err := m.UpdateStoppedInstance("i-1", func(v *vm.VM) { v.LastNode = "node-2" })
	require.NoError(t, err)

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, "node-2", record.Status.LastNode, "the mirror must carry what the mutation committed")
}

// A mutation that finds nothing to mutate must not conjure a mirror: the
// record key would then hold an instance the key it mirrors does not.
func TestUpdateStoppedInstance_AbsentLeavesNoMirror(t *testing.T) {
	m := newRecordManager(t)

	_, err := m.UpdateStoppedInstance("i-gone", func(v *vm.VM) { v.LastNode = "node-2" })
	require.Error(t, err)

	record, err := m.LoadInstanceRecord("i-gone")
	require.NoError(t, err)
	assert.Nil(t, record)
}

func TestDeleteStoppedInstance_ClearsBothKeySpaces(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	require.NoError(t, m.DeleteStoppedInstance("i-1"))

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Nil(t, record, "a delete that leaves the mirror behind resurrects the instance on the next read")

	stopped, err := m.ListStoppedInstances()
	require.NoError(t, err)
	assert.Empty(t, stopped)
}

// The claim stays on the key it always used — two atomic deletes would be two
// winners — and clears the mirror so the winner cannot leave the instance
// readable as though it were still claimable.
func TestClaimStoppedInstance_ClearsTheMirror(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	claimed, err := m.ClaimStoppedInstance("i-1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, "t3.nano", claimed.InstanceType)

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Nil(t, record, "a claimed instance must not survive at the record key")

	stopped, err := m.ListStoppedInstances()
	require.NoError(t, err)
	assert.Empty(t, stopped)

	_, err = m.ClaimStoppedInstance("i-1")
	assert.ErrorIs(t, err, vm.ErrStoppedInstanceClaimed, "only one caller may win a claim")
}

func TestWriteTerminatedInstance_MirrorsOntoTheRecordKey(t *testing.T) {
	m := newRecordManager(t)

	require.NoError(t, m.WriteTerminatedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	record, err := m.LoadTerminatedInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, "t3.nano", record.Spec.InstanceType)
}

func TestUpdateTerminatedInstance_MirrorsTheCommittedValue(t *testing.T) {
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

func TestListTerminatedInstances_MergesBothKeySpaces(t *testing.T) {
	m := newRecordManager(t)

	require.NoError(t, m.WriteTerminatedInstance("i-old", &vm.VM{ID: "i-old", InstanceType: "t3.nano"}))
	require.NoError(t, m.DeleteTerminatedInstanceRecord("i-old"))
	require.NoError(t, m.WriteTerminatedInstanceRecord("i-new", testRecord("i-new")))

	terminated, err := m.ListTerminatedInstances()

	require.NoError(t, err)
	ids := make([]string, 0, len(terminated))
	for _, instance := range terminated {
		ids = append(ids, instance.ID)
	}
	assert.ElementsMatch(t, []string{"i-old", "i-new"}, ids)
}

func TestDeleteTerminatedInstance_ClearsBothKeySpaces(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteTerminatedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	require.NoError(t, m.DeleteTerminatedInstance("i-1"))

	record, err := m.LoadTerminatedInstanceRecord("i-1")
	require.NoError(t, err)
	assert.Nil(t, record)

	terminated, err := m.ListTerminatedInstances()
	require.NoError(t, err)
	assert.Empty(t, terminated)
}

func TestLoadTerminatedInstance_FallsBackToTheKeyItReplaces(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteTerminatedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))
	require.NoError(t, m.DeleteTerminatedInstanceRecord("i-1"))

	got, err := m.LoadTerminatedInstance("i-1")

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "t3.nano", got.InstanceType)
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

// The node blob holds a whole node's running set in one record, so it is a
// split rather than a copy and is not this migration's to make.
func TestInstanceStateMigration_LeavesTheNodeBlobAlone(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := seedLegacyBucket(t, nc, daemon.InstanceStateBucket, daemon.StoppedInstancePrefix,
		&vm.VM{ID: "i-1", InstanceType: "t3.nano"})

	blob, err := json.Marshal(map[string]any{"instances": map[string]any{}})
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
