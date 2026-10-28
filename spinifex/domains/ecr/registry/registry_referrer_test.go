package ecrregistry

import (
	"context"
	"fmt"
	"net/http"
	"testing"

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
