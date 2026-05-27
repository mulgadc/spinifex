package migrate

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func extIPAMMigrationKV(t *testing.T) nats.KeyValue {
	t.Helper()
	_, nc := startTestNATS(t)
	return createTestBucket(t, nc, "ext-ipam-migration-test")
}

func runExtIPAMMigration(t *testing.T, kv nats.KeyValue) {
	t.Helper()
	for _, m := range DefaultRegistry.kvMigrations[extIPAMBucket] {
		if m.FromVersion == 1 && m.ToVersion == 2 {
			require.NoError(t, m.Run(KVContext{KV: kv, Logger: slog.Default()}))
			return
		}
	}
	t.Fatalf("v1→v2 migration not registered for %s", extIPAMBucket)
}

func TestExtIPAMMigration_RenamesType(t *testing.T) {
	kv := extIPAMMigrationKV(t)

	record := extIPAMRecordV1V2{
		PoolName:   "wan",
		RangeStart: "192.168.1.10",
		RangeEnd:   "192.168.1.250",
		Gateway:    "192.168.1.1",
		PrefixLen:  24,
		Allocated: map[string]extIPAllocV1V2{
			"192.168.1.10": {Type: "gateway", Note: "OVN router SNAT address"},
			"192.168.1.11": {Type: "auto_assign", ENIId: "eni-1", InstanceId: "i-1"},
			"192.168.1.12": {Type: "elastic_ip", AllocationID: "eipalloc-1"},
			"192.168.1.13": {Type: "junk-legacy", ENIId: "eni-zombie"},
		},
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = kv.Put("wan", data)
	require.NoError(t, err)

	runExtIPAMMigration(t, kv)

	entry, err := kv.Get("wan")
	require.NoError(t, err)
	var got extIPAMRecordV1V2
	require.NoError(t, json.Unmarshal(entry.Value(), &got))

	assert.Equal(t, "igw-lrp", got.Allocated["192.168.1.10"].Purpose)
	assert.Equal(t, "eni-public", got.Allocated["192.168.1.11"].Purpose)
	assert.Equal(t, "eip", got.Allocated["192.168.1.12"].Purpose)
	assert.Equal(t, "unknown", got.Allocated["192.168.1.13"].Purpose, "unrecognized legacy types tagged unknown")

	for ip, alloc := range got.Allocated {
		assert.Empty(t, alloc.Type, "legacy Type field cleared for %s", ip)
	}

	assert.Equal(t, "eni-1", got.Allocated["192.168.1.11"].ENIId, "non-Type fields preserved")
	assert.Equal(t, "eipalloc-1", got.Allocated["192.168.1.12"].AllocationID)
}

func TestExtIPAMMigration_IdempotentOnAlreadyMigrated(t *testing.T) {
	kv := extIPAMMigrationKV(t)

	record := extIPAMRecordV1V2{
		PoolName: "wan",
		Allocated: map[string]extIPAllocV1V2{
			"192.168.1.10": {Purpose: "igw-lrp"},
			"192.168.1.11": {Purpose: "eni-public", ENIId: "eni-1"},
		},
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = kv.Put("wan", data)
	require.NoError(t, err)
	rev := getKVRevision(t, kv, "wan")

	runExtIPAMMigration(t, kv)

	newRev := getKVRevision(t, kv, "wan")
	assert.Equal(t, rev, newRev, "no write when all records already have Purpose")
}

func TestExtIPAMMigration_EmptyBucket(t *testing.T) {
	kv := extIPAMMigrationKV(t)
	runExtIPAMMigration(t, kv)
}

func getKVRevision(t *testing.T, kv nats.KeyValue, key string) uint64 {
	t.Helper()
	entry, err := kv.Get(key)
	require.NoError(t, err)
	return entry.Revision()
}
