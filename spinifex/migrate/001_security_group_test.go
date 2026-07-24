package migrate

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sgTestAccountID = "123456789012"

func sgMigrationKV(t *testing.T) jetstream.KeyValue {
	t.Helper()
	_, nc := startTestNATS(t)
	return createTestBucket(t, nc, "sg-migration-test")
}

func runSGMigration(t *testing.T, kv jetstream.KeyValue) {
	t.Helper()
	for _, m := range DefaultRegistry.kvMigrations[sgBucket] {
		if m.FromVersion == 1 && m.ToVersion == 2 {
			require.NoError(t, m.Run(t.Context(), KVContext{KV: kv, Logger: slog.Default()}))
			return
		}
	}
	t.Fatalf("v1→v2 migration not registered for %s", sgBucket)
}

func TestSGMigration_AssignsBlankRuleIDs(t *testing.T) {
	kv := sgMigrationKV(t)

	record := sgRecord{
		GroupId:   "sg-0123456789abcdef0",
		GroupName: "pre-v2",
		VpcId:     "vpc-0123456789abcdef0",
		IngressRules: []sgRule{
			{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/24"},
			{IpProtocol: "tcp", FromPort: 443, ToPort: 443, CidrIp: "0.0.0.0/0"},
		},
		EgressRules: []sgRule{
			{IpProtocol: "-1", FromPort: 0, ToPort: 0, CidrIp: "0.0.0.0/0"},
		},
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), utils.AccountKey(sgTestAccountID, record.GroupId), data)
	require.NoError(t, err)

	runSGMigration(t, kv)

	entry, err := kv.Get(t.Context(), utils.AccountKey(sgTestAccountID, record.GroupId))
	require.NoError(t, err)
	var got sgRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &got))

	assert.Equal(t, record.GroupId, got.GroupId, "non-rule fields must be preserved")
	assert.Equal(t, record.VpcId, got.VpcId)
	require.Len(t, got.IngressRules, 2)
	require.Len(t, got.EgressRules, 1)
	seen := make(map[string]struct{})
	for _, r := range got.IngressRules {
		assert.Regexp(t, `^sgr-[0-9a-f]{17}$`, r.RuleId)
		seen[r.RuleId] = struct{}{}
	}
	for _, r := range got.EgressRules {
		assert.Regexp(t, `^sgr-[0-9a-f]{17}$`, r.RuleId)
		seen[r.RuleId] = struct{}{}
	}
	assert.Len(t, seen, 3, "all assigned IDs must be unique")
}

func TestSGMigration_IdempotentOnAlreadyMigrated(t *testing.T) {
	kv := sgMigrationKV(t)

	record := sgRecord{
		GroupId:   "sg-0123456789abcdef0",
		GroupName: "already-v2",
		VpcId:     "vpc-0123456789abcdef0",
		IngressRules: []sgRule{
			{RuleId: "sgr-aaaaaaaaaaaaaaaaa", IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/24"},
		},
		EgressRules: []sgRule{
			{RuleId: "sgr-bbbbbbbbbbbbbbbbb", IpProtocol: "-1", FromPort: 0, ToPort: 0, CidrIp: "0.0.0.0/0"},
		},
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	key := utils.AccountKey(sgTestAccountID, record.GroupId)
	_, err = kv.Put(t.Context(), key, data)
	require.NoError(t, err)

	entryBefore, err := kv.Get(t.Context(), key)
	require.NoError(t, err)
	revBefore := entryBefore.Revision()

	runSGMigration(t, kv)

	entryAfter, err := kv.Get(t.Context(), key)
	require.NoError(t, err)
	assert.Equal(t, revBefore, entryAfter.Revision(), "fully-populated records must not be rewritten")
}

func TestSGMigration_PartialBackfill(t *testing.T) {
	kv := sgMigrationKV(t)

	record := sgRecord{
		GroupId:   "sg-0123456789abcdef0",
		GroupName: "partial",
		VpcId:     "vpc-0123456789abcdef0",
		IngressRules: []sgRule{
			{RuleId: "sgr-aaaaaaaaaaaaaaaaa", IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/24"},
			{IpProtocol: "tcp", FromPort: 80, ToPort: 80, CidrIp: "0.0.0.0/0"},
		},
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), utils.AccountKey(sgTestAccountID, record.GroupId), data)
	require.NoError(t, err)

	runSGMigration(t, kv)

	entry, err := kv.Get(t.Context(), utils.AccountKey(sgTestAccountID, record.GroupId))
	require.NoError(t, err)
	var got sgRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &got))
	assert.Equal(t, "sgr-aaaaaaaaaaaaaaaaa", got.IngressRules[0].RuleId)
	assert.Regexp(t, `^sgr-[0-9a-f]{17}$`, got.IngressRules[1].RuleId)
}

func TestSGMigration_EmptyBucket(t *testing.T) {
	kv := sgMigrationKV(t)
	runSGMigration(t, kv)
}

// casConflictKV wraps a jetstream.KeyValue and forces the next failuresLeft
// Update calls to return jetstream.ErrKeyExists, simulating the JetStream
// wrong-last-sequence response that backfillSGRuleIDs retries on.
type casConflictKV struct {
	jetstream.KeyValue

	failuresLeft int
}

func (k *casConflictKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if k.failuresLeft > 0 {
		k.failuresLeft--
		return 0, jetstream.ErrKeyExists
	}
	return k.KeyValue.Update(ctx, key, value, last)
}

func seedSGRecord(t *testing.T, kv jetstream.KeyValue) string {
	t.Helper()
	record := sgRecord{
		GroupId:   "sg-0123456789abcdef0",
		GroupName: "cas",
		VpcId:     "vpc-0123456789abcdef0",
		IngressRules: []sgRule{
			{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/24"},
		},
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	key := utils.AccountKey(sgTestAccountID, record.GroupId)
	_, err = kv.Put(t.Context(), key, data)
	require.NoError(t, err)
	return key
}

func TestSGMigration_RetriesOnCASConflict(t *testing.T) {
	kv := sgMigrationKV(t)
	key := seedSGRecord(t, kv)

	stub := &casConflictKV{KeyValue: kv, failuresLeft: sgMigrationMaxRetries - 1}
	err := backfillSGRuleIDs(t.Context(), KVContext{KV: stub, Logger: slog.Default()}, key)
	require.NoError(t, err)
	assert.Equal(t, 0, stub.failuresLeft, "all injected conflicts must have been consumed")

	entry, err := kv.Get(t.Context(), key)
	require.NoError(t, err)
	var got sgRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &got))
	require.Len(t, got.IngressRules, 1)
	assert.Regexp(t, `^sgr-[0-9a-f]{17}$`, got.IngressRules[0].RuleId)
}

func TestSGMigration_FailsAfterCASRetryExhaustion(t *testing.T) {
	kv := sgMigrationKV(t)
	key := seedSGRecord(t, kv)

	stub := &casConflictKV{KeyValue: kv, failuresLeft: sgMigrationMaxRetries + 1}
	err := backfillSGRuleIDs(t.Context(), KVContext{KV: stub, Logger: slog.Default()}, key)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeded 3 CAS retries")
	assert.ErrorIs(t, err, jetstream.ErrKeyExists, "exhaustion error must wrap the underlying CAS conflict")

	entry, err := kv.Get(t.Context(), key)
	require.NoError(t, err)
	var got sgRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &got))
	assert.Empty(t, got.IngressRules[0].RuleId, "record must be untouched after retry exhaustion")
}
