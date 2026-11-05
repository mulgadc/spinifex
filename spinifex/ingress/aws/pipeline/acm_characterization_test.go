package pipeline

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	acmdomain "github.com/mulgadc/spinifex/spinifex/domains/acm"
	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestACMActionFromTarget_FormsAndTelemetryAgree pins how the ACM registration
// resolves the X-Amz-Target header for dispatch and telemetry.
func TestACMActionFromTarget_FormsAndTelemetryAgree(t *testing.T) {
	cases := []struct {
		name       string
		target     string
		wantAction string
	}{
		{"service prefix", "CertificateManager.ListCertificates", "ListCertificates"},
		{"unrelated two-segment prefix", "a.b.ListCertificates", "ListCertificates"},
		{"bare action", "ListCertificates", "ListCertificates"},
		{"trailing dot", "CertificateManager.", ""},
	}
	acmEntry, ok := withACM(&Config{}, acmawsapi.Deps{}).registered(acmawsapi.ServiceName)
	require.True(t, ok)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("X-Amz-Target", tc.target)
			assert.Equal(t, tc.wantAction, acmEntry.ResolveAction(req))
		})
	}
}

// TestACMPipeline_TraceActionEnricher_SpanNamedServiceAction pins that the
// server span is renamed to "acm.<Action>" and carries aws.action once a real
// ACM request runs through SigV4AuthMiddleware and traceActionEnricher.
func TestACMPipeline_TraceActionEnricher_SpanNamedServiceAction(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	defer otel.SetTracerProvider(prev)

	gw, _ := acmPipelineApp(t, allowAllACMDocs())
	subscribeACMCounter(t, gwfix.DepsOf(gw).NATS, "acm.ListCertificates", []byte(`{}`))

	r := chi.NewRouter()
	r.Use(otelsetup.HTTPMiddleware("awsgw"))
	r.Use(gw.SigV4AuthMiddleware())
	r.Use(traceActionEnricher)
	r.HandleFunc("/*", gw.Request)

	target := "CertificateManager.ListCertificates"
	doRequest(r, signACMRequest(t, target, []byte("{}")))

	// The HTTP server span is the one traceActionEnricher renames. A second,
	// unrelated client span covers the ListCertificates NATS hop itself.
	spans := sr.Ended()
	idx := slices.IndexFunc(spans, func(s sdktrace.ReadOnlySpan) bool { return s.Name() == "acm.ListCertificates" })
	require.GreaterOrEqual(t, idx, 0, "no span named acm.ListCertificates among %d ended spans", len(spans))
	span := spans[idx]
	got := map[string]string{}
	for _, kv := range span.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "ListCertificates", got["aws.action"])
	assert.Equal(t, "acm", got["aws.service"])
}

// An absent certificate through the signed gateway, against the real ACM service,
// answers 400 with ResourceNotFoundException in the JSON 1.1 envelope.
func TestACMPipeline_DescribeAbsentCertificate_Is400ResourceNotFoundException(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := acmdomain.NewACMServiceImplWithNATS(t.Context(), nil, nc, masterKey)
	require.NoError(t, err)

	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	gw := withVerifier(gwfix.WithDeps(&Config{DisableLogging: true, Region: gwfix.Region}, gwfix.Deps{NATS: nc, IAM: &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID: gwfix.AccessKey, SecretAccessKey: encryptedSecret,
					UserName: "alice", AccountID: authzAccountID, Status: "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return allowAllACMDocs(), nil },
	}}))
	withACM(gw, acmawsapi.Deps{})

	sub, err := nc.Subscribe("acm.DescribeCertificate", func(msg *nats.Msg) {
		var in acm.DescribeCertificateInput
		if err := json.Unmarshal(msg.Data, &in); err != nil {
			_ = msg.Respond(awserrors.GenerateErrorPayload(awserrors.ErrorInvalidParameterValue))
			return
		}
		out, err := svc.DescribeCertificate(t.Context(), &in, natsmsg.AccountIDFromMsg(msg))
		if err != nil {
			_ = msg.Respond(awserrors.GenerateErrorPayload(awserrors.ValidErrorCodeFromError(err)))
			return
		}
		b, _ := json.Marshal(out)
		_ = msg.Respond(b)
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)

	arn := "arn:aws:acm:" + gwfix.Region + ":" + authzAccountID + ":certificate/12345678-1234-1234-1234-123456789012"
	resp := doRequest(r, signACMRequest(t, "CertificateManager.DescribeCertificate", []byte(`{"CertificateArn":"`+arn+`"}`)))

	assert.Equal(t, 400, resp.StatusCode)
	var env struct {
		Type string `json:"__type"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &env), string(body))
	assert.Equal(t, "ResourceNotFoundException", env.Type)
}
