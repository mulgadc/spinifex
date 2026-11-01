package pipeline

import (
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestECRActionFromTarget_FormsAndTelemetryAgree pins how the ECR
// registration's resolver (dispatch and telemetry/throttle) and the legacy
// resolveNonQueryAction each resolve the X-Amz-Target header today. A refactor
// that lets them diverge breaks this.
func TestECRActionFromTarget_FormsAndTelemetryAgree(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantAction string
	}{
		{"service prefix", awsapi.TargetPrefix + ".ListRepositories", "ListRepositories"},
		{"unrelated two-segment prefix", "a.b.ListRepositories", "ListRepositories"},
		{"bare action", "ListRepositories", "ListRepositories"},
		{"trailing dot", awsapi.TargetPrefix + ".", ""},
	}
	ecrEntry, ok := withECR(&GatewayConfig{}, awsapi.Deps{}).registered(awsapi.ServiceName)
	require.True(t, ok)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Amz-Target", tc.target)
			assert.Equal(t, tc.wantAction, ecrEntry.ResolveAction(req))

			assert.Equal(t, tc.wantAction, resolveNonQueryAction(req),
				"dispatch and telemetry parsers must agree on %q", tc.target)
		})
	}
}

// TestECRInventory_ReachableStubsAnswer501 pins the subset of the raw stub set
// that nothing intercepts ahead of the handler table, i.e. what the HTTP
// pipeline actually answers 501 for today.
func TestECRInventory_ReachableStubsAnswer501(t *testing.T) {
	reachableStubs := dispatch.AWSOperationInventory(ecrRegistrationInventory())["ecr"].Stubbed
	want := []string{
		"ListRepositories", "BatchCheckLayerAvailability", "GetDownloadUrlForLayer",
		"InitiateLayerUpload", "UploadLayerPart", "CompleteLayerUpload", "GetRegistryPolicy",
		"PutRegistryPolicy", "PutReplicationConfiguration", "ReplicateImage",
	}
	assert.ElementsMatch(t, want, reachableStubs)

	gw := withVerifier(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()}))
	for _, action := range reachableStubs {
		t.Run(action, func(t *testing.T) {
			err := gw.serveECR(httptest.NewRecorder(),
				setupECRRequest(awsapi.TargetPrefix+"."+action, "{}"))
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
		})
	}
}

// TestECRInventory_ScanningActionsReturnOperationNotSupported pins that every
// unsupported scanning action answers OperationNotSupportedException (400),
// not NotImplemented (501).
func TestECRInventory_ScanningActionsReturnOperationNotSupported(t *testing.T) {
	gw := withVerifier(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()}))
	for _, action := range awsapi.UnsupportedActionNames() {
		t.Run(action, func(t *testing.T) {
			err := gw.serveECR(httptest.NewRecorder(),
				setupECRRequest(awsapi.TargetPrefix+"."+action, "{}"))
			require.Error(t, err)
			code, _, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok)
			assert.Equal(t, awserrors.ErrorOperationNotSupported, code)
		})
	}
}

// TestECRPipeline_TraceActionEnricher_SpanNamedServiceAction pins that the
// server span is renamed to "ecr.<Action>" and carries aws.action once a real
// ECR request runs through SigV4AuthMiddleware and traceActionEnricher.
func TestECRPipeline_TraceActionEnricher_SpanNamedServiceAction(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	gw, _ := ecrPipelineApp(t, allowAllECRDocs())
	r := chi.NewRouter()
	r.Use(otelsetup.HTTPMiddleware("awsgw"))
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(traceActionEnricher)
	r.HandleFunc("/*", gw.Request)

	target := awsapi.TargetPrefix + ".ListRepositories"
	doRequest(r, signECRRequest(t, target, []byte("{}")))

	spans := sr.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "ecr.ListRepositories", span.Name())
	got := map[string]string{}
	for _, kv := range span.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "ListRepositories", got["aws.action"])
	assert.Equal(t, "ecr", got["aws.service"])
}

// TestECRRequest_ECSPrefixedNonECRActionIsInvalidAction pins that a target
// carrying another service's prefix but naming an action outside awsapi.Actions
// fails as an unrecognised ECR action, not as a cross-service error.
func TestECRRequest_ECSPrefixedNonECRActionIsInvalidAction(t *testing.T) {
	gw := withVerifier(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()}))
	err := gw.serveECR(httptest.NewRecorder(),
		setupECRRequest("AmazonEC2ContainerServiceV20141113.ListClusters", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// TestECRRequest_TrailingDotIsMissingAction pins that a target ending in "."
// resolves to an empty action, the same as an absent target.
func TestECRRequest_TrailingDotIsMissingAction(t *testing.T) {
	gw := withVerifier(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()}))
	err := gw.serveECR(httptest.NewRecorder(),
		setupECRRequest(awsapi.TargetPrefix+".", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingAction, err.Error())
}

// TestECRRequest_UnrelatedPrefixDispatchesByBareAction pins that a target with
// any "<prefix>.<Action>" shape (not just the ECR prefix) dispatches on the
// bare action name today.
func TestECRRequest_UnrelatedPrefixDispatchesByBareAction(t *testing.T) {
	gw := withVerifier(gwfix.WithDeps(&GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()}))
	err := gw.serveECR(httptest.NewRecorder(), setupECRRequest("a.b.ListRepositories", "{}"))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

// A registryIds field naming a foreign account must not reach the token: its
// account and subject come from the verified caller, never the body.
func TestGetAuthorizationToken_IgnoresRequestSuppliedIdentity(t *testing.T) {
	iss, verify := newECRAuth(t)
	endpoint := awsapi.RepositoryEndpoint{Region: ecrTestRegion, ServicesDomain: ecrTestSuffix}
	gw := withECR(withVerifier(gwfix.WithDeps(&GatewayConfig{
		Region:         ecrTestRegion,
		DisableLogging: true,
	}, gwfix.Deps{IAM: allowAllIAMService()})), awsapi.Deps{AuthorizationToken: awsapi.NewAuthorizationTokenActionService(iss, endpoint)})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"registryIds":["999999999999"]}`))
	ctx := withPrincipalField(req.Context(), fieldAccountID, ecrTestAccount)
	ctx = withPrincipalField(ctx, fieldPrincipalType, gwfix.PrincipalTypeUser)
	ctx = withPrincipalField(ctx, fieldIdentity, "dev")
	ctx = withPrincipalField(ctx, fieldAccessKey, "AKIAGETAUTHTOKENTEST1")
	req.Header.Set("X-Amz-Target", awsapi.TargetPrefix+".GetAuthorizationToken")
	w := httptest.NewRecorder()
	require.NoError(t, gw.serveECR(w, req.WithContext(ctx)))
	require.Equal(t, http.StatusOK, w.Code)

	var out struct {
		AuthorizationData []struct {
			AuthorizationToken string `json:"authorizationToken"`
		} `json:"authorizationData"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.AuthorizationData, 1)

	decoded, err := decodeAuthToken(out.AuthorizationData[0].AuthorizationToken)
	require.NoError(t, err)
	claims, err := verify.Verify(decoded)
	require.NoError(t, err)
	assert.Equal(t, ecrTestAccount, claims.AccountID)
	assert.Equal(t, "arn:aws:iam::"+ecrTestAccount+":user/dev", claims.Subject)
}
