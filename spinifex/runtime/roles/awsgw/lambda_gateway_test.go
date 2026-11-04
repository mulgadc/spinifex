package awsgw

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	lambdaawsapi "github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/stretchr/testify/require"
)

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
	return signedLambdaInvokeAt(t, app, "/2015-03-31/functions/f/invocations")
}

// signedLambdaInvokeAt sends a signed Invoke to target, a path with any query.
func signedLambdaInvokeAt(t *testing.T, app http.Handler, target string) *http.Response {
	t.Helper()
	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Host = "localhost:9999"
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, lambdaawsapi.ServiceName, time.Now().UTC())
	w := httptest.NewRecorder()
	app.ServeHTTP(w, req)
	return w.Result()
}
