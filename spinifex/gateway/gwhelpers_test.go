package gateway

import (
	"bytes"
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	rdsawsapi "github.com/mulgadc/spinifex/spinifex/domains/rds/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withACM registers the ACM control plane on gw backed by deps, as the awsgw
// role composes it. An unset deps.NATS takes gw.NATSConn.
func withACM(gw *GatewayConfig, deps acmawsapi.Deps) *GatewayConfig {
	if deps.NATS == nil {
		deps.NATS = gw.NATSConn
	}
	b := dispatch.NewBuilder()
	if err := b.Register(acmawsapi.NewRegistration(deps)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}

// acmPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does, on
// live test NATS, with docs as every principal's policy. The region is
// gwfix.Region because gwfix.SignRequestAs always signs for it.
func acmPipelineApp(t *testing.T, docs []iamdomain.PolicyDocument) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
		NATSConn:       nc,
		IAMService: &gwfix.PolicyIAM{
			IAM: gwfix.IAM{
				MasterKey: gwfix.MasterKey,
				AccessKeys: map[string]*iamdomain.AccessKey{
					gwfix.AccessKey: {
						AccessKeyID:     gwfix.AccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       authzAccountID,
						Status:          "Active",
					},
				},
			},
			GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
		},
	})

	withACM(gw, acmawsapi.Deps{})

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

// allowAllACMDocs is the policy acmPipelineApp uses when the test is not
// exercising authorization itself.
func allowAllACMDocs() []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
		},
	}}
}

// subscribeACMCounter answers subject with payload and counts invocations, so
// a denial can be proven to short-circuit before the handler ever runs.
func subscribeACMCounter(t *testing.T, nc *nats.Conn, subject string, payload []byte) *int {
	t.Helper()
	calls := 0
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		calls++
		_ = msg.Respond(payload)
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &calls
}

// signACMRequest builds a POST / signed with credential-scope service "acm",
// the shape every ACM JSON 1.1 client sends.
func signACMRequest(t *testing.T, target string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, "acm", time.Now().UTC())
	return req
}

func setupRDSRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx := reqctx.WithService(req.Context(), "rds")
	ctx = withPrincipalField(ctx, fieldAccountID, rdsTestAccountID)
	return withTestIdentity(req.WithContext(ctx))
}

// Counts policy resolutions, so a test can tell an authorized request from one
// that failed some other guard before the evaluator was ever reached.
type rdsPolicyProbe struct{ consulted int }

// A gateway whose IAM service answers with policies, so the RDS dispatch path
// runs a real evaluation for either principal branch.
func newRDSPolicyGateway(policies []iamdomain.PolicyDocument) (*GatewayConfig, *rdsPolicyProbe) {
	return newRDSGatewayWithResolver(func(_, _ string) ([]iamdomain.PolicyDocument, error) {
		return policies, nil
	})
}

// A signed customer request: an IAM user with a resolvable identity, which is
// what turns the policy check from the pre-IAM bypass into a real evaluation.
func setupRDSUserRequest(body, accountID string) *http.Request {
	req := setupRDSRequest(body)
	ctx := withPrincipalField(req.Context(), fieldAccountID, accountID)
	ctx = withPrincipalField(ctx, fieldIdentity, "alice")
	ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeUser)
	return req.WithContext(ctx)
}

const rdsTestAccountID = "123456789012"

// rdsRegistrationInventory is the inventory the RDS registration declares to a
// registry, keyed as AWSOperationInventory expects.
func rdsRegistrationInventory() map[string]dispatch.Inventory {
	return withRDS(&GatewayConfig{}).Services.Inventory()
}

// serveRDS dispatches r to the registered RDS service exactly as Request does,
// registering it first from the fields gw holds when the test has not.
func (gw *GatewayConfig) serveRDS(w http.ResponseWriter, r *http.Request) error {
	if gw.Services == nil {
		withRDS(gw)
	}
	entry, ok := gw.registered(rdsawsapi.ServiceName)
	if !ok {
		panic("RDS is not registered on this gateway")
	}
	return gw.dispatchRegistered(entry, w, r)
}

func newRDSGatewayWithResolver(
	resolve func(accountID, name string) ([]iamdomain.PolicyDocument, error),
) (*GatewayConfig, *rdsPolicyProbe) {
	probe := &rdsPolicyProbe{}
	counted := func(accountID, name string) ([]iamdomain.PolicyDocument, error) {
		probe.consulted++
		return resolve(accountID, name)
	}
	// Both branches are wired: an assumed-role principal resolves by role, and a
	// test that only stubbed users would miss it entirely.
	return withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         "ap-southeast-2",
		IAMService: &gwfix.PolicyIAM{
			GetUserPoliciesFn: counted,
			GetRolePoliciesFn: counted,
		},
	}), probe
}

// withRDS registers RDS on gw with the NATS connection and node count it holds,
// as the awsgw role composes it. Set both on gw before calling.
func withRDS(gw *GatewayConfig) *GatewayConfig {
	deps := rdsawsapi.Deps{NATS: gw.NATSConn, ExpectedNodes: gw.ExpectedNodes}
	if quotaFor(gw) != nil {
		deps.Quota = quotaFor(gw)
	}
	b := dispatch.NewBuilder()
	if err := b.Register(rdsawsapi.NewRegistration(deps)); err != nil {
		panic(err)
	}
	gw.Services = b.Build()
	return gw
}
