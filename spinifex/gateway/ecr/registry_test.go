package gateway_ecr

import (
	"context"

	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccount = "000000000000"

func newTestRegistry() *Registry {
	return NewRegistry(objectstore.NewMemoryObjectStore(), ecr.NewMemoryMetaStore(), testAccount)
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func do(reg *Registry, method, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(string(body)))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	reg.ServeHTTP(w, r)
	return w
}

// seedRepo creates the repository metadata so push handlers — which require an
// existing repository — accept writes. Idempotent.
func seedRepo(t *testing.T, reg *Registry, repo string) {
	t.Helper()
	require.NoError(t, reg.Meta.PutRepo(context.Background(), testAccount, ecr.RepoMeta{Name: repo}))
}

// pushBlob runs the monolithic upload-start + PUT?digest round-trip.
func pushBlob(t *testing.T, reg *Registry, repo string, data []byte) string {
	t.Helper()
	seedRepo(t, reg, repo)
	w := do(reg, http.MethodPost, "/v2/"+repo+"/blobs/uploads/", nil, nil)
	require.Equal(t, http.StatusAccepted, w.Code)
	loc := w.Header().Get("Location")
	require.NotEmpty(t, loc)

	dg := digestOf(data)
	w = do(reg, http.MethodPut, loc+"?digest="+dg, data, nil)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, dg, w.Header().Get("Docker-Content-Digest"))
	return dg
}

func TestBlobUpload_Monolithic_And_Head_Get(t *testing.T) {
	reg := newTestRegistry()
	data := []byte("hello layer bytes")
	dg := pushBlob(t, reg, "team/app", data)

	w := do(reg, http.MethodHead, "/v2/team/app/blobs/"+dg, nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, strconv.Itoa(len(data)), w.Header().Get("Content-Length"))
	assert.Equal(t, dg, w.Header().Get("Docker-Content-Digest"))

	w = do(reg, http.MethodGet, "/v2/team/app/blobs/"+dg, nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, data, w.Body.Bytes())
}

func TestBlobHead_NotFound(t *testing.T) {
	reg := newTestRegistry()
	w := do(reg, http.MethodHead, "/v2/team/app/blobs/"+digestOf([]byte("absent")), nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestBlobGet_MalformedDigest(t *testing.T) {
	reg := newTestRegistry()
	w := do(reg, http.MethodGet, "/v2/team/app/blobs/sha256:zzz", nil, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCode(t, w, "DIGEST_INVALID")
}

func TestBlobUpload_Chunked(t *testing.T) {
	reg := newTestRegistry()
	seedRepo(t, reg, "r/repo")
	c1 := []byte("first-chunk-")
	c2 := []byte("second-chunk")
	full := append(append([]byte{}, c1...), c2...)
	dg := digestOf(full)

	w := do(reg, http.MethodPost, "/v2/r/repo/blobs/uploads/", nil, nil)
	require.Equal(t, http.StatusAccepted, w.Code)
	loc := w.Header().Get("Location")

	w = do(reg, http.MethodPatch, loc, c1, nil)
	require.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, fmt.Sprintf("0-%d", len(c1)-1), w.Header().Get("Range"))

	w = do(reg, http.MethodPatch, loc, c2, nil)
	require.Equal(t, http.StatusAccepted, w.Code)

	w = do(reg, http.MethodPut, loc+"?digest="+dg, nil, nil)
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	w = do(reg, http.MethodGet, "/v2/r/repo/blobs/"+dg, nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, full, w.Body.Bytes())
}

func TestBlobUpload_DigestMismatch(t *testing.T) {
	reg := newTestRegistry()
	seedRepo(t, reg, "r/repo")
	w := do(reg, http.MethodPost, "/v2/r/repo/blobs/uploads/", nil, nil)
	loc := w.Header().Get("Location")
	w = do(reg, http.MethodPut, loc+"?digest="+digestOf([]byte("wrong")), []byte("actual"), nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCode(t, w, "DIGEST_INVALID")
}

func TestBlobUpload_Cancel(t *testing.T) {
	reg := newTestRegistry()
	seedRepo(t, reg, "r/repo")
	w := do(reg, http.MethodPost, "/v2/r/repo/blobs/uploads/", nil, nil)
	loc := w.Header().Get("Location")

	w = do(reg, http.MethodDelete, loc, nil, nil)
	assert.Equal(t, http.StatusNoContent, w.Code)

	// Second cancel is unknown.
	w = do(reg, http.MethodDelete, loc, nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// putManifestReferencing PUTs a minimal image manifest into repo that
// references digest as its config blob, so digest is provably reachable
// from repo for mountedFromValidSource.
func putManifestReferencing(t *testing.T, reg *Registry, repo, digest string) {
	t.Helper()
	manifest := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`,
		mediaTypeDockerManifest, digest)
	w := do(reg, http.MethodPut, "/v2/"+repo+"/manifests/v1", manifest,
		map[string]string{"Content-Type": mediaTypeDockerManifest})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
}

func TestBlobMount_HitAndMiss(t *testing.T) {
	reg := newTestRegistry()
	data := []byte("shared-layer")
	dg := pushBlob(t, reg, "team/source", data)
	putManifestReferencing(t, reg, "team/source", dg)
	seedRepo(t, reg, "team/dest")

	// Mount hit: blob in the account pool and reachable via a manifest in the
	// source repo -> 201 without upload.
	w := do(reg, http.MethodPost,
		"/v2/team/dest/blobs/uploads/?mount="+dg+"&from=team/source", nil, nil)
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, dg, w.Header().Get("Docker-Content-Digest"))

	// Mount miss: unknown digest -> falls back to upload start (202).
	w = do(reg, http.MethodPost,
		"/v2/team/dest/blobs/uploads/?mount="+digestOf([]byte("nope"))+"&from=team/source", nil, nil)
	assert.Equal(t, http.StatusAccepted, w.Code)
}

// TestBlobMount_MissingFromFallsThroughToUpload verifies mount is only
// recognised when both mount and from are present; mount alone is treated
// as a plain upload start, matching the authorization middleware's
// classification of the same request.
func TestBlobMount_MissingFromFallsThroughToUpload(t *testing.T) {
	reg := newTestRegistry()
	data := []byte("shared-layer")
	dg := pushBlob(t, reg, "team/source", data)
	putManifestReferencing(t, reg, "team/source", dg)
	seedRepo(t, reg, "team/dest")

	w := do(reg, http.MethodPost, "/v2/team/dest/blobs/uploads/?mount="+dg, nil, nil)
	assert.Equal(t, http.StatusAccepted, w.Code)
}

// TestBlobMount_UnreferencedDigestFallsThrough verifies a digest that exists
// in the account-wide blob pool but is not referenced by any manifest in the
// named source repository is rejected as a mount — proving digest existence
// alone is not enough to mount cross-repo.
func TestBlobMount_UnreferencedDigestFallsThrough(t *testing.T) {
	reg := newTestRegistry()
	unreferenced := pushBlob(t, reg, "team/other", []byte("not-in-source"))
	seedRepo(t, reg, "team/source")
	seedRepo(t, reg, "team/dest")

	w := do(reg, http.MethodPost,
		"/v2/team/dest/blobs/uploads/?mount="+unreferenced+"&from=team/source", nil, nil)
	assert.Equal(t, http.StatusAccepted, w.Code)
}

// TestBlobMount_NonexistentSourceFallsThrough verifies naming a source
// repository that does not exist never mounts, and never distinguishes
// itself from an unreferenced-digest miss (both fall through identically).
func TestBlobMount_NonexistentSourceFallsThrough(t *testing.T) {
	reg := newTestRegistry()
	dg := pushBlob(t, reg, "team/elsewhere", []byte("some-bytes"))
	seedRepo(t, reg, "team/dest")

	w := do(reg, http.MethodPost,
		"/v2/team/dest/blobs/uploads/?mount="+dg+"&from=team/does-not-exist", nil, nil)
	assert.Equal(t, http.StatusAccepted, w.Code)
}

func TestManifest_PutGetHead_ImageManifest(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"

	cfg := []byte(`{"config":true}`)
	layer := []byte("layer-bytes-xyz")
	cfgDg := pushBlob(t, reg, repo, cfg)
	layerDg := pushBlob(t, reg, repo, layer)

	manifest := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s"},"layers":[{"digest":"%s"}]}`,
		mediaTypeDockerManifest, cfgDg, layerDg)
	mdg := digestOf(manifest)

	w := do(reg, http.MethodPut, "/v2/"+repo+"/manifests/v1", manifest,
		map[string]string{"Content-Type": mediaTypeDockerManifest})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, mdg, w.Header().Get("Docker-Content-Digest"))

	// HEAD by tag.
	w = do(reg, http.MethodHead, "/v2/"+repo+"/manifests/v1", nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, mdg, w.Header().Get("Docker-Content-Digest"))

	// GET by digest returns bytes + stored content type.
	w = do(reg, http.MethodGet, "/v2/"+repo+"/manifests/"+mdg, nil, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, manifest, w.Body.Bytes())
	assert.Equal(t, mediaTypeDockerManifest, w.Header().Get("Content-Type"))
}

func TestManifest_Put_MissingBlob(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	seedRepo(t, reg, repo)
	manifest := fmt.Appendf(nil,
		`{"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`,
		mediaTypeDockerManifest, digestOf([]byte("nope")))
	w := do(reg, http.MethodPut, "/v2/"+repo+"/manifests/v1", manifest,
		map[string]string{"Content-Type": mediaTypeDockerManifest})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCode(t, w, "MANIFEST_BLOB_UNKNOWN")
}

func TestManifest_Get_AcceptMismatch(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	cfg := pushBlob(t, reg, repo, []byte("c"))
	manifest := fmt.Appendf(nil,
		`{"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`, mediaTypeDockerManifest, cfg)
	do(reg, http.MethodPut, "/v2/"+repo+"/manifests/v1", manifest,
		map[string]string{"Content-Type": mediaTypeDockerManifest})

	w := do(reg, http.MethodGet, "/v2/"+repo+"/manifests/v1", nil,
		map[string]string{"Accept": mediaTypeOCIIndex})
	assert.Equal(t, http.StatusNotAcceptable, w.Code)
}

// TestManifest_Get_MultipleAcceptHeaders reproduces skopeo, which sends each
// acceptable manifest media type as its own Accept header line rather than one
// comma-joined value. Negotiation must consider every line, not just the first.
func TestManifest_Get_MultipleAcceptHeaders(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	cfg := pushBlob(t, reg, repo, []byte("c"))
	manifest := fmt.Appendf(nil,
		`{"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`, mediaTypeDockerManifest, cfg)
	do(reg, http.MethodPut, "/v2/"+repo+"/manifests/v1", manifest,
		map[string]string{"Content-Type": mediaTypeDockerManifest})

	r := httptest.NewRequest(http.MethodGet, "/v2/"+repo+"/manifests/v1", nil)
	r.Header.Add("Accept", mediaTypeOCIIndex)
	r.Header.Add("Accept", mediaTypeOCIManifest)
	r.Header.Add("Accept", mediaTypeDockerManifest)
	w := httptest.NewRecorder()
	reg.ServeHTTP(w, r)

	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, manifest, w.Body.Bytes())
}

func TestManifest_Get_Unknown(t *testing.T) {
	reg := newTestRegistry()
	w := do(reg, http.MethodGet, "/v2/team/app/manifests/missing", nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assertCode(t, w, "MANIFEST_UNKNOWN")
}

// TestStartUpload_RepoNotCreated asserts a blob upload to a repository that was
// never created is rejected up front — ECR does not auto-create on push.
func TestStartUpload_RepoNotCreated(t *testing.T) {
	reg := newTestRegistry()
	w := do(reg, http.MethodPost, "/v2/team/ghost/blobs/uploads/", nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assertCode(t, w, "NAME_UNKNOWN")
}

// TestPutManifest_RepoNotCreated asserts a manifest PUT to an uncreated
// repository is rejected with NAME_UNKNOWN.
func TestPutManifest_RepoNotCreated(t *testing.T) {
	reg := newTestRegistry()
	manifest := fmt.Appendf(nil, `{"mediaType":"%s","layers":[]}`, mediaTypeDockerManifest)
	w := do(reg, http.MethodPut, "/v2/team/ghost/manifests/v1", manifest,
		map[string]string{"Content-Type": mediaTypeDockerManifest})
	assert.Equal(t, http.StatusNotFound, w.Code)
	assertCode(t, w, "NAME_UNKNOWN")
}

func TestManifest_ImageIndex_ChildValidation(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"

	// Push a child image manifest first.
	cfg := pushBlob(t, reg, repo, []byte("cfg"))
	child := fmt.Appendf(nil,
		`{"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`, mediaTypeOCIManifest, cfg)
	childDg := digestOf(child)
	w := do(reg, http.MethodPut, "/v2/"+repo+"/manifests/"+childDg, child,
		map[string]string{"Content-Type": mediaTypeOCIManifest})
	require.Equal(t, http.StatusCreated, w.Code)

	// Index referencing the child with matching mediaType.
	index := fmt.Appendf(nil,
		`{"mediaType":"%s","manifests":[{"digest":"%s","mediaType":"%s"}]}`,
		mediaTypeOCIIndex, childDg, mediaTypeOCIManifest)
	w = do(reg, http.MethodPut, "/v2/"+repo+"/manifests/latest", index,
		map[string]string{"Content-Type": mediaTypeOCIIndex})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	// Index referencing a missing child -> MANIFEST_BLOB_UNKNOWN.
	bad := fmt.Appendf(nil,
		`{"mediaType":"%s","manifests":[{"digest":"%s","mediaType":"%s"}]}`,
		mediaTypeOCIIndex, digestOf([]byte("absent")), mediaTypeOCIManifest)
	w = do(reg, http.MethodPut, "/v2/"+repo+"/manifests/bad", bad,
		map[string]string{"Content-Type": mediaTypeOCIIndex})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCode(t, w, "MANIFEST_BLOB_UNKNOWN")

	// Index with mediaType mismatch -> MANIFEST_INVALID.
	mismatch := fmt.Appendf(nil,
		`{"mediaType":"%s","manifests":[{"digest":"%s","mediaType":"%s"}]}`,
		mediaTypeOCIIndex, childDg, mediaTypeDockerManifest)
	w = do(reg, http.MethodPut, "/v2/"+repo+"/manifests/mm", mismatch,
		map[string]string{"Content-Type": mediaTypeOCIIndex})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCode(t, w, "MANIFEST_INVALID")
}

func TestManifest_Delete_Tag(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	cfg := pushBlob(t, reg, repo, []byte("c"))
	manifest := fmt.Appendf(nil,
		`{"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`, mediaTypeDockerManifest, cfg)
	do(reg, http.MethodPut, "/v2/"+repo+"/manifests/v1", manifest,
		map[string]string{"Content-Type": mediaTypeDockerManifest})

	w := do(reg, http.MethodDelete, "/v2/"+repo+"/manifests/v1", nil, nil)
	assert.Equal(t, http.StatusAccepted, w.Code)

	w = do(reg, http.MethodDelete, "/v2/"+repo+"/manifests/v1", nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// TestManifest_Delete_ByDigest proves delete-by-digest is no longer a no-op: it
// removes the manifest record + object, and a subsequent GET is a 404.
func TestManifest_Delete_ByDigest(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	cfg := pushBlob(t, reg, repo, []byte("cfg-bytes"))
	manifest := fmt.Appendf(nil,
		`{"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`, mediaTypeDockerManifest, cfg)
	mdg := digestOf(manifest)
	do(reg, http.MethodPut, "/v2/"+repo+"/manifests/v1", manifest,
		map[string]string{"Content-Type": mediaTypeDockerManifest})

	w := do(reg, http.MethodDelete, "/v2/"+repo+"/manifests/"+mdg, nil, nil)
	assert.Equal(t, http.StatusAccepted, w.Code)

	// The record is gone: GET by digest now 404s, and the object was reclaimed.
	w = do(reg, http.MethodGet, "/v2/"+repo+"/manifests/"+mdg, nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)

	// Deleting again -> 404 (record already gone).
	w = do(reg, http.MethodDelete, "/v2/"+repo+"/manifests/"+mdg, nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTagsList(t *testing.T) {
	reg := newTestRegistry()
	repo := "team/app"
	cfg := pushBlob(t, reg, repo, []byte("c"))
	manifest := fmt.Appendf(nil,
		`{"mediaType":"%s","config":{"digest":"%s"},"layers":[]}`, mediaTypeDockerManifest, cfg)
	for _, tag := range []string{"v1", "v2"} {
		do(reg, http.MethodPut, "/v2/"+repo+"/manifests/"+tag, manifest,
			map[string]string{"Content-Type": mediaTypeDockerManifest})
	}

	w := do(reg, http.MethodGet, "/v2/"+repo+"/tags/list", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, repo, resp.Name)
	assert.ElementsMatch(t, []string{"v1", "v2"}, resp.Tags)
}

func TestTagsList_UnknownRepo(t *testing.T) {
	reg := newTestRegistry()
	w := do(reg, http.MethodGet, "/v2/team/ghost/tags/list", nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCatalog(t *testing.T) {
	reg := newTestRegistry()
	pushBlob(t, reg, "team/a", []byte("a"))
	pushBlob(t, reg, "team/b", []byte("b"))

	w := do(reg, http.MethodGet, "/v2/_catalog", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Repositories []string `json:"repositories"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.ElementsMatch(t, []string{"team/a", "team/b"}, resp.Repositories)
}

func TestDispatch_InvalidRepoName(t *testing.T) {
	reg := newTestRegistry()
	w := do(reg, http.MethodGet, "/v2/Team/App/tags/list", nil, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCode(t, w, "NAME_INVALID")
}

func TestDispatch_UnknownPath(t *testing.T) {
	reg := newTestRegistry()
	w := do(reg, http.MethodGet, "/v2/team/app/frobnicate", nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSplitV2Path(t *testing.T) {
	name, kind, ref, ok := splitV2Path("team/app/blobs/sha256:abc")
	require.True(t, ok)
	assert.Equal(t, "team/app", name)
	assert.Equal(t, "blobs", kind)
	assert.Equal(t, "sha256:abc", ref)

	name, kind, ref, ok = splitV2Path("a/b/c/manifests/latest")
	require.True(t, ok)
	assert.Equal(t, "a/b/c", name)
	assert.Equal(t, "manifests", kind)
	assert.Equal(t, "latest", ref)

	_, _, _, ok = splitV2Path("noslashmarker")
	assert.False(t, ok)
}

func TestAcceptsType(t *testing.T) {
	assert.True(t, acceptsType("", mediaTypeDockerManifest))
	assert.True(t, acceptsType("*/*", mediaTypeDockerManifest))
	assert.True(t, acceptsType("application/*", mediaTypeDockerManifest))
	assert.True(t, acceptsType(mediaTypeOCIIndex+", "+mediaTypeDockerManifest, mediaTypeDockerManifest))
	assert.False(t, acceptsType(mediaTypeOCIIndex, mediaTypeDockerManifest))
}

func TestDetectManifestType(t *testing.T) {
	assert.Equal(t, mediaTypeOCIIndex,
		detectManifestType([]byte(`{"manifests":[{"digest":"x"}]}`)))
	assert.Equal(t, mediaTypeDockerManifest,
		detectManifestType([]byte(`{"config":{"digest":"x"}}`)))
	assert.Equal(t, mediaTypeOCIManifest,
		detectManifestType([]byte(`{"mediaType":"`+mediaTypeOCIManifest+`"}`)))
}

func assertCode(t *testing.T, w *httptest.ResponseRecorder, code string) {
	t.Helper()
	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotEmpty(t, body.Errors)
	assert.Equal(t, code, body.Errors[0].Code)
}

// memoryObjectStore satisfies the ObjectStore interface for completeness check.
var _ objectstore.ObjectStore = objectstore.NewMemoryObjectStore()
