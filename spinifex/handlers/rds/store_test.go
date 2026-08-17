package handlers_rds

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountBucketName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "rds-account-123456789012", AccountBucketName(testAccountID))
}

func TestKeyPaths(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "db-instances/orders-db", DBInstanceKey("orders-db"))
	assert.Equal(t, "db-snapshots/orders-db-final", DBSnapshotKey("orders-db-final"))
	assert.Equal(t, "db-subnet-groups/prod-db-subnets", DBSubnetGroupKey("prod-db-subnets"))
	assert.Equal(t, "db-parameter-groups/pg16/meta", DBParameterGroupMetaKey("pg16"))
	assert.Equal(t, "db-parameter-groups/pg16/params/shared_buffers",
		DBParameterGroupParamKey("pg16", "shared_buffers"))
	assert.Equal(t, "backups/orders-db/automated/20260724T170000Z",
		AutomatedBackupKey("orders-db", "20260724T170000Z"))
	assert.Equal(t, "retained-volumes/vol-abc123", RetainedVolumeKey("vol-abc123"))
	assert.Equal(t, "instance-index/i-abc123", InstanceIndexKey("i-abc123"))
}

// Each key must sit under its own prefix, so an enumeration of one resource type
// can never pick up another's records.
func TestKeyPathsSitUnderTheirPrefix(t *testing.T) {
	t.Parallel()
	assert.True(t, strings.HasPrefix(DBInstanceKey("x"), DBInstancesPrefix()))
	assert.True(t, strings.HasPrefix(DBSnapshotKey("x"), DBSnapshotsPrefix()))
	assert.True(t, strings.HasPrefix(DBSubnetGroupKey("x"), DBSubnetGroupsPrefix()))
	assert.True(t, strings.HasPrefix(DBParameterGroupMetaKey("x"), DBParameterGroupsPrefix()))
	assert.True(t, strings.HasPrefix(DBParameterGroupParamKey("x", "p"), DBParameterGroupParamsPrefix("x")))
	assert.True(t, strings.HasPrefix(AutomatedBackupKey("x", "t"), AutomatedBackupsPrefix("x")))
	assert.True(t, strings.HasPrefix(RetainedVolumeKey("v"), RetainedVolumesPrefix()))
	assert.True(t, strings.HasPrefix(InstanceIndexKey("i"), InstanceIndexPrefix()))

	// A parameter group's values must not be reachable by listing groups' meta
	// records, or DescribeDBParameterGroups would report one group per parameter.
	assert.False(t, strings.HasPrefix(DBParameterGroupParamKey("x", "p"), DBParameterGroupMetaKey("x")))
}

func TestNewStore_NilConn(t *testing.T) {
	t.Parallel()
	_, err := NewStore(nil)
	require.Error(t, err)
}

func TestNewStore_Valid(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	s, err := NewStore(nc)
	require.NoError(t, err)
	require.NotNil(t, s)
}

func TestGetOrCreateAccountBucket_Idempotent(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	kv1, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, kv1)

	kv2, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, kv2)

	assert.Equal(t, AccountBucketName(testAccountID), kv1.Bucket())
	assert.Equal(t, kv1.Bucket(), kv2.Bucket())
}

func TestGetOrCreateSystemBucket_Idempotent(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	kv1, err := GetOrCreateSystemBucket(t.Context(), js)
	require.NoError(t, err)
	kv2, err := GetOrCreateSystemBucket(t.Context(), js)
	require.NoError(t, err)

	assert.Equal(t, KVBucketRDSSystem, kv1.Bucket())
	assert.Equal(t, kv1.Bucket(), kv2.Bucket())
}
