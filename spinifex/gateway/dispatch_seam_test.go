// Tests for the generic gateway consuming the dispatch registration seam
// with fake services registered only here, independent of any production
// registration.

package gateway

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Fake service names/actions registered only in this file.
const (
	fakeJSONService       = "testsvc"
	fakeXMLService        = "testsvcxml"
	fakeNoResolverService = "testsvcnoresolver"
	fakeAction            = "FakeAction"
	fakeXMLAction         = "XMLAction"
	fakeNoResolverAction  = "NoResolverAction"
)

// dispatchCapture records what a fake Dispatcher observed and controls what
// it does, so one Dispatcher body can drive every seam test's distinct need.
type dispatchCapture struct {
	accountID string
	region    string
	body      []byte
	bodyRead  bool

	// authorizeResource, when non-empty, makes the dispatcher call
	// inv.Authorize before succeeding, and return its error if any.
	authorizeResource string
	authorizeCalled   bool
	authorized        bool

	// resolveActor makes the dispatcher call inv.Actor and record the result.
	resolveActor bool
	actor        dispatch.Actor
	actorErr     error

	// dispatchErr, if set, is returned after the steps above run.
	dispatchErr error
}

func (c *dispatchCapture) dispatcher() dispatch.Dispatcher {
	return func(w http.ResponseWriter, inv dispatch.Invocation) error {
		body, err := io.ReadAll(inv.Request.Body)
		if err != nil {
			return err
		}
		c.accountID = inv.AccountID
		c.region = inv.Region
		c.body = body
		c.bodyRead = true

		if c.authorizeResource != "" {
			c.authorizeCalled = true
			if err := inv.Authorize(fakeJSONService, fakeAction, []string{c.authorizeResource}, nil); err != nil {
				return err
			}
			c.authorized = true
		}

		if c.resolveActor {
			actor, err := inv.Actor()
			c.actor, c.actorErr = actor, err
			if err != nil {
				return err
			}
		}

		if c.dispatchErr != nil {
			return c.dispatchErr
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
		return nil
	}
}

func fakeJSONRegistration(c *dispatchCapture) dispatch.Registration {
	return dispatch.Registration{
		Service:       fakeJSONService,
		ResolveAction: func(r *http.Request) string { return fakeAction },
		Dispatch:      c.dispatcher(),
		Errors:        dispatch.ErrorEnvelopeJSON,
		Inventory:     dispatch.Inventory{Registered: []string{fakeAction}},
	}
}

func fakeXMLRegistration(c *dispatchCapture) dispatch.Registration {
	return dispatch.Registration{
		Service:       fakeXMLService,
		ResolveAction: func(r *http.Request) string { return fakeXMLAction },
		Dispatch:      c.dispatcher(),
		Errors:        dispatch.ErrorEnvelopeXML,
		Inventory:     dispatch.Inventory{Registered: []string{fakeXMLAction}},
	}
}

// fakeNoResolverRegistration carries no ResolveAction, mirroring a legacy
// request no router matches: nothing names its action ahead of dispatch.
func fakeNoResolverRegistration(c *dispatchCapture) dispatch.Registration {
	return dispatch.Registration{
		Service:   fakeNoResolverService,
		Dispatch:  c.dispatcher(),
		Errors:    dispatch.ErrorEnvelopeJSON,
		Inventory: dispatch.Inventory{Registered: []string{fakeNoResolverAction}},
	}
}

// dispatchSeamApp wires SigV4AuthMiddleware -> traceActionEnricher -> Request
// on live test NATS, with regs registered through dispatch.NewBuilder, so a
// fake service's full pipeline mirrors ecrPipelineApp for a real one.
func dispatchSeamApp(t *testing.T, docs []handlers_iam.PolicyDocument, regs ...dispatch.Registration) (*GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	b := dispatch.NewBuilder()
	for _, reg := range regs {
		require.NoError(t, b.Register(reg))
	}

	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		NATSConn:       nc,
		Services:       b.Build(),
		IAMService: &policyMockIAMService{
			mockIAMService: mockIAMService{
				masterKey: testMasterKey,
				accessKeys: map[string]*handlers_iam.AccessKey{
					testAccessKey: {
						AccessKeyID:     testAccessKey,
						SecretAccessKey: encryptedSecret,
						UserName:        "alice",
						AccountID:       ecrTestAccount,
						Status:          "Active",
					},
				},
			},
			getUserPoliciesFn: func(_, _ string) ([]handlers_iam.PolicyDocument, error) { return docs, nil },
		},
	}

	r := chi.NewRouter()
	r.Use(requestAuditMiddleware)
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(traceActionEnricher)
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

// signDispatchRequest builds a POST / signed for a fake service, the same
// shape a JSON 1.1 client sends.
func signDispatchRequest(t *testing.T, service string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Host = "localhost:9999"
	req.Header.Set("X-Amz-Target", "Whatever."+fakeAction)
	signTestRequestAs(t, req, body, testAccessKey, testSecretKey, service, time.Now().UTC())
	return req
}

// --- a. request reaches the registered dispatcher with full context -------

func TestDispatchSeam_RequestReachesDispatcherWithContext(t *testing.T) {
	capt := &dispatchCapture{}
	_, app := dispatchSeamApp(t, allowAllECRDocs(), fakeJSONRegistration(capt))

	body := []byte(`{"hello":"world"}`)
	resp := doRequest(app, signDispatchRequest(t, fakeJSONService, body))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, capt.bodyRead, "dispatcher must have read the request body")
	assert.Equal(t, ecrTestAccount, capt.accountID)
	assert.Equal(t, testRegion, capt.region)
	assert.Equal(t, body, capt.body, "dispatcher must see the full body")
}

// --- b. resolver's action reaches throttle/trace/audit ---------------------

func TestDispatchSeam_ResolvedActionReachesThrottleTraceAndAudit(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	capt := &dispatchCapture{}
	gw, _ := dispatchSeamApp(t, allowAllECRDocs(), fakeJSONRegistration(capt))

	var gotCtxAction string
	var gotAudit *requestAudit
	r := chi.NewRouter()
	r.Use(otelsetup.HTTPMiddleware("awsgw"))
	r.Use(requestAuditMiddleware)
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(traceActionEnricher)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotCtxAction, _ = r.Context().Value(ctxAction).(string)
			next.ServeHTTP(w, r)
			gotAudit = auditFrom(r.Context())
		})
	})
	r.HandleFunc("/*", gw.Request)

	doRequest(r, signDispatchRequest(t, fakeJSONService, []byte("{}")))

	assert.Equal(t, fakeAction, gotCtxAction, "resolver's label must reach ctxAction for the throttle key")

	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, fakeJSONService+"."+fakeAction, span.Name())
	got := map[string]string{}
	for _, kv := range span.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, fakeAction, got["aws.action"])
	assert.Equal(t, fakeJSONService, got["aws.service"])

	require.NotNil(t, gotAudit)
	assert.Equal(t, fakeAction, gotAudit.action)
}

// --- c. Authorize denies or allows ------------------------------------------

func TestDispatchSeam_AuthorizeDeniesOnMismatchAllowsOnMatch(t *testing.T) {
	const allowedResource = "arn:aws:testsvc:" + testRegion + ":" + ecrTestAccount + ":widget/widget1"
	const otherResource = "arn:aws:testsvc:" + testRegion + ":" + ecrTestAccount + ":widget/other"

	t.Run("scoped to a different resource is denied", func(t *testing.T) {
		capt := &dispatchCapture{authorizeResource: allowedResource}
		scopedDocs := []handlers_iam.PolicyDocument{{
			Version: "2012-10-17",
			Statement: []handlers_iam.Statement{
				{Effect: "Allow", Action: handlers_iam.StringOrArr{"testsvc:FakeAction"}, Resource: handlers_iam.StringOrArr{otherResource}},
			},
		}}
		_, app := dispatchSeamApp(t, scopedDocs, fakeJSONRegistration(capt))
		resp := doRequest(app, signDispatchRequest(t, fakeJSONService, []byte("{}")))

		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		body := decodeJSONError(t, resp)
		assert.Equal(t, "AccessDeniedException", body.Type)
		assert.True(t, capt.authorizeCalled)
		assert.False(t, capt.authorized)
	})

	t.Run("matching policy allows", func(t *testing.T) {
		capt := &dispatchCapture{authorizeResource: allowedResource}
		matchingDocs := []handlers_iam.PolicyDocument{{
			Version: "2012-10-17",
			Statement: []handlers_iam.Statement{
				{Effect: "Allow", Action: handlers_iam.StringOrArr{"testsvc:FakeAction"}, Resource: handlers_iam.StringOrArr{allowedResource}},
			},
		}}
		_, app := dispatchSeamApp(t, matchingDocs, fakeJSONRegistration(capt))
		resp := doRequest(app, signDispatchRequest(t, fakeJSONService, []byte("{}")))

		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.True(t, capt.authorized)
	})
}

// --- d. Actor returns the verified caller, ignoring the body ---------------

func TestDispatchSeam_ActorReturnsVerifiedCallerIgnoringBody(t *testing.T) {
	capt := &dispatchCapture{resolveActor: true}
	_, app := dispatchSeamApp(t, allowAllECRDocs(), fakeJSONRegistration(capt))

	// A foreign account/ARN in the body must never reach the Actor: it comes
	// from the verified SigV4 context, never from request content.
	body := []byte(`{"accountId":"999999999999","callerArn":"arn:aws:iam::999999999999:user/evil"}`)
	resp := doRequest(app, signDispatchRequest(t, fakeJSONService, body))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, capt.actorErr)
	assert.Equal(t, ecrTestAccount, capt.actor.AccountID)
	assert.Equal(t, "arn:aws:iam::"+ecrTestAccount+":user/alice", capt.actor.CallerARN)
	assert.Equal(t, principalTypeUser, capt.actor.PrincipalType)
	assert.Equal(t, testAccessKey, capt.actor.AccessKeyID)
}

// --- e. dispatcher error renders in the registration's declared envelope ---

func TestDispatchSeam_DispatcherErrorRendersInDeclaredEnvelope(t *testing.T) {
	t.Run("JSON envelope service", func(t *testing.T) {
		capt := &dispatchCapture{dispatchErr: errors.New(awserrors.ErrorInternalError)}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), fakeJSONRegistration(capt))
		resp := doRequest(app, signDispatchRequest(t, fakeJSONService, []byte("{}")))

		assert.Equal(t, eksJSONContentType, resp.Header.Get("Content-Type"))
		assert.Equal(t, jsonErrorType(awserrors.ErrorInternalError), resp.Header.Get("X-Amzn-Errortype"))
		body := decodeJSONError(t, resp)
		assert.Equal(t, jsonErrorType(awserrors.ErrorInternalError), body.Type)
	})

	t.Run("XML envelope service", func(t *testing.T) {
		capt := &dispatchCapture{dispatchErr: errors.New(awserrors.ErrorInternalError)}
		_, app := dispatchSeamApp(t, allowAllECRDocs(), fakeXMLRegistration(capt))
		resp := doRequest(app, signDispatchRequest(t, fakeXMLService, []byte("{}")))

		assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Contains(t, string(b), awserrors.ErrorInternalError)
	})
}

// --- f. NATS down gets the same 503 a legacy service gets ------------------

func TestDispatchSeam_NATSDownGetsSame503AsLegacy(t *testing.T) {
	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(fakeJSONRegistration(&dispatchCapture{})))

	gw := &GatewayConfig{DisableLogging: true, IAMService: allowAllIAMService(), Services: b.Build()}

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := context.WithValue(req.Context(), ctxService, fakeJSONService)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	gw.Request(w, req)
	resp := w.Result()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, eksJSONContentType, resp.Header.Get("Content-Type"))
	body := decodeJSONError(t, resp)
	assert.Equal(t, "ServiceUnavailableException", body.Type)
}

// --- g. an unregistered, unknown service still gets InvalidAction ----------

// A non-nil, populated registry must not change the answer for a scope
// nothing serves: the existing UnservedService characterization tests in
// auth_test.go pin the same behaviour with a nil registry.
func TestDispatchSeam_UnregisteredUnknownServiceStillInvalidAction(t *testing.T) {
	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(fakeJSONRegistration(&dispatchCapture{})))

	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	require.NoError(t, err)
	gw := &GatewayConfig{
		DisableLogging: true,
		Region:         testRegion,
		Services:       b.Build(),
		IAMService: &mockIAMService{
			masterKey: testMasterKey,
			accessKeys: map[string]*handlers_iam.AccessKey{
				testAccessKey: {AccessKeyID: testAccessKey, SecretAccessKey: encryptedSecret, UserName: "root", Status: "Active"},
			},
		},
	}

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequestAs(t, req, nil, testAccessKey, testSecretKey, "route53", time.Now().UTC())

	resp := doRequest(r, req)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var parsed struct {
		Error struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		} `xml:"Error"`
	}
	require.NoError(t, xml.Unmarshal(body, &parsed))
	assert.Equal(t, awserrors.ErrorInvalidAction, parsed.Error.Code)
	assert.Contains(t, parsed.Error.Message, "route53")
}

// --- h. ValidateServices overlap guard --------------------------------------

func TestValidateServices(t *testing.T) {
	t.Run("nil registry passes", func(t *testing.T) {
		gw := &GatewayConfig{}
		assert.NoError(t, gw.ValidateServices())
	})

	t.Run("empty registry passes", func(t *testing.T) {
		gw := &GatewayConfig{Services: dispatch.NewBuilder().Build()}
		assert.NoError(t, gw.ValidateServices())
	})

	t.Run("claiming a legacy name fails", func(t *testing.T) {
		b := dispatch.NewBuilder()
		require.NoError(t, b.Register(dispatch.Registration{
			Service:   "acm",
			Dispatch:  func(http.ResponseWriter, dispatch.Invocation) error { return nil },
			Errors:    dispatch.ErrorEnvelopeJSON,
			Inventory: dispatch.Inventory{Registered: []string{fakeAction}},
		}))
		gw := &GatewayConfig{Services: b.Build()}
		assert.Error(t, gw.ValidateServices())
	})
}

// --- i. a registered service with no resolver still dispatches -------------

// The throttle key function falls back to the literal "unknown" for a
// request with no ctxAction (gateway.go's throttleKeyFuncs), exactly what an
// unresolved legacy request leaves it at today.
func TestDispatchSeam_NoResolverDispatchesWithLegacyFallback(t *testing.T) {
	capt := &dispatchCapture{}
	gw, _ := dispatchSeamApp(t, allowAllECRDocs(), fakeNoResolverRegistration(capt))

	var ctxActionSet bool
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, ctxActionSet = r.Context().Value(ctxAction).(string)
			next.ServeHTTP(w, r)
		})
	})
	r.HandleFunc("/*", gw.Request)

	resp := doRequest(r, signDispatchRequest(t, fakeNoResolverService, []byte("{}")))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, capt.bodyRead, "dispatch must still run with no resolver")
	assert.False(t, ctxActionSet, "no resolver means ctxAction is never set, same as an unresolved legacy request")

	key, err := gw.throttleKeyFuncs()[1](httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	assert.Equal(t, "unknown", key, "the documented fallback for a request with no ctxAction")
}

// compile-time sanity: AuthorizeFunc/ActorFunc match the closures Request builds.
var (
	_ dispatch.AuthorizeFunc = (func(string, string, []string, iampolicy.ConditionKeys) error)(nil)
	_ dispatch.ActorFunc     = (func() (dispatch.Actor, error))(nil)
)
