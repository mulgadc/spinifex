package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRepoLifecycleGateway wires the repo create/describe/delete + manifest
// subjects against an embedded KV-backed service, returning a gateway client.
func newRepoLifecycleGateway(t *testing.T) (*GatewayConfig, *nats.Conn) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	svc := handlers_ecr.NewKVMetaService(js)
	serveECRMeta(t, nc, handlers_ecr.SubjectRepoCreate, svc.RepoCreate)
	serveECRMeta(t, nc, handlers_ecr.SubjectRepoDescribe, svc.RepoDescribe)
	serveECRMeta(t, nc, handlers_ecr.SubjectRepoDelete, svc.RepoDelete)
	serveECRMeta(t, nc, handlers_ecr.SubjectManifestPut, svc.ManifestPut)
	serveECRMeta(t, nc, handlers_ecr.SubjectManifestList, svc.ManifestList)
	gw := &GatewayConfig{
		NATSConn: nc, Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, DisableLogging: true,
	}
	return gw, nc
}

func ecrLifecycleRequest(t *testing.T, gw *GatewayConfig, handler func(*GatewayConfig, http.ResponseWriter, *http.Request) error, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxAccountID, ecrTestAccount)
	w := httptest.NewRecorder()
	return w, handler(gw, w, req.WithContext(ctx))
}

func createRepo(t *testing.T, gw *GatewayConfig, body string) (*httptest.ResponseRecorder, error) {
	return ecrLifecycleRequest(t, gw, (*GatewayConfig).handleCreateRepository, body)
}

func deleteRepo(t *testing.T, gw *GatewayConfig, body string) (*httptest.ResponseRecorder, error) {
	return ecrLifecycleRequest(t, gw, (*GatewayConfig).handleDeleteRepository, body)
}

type repoOut struct {
	Repository struct {
		RepositoryName     string  `json:"repositoryName"`
		RegistryID         string  `json:"registryId"`
		RepositoryArn      string  `json:"repositoryArn"`
		RepositoryURI      string  `json:"repositoryUri"`
		ImageTagMutability string  `json:"imageTagMutability"`
		CreatedAt          float64 `json:"createdAt"`
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
			assert.Equal(t, tc.expect, err.Error())
		})
	}
}

// noAccountRequest builds a request without the auth-context account ID, which
// both handlers reject with ServerInternal before touching the store.
func noAccountRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
}

func TestCreateRepository_NoAccountAndMalformed(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)

	err := gw.handleCreateRepository(httptest.NewRecorder(), noAccountRequest(`{"repositoryName":"team/app"}`))
	require.Error(t, err)
	assert.Equal(t, "ServerInternal", err.Error())

	_, err = createRepo(t, gw, `{`)
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", err.Error())
}

func TestDeleteRepository_NoAccountAndMalformed(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)

	err := gw.handleDeleteRepository(httptest.NewRecorder(), noAccountRequest(`{"repositoryName":"team/app"}`))
	require.Error(t, err)
	assert.Equal(t, "ServerInternal", err.Error())

	_, err = deleteRepo(t, gw, `{`)
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", err.Error())
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
