package daemon_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file characterise the instance-state persistence formats
// as they are today. They pin current behaviour so a later change to the
// record or the local file is visible; they do not describe the target.

func baselineVM() *vm.VM {
	return &vm.VM{
		ID:           "i-0123456789abcdef0",
		Status:       vm.StateRunning,
		InstanceType: "t3.micro",
		DesiredState: vm.DesiredRunning,
		LastNode:     "node-1",
		AZ:           "us-east-1a",
		AccountID:    "111122223333",
	}
}

const baselineLocalFile = `{"schema_version":1,"vms":{"i-0123456789abcdef0":{"id":"i-0123456789abcdef0","status":"running","instance_type":"t3.micro","config":{"name":"","pid_file":"","qmp_socket":"","enable_kvm":false,"no_graphic":false,"machine_type":"","cpu_type":"","cpu_count":0,"memory":0,"drives":null,"devices":null,"net_devs":null,"instance_type":"","architecture":""},"ebs_requests":{"Requests":null},"eni_requests":{"available_slots":null,"attached_by_eni_id":null},"last_node":"node-1","az":"us-east-1a","health":{"crash_count":0,"last_crash_time":"0001-01-01T00:00:00Z","restart_count":0,"first_crash_time":"0001-01-01T00:00:00Z","qmp_consecutive_failures":0},"account_id":"111122223333"}}}`

func TestCurrentBehaviour_LocalStateFileExactBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "instance-state.json")
	data, err := daemon.MarshalLocalState(map[string]*vm.VM{"i-0123456789abcdef0": baselineVM()})
	require.NoError(t, err)
	require.NoError(t, daemon.WriteLocalStateBytes(path, data))

	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, baselineLocalFile, string(onDisk),
		"compact JSON, no trailing newline, whole vm.VM per instance and no generation of any kind")
}

func TestCurrentBehaviour_LocalStateSchemaVersionFailures(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "missing", content: `{"vms":{}}`, wantErr: "unknown schema_version 0 (expected 1)"},
		{name: "older", content: `{"schema_version":0,"vms":{}}`, wantErr: "unknown schema_version 0 (expected 1)"},
		{name: "newer", content: `{"schema_version":2,"vms":{}}`, wantErr: "unknown schema_version 2 (expected 1)"},
		{name: "wrong type", content: `{"schema_version":"1","vms":{}}`, wantErr: "parse local state"},
		{name: "truncated", content: `{"schema_version":1,"vms":{"i-1":{"id":`, wantErr: "parse local state"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "instance-state.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))

			state, err := daemon.ReadLocalState(path)
			require.Error(t, err)
			assert.Nil(t, state)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// A file written by a newer binary at the same schema version is read, and the
// fields this binary does not know are gone after the next rewrite.
func TestCurrentBehaviour_LocalStateUnknownFieldsAreDroppedOnRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instance-state.json")
	newer := `{"schema_version":1,"journal":{"seq":9},"vms":{"i-1":{"id":"i-1","status":"running","assignment_generation":4}}}`
	require.NoError(t, os.WriteFile(path, []byte(newer), 0o600))

	state, err := daemon.ReadLocalState(path)
	require.NoError(t, err)
	require.Contains(t, state.VMS, "i-1")

	data, err := daemon.MarshalLocalState(state.VMS)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "journal", "LOST: unknown top-level field")
	assert.NotContains(t, string(data), "assignment_generation", "LOST: unknown per-instance field")
}

func TestCurrentBehaviour_InstanceBucketConfiguration(t *testing.T) {
	_, nc := newRecordManagerConn(t)
	js, err := jetstream.New(nc)
	require.NoError(t, err)

	cases := []struct {
		bucket  string
		history int64
		ttl     time.Duration
		version int
	}{
		{bucket: "spinifex-instance-state", history: 1, ttl: 0, version: 5},
		{bucket: "spinifex-terminated-instances", history: 1, ttl: time.Hour, version: 3},
	}
	for _, tc := range cases {
		kv, err := js.KeyValue(t.Context(), tc.bucket)
		require.NoError(t, err, tc.bucket)
		status, err := kv.Status(t.Context())
		require.NoError(t, err)
		assert.Equal(t, tc.history, status.History(), tc.bucket)
		assert.Equal(t, tc.ttl, status.TTL(), tc.bucket)

		v, err := kvutil.ReadVersion(t.Context(), kv)
		require.NoError(t, err)
		assert.Equal(t, tc.version, v, tc.bucket)
	}
	assert.Equal(t, "spinifex-instance-state", daemon.InstanceStateBucket)
	assert.Equal(t, "spinifex-terminated-instances", daemon.TerminatedInstanceBucket)
	assert.Equal(t, "i.", daemon.InstanceRecordPrefix)
}

const baselineRecord = `{"metadata":{"name":"i-0123456789abcdef0","account_id":"111122223333"},"spec":{"instance_type":"t3.micro","config":{"name":"","pid_file":"","qmp_socket":"","enable_kvm":false,"no_graphic":false,"machine_type":"","cpu_type":"","cpu_count":0,"memory":0,"drives":null,"devices":null,"net_devs":null,"instance_type":"","architecture":""}},"status":{"status":"running","health":{"crash_count":0,"last_crash_time":"0001-01-01T00:00:00Z","restart_count":0,"first_crash_time":"0001-01-01T00:00:00Z","qmp_consecutive_failures":0},"last_node":"node-2","az":"us-east-1b"}}`

// The running-set writer stamps the node and AZ it was given over whatever the
// instance carried, writes at i.<id> and carries no schema version per record.
func TestCurrentBehaviour_RecordKeyAndWireForm(t *testing.T) {
	m, nc := newRecordManagerConn(t)
	m.WriteRunningSet("node-2", "us-east-1b", map[string]*vm.VM{"i-0123456789abcdef0": baselineVM()})

	kv := instanceStateKV(t, nc)
	entry, err := kv.Get(t.Context(), "i.i-0123456789abcdef0")
	require.NoError(t, err)
	assert.Equal(t, baselineRecord, string(entry.Value()))

	var decoded vm.InstanceRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &decoded))
	assert.Equal(t, "i-0123456789abcdef0", vm.VMFromRecord(&decoded).ID)
}

func instanceStateKV(t *testing.T, nc *nats.Conn) jetstream.KeyValue {
	t.Helper()
	return bucketKV(t, nc, daemon.InstanceStateBucket)
}

func bucketKV(t *testing.T, nc *nats.Conn, bucket string) jetstream.KeyValue {
	t.Helper()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(t.Context(), bucket)
	require.NoError(t, err)
	return kv
}

// The bucket version is checked once, when a daemon-configured handle opens the
// bucket. A handle already open keeps writing after a newer binary stamps it,
// and readers that open the bucket by name see no gate at all.
func TestCurrentBehaviour_BucketVersionGate(t *testing.T) {
	oldWriter, nc := newRecordManagerConn(t)
	kv := instanceStateKV(t, nc)
	require.NoError(t, kvutil.WriteVersion(t.Context(), kv, daemon.InstanceStateBucketVersion+1))

	fresh, err := daemon.NewJetStreamManager(nc)
	require.NoError(t, err)
	err = fresh.InitKVBucket()
	var ahead migrate.SchemaAheadError
	require.ErrorAs(t, err, &ahead, "a daemon opening a bucket stamped ahead of it refuses to start")

	require.NoError(t, oldWriter.WriteInstanceRecord("i-1", (&vm.VM{ID: "i-1"}).Record()),
		"an already-open handle is not re-gated")

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	gatewayStyle := kvstore.New[vm.InstanceRecord](js, kvstore.Config{Name: daemon.InstanceStateBucket, History: 1})
	got, _, err := gatewayStyle.Get(t.Context(), "i.i-1")
	require.NoError(t, err, "a reader configured without the migration hook opens and decodes regardless of the stamp")
	assert.Equal(t, "i-1", got.Metadata.Name)
}

// newerRecord carries a generation pair and fields a newer binary might add.
const newerRecord = `{"metadata":{"name":"i-g","account_id":"111122223333","generation":7,"observed_generation":6},` +
	`"spec":{"instance_type":"t3.micro","desired_state":"%s","desired_generation":7},` +
	`"status":{"status":"%s","last_node":"node-1","assignment_fence":"f-1"}}`

func seedNewerRecord(t *testing.T, kv jetstream.KeyValue, desired vm.DesiredState, status vm.InstanceState) {
	t.Helper()
	raw := []byte(fmt.Sprintf(newerRecord, string(desired), string(status)))
	_, err := kv.Put(t.Context(), "i.i-g", raw)
	require.NoError(t, err)
}

// Characterises current behaviour, not the ADR-0007 target. For every KV
// writer of an instance record: does the generation pair a record already
// carries survive the write, and does a field this binary does not know?
func TestCurrentBehaviour_GenerationAcrossKVWriters(t *testing.T) {
	stopped := func(t *testing.T, kv jetstream.KeyValue) { seedNewerRecord(t, kv, vm.DesiredStopped, vm.StateStopped) }
	running := func(t *testing.T, kv jetstream.KeyValue) { seedNewerRecord(t, kv, vm.DesiredRunning, vm.StateRunning) }

	cases := []struct {
		name       string
		terminated bool
		seed       func(*testing.T, jetstream.KeyValue)
		write      func(*testing.T, *daemon.JetStreamManager)
		preserved  bool
	}{
		{name: "UpdateInstanceRecord", seed: running, preserved: true,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				_, err := m.UpdateInstanceRecord("i-g", func(r *vm.InstanceRecord) { r.Status.Health.RestartCount++ })
				require.NoError(t, err)
			}},
		{name: "WriteInstanceRecord of a loaded record", seed: running, preserved: true,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				r, err := m.LoadInstanceRecord("i-g")
				require.NoError(t, err)
				require.NoError(t, m.WriteInstanceRecord("i-g", r))
			}},
		{name: "ClaimRecoverableInstance", seed: running, preserved: true,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				_, err := m.ClaimRecoverableInstance("i-g", "node-1", "node-2")
				require.NoError(t, err)
			}},
		{name: "AbandonRecovery", seed: running, preserved: true,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				applied, err := m.AbandonRecovery("i-g", "node-1", "Server.HostRecoveryFailed", "test")
				require.NoError(t, err)
				require.True(t, applied)
			}},
		{name: "ClaimStoppedInstance", seed: stopped, preserved: true,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				_, err := m.ClaimStoppedInstance("i-g")
				require.NoError(t, err)
			}},
		{name: "WriteRunningSet", seed: running, preserved: false,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				r, err := m.LoadInstanceRecord("i-g")
				require.NoError(t, err)
				v := vm.VMFromRecord(r)
				v.Health.RestartCount++
				res := m.WriteRunningSet("node-1", "", map[string]*vm.VM{"i-g": v})
				require.Equal(t, 1, res.Written)
			}},
		{name: "UpdateStoppedInstance", seed: stopped, preserved: false,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				_, err := m.UpdateStoppedInstance("i-g", func(v *vm.VM) { v.Health.RestartCount++ })
				require.NoError(t, err)
			}},
		{name: "WriteStoppedInstance", seed: stopped, preserved: false,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				v, err := m.LoadStoppedInstance("i-g")
				require.NoError(t, err)
				require.NoError(t, m.WriteStoppedInstance("i-g", v))
			}},
		{name: "UpdateTerminatedInstanceRecord", terminated: true, seed: running, preserved: true,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				_, err := m.UpdateTerminatedInstanceRecord("i-g", func(r *vm.InstanceRecord) { r.Status.Status = vm.StateTerminated })
				require.NoError(t, err)
			}},
		{name: "UpdateTerminatedInstance", terminated: true, seed: running, preserved: false,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				_, err := m.UpdateTerminatedInstance("i-g", func(v *vm.VM) { v.Status = vm.StateTerminated })
				require.NoError(t, err)
			}},
		{name: "WriteTerminatedInstance", terminated: true, seed: running, preserved: false,
			write: func(t *testing.T, m *daemon.JetStreamManager) {
				v, err := m.LoadTerminatedInstance("i-g")
				require.NoError(t, err)
				require.NoError(t, m.WriteTerminatedInstance("i-g", v))
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, nc := newRecordManagerConn(t)
			bucket := daemon.InstanceStateBucket
			if tc.terminated {
				bucket = daemon.TerminatedInstanceBucket
			}
			kv := bucketKV(t, nc, bucket)
			tc.seed(t, kv)

			tc.write(t, m)

			entry, err := kv.Get(t.Context(), "i.i-g")
			require.NoError(t, err)
			var raw struct {
				Metadata map[string]json.RawMessage `json:"metadata"`
				Spec     map[string]json.RawMessage `json:"spec"`
				Status   map[string]json.RawMessage `json:"status"`
			}
			require.NoError(t, json.Unmarshal(entry.Value(), &raw))

			if tc.preserved {
				assert.JSONEq(t, `7`, string(raw.Metadata["generation"]), "PRESERVED: Generation")
				assert.JSONEq(t, `6`, string(raw.Metadata["observed_generation"]), "PRESERVED: ObservedGeneration")
			} else {
				assert.NotContains(t, raw.Metadata, "generation", "LOST: Generation")
				assert.NotContains(t, raw.Metadata, "observed_generation", "LOST: ObservedGeneration")
			}
			assert.NotContains(t, raw.Spec, "desired_generation", "LOST on every writer: unknown spec field")
			assert.NotContains(t, raw.Status, "assignment_fence", "LOST on every writer: unknown status field")
		})
	}
}

// Characterises current behaviour: reading a record into the running set goes
// through vm.VM, so the generation pair is gone before the recovery merge.
func TestCurrentBehaviour_LoadStateDropsGeneration(t *testing.T) {
	m, nc := newRecordManagerConn(t)
	require.NoError(t, m.WriteNodeMarker("node-1"))
	seedNewerRecord(t, instanceStateKV(t, nc), vm.DesiredRunning, vm.StateRunning)

	vms, found, err := m.LoadState("node-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Contains(t, vms, "i-g")
	rebuilt := vms["i-g"].Record()
	assert.Zero(t, rebuilt.Metadata.Generation, "LOST: Generation")
	assert.Zero(t, rebuilt.Metadata.ObservedGeneration, "LOST: ObservedGeneration")
}
