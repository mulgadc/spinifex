package gateway_ecr

import (
	"context"

	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blobPutFailStore wraps a MemoryObjectStore and fails PutObject for blob-pool
// keys, simulating a predastore write error during upload finalize.
type blobPutFailStore struct {
	*objectstore.MemoryObjectStore
}

func (s *blobPutFailStore) PutObject(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if input.Key != nil && strings.HasPrefix(*input.Key, "blobs/") {
		return nil, errors.New("predastore unavailable")
	}
	return s.MemoryObjectStore.PutObject(ctx, input)
}

// TestFinishUpload_StoreFailure_CleansUp proves that when the blob PutObject
// fails on finalize, neither the temp upload object nor the upload KV record is
// left orphaned (so the uuid is reusable and no partial blob lingers).
func TestFinishUpload_StoreFailure_CleansUp(t *testing.T) {
	mem := objectstore.NewMemoryObjectStore()
	store := &blobPutFailStore{MemoryObjectStore: mem}
	meta := ecr.NewMemoryMetaStore()
	reg := NewRegistry(store, meta, testAccount)
	seedRepo(t, reg, "team/app")

	w := do(reg, http.MethodPost, "/v2/team/app/blobs/uploads/", nil, nil)
	require.Equal(t, http.StatusAccepted, w.Code)
	loc := w.Header().Get("Location")

	// PATCH a chunk so a temp upload object exists, then finalize (empty body)
	// where the blob PutObject fails — both the temp object and the upload
	// record must be cleaned up.
	data := []byte("a blob that will fail to store")
	w = do(reg, http.MethodPatch, loc, data, nil)
	require.Equal(t, http.StatusAccepted, w.Code)

	dg := digestOf(data)
	w = do(reg, http.MethodPut, loc+"?digest="+dg, nil, nil)
	require.Equal(t, http.StatusInternalServerError, w.Code)

	uploadID := strings.TrimPrefix(loc, "/v2/team/app/blobs/uploads/")
	_, _, err := meta.GetUpload(context.Background(), testAccount, uploadID)
	assert.ErrorIs(t, err, ecr.ErrNotFound, "upload record must be cleaned up after finalize failure")

	listed, err := mem.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: awsString(ecr.AccountBucket(testAccount)),
		Prefix: awsString("uploads/"),
	})
	require.NoError(t, err)
	assert.Empty(t, listed.Contents, "no temp upload bytes may linger after finalize failure")
}

func awsString(s string) *string { return &s }

// TestUploadCAS_SameRevisionSingleWinner proves the serialization point: two
// updates issued from the same observed revision cannot both commit. One wins,
// the other gets ErrConflict — so a racing PATCH is rejected, never merged.
func TestUploadCAS_SameRevisionSingleWinner(t *testing.T) {
	meta := ecr.NewMemoryMetaStore()
	rev, err := meta.PutUpload(context.Background(), testAccount, "u-race", ecr.UploadState{RepoName: "r"})
	require.NoError(t, err)

	stA := ecr.UploadState{RepoName: "r", CommittedBytes: 8, BytesKey: "uploads/u-race/a"}
	stB := ecr.UploadState{RepoName: "r", CommittedBytes: 8, BytesKey: "uploads/u-race/b"}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, st := range []ecr.UploadState{stA, stB} {
		wg.Add(1)
		go func(idx int, s ecr.UploadState) {
			defer wg.Done()
			_, errs[idx] = meta.UpdateUpload(context.Background(), testAccount, "u-race", s, rev)
		}(i, st)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			wins++
		case errors.Is(e, ecr.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", e)
		}
	}
	assert.Equal(t, 1, wins, "exactly one CAS update may commit")
	assert.Equal(t, 1, conflicts, "the racing update must conflict, not silently merge")

	// The committed record points at exactly one byte key — never a mix.
	final, _, err := meta.GetUpload(context.Background(), testAccount, "u-race")
	require.NoError(t, err)
	assert.Contains(t, []string{"uploads/u-race/a", "uploads/u-race/b"}, final.BytesKey)
}

// TestPatchUpload_ConcurrentNoCorruption fires overlapping chunked PATCHes and
// finalizes. Whichever PATCHes commit, the bytes finally stored under the blob
// digest must equal the bytes the hash was computed over — i.e. the recorded
// hash state and the recorded byte object never desynchronise.
func TestPatchUpload_ConcurrentNoCorruption(t *testing.T) {
	for iter := range 25 {
		reg := newTestRegistry()
		repo := "team/race"
		seedRepo(t, reg, repo)

		w := do(reg, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", nil, nil)
		require.Equal(t, http.StatusAccepted, w.Code)
		loc := w.Header().Get("Location")

		var wg sync.WaitGroup
		for _, chunk := range [][]byte{[]byte("AAAA"), []byte("BBBB"), []byte("CCCC")} {
			wg.Add(1)
			go func(body []byte) {
				defer wg.Done()
				do(reg, http.MethodPatch, loc, body, nil)
			}(chunk)
		}
		wg.Wait()

		// Read the committed bytes the server recorded, finalize against their
		// digest, and require the stored blob to match exactly.
		st, _, err := reg.Meta.GetUpload(context.Background(), testAccount, strings.TrimPrefix(loc, "/v2/"+repo+"/blobs/uploads/"))
		require.NoError(t, err)
		committed := reg.readUploadBytesAt(context.Background(), st.BytesKey)
		dg := digestOf(committed)

		w = do(reg, http.MethodPut, loc+"?digest="+dg, nil, nil)
		require.Equal(t, http.StatusCreated, w.Code, "iter %d body: %s", iter, w.Body.String())

		w = do(reg, http.MethodGet, "/v2/"+repo+"/blobs/"+dg, nil, nil)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, committed, w.Body.Bytes(), "stored bytes must equal the digest-verified bytes")
	}
}
