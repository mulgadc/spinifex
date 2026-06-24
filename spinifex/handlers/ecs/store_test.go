package handlers_ecs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountBucketName(t *testing.T) {
	assert.Equal(t, "ecs-account-123456789012", AccountBucketName("123456789012"))
}

func TestNewStore_NilConn(t *testing.T) {
	_, err := NewStore(nil)
	require.Error(t, err)
}

// Key-path helpers must produce the ecs-v1.md Q2 layout exactly: prefixes are
// what the List* enumerations watch, so a drift here silently breaks listing.
func TestKeyPaths(t *testing.T) {
	assert.Equal(t, "clusters/web/meta", ClusterMetaKey("web"))

	assert.Equal(t, "clusters/web/instances/", InstancesPrefix("web"))
	assert.Equal(t, "clusters/web/instances/i-abc", InstanceKey("web", "i-abc"))

	assert.Equal(t, "clusters/web/tasks/", TasksPrefix("web"))
	assert.Equal(t, "clusters/web/tasks/t-abc", TaskKey("web", "t-abc"))

	assert.Equal(t, "clusters/web/services/", ServicesPrefix("web"))
	assert.Equal(t, "clusters/web/services/api", ServiceKey("web", "api"))

	assert.Equal(t, "taskdef-families/", TaskDefFamiliesPrefix())
	assert.Equal(t, "taskdef-families/nginx/latest-rev", TaskDefLatestRevKey("nginx"))
	assert.Equal(t, "taskdef-families/nginx/revs/", TaskDefRevsPrefix("nginx"))
	assert.Equal(t, "taskdef-families/nginx/revs/3", TaskDefRevKey("nginx", 3))

	assert.Equal(t, "123456789012/web", LeaderLeaseKey("123456789012", "web"))
}

// Prefix helpers must be a true prefix of their per-record key so a KV
// prefix-watch over the prefix sees the record key.
func TestPrefixContainment(t *testing.T) {
	assert.Contains(t, InstanceKey("c", "x"), InstancesPrefix("c"))
	assert.Contains(t, TaskKey("c", "x"), TasksPrefix("c"))
	assert.Contains(t, ServiceKey("c", "x"), ServicesPrefix("c"))
	assert.Contains(t, TaskDefRevKey("f", 1), TaskDefRevsPrefix("f"))
}
