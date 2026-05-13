package migrate

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sgTestAccountID = "123456789012"

func sgMigrationKV(t *testing.T) nats.KeyValue {
	t.Helper()
	_, nc := startTestNATS(t)
	return createTestBucket(t, nc, "sg-migration-test")
}

func runSGMigration(t *testing.T, kv nats.KeyValue) {
	t.Helper()
	for _, m := range DefaultRegistry.kvMigrations[sgBucket] {
		if m.FromVersion == 1 && m.ToVersion == 2 {
			require.NoError(t, m.Run(KVContext{KV: kv, Logger: slog.Default()}))
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
	_, err = kv.Put(utils.AccountKey(sgTestAccountID, record.GroupId), data)
	require.NoError(t, err)

	runSGMigration(t, kv)

	entry, err := kv.Get(utils.AccountKey(sgTestAccountID, record.GroupId))
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
	_, err = kv.Put(key, data)
	require.NoError(t, err)

	entryBefore, err := kv.Get(key)
	require.NoError(t, err)
	revBefore := entryBefore.Revision()

	runSGMigration(t, kv)

	entryAfter, err := kv.Get(key)
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
	_, err = kv.Put(utils.AccountKey(sgTestAccountID, record.GroupId), data)
	require.NoError(t, err)

	runSGMigration(t, kv)

	entry, err := kv.Get(utils.AccountKey(sgTestAccountID, record.GroupId))
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
