package pipeline

import (
	"bytes"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
)

// The throttle rejection per service, through the registrations production
// composes where there is one. EKS is a JSON registration, so it never takes
// the XML arm.
func TestWriteThrottleError_ByService(t *testing.T) {
	for _, tc := range []struct {
		svc, code, contains string
		status              int
		contentType         string
		with                func(*Config) *Config
	}{
		{"iam", awserrors.ErrorThrottling, "<ErrorResponse>", http.StatusServiceUnavailable, "application/xml", withIAM},
		{"sts", awserrors.ErrorThrottling, "<ErrorResponse>", http.StatusServiceUnavailable, "application/xml", withSTS},
		{"eks", "", `"__type"`, 0, envelope.JSONContentType, withContainers},
		{"s3", awserrors.ErrorRequestLimitExceeded, "<Error>", http.StatusServiceUnavailable, "application/xml", func(gw *Config) *Config { return withServices(gw) }},
		{"unserved-service", awserrors.ErrorRequestLimitExceeded, "<ErrorResponse>", http.StatusServiceUnavailable, "application/xml", nil},
		{"", awserrors.ErrorRequestLimitExceeded, "<Response>", http.StatusServiceUnavailable, "application/xml", nil},
	} {
		t.Run(tc.svc, func(t *testing.T) {
			gw := &Config{DisableLogging: true}
			if tc.with != nil {
				gw = tc.with(gw)
			}
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req = req.WithContext(reqctx.WithService(req.Context(), tc.svc))
			w := httptest.NewRecorder()

			gw.writeThrottleError(w, req)

			body, _ := io.ReadAll(w.Result().Body)
			if tc.status != 0 {
				assert.Equal(t, tc.status, w.Result().StatusCode)
			}
			assert.Equal(t, tc.contentType, w.Result().Header.Get("Content-Type"))
			if tc.code != "" {
				assert.Contains(t, string(body), "<Code>"+tc.code+"</Code>")
			}
			assert.Contains(t, string(body), tc.contains)
		})
	}
}

// An unparsed signature leaves no service, and the rejection keeps EC2's shape.
func TestXMLErrorBody_EmptyServiceKeepsEC2Shape(t *testing.T) {
	body := (&Config{}).xmlErrorBody("", "AccessDenied", "denied", "req-1", "/")

	assert.Equal(t, "Response", xmlRootName(t, body))
	assert.Contains(t, string(body), "<Errors>")
}

// A global service accepts its own signing region and the gateway's, and
// nothing else; a regional service accepts only the gateway's.
func TestReqctxPipeline_GlobalSigningRegionRejectsOthers(t *testing.T) {
	probe := func(w http.ResponseWriter, _ dispatch.Invocation) error {
		w.WriteHeader(http.StatusOK)
		return nil
	}
	app := reqctxGateway(t, reqctxGatewayEnv{NATS: natsUp, Probe: probe}, statement("Allow", "*", "*")).SetupRoutes()

	body := []byte("Action=ProbeAction")
	for _, tc := range []struct {
		region string
		want   int
	}{
		{"us-east-1", http.StatusOK},
		{reqctxRegion, http.StatusOK},
		{"eu-west-1", http.StatusForbidden},
	} {
		t.Run(tc.region, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
			got := doRequest(app, signReqctx(t, req, body, "route53", tc.region))
			assert.Equal(t, tc.want, got.StatusCode)
		})
	}
}

// The anonymous exchange runs ahead of authentication, so an oversize body is
// handed back unread to the signed chain instead of being dispatched.
func TestAnonymousSTS_OversizeBodyPassesThroughUnread(t *testing.T) {
	gw := &Config{DisableLogging: true, PreAuth: []dispatch.PreAuth{stsawsapi.NewPreAuth(stsawsapi.Deps{})}}
	big := "Action=AssumeRoleWithWebIdentity&WebIdentityToken=" + strings.Repeat("a", int(sigv4.MaxPayloadLen)+1)

	var seen []byte
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen, _ = io.ReadAll(r.Body) })
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(big))
	rec := httptest.NewRecorder()

	gw.preAuthInterceptor(next).ServeHTTP(rec, req)

	assert.Equal(t, big, string(seen), "the whole body must reach the next handler")
}

// With no anonymous capability composed, the action fails closed.
func TestAnonymousSTS_NilCapabilityIsAnInternalError(t *testing.T) {
	gw := &Config{DisableLogging: true, PreAuth: []dispatch.PreAuth{stsawsapi.NewPreAuth(stsawsapi.Deps{})}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=AssumeRoleWithWebIdentity&WebIdentityToken=x"))
	rec := httptest.NewRecorder()

	gw.preAuthInterceptor(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("must not fall through to the signed chain")
	})).ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "<Code>"+awserrors.ErrorInternalError+"</Code>")
}

// A pre-authentication request is served before SigV4 and before the
// throttle, so an exhausted bucket never reaches it.
func TestPreAuth_RunsAheadOfSigV4AndThrottle(t *testing.T) {
	served := 0
	gw := withIAM(withVerifier(gwfix.WithDeps(&Config{
		DisableLogging: true,
		Throttler:      newThrottler(t),
		PreAuth: []dispatch.PreAuth{{
			Service: "svc",
			Serve: func(w http.ResponseWriter, _ *http.Request) (bool, error) {
				served++
				w.WriteHeader(http.StatusOK)
				return true, nil
			},
		}},
	}, gwfix.Deps{IAM: allowAllIAMService()})))
	app := gw.SetupRoutes()

	for range 5 {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=Anything"))
		assert.Equal(t, http.StatusOK, doRequest(app, req).StatusCode)
	}
	assert.Equal(t, 5, served)
}
