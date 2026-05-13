package handlers_ec2_vpc

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sgMigrationBucket(t *testing.T) (nats.KeyValue, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js, err := nc.JetStream()
	require.NoError(t, err)
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "sg-migration-test", History: 1})
	require.NoError(t, err)
	return kv, nc
}

func TestSGMigration_AssignsBlankRuleIDs(t *testing.T) {
	kv, _ := sgMigrationBucket(t)

	record := SecurityGroupRecord{
		GroupId:   "sg-0123456789abcdef0",
		GroupName: "pre-v2",
		VpcId:     "vpc-0123456789abcdef0",
		IngressRules: []SGRule{
			{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/24"},
			{IpProtocol: "tcp", FromPort: 443, ToPort: 443, CidrIp: "0.0.0.0/0"},
		},
		EgressRules: []SGRule{
			{IpProtocol: "-1", FromPort: 0, ToPort: 0, CidrIp: "0.0.0.0/0"},
		},
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = kv.Put(utils.AccountKey(testAccountID, record.GroupId), data)
	require.NoError(t, err)

	ctx := migrate.KVContext{KV: kv, Logger: slog.Default()}
	require.NoError(t, migrateAssignRuleIDs(ctx))

	entry, err := kv.Get(utils.AccountKey(testAccountID, record.GroupId))
	require.NoError(t, err)
	var got SecurityGroupRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &got))

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
	kv, _ := sgMigrationBucket(t)

	record := SecurityGroupRecord{
		GroupId:   "sg-0123456789abcdef0",
		GroupName: "already-v2",
		VpcId:     "vpc-0123456789abcdef0",
		IngressRules: []SGRule{
			{RuleId: "sgr-aaaaaaaaaaaaaaaaa", IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/24"},
		},
		EgressRules: []SGRule{
			{RuleId: "sgr-bbbbbbbbbbbbbbbbb", IpProtocol: "-1", FromPort: 0, ToPort: 0, CidrIp: "0.0.0.0/0"},
		},
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	key := utils.AccountKey(testAccountID, record.GroupId)
	_, err = kv.Put(key, data)
	require.NoError(t, err)

	entryBefore, err := kv.Get(key)
	require.NoError(t, err)
	revBefore := entryBefore.Revision()

	ctx := migrate.KVContext{KV: kv, Logger: slog.Default()}
	require.NoError(t, migrateAssignRuleIDs(ctx))

	entryAfter, err := kv.Get(key)
	require.NoError(t, err)
	assert.Equal(t, revBefore, entryAfter.Revision(), "fully-populated records must not be rewritten")
}

func TestSGMigration_PartialBackfill(t *testing.T) {
	kv, _ := sgMigrationBucket(t)

	record := SecurityGroupRecord{
		GroupId:   "sg-0123456789abcdef0",
		GroupName: "partial",
		VpcId:     "vpc-0123456789abcdef0",
		IngressRules: []SGRule{
			{RuleId: "sgr-aaaaaaaaaaaaaaaaa", IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/24"},
			{IpProtocol: "tcp", FromPort: 80, ToPort: 80, CidrIp: "0.0.0.0/0"},
		},
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = kv.Put(utils.AccountKey(testAccountID, record.GroupId), data)
	require.NoError(t, err)

	ctx := migrate.KVContext{KV: kv, Logger: slog.Default()}
	require.NoError(t, migrateAssignRuleIDs(ctx))

	entry, err := kv.Get(utils.AccountKey(testAccountID, record.GroupId))
	require.NoError(t, err)
	var got SecurityGroupRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &got))
	assert.Equal(t, "sgr-aaaaaaaaaaaaaaaaa", got.IngressRules[0].RuleId)
	assert.Regexp(t, `^sgr-[0-9a-f]{17}$`, got.IngressRules[1].RuleId)
}

func TestSGMigration_EmptyBucket(t *testing.T) {
	kv, _ := sgMigrationBucket(t)

	ctx := migrate.KVContext{KV: kv, Logger: slog.Default()}
	require.NoError(t, migrateAssignRuleIDs(ctx))
}
