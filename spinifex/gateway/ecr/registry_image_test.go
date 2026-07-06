package gateway_ecr

import (
	"context"

	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// objectExists reports whether a predastore key is present in the test account
// bucket.
func objectExists(reg *Registry, key string) bool {
	_, err := reg.Store.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(ecr.AccountBucket(testAccount)),
		Key:    aws.String(key),
	})
	return err == nil
}

// storeLayersManifest stores a manifest referencing the given layer blobs under
// repo:tag, returning its digest.
func storeLayersManifest(t *testing.T, reg *Registry, repo, tag string, layers ...string) string {
	t.Helper()
	refs := make([]string, len(layers))
	for i, l := range layers {
		refs[i] = fmt.Sprintf(`{"digest":"%s"}`, l)
	}
	body := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":"%s","layers":[%s]}`,
		mediaTypeDockerManifest, strings.Join(refs, ","))
	digest, err := reg.StoreManifest(context.Background(), testAccount, repo, tag, mediaTypeDockerManifest, body)
	require.NoError(t, err)
	return digest
}

func TestDeleteImage_ReclaimsBlobsKeepsShared(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	shared := pushBlob(t, reg, repo, []byte("shared-layer-bytes"))
	exclusive := pushBlob(t, reg, repo, []byte("exclusive-layer-bytes"))

	digA := storeLayersManifest(t, reg, repo, "a", shared, exclusive)
	digB := storeLayersManifest(t, reg, repo, "b", shared)

	require.True(t, objectExists(reg, ecr.ManifestKey(repo, digA)))
	require.True(t, objectExists(reg, ecr.BlobKey(exclusive)))

	got, err := reg.DeleteImage(context.Background(), testAccount, repo, "", digA)
	require.NoError(t, err)
	assert.Equal(t, digA, got)

	// Manifest A object + its exclusive blob reclaimed.
	assert.False(t, objectExists(reg, ecr.ManifestKey(repo, digA)), "manifest A object should be gone")
	assert.False(t, objectExists(reg, ecr.BlobKey(exclusive)), "exclusive blob should be reclaimed")

	// Shared blob + manifest B survive (B still references the shared blob).
	assert.True(t, objectExists(reg, ecr.BlobKey(shared)), "shared blob must survive")
	assert.True(t, objectExists(reg, ecr.ManifestKey(repo, digB)), "manifest B object must survive")
	_, _, _, err = reg.GetManifest(context.Background(), testAccount, repo, digB, nil)
	require.NoError(t, err)
}

// seedImage pushes a config + layer blob and stores an image manifest tagged
// `tag`, returning the manifest bytes and digest.
func seedImage(t *testing.T, reg *Registry, repo, tag string) ([]byte, string) {
	t.Helper()
	cfgDg := pushBlob(t, reg, repo, []byte("cfg-"+repo+"-"+tag))
	layerDg := pushBlob(t, reg, repo, []byte("layer-"+repo+"-"+tag))
	manifest := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`,
		mediaTypeDockerManifest, cfgDg, layerDg)
	digest, err := reg.StoreManifest(context.Background(), testAccount, repo, tag, mediaTypeDockerManifest, manifest)
	require.NoError(t, err)
	return manifest, digest
}

func TestStoreAndGetManifest_RoundTrip(t *testing.T) {
	reg := newTestRegistry()
	manifest, digest := seedImage(t, reg, "team/app", "v1")

	// Fetch by tag.
	body, mediaType, gotDigest, err := reg.GetManifest(context.Background(), testAccount, "team/app", "v1", nil)
	require.NoError(t, err)
	assert.Equal(t, manifest, body)
	assert.Equal(t, mediaTypeDockerManifest, mediaType)
	assert.Equal(t, digest, gotDigest)

	// Fetch by digest.
	body, _, _, err = reg.GetManifest(context.Background(), testAccount, "team/app", digest, nil)
	require.NoError(t, err)
	assert.Equal(t, manifest, body)

	// acceptedMediaTypes match.
	_, _, _, err = reg.GetManifest(context.Background(), testAccount, "team/app", "v1", []string{mediaTypeDockerManifest})
	require.NoError(t, err)

	// acceptedMediaTypes mismatch -> ErrImageNotFound (Q14).
	_, _, _, err = reg.GetManifest(context.Background(), testAccount, "team/app", "v1", []string{mediaTypeOCIIndex})
	assert.ErrorIs(t, err, ErrImageNotFound)

	// Unknown tag.
	_, _, _, err = reg.GetManifest(context.Background(), testAccount, "team/app", "ghost", nil)
	assert.ErrorIs(t, err, ErrImageNotFound)
}

func TestListImages_TaggedAndUntagged(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	seedImage(t, reg, repo, "v1")
	seedImage(t, reg, repo, "v2")

	// An untagged manifest: store with a digest reference so no tag is written.
	manifest := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`,
		mediaTypeDockerManifest, pushBlob(t, reg, repo, []byte("cfg-untagged")))
	untaggedDigest := digestOf(manifest)
	_, err := reg.StoreManifest(context.Background(), testAccount, repo, untaggedDigest, mediaTypeDockerManifest, manifest)
	require.NoError(t, err)

	records, err := reg.ListImages(context.Background(), testAccount, repo)
	require.NoError(t, err)

	tags := map[string]bool{}
	untagged := 0
	for _, rec := range records {
		if len(rec.Tags) == 0 {
			untagged++
			assert.Equal(t, untaggedDigest, rec.Digest)
			continue
		}
		for _, tag := range rec.Tags {
			tags[tag] = true
		}
		assert.Positive(t, rec.Size)
	}
	assert.True(t, tags["v1"])
	assert.True(t, tags["v2"])
	assert.Equal(t, 1, untagged)

	// Missing repo -> ErrNotFound.
	_, err = reg.ListImages(context.Background(), testAccount, "team/ghost")
	assert.ErrorIs(t, err, ecr.ErrNotFound)
}

// TestListImages_KVDigestForm guards the production JetStream-KV path: its
// ListManifests returns tokenized keys (':'->'-'), so ListImages must
// canonicalize on meta.Digest to emit a real digest and keep tag association.
// The memory store returns real digests and would mask this.
func TestListImages_KVDigestForm(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	reg := NewRegistry(objectstore.NewMemoryObjectStore(), ecr.NewKVMetaStore(js), testAccount)
	repo := "team/app"
	require.NoError(t, reg.Meta.PutRepo(context.Background(), testAccount, ecr.RepoMeta{Name: repo}))

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	digest, err := reg.StoreManifest(context.Background(), testAccount, repo, "v1", mediaTypeDockerManifest, manifest)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(digest, "sha256:"))

	records, err := reg.ListImages(context.Background(), testAccount, repo)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, digest, records[0].Digest, "digest must be the colon form, not the KV token")
	assert.Equal(t, []string{"v1"}, records[0].Tags, "tag association must survive KV tokenization")
}

func TestDeleteImage_ByTagAndByDigest(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	_, digest := seedImage(t, reg, repo, "v1")
	seedImage(t, reg, repo, "v2") // shares nothing; distinct digest

	// Delete by tag removes only the tag pointer; the manifest stays.
	got, err := reg.DeleteImage(context.Background(), testAccount, repo, "v1", "")
	require.NoError(t, err)
	assert.Equal(t, digest, got)
	_, _, _, err = reg.GetManifest(context.Background(), testAccount, repo, "v1", nil)
	assert.ErrorIs(t, err, ErrImageNotFound)
	_, _, _, err = reg.GetManifest(context.Background(), testAccount, repo, digest, nil)
	require.NoError(t, err) // manifest still resolvable by digest

	// Delete by digest removes the manifest meta.
	got, err = reg.DeleteImage(context.Background(), testAccount, repo, "", digest)
	require.NoError(t, err)
	assert.Equal(t, digest, got)
	_, _, _, err = reg.GetManifest(context.Background(), testAccount, repo, digest, nil)
	assert.ErrorIs(t, err, ErrImageNotFound)

	// Deleting an absent tag/digest -> ErrImageNotFound.
	_, err = reg.DeleteImage(context.Background(), testAccount, repo, "ghost", "")
	assert.ErrorIs(t, err, ErrImageNotFound)
	_, err = reg.DeleteImage(context.Background(), testAccount, repo, "", digestOf([]byte("absent")))
	assert.ErrorIs(t, err, ErrImageNotFound)
}

func TestDeleteImage_ByDigestClearsTags(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	_, digest := seedImage(t, reg, repo, "v1")
	// Add a second tag pointing at the same digest.
	require.NoError(t, reg.Meta.PutTag(context.Background(), testAccount, repo, "latest", digest))

	got, err := reg.DeleteImage(context.Background(), testAccount, repo, "", digest)
	require.NoError(t, err)
	assert.Equal(t, digest, got)

	tags, err := reg.Meta.ListTags(context.Background(), testAccount, repo)
	require.NoError(t, err)
	assert.Empty(t, tags)
}

func TestStoreManifest_ImmutableTag(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	manA := fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":"%s","layers":[{"digest":"%s"}]}`,
		mediaTypeDockerManifest, pushBlob(t, reg, repo, []byte("layer-a")))
	manB := fmt.Appendf(nil, `{"schemaVersion":2,"mediaType":"%s","layers":[{"digest":"%s"}]}`,
		mediaTypeDockerManifest, pushBlob(t, reg, repo, []byte("layer-b")))

	digA, err := reg.StoreManifest(context.Background(), testAccount, repo, "v1", mediaTypeDockerManifest, manA)
	require.NoError(t, err)

	// Flip the repo to IMMUTABLE.
	require.NoError(t, reg.Meta.PutRepo(context.Background(), testAccount, ecr.RepoMeta{Name: repo, ImageTagMutability: ecr.TagMutabilityImmutable}))

	// Re-pushing the same tag onto the same digest is idempotent, not a conflict.
	got, err := reg.StoreManifest(context.Background(), testAccount, repo, "v1", mediaTypeDockerManifest, manA)
	require.NoError(t, err)
	assert.Equal(t, digA, got)

	// Re-pushing the tag onto a different digest is rejected.
	_, err = reg.StoreManifest(context.Background(), testAccount, repo, "v1", mediaTypeDockerManifest, manB)
	var mErr *ManifestStoreError
	require.True(t, errors.As(err, &mErr))
	assert.Equal(t, "TAG_IMMUTABLE", mErr.Code)
	assert.Equal(t, http.StatusConflict, mErr.Status)

	// A new tag and a digest reference are still allowed on an immutable repo.
	_, err = reg.StoreManifest(context.Background(), testAccount, repo, "v2", mediaTypeDockerManifest, manB)
	require.NoError(t, err)
	_, err = reg.StoreManifest(context.Background(), testAccount, repo, digestOf(manB), mediaTypeDockerManifest, manB)
	require.NoError(t, err)
}

func TestStoreManifest_BadDigestReference(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	cfgDg := pushBlob(t, reg, repo, []byte("cfg"))
	manifest := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`,
		mediaTypeDockerManifest, cfgDg)

	_, err := reg.StoreManifest(context.Background(), testAccount, repo, digestOf([]byte("wrong")), mediaTypeDockerManifest, manifest)
	var mErr *ManifestStoreError
	require.True(t, errors.As(err, &mErr))
	assert.Equal(t, "DIGEST_INVALID", mErr.Code)
}
