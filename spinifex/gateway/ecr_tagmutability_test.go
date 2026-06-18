package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func putTagMutability(t *testing.T, gw *GatewayConfig, body string) (*httptest.ResponseRecorder, error) {
	return ecrLifecycleRequest(t, gw, (*GatewayConfig).handlePutImageTagMutability, body)
}

type tagMutabilityOut struct {
	RegistryID         string `json:"registryId"`
	RepositoryName     string `json:"repositoryName"`
	ImageTagMutability string `json:"imageTagMutability"`
}

func TestCreateRepository_Immutable(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)
	w, err := createRepo(t, gw, `{"repositoryName":"team/app","imageTagMutability":"IMMUTABLE"}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)

	var out repoOut
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "IMMUTABLE", out.Repository.ImageTagMutability)

	// Persisted: a named describe reflects the stored value.
	dw := describeReposRequest(t, gw, `{"repositoryNames":["team/app"]}`)
	var dout describeReposOut
	require.NoError(t, json.Unmarshal(dw.Body.Bytes(), &dout))
	require.Len(t, dout.Repositories, 1)
	assert.Equal(t, "IMMUTABLE", dout.Repositories[0].ImageTagMutability)
}

func TestCreateRepository_InvalidMutability(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)
	_, err := createRepo(t, gw, `{"repositoryName":"team/app","imageTagMutability":"SOMETIMES"}`)
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", err.Error())
}

func TestPutImageTagMutability_Happy(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)
	_, err := createRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)

	w, err := putTagMutability(t, gw, `{"repositoryName":"team/app","imageTagMutability":"IMMUTABLE"}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)
	var out tagMutabilityOut
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, ecrTestAccount, out.RegistryID)
	assert.Equal(t, "team/app", out.RepositoryName)
	assert.Equal(t, "IMMUTABLE", out.ImageTagMutability)

	// Persisted.
	dw := describeReposRequest(t, gw, `{"repositoryNames":["team/app"]}`)
	var dout describeReposOut
	require.NoError(t, json.Unmarshal(dw.Body.Bytes(), &dout))
	assert.Equal(t, "IMMUTABLE", dout.Repositories[0].ImageTagMutability)

	// Flip back to MUTABLE.
	w, err = putTagMutability(t, gw, `{"repositoryName":"team/app","imageTagMutability":"MUTABLE"}`)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "MUTABLE", out.ImageTagMutability)
}

func TestPutImageTagMutability_Errors(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)
	_, err := createRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)

	cases := []struct {
		name, body, expect string
	}{
		{"missing value", `{"repositoryName":"team/app"}`, "InvalidParameterValue"},
		{"invalid value", `{"repositoryName":"team/app","imageTagMutability":"NOPE"}`, "InvalidParameterValue"},
		{"invalid name", `{"repositoryName":"Team/App","imageTagMutability":"IMMUTABLE"}`, "InvalidParameterValue"},
		{"cross-account", `{"repositoryName":"team/app","registryId":"999999999999","imageTagMutability":"IMMUTABLE"}`, "AccessDenied"},
		{"not found", `{"repositoryName":"team/ghost","imageTagMutability":"IMMUTABLE"}`, "RepositoryNotFoundException"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := putTagMutability(t, gw, tc.body)
			require.Error(t, err)
			assert.Equal(t, tc.expect, err.Error())
		})
	}
}

func TestPutImageTagMutability_NoAccountAndMalformed(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)

	err := gw.handlePutImageTagMutability(httptest.NewRecorder(), noAccountRequest(`{"repositoryName":"team/app","imageTagMutability":"IMMUTABLE"}`))
	require.Error(t, err)
	assert.Equal(t, "ServerInternal", err.Error())

	_, err = putTagMutability(t, gw, `{`)
	require.Error(t, err)
	assert.Equal(t, "InvalidParameterValue", err.Error())
}

func TestECRRequest_PutImageTagMutabilityDispatched(t *testing.T) {
	gw, _ := newRepoLifecycleGateway(t)
	_, err := createRepo(t, gw, `{"repositoryName":"team/app"}`)
	require.NoError(t, err)

	req := setupECRRequest("AmazonEC2ContainerRegistry_V20150921.PutImageTagMutability", `{"repositoryName":"team/app","imageTagMutability":"IMMUTABLE"}`)
	req = req.WithContext(context.WithValue(req.Context(), ctxAccountID, ecrTestAccount))
	w := httptest.NewRecorder()
	require.NoError(t, gw.ECR_Request(w, req))
	assert.Equal(t, http.StatusOK, w.Code)
}
