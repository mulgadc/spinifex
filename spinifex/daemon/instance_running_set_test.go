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

const localNode = "node-1"

func runningVM(id, instanceType string) *vm.VM {
	return &vm.VM{ID: id, InstanceType: instanceType, Status: vm.StateRunning, LastNode: localNode}
}

func runningSet(vms ...*vm.VM) map[string]*vm.VM {
	set := make(map[string]*vm.VM, len(vms))
	for _, v := range vms {
		set[v.ID] = v
	}
	return set
}

// writeRunningSet does what persistState does: the presence marker, then the
// records. Both, because a set written without a marker is a set no reader will
// admit exists.
func writeRunningSet(t *testing.T, m *daemon.JetStreamManager, vms ...*vm.VM) {
	t.Helper()
	require.NoError(t, m.WriteNodeMarker(localNode))
	m.WriteRunningSet(localNode, runningSet(vms...))
}

// recordRevision reads the revision of a record straight from the bucket, so a
// test can tell a write that did nothing from one that did not happen.
func recordRevision(t *testing.T, nc *nats.Conn, instanceID string) uint64 {
	t.Helper()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(context.Background(), daemon.InstanceStateBucket)
	require.NoError(t, err)
	entry, err := kv.Get(context.Background(), daemon.InstanceRecordPrefix+instanceID)
	require.NoError(t, err)
	return entry.Revision()
}

func TestWriteRunningSet_WritesARecordPerMember(t *testing.T) {
	m := newRecordManager(t)

	writeRunningSet(t, m, runningVM("i-1", "t3.nano"), runningVM("i-2", "t3.micro"))

	records, err := m.ListInstanceRecords()
	require.NoError(t, err)
	byID := make(map[string]string, len(records))
	for _, record := range records {
		byID[record.Metadata.Name] = record.Spec.InstanceType
	}
	assert.Equal(t, map[string]string{"i-1": "t3.nano", "i-2": "t3.micro"}, byID,
		"a set of two instances must become two records")
}

func TestLoadState_ReadsTheNodesRecords(t *testing.T) {
	m := newRecordManager(t)
	writeRunningSet(t, m, runningVM("i-1", "t3.nano"), runningVM("i-2", "t3.micro"))

	loaded, found, err := m.LoadState(localNode)

	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, loaded, 2)
	assert.Equal(t, "t3.nano", loaded["i-1"].InstanceType)
	assert.Equal(t, "t3.micro", loaded["i-2"].InstanceType)
}

// The key space is shared by every node, so a listing is not a running set
// until it is filtered by who owns each record.
func TestLoadState_IgnoresAnotherNodesRecords(t *testing.T) {
	m := newRecordManager(t)
	writeRunningSet(t, m, runningVM("i-1", "t3.nano"))

	elsewhere := &vm.VM{ID: "i-2", InstanceType: "t3.micro", Status: vm.StateRunning, LastNode: "node-2"}
	require.NoError(t, m.WriteInstanceRecord("i-2", elsewhere.Record()))

	loaded, found, err := m.LoadState(localNode)

	require.NoError(t, err)
	require.True(t, found)
	assert.NotContains(t, loaded, "i-2")
	assert.Len(t, loaded, 1)
}

// The distinction the presence marker exists for. Both cases scan no records,
// and restore replaces a node's local state with what it reads here whenever it
// believes the cluster has a record of that node — so reading "no instances" as
// "no such node", or the reverse, is the difference between a node coming back
// with its instances and coming back empty.
func TestLoadState_FoundIsTheMarkerNotTheRecordCount(t *testing.T) {
	m := newRecordManager(t)

	loaded, found, err := m.LoadState(localNode)
	require.NoError(t, err)
	assert.False(t, found, "a node that has never written a marker is not in the cluster's record")
	assert.Empty(t, loaded)

	require.NoError(t, m.WriteNodeMarker(localNode))

	loaded, found, err = m.LoadState(localNode)
	require.NoError(t, err)
	assert.True(t, found, "a node with a marker and no instances is a known, empty node")
	assert.Empty(t, loaded)
}

// An operator-stopped instance belongs to the stopped set, and the running set
// is the same keys filtered. Returning it here would relaunch it on restore.
func TestLoadState_OmitsAnOperatorStoppedRecord(t *testing.T) {
	m := newRecordManager(t)
	writeRunningSet(t, m, runningVM("i-1", "t3.nano"))
	require.NoError(t, m.WriteStoppedInstance("i-1", runningVM("i-1", "t3.nano")))

	loaded, found, err := m.LoadState(localNode)

	require.NoError(t, err)
	require.True(t, found)
	assert.NotContains(t, loaded, "i-1")
}

// The conversion's totality is guarded in the vm package; what this adds is
// that the running-set path actually routes through it, on fields from both
// halves of the record rather than the instance type alone.
func TestWriteRunningSet_CarriesBothHalvesOfTheRecord(t *testing.T) {
	m := newRecordManager(t)
	instance := runningVM("i-1", "t3.nano")
	instance.DesiredState = vm.DesiredRunning
	instance.PublicIP = "10.0.0.9"
	instance.ENIId = "eni-1"
	instance.HostfwdPorts = []int{2222}

	writeRunningSet(t, m, instance)

	loaded, _, err := m.LoadState(localNode)
	require.NoError(t, err)
	require.Contains(t, loaded, "i-1")
	got := loaded["i-1"]
	assert.Equal(t, vm.DesiredRunning, got.DesiredState)
	assert.Equal(t, []int{2222}, got.HostfwdPorts)
	assert.Equal(t, "10.0.0.9", got.PublicIP)
	assert.Equal(t, "eni-1", got.ENIId)
	assert.Equal(t, localNode, got.LastNode)
}

// Ownership is stamped by the write rather than carried in from the caller:
// this is the only place that knows whose running set is being written.
func TestWriteRunningSet_StampsOwnership(t *testing.T) {
	m := newRecordManager(t)
	unowned := &vm.VM{ID: "i-1", InstanceType: "t3.nano", Status: vm.StateRunning}

	require.NoError(t, m.WriteNodeMarker(localNode))
	m.WriteRunningSet(localNode, runningSet(unowned))

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, localNode, record.Status.LastNode)
}

func TestWriteRunningSet_RetiresAMemberThatLeft(t *testing.T) {
	m := newRecordManager(t)
	writeRunningSet(t, m, runningVM("i-1", "t3.nano"), runningVM("i-2", "t3.micro"))

	writeRunningSet(t, m, runningVM("i-1", "t3.nano"))

	gone, err := m.LoadInstanceRecord("i-2")
	require.NoError(t, err)
	assert.Nil(t, gone, "a record left behind by a terminated instance never expires from this bucket")

	kept, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	assert.NotNil(t, kept)
}

// One key holds an instance for its whole life, so the record an instance
// leaves the running set with is the same one the stopped set has just taken
// over. An unconditional retire would delete the stopped instance it became.
func TestWriteRunningSet_HandsOverAMemberThatStopped(t *testing.T) {
	m := newRecordManager(t)
	writeRunningSet(t, m, runningVM("i-1", "t3.nano"))

	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano", Status: vm.StateStopped}))
	writeRunningSet(t, m)

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record, "the stopped set owns this record now")

	stopped, err := m.ListStoppedInstances()
	require.NoError(t, err)
	require.Len(t, stopped, 1)
	assert.Equal(t, "i-1", stopped[0].ID)
}

// Rewriting every record on every state change would reintroduce, one key at a
// time, the contention splitting the blob exists to remove.
func TestWriteRunningSet_WritesOnlyWhatChanged(t *testing.T) {
	m, nc := newRecordManagerConn(t)
	writeRunningSet(t, m, runningVM("i-1", "t3.nano"), runningVM("i-2", "t3.micro"))
	before1, before2 := recordRevision(t, nc, "i-1"), recordRevision(t, nc, "i-2")

	writeRunningSet(t, m, runningVM("i-1", "t3.nano"), runningVM("i-2", "t3.micro"))

	assert.Equal(t, before1, recordRevision(t, nc, "i-1"), "an unchanged instance must not be rewritten")
	assert.Equal(t, before2, recordRevision(t, nc, "i-2"))

	changed := runningVM("i-1", "t3.nano")
	changed.PublicIP = "10.0.0.9"
	writeRunningSet(t, m, changed, runningVM("i-2", "t3.micro"))

	assert.Greater(t, recordRevision(t, nc, "i-1"), before1, "a changed instance must be rewritten")
	assert.Equal(t, before2, recordRevision(t, nc, "i-2"), "and only that instance")
}

// A restarted process has no memory of what it last wrote, so it primes itself
// from the bucket rather than rewriting every record to find out.
func TestWriteRunningSet_SeedsFromWhatIsAlreadyThere(t *testing.T) {
	first, nc := newRecordManagerConn(t)
	writeRunningSet(t, first, runningVM("i-1", "t3.nano"))
	before := recordRevision(t, nc, "i-1")

	second, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, second.InitKVBucket())
	writeRunningSet(t, second, runningVM("i-1", "t3.nano"))

	assert.Equal(t, before, recordRevision(t, nc, "i-1"))
}

// Ownership is read from status.LastNode, the same field the GC uses. A node
// must not retire a record for an instance that is running somewhere else.
func TestWriteRunningSet_LeavesAnotherNodesRecordsAlone(t *testing.T) {
	m := newRecordManager(t)
	elsewhere := &vm.VM{ID: "i-elsewhere", InstanceType: "t3.nano", Status: vm.StateRunning, LastNode: "node-2"}
	require.NoError(t, m.WriteInstanceRecord("i-elsewhere", elsewhere.Record()))

	writeRunningSet(t, m, runningVM("i-1", "t3.nano"))

	record, err := m.LoadInstanceRecord("i-elsewhere")
	require.NoError(t, err)
	assert.NotNil(t, record)
}

// seedNodeBlobBucket creates the instance-state bucket at the version before
// the blob split, holding one node's running set as a single record.
func seedNodeBlobBucket(t *testing.T, nc *nats.Conn, nodeID string, vms map[string]*vm.VM) jetstream.KeyValue {
	t.Helper()
	ctx := context.Background()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: daemon.InstanceStateBucket, History: 1})
	require.NoError(t, err)

	data, err := json.Marshal(daemon.LocalState{SchemaVersion: daemon.LocalStateSchemaVersion, VMS: vms})
	require.NoError(t, err)
	_, err = kv.Put(ctx, daemon.InstanceStatePrefix+nodeID, data)
	require.NoError(t, err)

	require.NoError(t, kvutil.WriteVersion(ctx, kv, 2))
	return kv
}

// readNodeBlob returns the running-set blob at the key the record space
// replaced, or nil if it is gone.
func readNodeBlob(t *testing.T, kv jetstream.KeyValue, nodeID string) *daemon.LocalState {
	t.Helper()
	entry, err := kv.Get(context.Background(), daemon.InstanceStatePrefix+nodeID)
	if err != nil {
		return nil
	}
	var blob daemon.LocalState
	require.NoError(t, json.Unmarshal(entry.Value(), &blob))
	return &blob
}

func TestInstanceStateMigration_SplitsTheNodeBlob(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	seedNodeBlobBucket(t, nc, localNode, runningSet(runningVM("i-1", "t3.nano"), runningVM("i-2", "t3.micro")))

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())

	records, err := m.ListInstanceRecords()
	require.NoError(t, err)
	byID := make(map[string]string, len(records))
	for _, record := range records {
		byID[record.Metadata.Name] = record.Spec.InstanceType
	}
	assert.Equal(t, map[string]string{"i-1": "t3.nano", "i-2": "t3.micro"}, byID)
}

// The split copies the blob rather than consuming it, and nothing after the
// cutover writes over it. It is what a node rolled back to the release before
// the cutover reads to find out what it was running, so leaving it intact is
// the whole of what makes the crossing reversible.
func TestInstanceStateMigration_LeavesTheNodeBlobIntact(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := seedNodeBlobBucket(t, nc, localNode, runningSet(runningVM("i-1", "t3.nano")))

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())
	writeRunningSet(t, m, runningVM("i-1", "t3.nano"))

	blob := readNodeBlob(t, kv, localNode)
	require.NotNil(t, blob, "the blob the split copied from must survive the migration")
	assert.Contains(t, blob.VMS, "i-1", "and must not be emptied by the marker that replaces it")
}

// The marker is a key of its own for that reason: written onto the blob it
// would answer the presence question by destroying the answer to the other one.
func TestInstanceStateMigration_SeedsAPresenceMarkerPerNode(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := seedNodeBlobBucket(t, nc, localNode, runningSet(runningVM("i-1", "t3.nano")))

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())

	_, err = kv.Get(context.Background(), daemon.NodePresencePrefix+localNode)
	require.NoError(t, err, "a node that had a blob must be present without having written anything yet")

	loaded, found, err := m.LoadState(localNode)
	require.NoError(t, err)
	assert.True(t, found, "so it can recover its instances from KV across the crossing")
	assert.Contains(t, loaded, "i-1")
}

// Every node's set is copied, not just the one running the migration: it runs
// once per bucket, and a node that is down would otherwise have no records
// until it came back.
func TestInstanceStateMigration_SplitsEveryNodesBlob(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := seedNodeBlobBucket(t, nc, localNode, runningSet(runningVM("i-1", "t3.nano")))

	other := &vm.VM{ID: "i-2", InstanceType: "t3.micro", Status: vm.StateRunning, LastNode: "node-2"}
	data, err := json.Marshal(daemon.LocalState{
		SchemaVersion: daemon.LocalStateSchemaVersion,
		VMS:           map[string]*vm.VM{other.ID: other},
	})
	require.NoError(t, err)
	_, err = kv.Put(context.Background(), daemon.InstanceStatePrefix+"node-2", data)
	require.NoError(t, err)

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())

	record, err := m.LoadInstanceRecord("i-2")
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, "node-2", record.Status.LastNode)
}

func TestInstanceStateMigration_BlobSplitIsSafeToRunTwice(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := seedNodeBlobBucket(t, nc, localNode, runningSet(runningVM("i-1", "t3.nano")))

	first, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, first.InitKVBucket())
	before := recordRevision(t, nc, "i-1")

	require.NoError(t, kvutil.WriteVersion(context.Background(), kv, 2))
	second, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, second.InitKVBucket())

	assert.Equal(t, before, recordRevision(t, nc, "i-1"), "a second run must not overwrite what the first copied")
}
