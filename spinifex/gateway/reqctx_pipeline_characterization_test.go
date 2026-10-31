// Characterization tests for the orders and request-scoped facts the signed
// pipeline carries today, driven through SetupRoutes: throttle against both
// readiness gates and the policy gate, /admin against selection and the second
// gate, the request id each error writer mints against the audit record, and
// the credential-scope Region against the configured one. They assert current
// behaviour, not AWS parity.

package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/ratelimit"
	"github.com/mulgadc/spinifex/internal/testkit"
	ecsawsapi "github.com/mulgadc/spinifex/spinifex/domains/ecs/awsapi"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// reqctxRegion is the gateway's configured Region in these tests, distinct from
// the us-east-1 a global service signs with.
const reqctxRegion = "ap-southeast-2"

// natsState is how the message bus looks to the pipeline.
type natsState int

const (
	natsUp natsState = iota
	// natsAbsent is a nil connection: the SigV4 gate skips it, the second gate does not.
	natsAbsent
	// natsClosed is a connection that exists and reports not connected.
	natsClosed
)

type reqctxGatewayEnv struct {
	NATS      natsState
	Throttler *ratelimit.Throttler
	// Probe, when set, is registered as a route53 dispatcher.
	Probe dispatch.Dispatcher
}

// reqctxGateway builds a gateway with ECS, IAM, the operator surface and an
// optional route53 probe over a caller who holds only the given statements.
func reqctxGateway(t *testing.T, env reqctxGatewayEnv, statements ...iamdomain.Statement) *GatewayConfig {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: statements}}
	base := gwfix.PolicyIAM{
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
	}
	svc := &iamAuthzPolicyService{
		PolicyIAM: base,
		arns:      map[string]string{"alice": "arn:aws:iam::" + authzAccountID + ":user/alice"},
	}

	var nc *nats.Conn
	if env.NATS != natsAbsent {
		_, nc = testutil.StartTestNATS(t)
		if env.NATS == natsClosed {
			nc.Close()
		}
	}
	gw := withVerifier(&GatewayConfig{
		DisableLogging: true,
		Region:         reqctxRegion,
		NATSConn:       nc,
		IAMService:     svc,
		Throttler:      env.Throttler,
	})
	regs := []dispatch.Registration{
		ecsawsapi.NewRegistration(ecsawsapi.Deps{NATS: nc}),
		iamRegistration(gw),
		operatoradmin.NewRegistration(operatoradmin.Deps{NATS: nc, IAM: svc, Quota: quotaFor(gw)}),
	}
	if env.Probe != nil {
		regs = append(regs, dispatch.Registration{
			Service:       "route53",
			ResolveAction: func(*http.Request) string { return "ProbeAction" },
			Dispatch:      env.Probe,
			Errors:        dispatch.ErrorEnvelopeXML,
			Inventory:     dispatch.Inventory{Registered: []string{"ProbeAction"}},
		})
	}
	return withServices(gw, regs...)
}

func newThrottler(t *testing.T) *ratelimit.Throttler {
	t.Helper()
	th := ratelimit.New(ratelimit.Config{Enabled: true, Rate: 1, Burst: 1})
	t.Cleanup(th.Stop)
	return th
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// signReqctx signs req for service in region as the test caller.
func signReqctx(t *testing.T, req *http.Request, body []byte, service, region string) *http.Request {
	t.Helper()
	req.Host = "localhost:9999"
	payloadHash := sha256Hex(body)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	require.NoError(t, v4.NewSigner().SignHTTP(t.Context(),
		aws.Credentials{AccessKeyID: gwfix.AccessKey, SecretAccessKey: gwfix.SecretKey},
		req, payloadHash, service, region, time.Now().UTC()))
	return req
}

// ecsListClusters is a signed ECS JSON-protocol request.
func ecsListClusters(t *testing.T) *http.Request {
	t.Helper()
	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Amz-Target", ecsPipelineTarget+"ListClusters")
	return signReqctx(t, req, body, "ecs", reqctxRegion)
}

// iamGetUser is a signed IAM Query-protocol request.
func iamGetUser(t *testing.T) *http.Request {
	t.Helper()
	body := []byte("Action=GetUser&Version=2010-05-08")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	return signReqctx(t, req, body, "iam", reqctxRegion)
}

var xmlCodePattern = regexp.MustCompile(`<Code>([^<]+)</Code>`)

type anyOutcome struct {
	Status int
	Code   string
	Body   string
}

// probeAny serves req and reads the error code from the JSON error-type header
// or, failing that, the XML <Code>.
func probeAny(app http.Handler, req *http.Request) anyOutcome {
	resp := doRequest(app, req)
	raw, _ := io.ReadAll(resp.Body)
	out := anyOutcome{Status: resp.StatusCode, Body: string(raw), Code: resp.Header.Get("X-Amzn-Errortype")}
	if out.Code == "" {
		if m := xmlCodePattern.FindSubmatch(raw); m != nil {
			out.Code = string(m[1])
		}
	}
	return out
}

// requests the throttle, readiness and policy orders are pinned for: one JSON
// service and one Query service.
var reqctxServices = []struct {
	name string
	make func(*testing.T) *http.Request
}{
	{"ecs json", ecsListClusters},
	{"iam query", iamGetUser},
}

// With the bus up and a caller the policy refuses, the first call is refused by
// the policy gate and the second by the throttle: the throttle spends its
// token ahead of the policy gate.
func TestReqctxPipeline_ThrottleRunsAheadOfPolicyGate(t *testing.T) {
	for _, svc := range reqctxServices {
		t.Run(svc.name, func(t *testing.T) {
			app := reqctxGateway(t, reqctxGatewayEnv{NATS: natsUp, Throttler: newThrottler(t)}).SetupRoutes()
			first := probeAny(app, svc.make(t))
			second := probeAny(app, svc.make(t))
			t.Logf("first=%+v second=%+v", first, second)
			assert.Equal(t, http.StatusForbidden, first.Status, first.Body)
			assert.Contains(t, first.Code, "AccessDenied", first.Body)
			assert.NotEqual(t, first.Code, second.Code, second.Body)
			assert.Contains(t, []string{"Throttling", "ThrottlingException", "RequestLimitExceeded", "RequestLimitExceededException"}, second.Code, second.Body)
		})
	}
}

// A nil connection passes the SigV4 gate, spends the throttle token, and is
// refused by the second gate ahead of the policy gate. The repeat is refused by
// the throttle, so the throttle runs ahead of the second gate.
func TestReqctxPipeline_AbsentNATSIsCaughtByTheSecondGateAfterThrottle(t *testing.T) {
	for _, svc := range reqctxServices {
		t.Run(svc.name, func(t *testing.T) {
			app := reqctxGateway(t, reqctxGatewayEnv{NATS: natsAbsent, Throttler: newThrottler(t)}).SetupRoutes()
			first := probeAny(app, svc.make(t))
			second := probeAny(app, svc.make(t))
			t.Logf("first=%+v second=%+v", first, second)
			assert.Equal(t, http.StatusServiceUnavailable, first.Status, first.Body)
			assert.Contains(t, first.Code, "ServiceUnavailable", first.Body)
			assert.Contains(t, first.Body, "cluster unavailable")
			assert.NotEqual(t, first.Code, second.Code, second.Body)
		})
	}
}

// A connection that reports not connected is refused by the SigV4 gate before
// the throttle runs, so no token is spent and every repeat gets the same 503.
func TestReqctxPipeline_ClosedNATSIsCaughtByTheFirstGateBeforeThrottle(t *testing.T) {
	for _, svc := range reqctxServices {
		t.Run(svc.name, func(t *testing.T) {
			app := reqctxGateway(t, reqctxGatewayEnv{NATS: natsClosed, Throttler: newThrottler(t)}).SetupRoutes()
			first := probeAny(app, svc.make(t))
			second := probeAny(app, svc.make(t))
			third := probeAny(app, svc.make(t))
			t.Logf("first=%+v", first)
			assert.Equal(t, http.StatusServiceUnavailable, first.Status, first.Body)
			assert.Contains(t, first.Code, "ServiceUnavailable", first.Body)
			assert.Contains(t, first.Body, "cluster unavailable")
			assert.Equal(t, first.Code, second.Code)
			assert.Equal(t, first.Code, third.Code)
			assert.Equal(t, first.Status, third.Status)
		})
	}
}

// signAdmin signs a /admin/<method> request for the given credential-scope service.
func signAdmin(t *testing.T, service, method string) *http.Request {
	t.Helper()
	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/"+method, bytes.NewReader(body))
	return signReqctx(t, req, body, service, reqctxRegion)
}

// signSpinifexQuery signs a Query-protocol call to the spinifex service on "/".
func signSpinifexQuery(t *testing.T, action string) *http.Request {
	t.Helper()
	body := []byte("Action=" + action)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	return signReqctx(t, req, body, operatoradmin.ServiceName, reqctxRegion)
}

// /admin does not run the second readiness gate: with an absent connection the
// same unknown method is dispatched and answered 400 in the admin JSON, where the
// identical call on "/" is refused 503 by the gate.
func TestReqctxPipeline_AdminBypassesTheSecondGate(t *testing.T) {
	app := reqctxGateway(t, reqctxGatewayEnv{NATS: natsAbsent}).SetupRoutes()

	viaAdmin := probeAny(app, signAdmin(t, operatoradmin.ServiceName, "PurgeEverything"))
	viaRoot := probeAny(app, signSpinifexQuery(t, "PurgeEverything"))
	t.Logf("admin=%+v root=%+v", viaAdmin, viaRoot)

	assert.Equal(t, http.StatusBadRequest, viaAdmin.Status, viaAdmin.Body)
	assert.Contains(t, viaAdmin.Body, `"code":"InvalidAction"`)
	assert.Equal(t, http.StatusServiceUnavailable, viaRoot.Status, viaRoot.Body)
	assert.Contains(t, viaRoot.Body, "cluster unavailable")
}

// /admin resolves the spinifex registration itself rather than the signed
// service: a call signed for IAM reaches the same dispatcher and answers in
// the admin JSON, not IAM's XML.
func TestReqctxPipeline_AdminIgnoresTheSignedService(t *testing.T) {
	app := reqctxGateway(t, reqctxGatewayEnv{NATS: natsUp}).SetupRoutes()

	signedSpinifex := probeAny(app, signAdmin(t, operatoradmin.ServiceName, "PurgeEverything"))
	signedIAM := probeAny(app, signAdmin(t, "iam", "PurgeEverything"))
	t.Logf("spinifex=%+v iam=%+v", signedSpinifex, signedIAM)

	assert.Equal(t, signedSpinifex.Status, signedIAM.Status)
	assert.Equal(t, http.StatusBadRequest, signedIAM.Status, signedIAM.Body)
	assert.Contains(t, signedIAM.Body, `"code":"InvalidAction"`)
	assert.NotContains(t, signedIAM.Body, "<")
}

var (
	xmlRequestIDPattern  = regexp.MustCompile(`<RequestI[dD]>([^<]+)</RequestI[dD]>`)
	jsonRequestIDPattern = regexp.MustCompile(`"requestId":"([^"]+)"`)
	uuidPattern          = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

const edgeRequestID = "4f1c2b7e9a0d4e6f8b3c5a7d9e1f2a3b"

// auditedPipeline serves SetupRoutes' handler behind a probe that remembers the
// audit record the gateway wrote for the request. SetupRoutes installs its own
// audit.Middleware, so the probe reads the record from the dispatch path's
// context through a tap installed on the same chain.
func auditedPipeline(t *testing.T, gw *GatewayConfig) (http.Handler, func() *audit.Record) {
	t.Helper()
	var got *audit.Record
	r := chi.NewRouter()
	r.Use(audit.Middleware)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			got = audit.From(req.Context())
			next.ServeHTTP(w, req)
		})
	})
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(TraceActionEnricher)
	if gw.Throttler != nil {
		r.Use(gw.Throttler.Middleware(gw.throttleKeyFuncs(), gw.writeThrottleError))
	}
	r.HandleFunc("/admin/{method}", gw.Admin_Request)
	r.HandleFunc("/*", gw.Request)
	return r, func() *audit.Record { return got }
}

// Each error writer mints its own request id, so the id in the error body never
// equals the id the audit record took from the edge's X-Request-ID.
func TestReqctxPipeline_ErrorBodyRequestIDDiffersFromAuditRecord(t *testing.T) {
	unsigned := func(*testing.T) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetUser"))
	}
	cases := []struct {
		name    string
		env     reqctxGatewayEnv
		req     func(*testing.T) *http.Request
		pattern *regexp.Regexp
		prime   func(*testing.T, http.Handler)
	}{
		{"auth rejection", reqctxGatewayEnv{NATS: natsUp}, unsigned, xmlRequestIDPattern, nil},
		{"cluster unavailable", reqctxGatewayEnv{NATS: natsClosed}, iamGetUser, xmlRequestIDPattern, nil},
		{"policy denial through ErrorHandler", reqctxGatewayEnv{NATS: natsUp}, iamGetUser, xmlRequestIDPattern, nil},
		{"throttle", reqctxGatewayEnv{NATS: natsUp}, iamGetUser, xmlRequestIDPattern, func(t *testing.T, h http.Handler) {
			probeAny(h, iamGetUser(t))
		}},
		{"admin", reqctxGatewayEnv{NATS: natsUp}, func(t *testing.T) *http.Request { return signAdmin(t, operatoradmin.ServiceName, "PurgeEverything") }, jsonRequestIDPattern, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "throttle" {
				c.env.Throttler = newThrottler(t)
			}
			app, record := auditedPipeline(t, reqctxGateway(t, c.env))
			if c.prime != nil {
				c.prime(t, app)
			}
			req := c.req(t)
			req.Header.Set("X-Request-ID", edgeRequestID)
			out := probeAny(app, req)
			t.Logf("outcome=%+v", out)

			m := c.pattern.FindStringSubmatch(out.Body)
			require.NotNil(t, m, "no request id in %s", out.Body)
			require.NotNil(t, record())
			assert.Equal(t, edgeRequestID, auditField(record(), "requestID"))
			assert.Regexp(t, uuidPattern, m[1])
			assert.NotEqual(t, edgeRequestID, m[1])
		})
	}
}

// The admin response carries the id it minted in a header as well, and that id
// is the one in the body, not the audit record's.
func TestReqctxPipeline_AdminHeaderRequestIDMatchesBody(t *testing.T) {
	app, record := auditedPipeline(t, reqctxGateway(t, reqctxGatewayEnv{NATS: natsUp}))
	req := signAdmin(t, operatoradmin.ServiceName, "PurgeEverything")
	req.Header.Set("X-Request-ID", edgeRequestID)
	resp := doRequest(app, req)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	m := jsonRequestIDPattern.FindStringSubmatch(string(raw))
	require.NotNil(t, m, string(raw))
	assert.Equal(t, m[1], resp.Header.Get("X-Amzn-Requestid"))
	assert.NotEqual(t, auditField(record(), "requestID"), m[1])
}

// Invocation.Region is the gateway's configured Region for every service. The
// credential-scope Region is not carried to the dispatcher; it reaches only the
// span, so a global service signed for us-east-1 differs between the two.
func TestReqctxPipeline_CredentialScopeRegionVersusInvocationRegion(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	var invRegion, invService string
	probe := func(w http.ResponseWriter, inv dispatch.Invocation) error {
		invRegion, invService = inv.Region, inv.Service
		w.WriteHeader(http.StatusOK)
		return nil
	}
	app := reqctxGateway(t, reqctxGatewayEnv{NATS: natsUp, Probe: probe}, statement("Allow", "*", "*")).SetupRoutes()

	body := []byte("Action=ProbeAction")
	for _, region := range []string{"us-east-1", reqctxRegion} {
		t.Run("route53 signed "+region, func(t *testing.T) {
			invRegion, invService = "", ""
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
			got := doRequest(app, signReqctx(t, req, body, "route53", region))
			require.Equal(t, http.StatusOK, got.StatusCode)

			assert.Equal(t, reqctxRegion, invRegion)
			assert.Equal(t, "route53", invService)
			var spanRegion string
			for _, s := range sr.Ended() {
				for _, kv := range s.Attributes() {
					if string(kv.Key) == "aws.region" {
						spanRegion = kv.Value.AsString()
					}
				}
			}
			assert.Equal(t, region, spanRegion)
		})
	}
}

// iamAuthzPolicyService resolves a role or user name to a fixed ARN.
type iamAuthzPolicyService struct {
	gwfix.PolicyIAM

	arns map[string]string
}

func (s *iamAuthzPolicyService) CanonicalResourceARN(_ string, _ arn.IAMResourceType, name string) (string, error) {
	resource, ok := s.arns[name]
	if !ok {
		return "", errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return resource, nil
}
