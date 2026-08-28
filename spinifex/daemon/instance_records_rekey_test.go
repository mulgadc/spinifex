package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The separator the per-resource key space first shipped with, before a dot
// made it filterable by subject token. Spelled out here rather than imported so
// the test still describes the old shape once the constant is gone.
const slashPrefix = "i/"

// seedSlashRecord creates bucket at version, holding one record at the
// slash-separated key a build of that version wrote.
func seedSlashRecord(t *testing.T, nc *nats.Conn, bucket string, version int, instance *vm.VM) jetstream.KeyValue {
	t.Helper()
	ctx := context.Background()

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, History: 1})
	require.NoError(t, err)

	data, err := json.Marshal(instance.Record())
	require.NoError(t, err)
	_, err = kv.Put(ctx, slashPrefix+instance.ID, data)
	require.NoError(t, err)

	require.NoError(t, kvutil.WriteVersion(ctx, kv, version))
	return kv
}

func TestRekey_MovesInstanceStateRecordsOntoTheDot(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := seedSlashRecord(t, nc, daemon.InstanceStateBucket, 3,
		&vm.VM{ID: "i-1", InstanceType: "t3.nano", LastNode: "node-1"})

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitKVBucket())

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record, "the record must be readable at its new key")
	assert.Equal(t, "t3.nano", record.Spec.InstanceType)

	_, err = kv.Get(context.Background(), slashPrefix+"i-1")
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "the old key must not be left as an orphan")
}

func TestRekey_MovesTerminatedRecordsOntoTheDot(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	kv := seedSlashRecord(t, nc, daemon.TerminatedInstanceBucket, 2,
		&vm.VM{ID: "i-1", InstanceType: "t3.nano", LastNode: "node-1"})

	m, err := daemon.NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, m.InitTerminatedInstanceBucket())

	record, err := m.LoadTerminatedInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)

	_, err = kv.Get(context.Background(), slashPrefix+"i-1")
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

// A bucket that never held the slash separator must come out the same as one
// that did, so a fresh install is not a special case.
func TestRekey_IsANoOpWithNothingToMove(t *testing.T) {
	m := newRecordManager(t)
	require.NoError(t, m.WriteStoppedInstance("i-1", &vm.VM{ID: "i-1", InstanceType: "t3.nano"}))

	record, err := m.LoadInstanceRecord("i-1")
	require.NoError(t, err)
	require.NotNil(t, record)
}

// The dot separator is the whole reason for the re-key: a reconciler Filter is
// a NATS subject filter, "*" matches one dot-delimited token, and "i/<id>" is a
// single token — so "i/*" matched nothing. This asserts the new shape is
// actually watchable, rather than trusting the reasoning that chose it.
func TestRecordKeys_AreWatchableByASubjectFilter(t *testing.T) {
	m, nc := newRecordManagerConn(t)

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(context.Background(), daemon.InstanceStateBucket)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watcher, err := kv.Watch(ctx, daemon.InstanceRecordPrefix+"*")
	require.NoError(t, err)
	defer func() { _ = watcher.Stop() }()

	// Drain the initial replay up to the end-of-initial-values marker, so what
	// the assertion sees is the write below rather than history.
	for entry := range watcher.Updates() {
		if entry == nil {
			break
		}
	}

	require.NoError(t, m.WriteInstanceRecord("i-watched", testRecord("i-watched")))

	select {
	case entry := <-watcher.Updates():
		require.NotNil(t, entry, "the filter must deliver the record write")
		assert.Equal(t, daemon.InstanceRecordPrefix+"i-watched", entry.Key())
	case <-ctx.Done():
		t.Fatal("no watch event for a record write: the prefix is not filterable")
	}
}

// The counterpart: the separator that shipped first is not watchable, which is
// what this change exists to fix. If this ever stops failing to deliver, the
// re-key was unnecessary and the reasoning above is wrong.
func TestRecordKeys_SlashSeparatorWasNotWatchable(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(context.Background(),
		jetstream.KeyValueConfig{Bucket: "rekey-watch-probe", History: 1})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	watcher, err := kv.Watch(ctx, slashPrefix+"*")
	require.NoError(t, err)
	defer func() { _ = watcher.Stop() }()

	for entry := range watcher.Updates() {
		if entry == nil {
			break
		}
	}

	_, err = kv.Put(context.Background(), slashPrefix+"i-1", []byte("{}"))
	require.NoError(t, err)

	select {
	case entry := <-watcher.Updates():
		t.Fatalf("the slash separator delivered a watch event for %q, so the re-key was not needed", entry.Key())
	case <-time.After(2 * time.Second):
	}
}
