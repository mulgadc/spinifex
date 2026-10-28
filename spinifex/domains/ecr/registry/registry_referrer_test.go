package ecrregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	referrerRepo     = "team/app"
	sigConfigType    = "application/vnd.example.sig.config.v1+json"
	sigArtifactType  = "application/vnd.example.sig.v1"
	absentSubjectDgt = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func imageWithSubject(cfgDigest, subject, artifactType string) string {
	extra := ""
	if artifactType != "" {
		extra = fmt.Sprintf(`"artifactType":%q,`, artifactType)
	}
	return fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,%s"config":{"mediaType":%q,"digest":%q,"size":3},"layers":[],"subject":{"mediaType":%q,"digest":%q,"size":10}}`,
		mediaTypeOCIManifest, extra, sigConfigType, cfgDigest, mediaTypeOCIManifest, subject)
}

func storedMeta(t *testing.T, reg *Registry, body string) (subject, artifactType string) {
	t.Helper()
	m, err := reg.Meta.GetManifestMeta(context.Background(), testAccount, referrerRepo, digestOf([]byte(body)))
	require.NoError(t, err)
	return m.SubjectDigest, m.ArtifactType
}

func TestPutManifest_SubjectRecordedAndAdvertised(t *testing.T) {
	reg := newTestRegistry()
	cfg := pushBlob(t, reg, referrerRepo, []byte("cfg"))

	tests := []struct {
		name         string
		artifactType string
		wantType     string
	}{
		{"artifactType falls back to the config mediaType", "", sigConfigType},
		{"explicit artifactType wins over the config mediaType", sigArtifactType, sigArtifactType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := imageWithSubject(cfg, absentSubjectDgt, tt.artifactType)
			w := do(reg, http.MethodPut, "/v2/"+referrerRepo+"/manifests/"+digestOf([]byte(body)), []byte(body),
				map[string]string{"Content-Type": mediaTypeOCIManifest})
			require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
			assert.Equal(t, absentSubjectDgt, w.Header().Get("OCI-Subject"))

			subject, artifactType := storedMeta(t, reg, body)
			assert.Equal(t, absentSubjectDgt, subject)
			assert.Equal(t, tt.wantType, artifactType)
		})
	}
}

func TestPutManifest_IndexSubject(t *testing.T) {
	reg := newTestRegistry()
	seedRepo(t, reg, referrerRepo)

	body := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[],"subject":{"mediaType":%q,"digest":%q,"size":10}}`,
		mediaTypeOCIIndex, mediaTypeOCIManifest, absentSubjectDgt)
	w := do(reg, http.MethodPut, "/v2/"+referrerRepo+"/manifests/"+digestOf([]byte(body)), []byte(body),
		map[string]string{"Content-Type": mediaTypeOCIIndex})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, absentSubjectDgt, w.Header().Get("OCI-Subject"))

	subject, artifactType := storedMeta(t, reg, body)
	assert.Equal(t, absentSubjectDgt, subject)
	assert.Empty(t, artifactType, "an index has no config mediaType to fall back on")
}

func TestPutManifest_NoSubjectLeavesFieldsEmpty(t *testing.T) {
	reg := newTestRegistry()
	cfg := pushBlob(t, reg, referrerRepo, []byte("cfg"))
	body := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"artifactType":%q,"config":{"mediaType":%q,"digest":%q,"size":3},"layers":[]}`,
		mediaTypeOCIManifest, sigArtifactType, sigConfigType, cfg)

	w := do(reg, http.MethodPut, "/v2/"+referrerRepo+"/manifests/v1", []byte(body),
		map[string]string{"Content-Type": mediaTypeOCIManifest})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.Empty(t, w.Header().Get("OCI-Subject"))

	subject, artifactType := storedMeta(t, reg, body)
	assert.Empty(t, subject)
	assert.Empty(t, artifactType, "artifactType is recorded only for manifests that declare a subject")
}

func TestPutManifest_MalformedSubjectDigestRejected(t *testing.T) {
	reg := newTestRegistry()
	cfg := pushBlob(t, reg, referrerRepo, []byte("cfg"))
	body := imageWithSubject(cfg, "sha256:nothex", "")

	w := do(reg, http.MethodPut, "/v2/"+referrerRepo+"/manifests/v1", []byte(body),
		map[string]string{"Content-Type": mediaTypeOCIManifest})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCode(t, w, "MANIFEST_INVALID")

	_, err := reg.Meta.GetManifestMeta(context.Background(), testAccount, referrerRepo, digestOf([]byte(body)))
	assert.Error(t, err, "a rejected manifest must not leave a record behind")
}

func TestPutManifest_IndexWithContentTypeParameterGetsNoConfigArtifactType(t *testing.T) {
	reg := newTestRegistry()
	seedRepo(t, reg, referrerRepo)

	body := fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[],"subject":{"mediaType":%q,"digest":%q,"size":10}}`,
		mediaTypeOCIIndex, mediaTypeOCIManifest, absentSubjectDgt)
	w := do(reg, http.MethodPut, "/v2/"+referrerRepo+"/manifests/v1", []byte(body),
		map[string]string{"Content-Type": mediaTypeOCIIndex + "; charset=utf-8"})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, absentSubjectDgt, w.Header().Get("OCI-Subject"))

	m, err := reg.Meta.GetManifestMeta(context.Background(), testAccount, referrerRepo, digestOf([]byte(body)))
	require.NoError(t, err)
	assert.Equal(t, mediaTypeOCIIndex, m.MediaType, "the stored type must not carry the parameter")
	assert.Empty(t, m.ArtifactType)
}

func TestDeleteReferrerManifest_RemovesItFromTheSubjectListing(t *testing.T) {
	reg := newTestRegistry()
	cfg := pushBlob(t, reg, referrerRepo, []byte("cfg"))
	body := imageWithSubject(cfg, absentSubjectDgt, sigArtifactType)
	dg := digestOf([]byte(body))
	w := do(reg, http.MethodPut, "/v2/"+referrerRepo+"/manifests/"+dg, []byte(body),
		map[string]string{"Content-Type": mediaTypeOCIManifest})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

	listed, err := reg.Meta.ListReferrers(context.Background(), testAccount, referrerRepo, absentSubjectDgt)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, dg, listed[0].Digest)
	assert.Equal(t, sigArtifactType, listed[0].ArtifactType)

	w = do(reg, http.MethodDelete, "/v2/"+referrerRepo+"/manifests/"+dg, nil, nil)
	require.Equal(t, http.StatusAccepted, w.Code, "body: %s", w.Body.String())

	listed, err = reg.Meta.ListReferrers(context.Background(), testAccount, referrerRepo, absentSubjectDgt)
	require.NoError(t, err)
	assert.Empty(t, listed)
}

// pushReferrer stores a signature-shaped manifest that names subject and returns its digest.
func pushReferrer(t *testing.T, reg *Registry, repo, subject, artifactType, blobData string) string {
	t.Helper()
	cfg := pushBlob(t, reg, repo, []byte(blobData))
	body := imageWithSubject(cfg, subject, artifactType)
	dg := digestOf([]byte(body))
	w := do(reg, http.MethodPut, "/v2/"+repo+"/manifests/"+dg, []byte(body),
		map[string]string{"Content-Type": mediaTypeOCIManifest})
	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	return dg
}

type referrersBody struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Manifests     []struct {
		MediaType    string `json:"mediaType"`
		Digest       string `json:"digest"`
		ArtifactType string `json:"artifactType"`
	} `json:"manifests"`
}

func getReferrersBody(t *testing.T, reg *Registry, path string) (*httptest.ResponseRecorder, referrersBody) {
	t.Helper()
	w := do(reg, http.MethodGet, path, nil, nil)
	var out referrersBody
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	}
	return w, out
}

func TestGetReferrers_ListsReferrersAsAnOCIIndex(t *testing.T) {
	reg := newTestRegistry()
	dgA := pushReferrer(t, reg, referrerRepo, absentSubjectDgt, sigArtifactType, "cfgA")
	dgB := pushReferrer(t, reg, referrerRepo, absentSubjectDgt, "application/vnd.example.sbom.v1", "cfgB")

	w, out := getReferrersBody(t, reg, "/v2/"+referrerRepo+"/referrers/"+absentSubjectDgt)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "application/vnd.oci.image.index.v1+json", w.Header().Get("Content-Type"))
	assert.Empty(t, w.Header().Get("OCI-Filters-Applied"))
	assert.Equal(t, 2, out.SchemaVersion)
	assert.Equal(t, mediaTypeOCIIndex, out.MediaType)
	got := map[string]string{}
	for _, m := range out.Manifests {
		assert.Equal(t, mediaTypeOCIManifest, m.MediaType)
		got[m.Digest] = m.ArtifactType
	}
	assert.Equal(t, map[string]string{dgA: sigArtifactType, dgB: "application/vnd.example.sbom.v1"}, got)
}

func TestGetReferrers_NoReferrersIsAnEmptyIndexNotAnError(t *testing.T) {
	reg := newTestRegistry()
	seedRepo(t, reg, referrerRepo)

	w := do(reg, http.MethodGet, "/v2/"+referrerRepo+"/referrers/"+absentSubjectDgt, nil, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "application/vnd.oci.image.index.v1+json", w.Header().Get("Content-Type"))
	assert.JSONEq(t, fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"manifests":[]}`, mediaTypeOCIIndex), w.Body.String())
}

func TestGetReferrers_ArtifactTypeFilter(t *testing.T) {
	reg := newTestRegistry()
	dgSig := pushReferrer(t, reg, referrerRepo, absentSubjectDgt, sigArtifactType, "cfgA")
	pushReferrer(t, reg, referrerRepo, absentSubjectDgt, "application/vnd.example.sbom.v1", "cfgB")

	w, out := getReferrersBody(t, reg, "/v2/"+referrerRepo+"/referrers/"+absentSubjectDgt+"?artifactType="+sigArtifactType)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "artifactType", w.Header().Get("OCI-Filters-Applied"))
	require.Len(t, out.Manifests, 1)
	assert.Equal(t, dgSig, out.Manifests[0].Digest)

	w, out = getReferrersBody(t, reg, "/v2/"+referrerRepo+"/referrers/"+absentSubjectDgt+"?artifactType=application/none")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "artifactType", w.Header().Get("OCI-Filters-Applied"), "the header reports the filter even when nothing matches")
	assert.Empty(t, out.Manifests)
	assert.Contains(t, w.Body.String(), `"manifests":[]`)
}

func TestGetReferrers_MalformedDigest(t *testing.T) {
	reg := newTestRegistry()
	seedRepo(t, reg, referrerRepo)

	w := do(reg, http.MethodGet, "/v2/"+referrerRepo+"/referrers/not-a-digest", nil, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assertCode(t, w, "DIGEST_INVALID")
}

func TestGetReferrers_UnknownRepository(t *testing.T) {
	reg := newTestRegistry()
	w := do(reg, http.MethodGet, "/v2/team/ghost/referrers/"+absentSubjectDgt, nil, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assertCode(t, w, "NAME_UNKNOWN")
}

func TestGetReferrers_DoesNotCrossRepositories(t *testing.T) {
	reg := newTestRegistry()
	pushReferrer(t, reg, referrerRepo, absentSubjectDgt, sigArtifactType, "cfgA")
	seedRepo(t, reg, "team/other")

	w, out := getReferrersBody(t, reg, "/v2/team/other/referrers/"+absentSubjectDgt)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, out.Manifests)
}

func TestReferrersRoute_OnlyGET(t *testing.T) {
	reg := newTestRegistry()
	seedRepo(t, reg, referrerRepo)
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPost, http.MethodPatch} {
		w := do(reg, method, "/v2/"+referrerRepo+"/referrers/"+absentSubjectDgt, []byte("{}"), nil)
		assert.Equal(t, http.StatusNotFound, w.Code, method)
		assertCode(t, w, "NAME_UNKNOWN")
	}
}

type failingReferrerStore struct{ ecr.MetaStore }

func (failingReferrerStore) ListReferrers(context.Context, string, string, string) ([]ecr.ReferrerDescriptor, error) {
	return nil, errors.New("no responders available for ecr.referrer.list")
}

func TestGetReferrers_MetadataFailureIsA5xxNotA404(t *testing.T) {
	reg := newTestRegistry()
	seedRepo(t, reg, referrerRepo)
	reg.Meta = failingReferrerStore{reg.Meta}

	w := do(reg, http.MethodGet, "/v2/"+referrerRepo+"/referrers/"+absentSubjectDgt, nil, nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
