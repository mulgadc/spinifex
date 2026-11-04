//go:build !lambdatestlink

package awsgw

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	lambdaawsapi "github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/stretchr/testify/require"
)

// spyFunctions counts every lookup and serves one Active function with stored code.
type spyFunctions struct{ gets atomic.Int32 }

var _ function.Client = (*spyFunctions)(nil)

func (s *spyFunctions) Get(_ context.Context, account, region, name string) (function.Record, uint64, error) {
	s.gets.Add(1)
	return function.Record{
		FunctionID: "f1", AccountID: account, Region: region, FunctionName: name, Runtime: "provided.al2023",
		Timeout: 3, MemorySize: 128, State: function.StateActive,
		Code: function.Code{RevisionID: "r1", State: function.CodeStored},
	}, 1, nil
}

func (*spyFunctions) Create(context.Context, string, string, function.Record) (uint64, error) {
	return 0, errors.New("unused")
}

func (*spyFunctions) CodeStored(context.Context, string, string, string, uint64, string, int64) (function.Record, uint64, error) {
	return function.Record{}, 0, errors.New("unused")
}

func (*spyFunctions) RollbackCreate(context.Context, string, string, string, uint64) error {
	return errors.New("unused")
}

// spyExec counts invocations that would start an attempt.
type spyExec struct{ calls atomic.Int32 }

func (e *spyExec) InvokeSync(context.Context, lambdaawsapi.SyncInvocation) (lambdaawsapi.SyncResult, error) {
	e.calls.Add(1)
	return lambdaawsapi.SyncResult{Payload: []byte(`{}`), ExecutedVersion: "$LATEST"}, nil
}

// defaultGateway composes the gateway as launchService does, with signing and an
// allow-all policy, and the given Lambda registrations appended to it.
func defaultGateway(t *testing.T, regs ...dispatch.Registration) http.Handler {
	t.Helper()
	secret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)
	iam := &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{gwfix.AccessKey: {
				AccessKeyID: gwfix.AccessKey, SecretAccessKey: secret, UserName: "alice",
				AccountID: "123456789012", Status: "Active",
			}},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) {
			return []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: []iamdomain.Statement{
				{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}},
			}}}, nil
		},
	}
	gw, err := composeGateway(gatewayInputs{
		Region:       gwfix.Region,
		NATS:         nc,
		IAM:          iam,
		Credentials:  credverify.New(iam, nil),
		Admission:    accountadmission.New(iam),
		PublicRoutes: PublicRoutes(nil, nil),
		PreAuth:      []dispatch.PreAuth{stsawsapi.NewPreAuth(stsawsapi.Deps{})},
	}, regs...)
	require.NoError(t, err)
	gw.DisableLogging = true
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return r
}

func signedLambdaInvoke(t *testing.T, app http.Handler) *http.Response {
	t.Helper()
	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/f/invocations", bytes.NewReader(body))
	req.Host = "localhost:9999"
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, lambdaawsapi.ServiceName, time.Now().UTC())
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)
	return w.Result()
}

// A default build composes the gateway with Lambda unrouted and no executor: a
// signed Invoke is refused before any Lambda handler runs and starts no attempt.
func TestDefaultBuild_InvokeNeverReachesLambda(t *testing.T) {
	require.Nil(t, lambdaTestExecutor(nil, "node1", nil), "a default build has no Lambda executor")

	meta, exec := &spyFunctions{}, &spyExec{}
	lambda := lambdaawsapi.NewRegistration(lambdaawsapi.Deps{Meta: meta, Exec: exec})

	resp := signedLambdaInvoke(t, defaultGateway(t, lambdaTestRoutes(lambda)...))
	require.NotEqual(t, http.StatusOK, resp.StatusCode, "a default build answered Invoke")
	require.Zero(t, meta.gets.Load(), "a default build ran a Lambda handler")
	require.Zero(t, exec.calls.Load(), "a default build started an attempt")

	// The same signed request reaches both once Lambda is routed, so the
	// refusal above comes from the missing route.
	resp = signedLambdaInvoke(t, defaultGateway(t, lambda))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int32(1), meta.gets.Load())
	require.Equal(t, int32(1), exec.calls.Load())
}
