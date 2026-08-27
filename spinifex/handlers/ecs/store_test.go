package handlers_ecs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func TestAccountBucketName(t *testing.T) {
	assert.Equal(t, "ecs-account-123456789012", AccountBucketName(testAccountID))
}

func TestNewStore_NilConn(t *testing.T) {
	_, err := NewStore(nil)
	require.Error(t, err)
}

func TestNewStore_Valid(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	s, err := NewStore(nc)
	require.NoError(t, err)
	require.NotNil(t, s)
}

func TestGetOrCreateAccountBucket_Idempotent(t *testing.T) {
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

func TestInitLeaderBucket_Idempotent(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)

	kv1, err := InitLeaderBucket(t.Context(), js)
	require.NoError(t, err)
	require.NotNil(t, kv1)
	assert.Equal(t, KVBucketECSLeader, kv1.Bucket())

	kv2, err := InitLeaderBucket(t.Context(), js)
	require.NoError(t, err)
	require.NotNil(t, kv2)
	assert.Equal(t, KVBucketECSLeader, kv2.Bucket())
}

// The scheduler calls InitLeaderBucket on every tick. Creating first and falling
// back on already-exists reaches the same handle, so idempotency alone cannot
// tell the two orders apart — only the API traffic can.
//
// On a multi-node cluster the stored bucket is replicated while this call asks
// for the default, and JetStream answers the mismatch with an error rather than
// a no-op: 10058, once per tick, forever. That is invisible from the caller and
// showed up only as a 16% JetStream API error rate on the meta leader.
func TestInitLeaderBucket_AttachesWithoutCreating(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)

	// Watch from before the first call. The server queues the API advisory after
	// the reply the caller waits on, so subscribing once the bucket exists can
	// still catch the create that first call legitimately made.
	creates := make(chan string, 8)
	sub, err := nc.Subscribe("$JS.EVENT.ADVISORY.API", func(m *nats.Msg) {
		var adv struct {
			Subject string `json:"subject"`
		}
		if json.Unmarshal(m.Data, &adv) == nil &&
			strings.HasPrefix(adv.Subject, "$JS.API.STREAM.CREATE.") {
			creates <- adv.Subject
		}
	})
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()
	require.NoError(t, nc.Flush())

	_, err = InitLeaderBucket(t.Context(), js)
	require.NoError(t, err)

	// Take the first, legitimate create off the channel before the call under
	// test, so anything left there afterwards can only have come from it.
	select {
	case <-creates:
	case <-time.After(5 * time.Second):
		t.Fatal("the first InitLeaderBucket must create the bucket")
	}

	_, err = InitLeaderBucket(t.Context(), js)
	require.NoError(t, err)
	require.NoError(t, nc.Flush())

	select {
	case subject := <-creates:
		t.Fatalf("InitLeaderBucket issued %s on an existing bucket; attach before create", subject)
	case <-time.After(250 * time.Millisecond):
	}
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
