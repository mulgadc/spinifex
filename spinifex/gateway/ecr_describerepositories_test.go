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
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveECRMeta wires a MetaService method to a NATS subject for the gateway-side
// DescribeRepositories tests, mirroring the daemon's handleNATSRequest.
func serveECRMeta[I any, O any](t *testing.T, nc *nats.Conn, subject string, fn func(context.Context, *I, string) (*O, error)) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		accountID := utils.AccountIDFromMsg(msg)
		in := new(I)
		if errResp := utils.UnmarshalJsonPayload(in, msg.Data); errResp != nil {
			_ = msg.Respond(errResp)
			return
		}
		out, err := fn(context.Background(), in, accountID)
		if err != nil {
			_ = msg.Respond(utils.GenerateErrorPayload("ServerInternal"))
			return
		}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func newDescribeReposGateway(t *testing.T, repos ...string) *GatewayConfig {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	svc := handlers_ecr.NewKVMetaService(js)
	serveECRMeta(t, nc, handlers_ecr.SubjectRepoCreate, svc.RepoCreate)
	serveECRMeta(t, nc, handlers_ecr.SubjectRepoDescribe, svc.RepoDescribe)
	serveECRMeta(t, nc, handlers_ecr.SubjectRepoList, svc.RepoList)

	store := handlers_ecr.NewNATSMetaStore(nc)
	for _, r := range repos {
		require.NoError(t, store.PutRepo(context.Background(), ecrTestAccount, handlers_ecr.RepoMeta{Name: r, CreatedAt: time.Now()}))
	}
	return &GatewayConfig{
		NATSConn: nc, Region: ecrTestRegion, InternalSuffix: ecrTestSuffix, DisableLogging: true,
	}
}

func describeReposRequest(t *testing.T, gw *GatewayConfig, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), ctxAccountID, ecrTestAccount)
	w := httptest.NewRecorder()
	require.NoError(t, gw.handleDescribeRepositories(w, req.WithContext(ctx)))
	return w
}

type describeReposOut struct {
	Repositories []struct {
		RepositoryName     string  `json:"repositoryName"`
		RegistryID         string  `json:"registryId"`
		RepositoryArn      string  `json:"repositoryArn"`
		RepositoryURI      string  `json:"repositoryUri"`
		ImageTagMutability string  `json:"imageTagMutability"`
		CreatedAt          float64 `json:"createdAt"`
	} `json:"repositories"`
}

func TestDescribeRepositories_ListsAccountScoped(t *testing.T) {
	gw := newDescribeReposGateway(t, "team/app", "team/web")
	w := describeReposRequest(t, gw, "{}")
	require.Equal(t, http.StatusOK, w.Code)

	var out describeReposOut
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.Repositories, 2)

	byName := map[string]int{}
	for i, r := range out.Repositories {
		byName[r.RepositoryName] = i
	}
	app := out.Repositories[byName["team/app"]]
	assert.Equal(t, ecrTestAccount, app.RegistryID)
	assert.Equal(t, "arn:aws:ecr:"+ecrTestRegion+":"+ecrTestAccount+":repository/team/app", app.RepositoryArn)
	assert.Equal(t, ecrTestAccount+".dkr.ecr."+ecrTestRegion+"."+ecrTestSuffix+"/team/app", app.RepositoryURI)
	assert.Equal(t, "MUTABLE", app.ImageTagMutability)
	assert.Positive(t, app.CreatedAt)
}

func TestECRRegistryHost_AppendsAdvertisedPort(t *testing.T) {
	parity := ecrTestAccount + ".dkr.ecr." + ecrTestRegion + "." + ecrTestSuffix
	cases := []struct {
		name         string
		registryHost string
		port         string
		want         string
	}{
		{"parity, port-less", "", "", parity},
		{"parity, 443 port-less", "", "443", parity},
		{"parity, 9999 appended", "", "9999", parity + ":9999"},
		{"gateway host, 9999 appended", "10.0.0.5", "9999", "10.0.0.5:9999"},
		{"gateway host, port-less", "registry.example.com", "", "registry.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &GatewayConfig{
				Region: ecrTestRegion, InternalSuffix: ecrTestSuffix,
				RegistryHost: tc.registryHost, RegistryPort: tc.port,
			}
			assert.Equal(t, tc.want, gw.ecrRegistryHost(ecrTestAccount))
			assert.Equal(t, tc.want+"/team/app", gw.ecrRepositoryUri(ecrTestAccount, "team/app"))
		})
	}
}

func TestDescribeRepositories_NameFilter(t *testing.T) {
	gw := newDescribeReposGateway(t, "team/app", "team/web")
	w := describeReposRequest(t, gw, `{"repositoryNames":["team/web"]}`)
	var out describeReposOut
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.Repositories, 1)
	assert.Equal(t, "team/web", out.Repositories[0].RepositoryName)
}

func TestDescribeRepositories_MissingNamedRepo(t *testing.T) {
	gw := newDescribeReposGateway(t, "team/app")
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"repositoryNames":["team/ghost"]}`))
	ctx := context.WithValue(req.Context(), ctxAccountID, ecrTestAccount)
	err := gw.handleDescribeRepositories(httptest.NewRecorder(), req.WithContext(ctx))
	require.Error(t, err)
	assert.Equal(t, "RepositoryNotFoundException", err.Error())
}

func TestDescribeRepositories_CrossAccountDenied(t *testing.T) {
	gw := newDescribeReposGateway(t, "team/app")
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"registryId":"999999999999"}`))
	ctx := context.WithValue(req.Context(), ctxAccountID, ecrTestAccount)
	err := gw.handleDescribeRepositories(httptest.NewRecorder(), req.WithContext(ctx))
	require.Error(t, err)
	assert.Equal(t, "AccessDenied", err.Error())
}

func TestECRRequest_DescribeRepositoriesDispatched(t *testing.T) {
	gw := newDescribeReposGateway(t, "team/app")
	req := setupECRRequest("AmazonEC2ContainerRegistry_V20150921.DescribeRepositories", "{}")
	ctx := context.WithValue(req.Context(), ctxAccountID, ecrTestAccount)
	w := httptest.NewRecorder()
	require.NoError(t, gw.ECR_Request(w, req.WithContext(ctx)))
	assert.Equal(t, http.StatusOK, w.Code)
}
