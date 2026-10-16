package tags

//test:in-package — entries are seeded in the private KV and legacy object
// layouts the scan has to read.

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/internal/testkit/readonlystore"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type listFailingObjectStore struct {
	*objectstore.MemoryObjectStore
}

func (s *listFailingObjectStore) ListObjectsV2(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	return nil, errors.New("injected listing failure")
}

// The scan returns the IDs DescribeTags would, from KV and legacy objects,
// limited to the asked prefixes and to the account, without any write.
func TestScanAccountResourceIDsListsPrefixedIDsFromBothStores(t *testing.T) {
	kv := testTagsBucket(t)
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	writer := NewTagsServiceImplWithStore(cfg, store, kv)
	for account, ids := range map[string][]string{
		"1111":  {"pg-0kv", "lt-0kv", "key-0kv", "i-0ignored", "vpc-0ignored"},
		"11112": {"pg-0neighbour"},
	} {
		for _, id := range ids {
			require.NoError(t, writer.PutResourceTags(t.Context(), account, id, map[string]string{"team": "secret"}))
		}
	}
	seedLegacyTags(t, store, "test-bucket", "1111", "pg-0legacy", map[string]string{"team": "secret"})

	reader := NewTagsServiceImplWithStore(cfg, readonlystore.NewObjectStore(t, store), readonlystore.NewKV(t, kv))
	ids, err := reader.ScanAccountResourceIDs(t.Context(), "1111", "key-", "pg-", "lt-")
	require.NoError(t, err)
	assert.Equal(t, []string{"key-0kv", "lt-0kv", "pg-0kv", "pg-0legacy"}, ids)
}

// A listing failure is an error, never an empty index.
func TestScanAccountResourceIDsReturnsAListingFailure(t *testing.T) {
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewTagsServiceImplWithStore(cfg, &listFailingObjectStore{MemoryObjectStore: objectstore.NewMemoryObjectStore()}, testTagsBucket(t))
	_, err := svc.ScanAccountResourceIDs(t.Context(), "1111", "pg-")
	require.Error(t, err)
}
