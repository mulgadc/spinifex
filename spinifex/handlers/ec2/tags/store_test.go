// unexported, and a test that could only reach them through DescribeTags could
// not tell a migrated resource from one that was never written.
//
//test:in-package — the read-through fallback and the key layout are
package handlers_ec2_tags

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestTagsCluster returns n independent service values sharing one bucket and
// one object store — the shape a multi-node cluster presents, where each node
// has its own service struct and only the store is common.
func newTestTagsCluster(t *testing.T, n int) []*TagsServiceImpl {
	t.Helper()
	kv := testTagsBucket(t)
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}

	nodes := make([]*TagsServiceImpl, n)
	for i := range nodes {
		nodes[i] = NewTagsServiceImplWithStore(cfg, store, kv)
	}
	return nodes
}

// seedLegacyTags writes a resource's tags where the store kept them before the
// move to KV, so a test can stand in for a cluster upgraded with tags already in
// place.
func seedLegacyTags(t *testing.T, store *objectstore.MemoryObjectStore, bucket, accountID, resourceID string, tags map[string]string) {
	t.Helper()
	data, err := json.Marshal(tags)
	require.NoError(t, err)
	_, err = store.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(getTagsKey(accountID, resourceID)),
		Body:   bytes.NewReader(data),
	})
	require.NoError(t, err)
}

// tagMap reads a describe back into a map, so an assertion names the keys it
// wants rather than an index into a slice whose order is not promised.
func tagMap(t *testing.T, svc *TagsServiceImpl, resourceID string) map[string]string {
	t.Helper()
	out, err := svc.DescribeTags(t.Context(), &ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("resource-id"), Values: []*string{aws.String(resourceID)}},
		},
	}, testAccountID)
	require.NoError(t, err)

	got := make(map[string]string, len(out.Tags))
	for _, tag := range out.Tags {
		got[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	return got
}

// This is the gap itself. Six writers each adding one key used to leave four.
//
// Each writer gets its own service value, because that is what makes the test
// able to fail: the old code serialised the merge on a mutex held by the service
// struct, so every caller inside one process was already safe and only a second
// node could lose an update. Driving one service from six goroutines would pass
// against the very code this test exists to catch.
func TestCreateTags_ConcurrentWritersOnSeparateNodesAllLand(t *testing.T) {
	const resourceID = "sg-race"
	const writers = 6

	nodes := newTestTagsCluster(t, writers)

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i, node := range nodes {
		wg.Go(func() {
			_, errs[i] = node.CreateTags(context.Background(), &ec2.CreateTagsInput{
				Resources: []*string{aws.String(resourceID)},
				Tags: []*ec2.Tag{
					{Key: aws.String(fmt.Sprintf("k%d", i)), Value: aws.String(fmt.Sprintf("v%d", i))},
				},
			}, testAccountID)
		})
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "writer %d", i)
	}

	got := tagMap(t, nodes[0], resourceID)
	require.Len(t, got, writers)
	for i := range writers {
		assert.Equal(t, fmt.Sprintf("v%d", i), got[fmt.Sprintf("k%d", i)])
	}
}

// A concurrent delete and add must not resurrect the deleted key or drop the
// added one, which is the same lost update seen from the other direction.
func TestDeleteTags_ConcurrentWithCreateKeepsBothDecisions(t *testing.T) {
	nodes := newTestTagsCluster(t, 2)
	svc, other := nodes[0], nodes[1]
	const resourceID = "sg-mixed"

	_, err := svc.CreateTags(t.Context(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String(resourceID)},
		Tags:      []*ec2.Tag{{Key: aws.String("doomed"), Value: aws.String("1")}},
	}, testAccountID)
	require.NoError(t, err)

	var wg sync.WaitGroup
	var addErr, delErr error
	wg.Go(func() {
		_, addErr = svc.CreateTags(context.Background(), &ec2.CreateTagsInput{
			Resources: []*string{aws.String(resourceID)},
			Tags:      []*ec2.Tag{{Key: aws.String("kept"), Value: aws.String("2")}},
		}, testAccountID)
	})
	wg.Go(func() {
		_, delErr = other.DeleteTags(context.Background(), &ec2.DeleteTagsInput{
			Resources: []*string{aws.String(resourceID)},
			Tags:      []*ec2.Tag{{Key: aws.String("doomed")}},
		}, testAccountID)
	})
	wg.Wait()
	require.NoError(t, addErr)
	require.NoError(t, delErr)

	got := tagMap(t, svc, resourceID)
	assert.Equal(t, map[string]string{"kept": "2"}, got)
}

// A cluster upgraded with tags already written must not lose them the first time
// something writes a new one.
func TestCreateTags_ReadsThroughToPreMoveTags(t *testing.T) {
	svc, store := setupTestTagsService(t)
	const resourceID = "vpc-legacy"
	seedLegacyTags(t, store, svc.config.Predastore.Bucket, testAccountID, resourceID, map[string]string{
		"Name": "old", "owner": "platform",
	})

	assert.Equal(t, map[string]string{"Name": "old", "owner": "platform"}, tagMap(t, svc, resourceID))

	_, err := svc.CreateTags(t.Context(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String(resourceID)},
		Tags:      []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"Name": "old", "owner": "platform", "env": "prod"}, tagMap(t, svc, resourceID))

	// The resource is migrated: its tags now answer from the bucket, so a second
	// write no longer depends on the object store being reachable.
	_, found, err := svc.readKVTags(t.Context(), testAccountID, resourceID)
	require.NoError(t, err)
	assert.True(t, found)
}

// DeleteTags on a resource that has never been written since the move has to see
// the pre-move keys too, or a delete silently does nothing.
func TestDeleteTags_RemovesAPreMoveTag(t *testing.T) {
	svc, store := setupTestTagsService(t)
	const resourceID = "subnet-legacy"
	seedLegacyTags(t, store, svc.config.Predastore.Bucket, testAccountID, resourceID, map[string]string{
		"Name": "old", "temp": "yes",
	})

	_, err := svc.DeleteTags(t.Context(), &ec2.DeleteTagsInput{
		Resources: []*string{aws.String(resourceID)},
		Tags:      []*ec2.Tag{{Key: aws.String("temp")}},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"Name": "old"}, tagMap(t, svc, resourceID))
}

// A resource in one store, the other, or both is listed once.
func TestDescribeTags_UnionsBothStoresWithoutDuplicating(t *testing.T) {
	svc, store := setupTestTagsService(t)
	bucket := svc.config.Predastore.Bucket

	seedLegacyTags(t, store, bucket, testAccountID, "vpc-only-legacy", map[string]string{"a": "1"})
	seedLegacyTags(t, store, bucket, testAccountID, "vpc-both", map[string]string{"a": "stale"})

	_, err := svc.CreateTags(t.Context(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("vpc-only-kv")},
		Tags:      []*ec2.Tag{{Key: aws.String("b"), Value: aws.String("2")}},
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateTags(t.Context(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("vpc-both")},
		Tags:      []*ec2.Tag{{Key: aws.String("a"), Value: aws.String("fresh")}},
	}, testAccountID)
	require.NoError(t, err)

	ids, err := svc.taggedResourceIDs(t.Context(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"vpc-both", "vpc-only-kv", "vpc-only-legacy"}, ids)

	// The migrated resource answers from the bucket, so the object it was
	// seeded from cannot shadow the value written since.
	assert.Equal(t, map[string]string{"a": "fresh"}, tagMap(t, svc, "vpc-both"))
}

// Deleting a migrated resource's tags has to clear the pre-move object too,
// or the fallback read hands the old set straight back.
func TestDeleteAllTags_ClearsBothStores(t *testing.T) {
	svc, store := setupTestTagsService(t)
	const resourceID = "i-terminated"
	seedLegacyTags(t, store, svc.config.Predastore.Bucket, testAccountID, resourceID, map[string]string{"Name": "gone"})

	_, err := svc.CreateTags(t.Context(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String(resourceID)},
		Tags:      []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("test")}},
	}, testAccountID)
	require.NoError(t, err)

	require.NoError(t, svc.DeleteAllTags(t.Context(), testAccountID, resourceID))

	assert.Empty(t, tagMap(t, svc, resourceID))

	ids, err := svc.taggedResourceIDs(t.Context(), testAccountID)
	require.NoError(t, err)
	assert.NotContains(t, ids, resourceID)

	// Idempotent: teardown runs more than once.
	require.NoError(t, svc.DeleteAllTags(t.Context(), testAccountID, resourceID))
}

// One account's keys must not be readable through another's listing, which the
// key layout is the only thing enforcing now the account is a key prefix rather
// than a directory.
func TestTaggedResourceIDs_ScopesToTheAccount(t *testing.T) {
	svc, _ := setupTestTagsService(t)
	const otherAccount = "222222222222"

	_, err := svc.CreateTags(t.Context(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("vpc-mine")},
		Tags:      []*ec2.Tag{{Key: aws.String("a"), Value: aws.String("1")}},
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateTags(t.Context(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("vpc-theirs")},
		Tags:      []*ec2.Tag{{Key: aws.String("a"), Value: aws.String("1")}},
	}, otherAccount)
	require.NoError(t, err)

	ids, err := svc.taggedResourceIDs(t.Context(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"vpc-mine"}, ids)
}

// PutResourceTags replaces the whole set rather than merging, because its caller
// is projecting a record that is already the source of truth.
func TestPutResourceTags_ReplacesTheWholeSet(t *testing.T) {
	svc, _ := setupTestTagsService(t)
	const resourceID = "i-projected"

	require.NoError(t, svc.PutResourceTags(t.Context(), testAccountID, resourceID, map[string]string{"a": "1", "b": "2"}))
	require.NoError(t, svc.PutResourceTags(t.Context(), testAccountID, resourceID, map[string]string{"a": "3"}))

	assert.Equal(t, map[string]string{"a": "3"}, tagMap(t, svc, resourceID))
}
