package migrate

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupIPAMMigrationKV(t *testing.T) (nats.KeyValue, nats.JetStreamContext) {
	t.Helper()
	_, nc := startTestNATS(t)
	js, err := nc.JetStream()
	require.NoError(t, err)
	kv := createTestBucket(t, nc, ipamBucket)
	return kv, js
}

func runIPAMMigration(t *testing.T, kv nats.KeyValue, js nats.JetStreamContext) {
	t.Helper()
	for _, m := range DefaultRegistry.kvMigrations[ipamBucket] {
		if m.FromVersion == 1 && m.ToVersion == 2 {
			require.NoError(t, m.Run(KVContext{KV: kv, JetStream: js, Logger: slog.Default()}))
			return
		}
	}
	t.Fatalf("v1→v2 migration not registered for %s", ipamBucket)
}

func seedENIBucket(t *testing.T, js nats.JetStreamContext, enis []eniRecordSnapshot) {
	t.Helper()
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: eniBucket, History: 1})
	require.NoError(t, err)
	for _, eni := range enis {
		data, err := json.Marshal(eni)
		require.NoError(t, err)
		_, err = kv.Put(eni.NetworkInterfaceId, data)
		require.NoError(t, err)
	}
}

func TestIPAMMigration_ConvertsV1ToV2WithOwnerBackfill(t *testing.T) {
	kv, js := setupIPAMMigrationKV(t)
	seedENIBucket(t, js, []eniRecordSnapshot{
		{NetworkInterfaceId: "eni-aaa", SubnetId: "subnet-1", PrivateIpAddress: "10.0.1.4"},
		{NetworkInterfaceId: "eni-bbb", SubnetId: "subnet-1", PrivateIpAddress: "10.0.1.5"},
		{NetworkInterfaceId: "eni-ccc", SubnetId: "subnet-2", PrivateIpAddress: "10.0.2.4"},
	})

	type legacyRecord struct {
		SubnetId  string   `json:"subnet_id"`
		CidrBlock string   `json:"cidr_block"`
		Allocated []string `json:"allocated"`
	}
	subnet1 := legacyRecord{
		SubnetId:  "subnet-1",
		CidrBlock: "10.0.1.0/24",
		Allocated: []string{"10.0.1.4", "10.0.1.5", "10.0.1.99"}, // .99 has no owning ENI
	}
	data, err := json.Marshal(subnet1)
	require.NoError(t, err)
	_, err = kv.Put("subnet-1", data)
	require.NoError(t, err)

	subnet2 := legacyRecord{
		SubnetId:  "subnet-2",
		CidrBlock: "10.0.2.0/24",
		Allocated: []string{"10.0.2.4"},
	}
	data, err = json.Marshal(subnet2)
	require.NoError(t, err)
	_, err = kv.Put("subnet-2", data)
	require.NoError(t, err)

	runIPAMMigration(t, kv, js)

	entry, err := kv.Get("subnet-1")
	require.NoError(t, err)
	var got ipamRecordV2
	require.NoError(t, json.Unmarshal(entry.Value(), &got))

	require.Len(t, got.Allocated, 3)
	assert.Equal(t, "10.0.1.4", got.Allocated[0].IP)
	assert.Equal(t, "eni-primary", got.Allocated[0].Purpose)
	assert.Equal(t, "eni-aaa", got.Allocated[0].OwnerID)
	assert.Equal(t, "10.0.1.5", got.Allocated[1].IP)
	assert.Equal(t, "eni-bbb", got.Allocated[1].OwnerID)
	assert.Equal(t, "10.0.1.99", got.Allocated[2].IP)
	assert.Equal(t, "unknown", got.Allocated[2].Purpose, "orphan IP tagged unknown")
	assert.Empty(t, got.Allocated[2].OwnerID)

	entry, err = kv.Get("subnet-2")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(entry.Value(), &got))
	require.Len(t, got.Allocated, 1)
	assert.Equal(t, "eni-ccc", got.Allocated[0].OwnerID)
}

func TestIPAMMigration_IdempotentOnAlreadyV2(t *testing.T) {
	kv, js := setupIPAMMigrationKV(t)

	v2 := ipamRecordV2{
		SubnetId:  "subnet-1",
		CidrBlock: "10.0.1.0/24",
		Allocated: []ipEntryV2{
			{IP: "10.0.1.4", Purpose: "eni-primary", OwnerID: "eni-aaa"},
		},
	}
	data, err := json.Marshal(v2)
	require.NoError(t, err)
	_, err = kv.Put("subnet-1", data)
	require.NoError(t, err)
	rev := getKVRevision(t, kv, "subnet-1")

	runIPAMMigration(t, kv, js)

	newRev := getKVRevision(t, kv, "subnet-1")
	assert.Equal(t, rev, newRev, "no write when record is already v2")
}

func TestIPAMMigration_NoJetStream_TagsUnknown(t *testing.T) {
	_, nc := startTestNATS(t)
	kv := createTestBucket(t, nc, ipamBucket)

	type legacyRecord struct {
		SubnetId  string   `json:"subnet_id"`
		CidrBlock string   `json:"cidr_block"`
		Allocated []string `json:"allocated"`
	}
	rec := legacyRecord{
		SubnetId:  "subnet-1",
		CidrBlock: "10.0.1.0/24",
		Allocated: []string{"10.0.1.4"},
	}
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = kv.Put("subnet-1", data)
	require.NoError(t, err)

	// Run migration with nil JetStream — schema conversion still runs but
	// every entry becomes Purpose=unknown.
	runIPAMMigration(t, kv, nil)

	entry, err := kv.Get("subnet-1")
	require.NoError(t, err)
	var got ipamRecordV2
	require.NoError(t, json.Unmarshal(entry.Value(), &got))
	require.Len(t, got.Allocated, 1)
	assert.Equal(t, "unknown", got.Allocated[0].Purpose)
}

func TestIPAMMigration_EmptyBucket(t *testing.T) {
	kv, js := setupIPAMMigrationKV(t)
	runIPAMMigration(t, kv, js)
}
