package gateway_ecr

import (
	"errors"
	"net/http"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingStore wraps MemoryObjectStore to observe and optionally fail the
// bucket-ensure path.
type countingStore struct {
	*objectstore.MemoryObjectStore

	ensureCalls int
	ensureErr   error
}

func (c *countingStore) EnsureBucket(bucket string) error {
	c.ensureCalls++
	if c.ensureErr != nil {
		return c.ensureErr
	}
	return c.MemoryObjectStore.EnsureBucket(bucket)
}

func TestRegistry_EnsureBucketFailureReturns500(t *testing.T) {
	store := &countingStore{MemoryObjectStore: objectstore.NewMemoryObjectStore(), ensureErr: errors.New("predastore down")}
	reg := NewRegistry(store, ecr.NewMemoryMetaStore(), testAccount)

	w := do(reg, http.MethodGet, "/v2/myrepo/tags/list", nil, nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestRegistry_EnsureBucketCachedAfterSuccess(t *testing.T) {
	store := &countingStore{MemoryObjectStore: objectstore.NewMemoryObjectStore()}
	reg := NewRegistry(store, ecr.NewMemoryMetaStore(), testAccount)

	do(reg, http.MethodGet, "/v2/myrepo/tags/list", nil, nil)
	do(reg, http.MethodGet, "/v2/other/tags/list", nil, nil)
	require.Equal(t, 1, store.ensureCalls, "bucket ensure must be cached after first success")
}
