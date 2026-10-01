package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRepoLifecycleGateway wires the repo create/describe/delete + manifest
// subjects against an embedded KV-backed service, returning a gateway client.
func newRepoLifecycleGateway(t *testing.T) (*GatewayConfig, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	svc := handlers_ecr.NewKVMetaService(js)
	serveECRMeta(t, nc, handlers_ecr.SubjectRepoCreate, svc.RepoCreate)
	serveECRMeta(t, nc, handlers_ecr.SubjectRepoDescribe, svc.RepoDescribe)
	serveECRMeta(t, nc, handlers_ecr.SubjectRepoDelete, svc.RepoDelete)
	serveECRMeta(t, nc, handlers_ecr.SubjectManifestPut, svc.ManifestPut)
	serveECRMeta(t, nc, handlers_ecr.SubjectManifestList, svc.ManifestList)
	gw := &GatewayConfig{
		NATSConn: nc, Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, DisableLogging: true,
		IAMService: allowAllIAMService(),
		ECRRepositoryActions: awsapi.NewRepositoryActionService(handlers_ecr.NewNATSMetaStore(nc), awsapi.RepositoryEndpoint{
			Region: ecrTestRegion, ServicesDomain: ecrTestSuffix,
		}),
	}
	return gw, nc
}

func ecrRepositoryRequest(t *testing.T, gw *GatewayConfig, action, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := setupECRRequest(awsapi.TargetPrefix+"."+action, body)
	ctx := context.WithValue(req.Context(), ctxAccountID, ecrTestAccount)
	w := httptest.NewRecorder()
	return w, gw.ECR_Request(w, req.WithContext(ctx))
}

func createRepo(t *testing.T, gw *GatewayConfig, body string) (*httptest.ResponseRecorder, error) {
	return ecrRepositoryRequest(t, gw, "CreateRepository", body)
}

func deleteRepo(t *testing.T, gw *GatewayConfig, body string) (*httptest.ResponseRecorder, error) {
	return ecrRepositoryRequest(t, gw, "DeleteRepository", body)
}

type repoOut struct {
	Repository struct {
		RepositoryName          string  `json:"repositoryName"`
		RegistryID              string  `json:"registryId"`
		RepositoryArn           string  `json:"repositoryArn"`
		RepositoryURI           string  `json:"repositoryUri"`
		ImageTagMutability      string  `json:"imageTagMutability"`
		CreatedAt               float64 `json:"createdAt"`
		EncryptionConfiguration struct {
			EncryptionType string `json:"encryptionType"`
		} `json:"encryptionConfiguration"`
		ImageScanningConfiguration struct {
			ScanOnPush bool `json:"scanOnPush"`
		} `json:"imageScanningConfiguration"`
	} `json:"repository"`
}

func TestCreateRepository_Happy(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)
	w, err := createRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)

	var out repoOut
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "team/app", out.Repository.RepositoryName)
	assert.Equal(t, ecrTestAccount, out.Repository.RegistryID)
	assert.Equal(t, "arn:aws:ecr:"+ecrTestRegion+":"+ecrTestAccount+":repository/team/app", out.Repository.RepositoryArn)
	assert.Equal(t, ecrTestAccount+".dkr.ecr."+ecrTestRegion+"."+ecrTestSuffix+"/team/app", out.Repository.RepositoryURI)
	assert.Equal(t, "MUTABLE", out.Repository.ImageTagMutability)
	assert.Positive(t, out.Repository.CreatedAt)
	assert.Equal(t, "AES256", out.Repository.EncryptionConfiguration.EncryptionType)
	assert.False(t, out.Repository.ImageScanningConfiguration.ScanOnPush)
}

// TestCreateRepository_Tags pins create-time tags surviving to the stored
// record. The wire keys are capitalized Key/Value because ecr.Tag carries no
// locationName; a lowercase spelling decodes to empty strings and is rejected.
func TestCreateRepository_Tags(t *testing.T) {
	gw, nc := newRepoLifecycleGateway(t)

	_, err := createRepo(t, gw, `{"repositoryName":"team/tagged","tags":[{"Key":"env","Value":"prod"},{"Key":"team","Value":""}]}`)
	require.NoError(t, err)

	meta, err := handlers_ecr.NewNATSMetaStore(nc).GetRepo(context.Background(), ecrTestAccount, "team/tagged")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "prod", "team": ""}, meta.Tags)

	// An absent list must leave the map nil, so the record is byte-identical to
	// one written before tags were accepted here.
	_, err = createRepo(t, gw, `{"repositoryName":"team/untagged"}`)
	require.NoError(t, err)
	meta, err = handlers_ecr.NewNATSMetaStore(nc).GetRepo(context.Background(), ecrTestAccount, "team/untagged")
	require.NoError(t, err)
	assert.Nil(t, meta.Tags)

	_, err = createRepo(t, gw, `{"repositoryName":"team/badtag","tags":[{"Key":"","Value":"x"}]}`)
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", awserrors.ValidErrorCodeFromError(err))
}

func TestCreateRepository_EncryptionAndScanningConfiguration(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)

	w, err := createRepo(t, gw, `{"repositoryName":"team/scanned","imageScanningConfiguration":{"scanOnPush":true}}`)
	require.NoError(t, err)
	var out repoOut
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "AES256", out.Repository.EncryptionConfiguration.EncryptionType)
	assert.True(t, out.Repository.ImageScanningConfiguration.ScanOnPush)

	w, err = createRepo(t, gw, `{"repositoryName":"team/aes","encryptionConfiguration":{"encryptionType":"AES256"}}`)
	require.NoError(t, err)
	out = repoOut{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "AES256", out.Repository.EncryptionConfiguration.EncryptionType)

	// KMS carries a client-facing message (awserrors.Errorf), so the code is
	// read via ResolveErrorDetail rather than a bare err.Error() comparison.
	_, err = createRepo(t, gw, `{"repositoryName":"team/kms","encryptionConfiguration":{"encryptionType":"KMS"}}`)
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, "InvalidParameterValue", code)
	assert.NotEmpty(t, message)

	_, err = createRepo(t, gw, `{"repositoryName":"team/bad","encryptionConfiguration":{"encryptionType":"bogus"}}`)
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", awserrors.ValidErrorCodeFromError(err))
}

func TestCreateRepository_Errors(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)
	_, err := createRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)

	cases := []struct {
		name, body, expect string
	}{
		{"already exists", `{"repositoryName":"team/app"}`, "RepositoryAlreadyExistsException"},
		{"invalid name", `{"repositoryName":"Team/App"}`, "InvalidParameterValue"},
		{"empty name", `{}`, "InvalidParameterValue"},
		{"cross-account", `{"repositoryName":"team/x","registryId":"999999999999"}`, "AccessDenied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := createRepo(t, gw, tc.body)
			require.Error(t, err)
			assert.Equal(t, tc.expect, awserrors.ValidErrorCodeFromError(err))
		})
	}
}

// AWS's CreateRepository names the pattern a refused repositoryName must match.
func TestCreateRepository_BadNameCarriesAWSMessage(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)
	_, err := createRepo(t, gw, `{"repositoryName":"Bad_Name!"}`)
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code)
	assert.Equal(t, `Invalid parameter at 'repositoryName' failed to satisfy constraint: 'must satisfy regular expression '[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*''`, message)
}

// noAccountRequest builds a request without the auth-context account ID, which
// generic ECR dispatch rejects with InternalError before touching the store.
func noAccountRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
}

func TestCreateRepository_NoAccountAndMalformed(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)

	req := noAccountRequest(`{"repositoryName":"team/app"}`)
	req.Header.Set("X-Amz-Target", awsapi.TargetPrefix+".CreateRepository")
	err := gw.ECR_Request(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Equal(t, "InternalError", err.Error())

	_, err = createRepo(t, gw, `{`)
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", awserrors.ValidErrorCodeFromError(err))
}

func TestDeleteRepository_NoAccountAndMalformed(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)

	req := noAccountRequest(`{"repositoryName":"team/app"}`)
	req.Header.Set("X-Amz-Target", awsapi.TargetPrefix+".DeleteRepository")
	err := gw.ECR_Request(httptest.NewRecorder(), req)
	require.Error(t, err)
	assert.Equal(t, "InternalError", err.Error())

	_, err = deleteRepo(t, gw, `{`)
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", awserrors.ValidErrorCodeFromError(err))
}

func TestDeleteRepository_Happy(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)
	_, err := createRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)

	w, err := deleteRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)
	var out repoOut
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "team/app", out.Repository.RepositoryName)
	assert.Equal(t, "AES256", out.Repository.EncryptionConfiguration.EncryptionType)

	// Gone afterwards.
	_, err = deleteRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.Error(t, err)
	assert.Equal(t, "RepositoryNotFoundException", err.Error())
}

func TestDeleteRepository_NotEmpty(t *testing.T) {
	gw, nc := newRepoLifecycleGateway(t)
	_, err := createRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)

	// Seed an image (manifest) so the repo is non-empty.
	store := handlers_ecr.NewNATSMetaStore(nc)
	require.NoError(t, store.PutManifestMeta(context.Background(), ecrTestAccount, "team/app", handlers_ecr.ManifestMeta{
		Digest: "sha256:" + strings.Repeat("a", 64), MediaType: "application/json", Size: 7, PushedAt: time.Now(),
	}))

	// Without force -> RepositoryNotEmptyException.
	_, err = deleteRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.Error(t, err)
	assert.Equal(t, "RepositoryNotEmptyException", err.Error())

	// With force -> deleted.
	w, err := deleteRepo(t, gw, `{"repositoryName":"team/app","force":true}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteRepository_CrossAccountDenied(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)
	_, err := createRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)
	_, err = deleteRepo(t, gw, `{"repositoryName":"team/app","registryId":"999999999999"}`)
	require.Error(t, err)
	assert.Equal(t, "AccessDenied", err.Error())
}

func TestECRRequest_CreateDeleteDispatched(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)

	create := setupECRRequest("AmazonEC2ContainerRegistry_V20150921.CreateRepository", `{"repositoryName":"team/app"}`)
	create = create.WithContext(context.WithValue(create.Context(), ctxAccountID, ecrTestAccount))
	wc := httptest.NewRecorder()
	require.NoError(t, gw.ECR_Request(wc, create))
	assert.Equal(t, http.StatusOK, wc.Code)

	del := setupECRRequest("AmazonEC2ContainerRegistry_V20150921.DeleteRepository", `{"repositoryName":"team/app"}`)
	del = del.WithContext(context.WithValue(del.Context(), ctxAccountID, ecrTestAccount))
	wd := httptest.NewRecorder()
	require.NoError(t, gw.ECR_Request(wd, del))
	assert.Equal(t, http.StatusOK, wd.Code)
}

func TestRepositoryActions_MissingComposition(t *testing.T) {
	gw := &GatewayConfig{
		Region:         ecrTestRegion,
		DisableLogging: true,
		IAMService:     allowAllIAMService(),
	}

	_, err := ecrRepositoryRequest(t, gw, "CreateRepository", `{"repositoryName":"team/app"}`)
	require.Error(t, err)
	assert.Equal(t, "ServerInternal", err.Error())
}
