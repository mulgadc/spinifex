package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/ecr"
	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newImageGateway wires a GatewayConfig with an in-memory OCI registry (memory
// object store + memory meta) so the inline image handlers exercise the real
// Registry helpers without NATS.
func newImageGateway(t *testing.T) *GatewayConfig {
	t.Helper()
	reg := gateway_ecr.NewRegistry(objectstore.NewMemoryObjectStore(), handlers_ecr.NewMemoryMetaStore(), ecrTestAccount)
	return &GatewayConfig{ECRRegistry: reg, Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, DisableLogging: true}
}

// seedGatewayRepo creates the repository metadata so push/PutImage handlers —
// which require an existing repository — accept writes. Idempotent.
func seedGatewayRepo(t *testing.T, gw *GatewayConfig, repo string) {
	t.Helper()
	require.NoError(t, gw.ECRRegistry.Meta.PutRepo(context.Background(), ecrTestAccount, handlers_ecr.RepoMeta{Name: repo}))
}

// seedTaggedImage stores a layerless manifest under repo:tag, returning its
// digest. A unique manifest per tag yields a unique digest.
func seedTaggedImage(t *testing.T, gw *GatewayConfig, repo, tag string) string {
	t.Helper()
	seedGatewayRepo(t, gw, repo)
	manifest := fmt.Appendf(nil,
		`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[],"annotations":{"tag":"%s"}}`, tag)
	digest, err := gw.ECRRegistry.StoreManifest(context.Background(), ecrTestAccount, repo, tag, "application/vnd.docker.distribution.manifest.v2+json", manifest)
	require.NoError(t, err)
	return digest
}

func imageReq(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return req.WithContext(context.WithValue(req.Context(), ctxAccountID, ecrTestAccount))
}

func callImage(t *testing.T, gw *GatewayConfig, h func(*GatewayConfig, http.ResponseWriter, *http.Request) error, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	w := httptest.NewRecorder()
	return w, h(gw, w, imageReq(t, body))
}

func TestListImages_TaggedUntaggedFilter(t *testing.T) {
	gw := newImageGateway(t)
	seedTaggedImage(t, gw, "team/app", "v1")
	seedTaggedImage(t, gw, "team/app", "v2")

	w, err := callImage(t, gw, (*GatewayConfig).handleListImages, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)
	var out ecr.ListImagesOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Len(t, out.ImageIds, 2)

	// TAGGED filter keeps both; UNTAGGED filter drops both.
	w, err = callImage(t, gw, (*GatewayConfig).handleListImages, `{"repositoryName":"team/app","filter":{"tagStatus":"UNTAGGED"}}`)
	require.NoError(t, err)
	var untagged ecr.ListImagesOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &untagged))
	assert.Empty(t, untagged.ImageIds)
}

func TestListImages_MissingRepo(t *testing.T) {
	gw := newImageGateway(t)
	_, err := callImage(t, gw, (*GatewayConfig).handleListImages, `{"repositoryName":"team/ghost"}`)
	require.Error(t, err)
	assert.Equal(t, "RepositoryNotFoundException", err.Error())
}

func TestDescribeImages_HappyAndNotFound(t *testing.T) {
	gw := newImageGateway(t)
	digest := seedTaggedImage(t, gw, "team/app", "v1")

	w, err := callImage(t, gw, (*GatewayConfig).handleDescribeImages, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)
	// AWS jsonutil emits imagePushedAt as an epoch float, which encoding/json
	// cannot decode into the SDK struct's *time.Time, so assert via a local view.
	var out struct {
		ImageDetails []struct {
			ImageDigest      string   `json:"imageDigest"`
			ImageTags        []string `json:"imageTags"`
			ImageSizeInBytes int64    `json:"imageSizeInBytes"`
			ImagePushedAt    float64  `json:"imagePushedAt"`
		} `json:"imageDetails"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.ImageDetails, 1)
	assert.Equal(t, digest, out.ImageDetails[0].ImageDigest)
	assert.Equal(t, []string{"v1"}, out.ImageDetails[0].ImageTags)
	assert.Positive(t, out.ImageDetails[0].ImageSizeInBytes)
	assert.Positive(t, out.ImageDetails[0].ImagePushedAt)

	// Asking for an absent imageId -> ImageNotFound.
	_, err = callImage(t, gw, (*GatewayConfig).handleDescribeImages, `{"repositoryName":"team/app","imageIds":[{"imageTag":"ghost"}]}`)
	require.Error(t, err)
	assert.Equal(t, "ImageNotFoundException", err.Error())
}

func TestBatchGetImage_PartialAndDigestWins(t *testing.T) {
	gw := newImageGateway(t)
	digest := seedTaggedImage(t, gw, "team/app", "v1")

	body := fmt.Sprintf(`{"repositoryName":"team/app","imageIds":[{"imageDigest":"%s","imageTag":"v1"},{"imageTag":"ghost"},{}]}`, digest)
	w, err := callImage(t, gw, (*GatewayConfig).handleBatchGetImage, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)

	var out ecr.BatchGetImageOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.Images, 1)
	assert.Equal(t, digest, *out.Images[0].ImageId.ImageDigest)
	assert.NotEmpty(t, *out.Images[0].ImageManifest)

	require.Len(t, out.Failures, 2)
	codes := map[string]bool{}
	for _, f := range out.Failures {
		codes[*f.FailureCode] = true
	}
	assert.True(t, codes["ImageNotFound"])
	assert.True(t, codes["MissingDigestAndTag"])
}

func TestBatchGetImage_CapExceeded(t *testing.T) {
	gw := newImageGateway(t)
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = `{"imageTag":"t"}`
	}
	body := `{"repositoryName":"team/app","imageIds":[` + strings.Join(ids, ",") + `]}`
	_, err := callImage(t, gw, (*GatewayConfig).handleBatchGetImage, body)
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", err.Error())
}

func TestPutImage_HappyAndMissingManifest(t *testing.T) {
	gw := newImageGateway(t)
	seedGatewayRepo(t, gw, "team/app")
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`
	body, _ := json.Marshal(map[string]string{
		"repositoryName":         "team/app",
		"imageManifest":          manifest,
		"imageManifestMediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"imageTag":               "v1",
	})
	w, err := callImage(t, gw, (*GatewayConfig).handlePutImage, string(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)
	var out ecr.PutImageOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "v1", *out.Image.ImageId.ImageTag)
	assert.True(t, strings.HasPrefix(*out.Image.ImageId.ImageDigest, "sha256:"))

	// It is now listable.
	w, err = callImage(t, gw, (*GatewayConfig).handleListImages, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)
	var list ecr.ListImagesOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Len(t, list.ImageIds, 1)

	// Missing manifest -> InvalidParameterValue.
	_, err = callImage(t, gw, (*GatewayConfig).handlePutImage, `{"repositoryName":"team/app","imageTag":"v2"}`)
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", err.Error())
}

// TestPutImage_RepoNotCreated asserts the JSON PutImage path rejects an uncreated
// repository with RepositoryNotFoundException (the AWS twin of OCI NAME_UNKNOWN).
func TestPutImage_RepoNotCreated(t *testing.T) {
	gw := newImageGateway(t)
	body, _ := json.Marshal(map[string]string{
		"repositoryName":         "team/ghost",
		"imageManifest":          `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`,
		"imageManifestMediaType": "application/vnd.docker.distribution.manifest.v2+json",
		"imageTag":               "v1",
	})
	_, err := callImage(t, gw, (*GatewayConfig).handlePutImage, string(body))
	require.Error(t, err)
	assert.Equal(t, "RepositoryNotFoundException", err.Error())
}

func TestBatchDeleteImage_Partial(t *testing.T) {
	gw := newImageGateway(t)
	digest := seedTaggedImage(t, gw, "team/app", "v1")

	body := fmt.Sprintf(`{"repositoryName":"team/app","imageIds":[{"imageDigest":"%s"},{"imageTag":"ghost"},{}]}`, digest)
	w, err := callImage(t, gw, (*GatewayConfig).handleBatchDeleteImage, body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)

	var out ecr.BatchDeleteImageOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.ImageIds, 1)
	assert.Equal(t, digest, *out.ImageIds[0].ImageDigest)
	require.Len(t, out.Failures, 2)

	// The image is gone.
	w, err = callImage(t, gw, (*GatewayConfig).handleListImages, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)
	var list ecr.ListImagesOutput
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Empty(t, list.ImageIds)
}

func TestImageHandlers_GuardRails(t *testing.T) {
	gw := newImageGateway(t)

	// Cross-account registryId -> AccessDenied.
	_, err := callImage(t, gw, (*GatewayConfig).handleListImages, `{"repositoryName":"team/app","registryId":"999999999999"}`)
	require.Error(t, err)
	assert.Equal(t, "AccessDenied", err.Error())

	// No auth account -> ServerInternal.
	w := httptest.NewRecorder()
	err = gw.handleListImages(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"repositoryName":"team/app"}`)))
	require.Error(t, err)
	assert.Equal(t, "ServerInternal", err.Error())

	// Nil registry -> ServerInternal.
	bare := &GatewayConfig{DisableLogging: true}
	err = bare.handleListImages(httptest.NewRecorder(), imageReq(t, `{"repositoryName":"team/app"}`))
	require.Error(t, err)
	assert.Equal(t, "ServerInternal", err.Error())
}
