package daemon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migrateFaultKV fails chosen calls on a real bucket: the key listing, reads
// and creates of named keys, and every delete.
type migrateFaultKV struct {
	jetstream.KeyValue

	keysErr   error
	getErr    map[string]error
	createErr map[string]error
	deleteErr error
}

func (k *migrateFaultKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if k.keysErr != nil {
		return nil, k.keysErr
	}
	return k.KeyValue.Keys(ctx, opts...)
}

func (k *migrateFaultKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if err := k.getErr[key]; err != nil {
		return nil, err
	}
	return k.KeyValue.Get(ctx, key)
}

func (k *migrateFaultKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if err := k.createErr[key]; err != nil {
		return 0, err
	}
	return k.KeyValue.Create(ctx, key, value, opts...)
}

func (k *migrateFaultKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if k.deleteErr != nil {
		return k.deleteErr
	}
	return k.KeyValue.Delete(ctx, key, opts...)
}

type migrateCase struct {
	name    string
	seed    map[string][]byte
	fault   migrateFaultKV
	wantErr string
	check   func(t *testing.T, kv jetstream.KeyValue)
}

// runMigrateCases runs each case against its own freshly seeded bucket.
func runMigrateCases(t *testing.T, run func(context.Context, migrate.KVContext) error, cases []migrateCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, f := faultBucket(t)
			for k, v := range tc.seed {
				_, err := f.KeyValue.Put(t.Context(), k, v)
				require.NoError(t, err)
			}
			kv := tc.fault
			kv.KeyValue = f.KeyValue

			err := run(t.Context(), migrate.KVContext{KV: &kv, Logger: slog.Default()})
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
			if tc.check != nil {
				tc.check(t, f.KeyValue)
			}
		})
	}
}

func assertKeyAbsent(t *testing.T, kv jetstream.KeyValue, key string) {
	t.Helper()
	_, err := kv.Get(context.Background(), key)
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "%s must not be written", key)
}

func notFound(key string) map[string]error {
	return map[string]error{key: jetstream.ErrKeyNotFound}
}

func injected(key string) map[string]error {
	return map[string]error{key: errInjected}
}

func TestRekeyRecordSeparator_Faults(t *testing.T) {
	t.Parallel()
	slashed := map[string][]byte{"i/i-1": []byte(`{}`)}

	runMigrateCases(t, rekeyRecordSeparator, []migrateCase{
		{name: "empty bucket"},
		{name: "listing fails", seed: slashed, fault: migrateFaultKV{keysErr: errInjected}, wantErr: "list keys"},
		{
			name: "record vanished mid-pass", seed: slashed, fault: migrateFaultKV{getErr: notFound("i/i-1")},
			check: func(t *testing.T, kv jetstream.KeyValue) { assertKeyAbsent(t, kv, "i.i-1") },
		},
		{name: "read fails", seed: slashed, fault: migrateFaultKV{getErr: injected("i/i-1")}, wantErr: "read i/i-1"},
		{name: "write fails", seed: slashed, fault: migrateFaultKV{createErr: injected("i.i-1")}, wantErr: "write i.i-1"},
		{
			name: "delete fails after the copy", seed: slashed, fault: migrateFaultKV{deleteErr: errInjected},
			wantErr: "delete i/i-1",
			check: func(t *testing.T, kv jetstream.KeyValue) {
				_, err := kv.Get(context.Background(), "i.i-1")
				assert.NoError(t, err, "the copy lands before the delete, so a rerun finds it")
			},
		},
	})
}

func TestCarryNodeOwnershipForward_Faults(t *testing.T) {
	t.Parallel()
	blob := mustMarshal(t, LocalState{SchemaVersion: LocalStateSchemaVersion, VMS: map[string]*vm.VM{"i-1": {ID: "i-1"}}})
	withRecord := map[string][]byte{
		"node.n1": blob,
		"i.i-1":   mustMarshal(t, (&vm.VM{ID: "i-1"}).Record()),
	}

	runMigrateCases(t, carryNodeOwnershipForward, []migrateCase{
		{name: "empty bucket"},
		{name: "listing fails", seed: withRecord, fault: migrateFaultKV{keysErr: errInjected}, wantErr: "list keys"},
		{
			name: "marker write fails", seed: withRecord,
			fault: migrateFaultKV{createErr: injected("nodepresence.n1")}, wantErr: "write nodepresence.n1",
		},
		{
			name: "blob vanished still seeds the marker", seed: withRecord,
			fault: migrateFaultKV{getErr: notFound("node.n1")},
			check: func(t *testing.T, kv jetstream.KeyValue) {
				_, err := kv.Get(context.Background(), "nodepresence.n1")
				assert.NoError(t, err)
			},
		},
		{name: "blob read fails", seed: withRecord, fault: migrateFaultKV{getErr: injected("node.n1")}, wantErr: "read node.n1"},
		{name: "blob corrupt", seed: map[string][]byte{"node.n1": []byte("{")}, wantErr: "decode node.n1"},
		{
			name: "blob from a newer build", seed: map[string][]byte{"node.n1": []byte(`{"schema_version":99}`)},
			wantErr: "newer than this node understands",
		},
		{
			name: "instance without a record is skipped", seed: map[string][]byte{"node.n1": blob},
			check: func(t *testing.T, kv jetstream.KeyValue) { assertKeyAbsent(t, kv, "i.i-1") },
		},
		{
			name: "record read fails", seed: withRecord, fault: migrateFaultKV{getErr: injected("i.i-1")},
			wantErr: "stamp owner on i.i-1",
		},
	})
}

func TestCopyInstancesForward_Faults(t *testing.T) {
	t.Parallel()
	stopped := map[string][]byte{"instance.i-1": mustMarshal(t, &vm.VM{ID: "i-1", InstanceType: "t3.nano"})}
	run := func(ctx context.Context, kvc migrate.KVContext) error {
		return copyInstancesForward(ctx, kvc, StoppedInstancePrefix)
	}

	runMigrateCases(t, run, []migrateCase{
		{name: "empty bucket"},
		{name: "listing fails", seed: stopped, fault: migrateFaultKV{keysErr: errInjected}, wantErr: "list keys"},
		{
			name: "instance vanished mid-pass", seed: stopped, fault: migrateFaultKV{getErr: notFound("instance.i-1")},
			check: func(t *testing.T, kv jetstream.KeyValue) { assertKeyAbsent(t, kv, "i.i-1") },
		},
		{name: "read fails", seed: stopped, fault: migrateFaultKV{getErr: injected("instance.i-1")}, wantErr: "read instance.i-1"},
		{name: "corrupt instance", seed: map[string][]byte{"instance.i-1": []byte("{")}, wantErr: "decode instance.i-1"},
		{name: "write fails", seed: stopped, fault: migrateFaultKV{createErr: injected("i.i-1")}, wantErr: "write i.i-1"},
		{
			name: "existing record is kept",
			seed: map[string][]byte{
				"instance.i-1": stopped["instance.i-1"],
				"i.i-1":        []byte(`{"kept":true}`),
			},
			check: func(t *testing.T, kv jetstream.KeyValue) {
				entry, err := kv.Get(context.Background(), "i.i-1")
				require.NoError(t, err)
				assert.JSONEq(t, `{"kept":true}`, string(entry.Value()))
			},
		},
	})
}

func TestCopyRunningSetsForward_Faults(t *testing.T) {
	t.Parallel()
	set := map[string][]byte{"node.n1": mustMarshal(t, LocalState{
		SchemaVersion: LocalStateSchemaVersion,
		VMS:           map[string]*vm.VM{"i-1": {ID: "i-1"}},
	})}

	runMigrateCases(t, copyRunningSetsForward, []migrateCase{
		{name: "empty bucket"},
		{name: "listing fails", seed: set, fault: migrateFaultKV{keysErr: errInjected}, wantErr: "list keys"},
		{
			name: "set vanished mid-pass", seed: set, fault: migrateFaultKV{getErr: notFound("node.n1")},
			check: func(t *testing.T, kv jetstream.KeyValue) { assertKeyAbsent(t, kv, "i.i-1") },
		},
		{name: "read fails", seed: set, fault: migrateFaultKV{getErr: injected("node.n1")}, wantErr: "read node.n1"},
		{name: "corrupt set", seed: map[string][]byte{"node.n1": []byte("{")}, wantErr: "decode node.n1"},
		{
			name: "set from a newer build", seed: map[string][]byte{"node.n1": []byte(`{"schema_version":99}`)},
			wantErr: "newer than this node understands",
		},
		{
			name:  "nil instance is skipped",
			seed:  map[string][]byte{"node.n1": []byte(`{"schema_version":1,"vms":{"i-1":null}}`)},
			check: func(t *testing.T, kv jetstream.KeyValue) { assertKeyAbsent(t, kv, "i.i-1") },
		},
		{name: "write fails", seed: set, fault: migrateFaultKV{createErr: injected("i.i-1")}, wantErr: "write i.i-1"},
		{
			name: "existing record is kept",
			seed: map[string][]byte{"node.n1": set["node.n1"], "i.i-1": []byte(`{"kept":true}`)},
			check: func(t *testing.T, kv jetstream.KeyValue) {
				entry, err := kv.Get(context.Background(), "i.i-1")
				require.NoError(t, err)
				assert.JSONEq(t, `{"kept":true}`, string(entry.Value()))
			},
		},
	})
}
