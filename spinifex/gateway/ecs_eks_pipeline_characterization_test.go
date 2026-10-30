// Characterization tests for ECS and EKS dispatch through the full SigV4
// pipeline as ecs.go and eks.go implement it today. They assert current
// behaviour, not AWS parity.

package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/stretchr/testify/require"
)

const ecsPipelineTarget = "AmazonEC2ContainerServiceV20141113."

// containerPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes
// does, with no NATS connection: a request that clears authorization and
// validation therefore stops at the handler with ServerInternal.
func containerPipelineApp(t *testing.T, statements ...iamdomain.Statement) http.Handler {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	svc := &roleStoreIAMService{
		policyMockIAMService: policyMockIAMService{
			mockIAMService: mockIAMService{
				masterKey: testMasterKey,
				accessKeys: map[string]*iamdomain.AccessKey{
					testAccessKey: {
						AccessKeyID:     testAccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       authzAccountID,
						Status:          "Active",
					},
				},
			},
			getUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		},
		roles: map[string]map[string]string{
			authzAccountID:   {"node-role": eksNodeRoleARN},
			foreignAccountID: {"node-role": "arn:aws:iam::" + foreignAccountID + ":role/eks/node-role"},
		},
	}
	_, nc := testutil.StartTestNATS(t)
	gw := withVerifier(withContainers(&GatewayConfig{DisableLogging: true, Region: testRegion, NATSConn: nc, IAMService: svc}))
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return r
}

func signContainerRequest(t *testing.T, service, method, path, target string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Host = "localhost:9999"
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	signTestRequestAs(t, req, body, testAccessKey, testSecretKey, service, time.Now().UTC())
	return req
}

type pipelineOutcome struct {
	Status int
	Type   string
	Header string
}

func probeContainer(t *testing.T, app http.Handler, req *http.Request) pipelineOutcome {
	t.Helper()
	resp := doRequest(app, req)
	out := pipelineOutcome{Status: resp.StatusCode, Header: resp.Header.Get("X-Amzn-Errortype")}
	if resp.StatusCode >= 400 {
		out.Type = decodeJSONError(t, resp).Type
	}
	return out
}

func ecsBody(role string) []byte {
	return []byte(`{"family":"f","taskRoleArn":"` + role + `","containerDefinitions":[{"name":"c","image":"i","memory":32}]}`)
}

func eksClusterBody(role string) []byte {
	return []byte(`{"name":"c","roleArn":"` + role + `","resourcesVpcConfig":{"subnetIds":["s"]}}`)
}

// reachedHandler is what a request that clears every gateway check looks like
// here: the handler runs, finds no NATS responder and answers InternalError.
const reachedHandler = "InternalErrorException"

func TestECSEKSPipeline_Outcomes(t *testing.T) {
	ecsAll := statement("Allow", "ecs:*", "*")
	eksAll := statement("Allow", "eks:*", "*")
	pass := statement("Allow", "iam:PassRole", "*")
	reg := ecsPipelineTarget + "RegisterTaskDefinition"
	own := "arn:aws:iam::" + authzAccountID + ":role/"

	cases := []struct {
		name   string
		app    http.Handler
		req    *http.Request
		status int
		typ    string
	}{
		{"ecs missing target", containerPipelineApp(t, ecsAll), signContainerRequest(t, "ecs", "POST", "/", "", []byte("{}")), 400, "MissingActionException"},
		{"ecs unknown action", containerPipelineApp(t, ecsAll), signContainerRequest(t, "ecs", "POST", "/", ecsPipelineTarget+"Nope", []byte("{}")), 400, "InvalidActionException"},
		{"ecs no ecs policy", containerPipelineApp(t), signContainerRequest(t, "ecs", "POST", "/", reg, ecsBody("")), 403, "AccessDeniedException"},
		{"ecs role without PassRole", containerPipelineApp(t, ecsAll), signContainerRequest(t, "ecs", "POST", "/", reg, ecsBody(own+"r")), 403, "AccessDeniedException"},
		{"ecs malformed role reaches handler", containerPipelineApp(t, ecsAll, pass), signContainerRequest(t, "ecs", "POST", "/", reg, ecsBody("nope")), 500, reachedHandler},
		{"ecs unparseable body", containerPipelineApp(t, ecsAll), signContainerRequest(t, "ecs", "POST", "/", reg, []byte("{")), 500, reachedHandler},
		{"eks unknown route", containerPipelineApp(t, eksAll), signContainerRequest(t, "eks", "GET", "/nope", "", nil), 400, "InvalidActionException"},
		{"eks no eks policy", containerPipelineApp(t), signContainerRequest(t, "eks", "GET", "/clusters", "", nil), 403, "AccessDeniedException"},
		{"eks list reaches handler", containerPipelineApp(t, eksAll), signContainerRequest(t, "eks", "GET", "/clusters", "", nil), 500, reachedHandler},
		{"eks create not an ARN", containerPipelineApp(t, eksAll, pass), signContainerRequest(t, "eks", "POST", "/clusters", "", eksClusterBody("x")), 403, "AccessDeniedException"},
		{"eks create foreign role", containerPipelineApp(t, eksAll, pass), signContainerRequest(t, "eks", "POST", "/clusters", "", eksClusterBody("arn:aws:iam::"+foreignAccountID+":role/r")), 403, "AccessDeniedException"},
		{"eks create existing role without PassRole", containerPipelineApp(t, eksAll), signContainerRequest(t, "eks", "POST", "/clusters", "", eksClusterBody(eksNodeRoleARN)), 403, "AccessDeniedException"},
		{"eks create missing role", containerPipelineApp(t, eksAll, pass), signContainerRequest(t, "eks", "POST", "/clusters", "", eksClusterBody(own+"gone")), 400, "InvalidParameterException"},
		{"eks create missing role without PassRole", containerPipelineApp(t, eksAll), signContainerRequest(t, "eks", "POST", "/clusters", "", eksClusterBody(own+"gone")), 403, "AccessDeniedException"},
		{"eks unparseable body", containerPipelineApp(t, eksAll), signContainerRequest(t, "eks", "POST", "/clusters", "", []byte("{")), 500, reachedHandler},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := probeContainer(t, c.app, c.req)
			require.Equal(t, c.status, got.Status)
			require.Equal(t, c.typ, got.Type)
			require.Equal(t, c.typ, got.Header)
		})
	}
}
