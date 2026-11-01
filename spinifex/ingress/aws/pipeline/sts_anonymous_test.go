package pipeline

import (
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAnonymousSTS_SignedRequestFallsThrough confirms a request with an Authorization
// header is not intercepted — it passes through to the signed surface.
func TestAnonymousSTS_SignedRequestFallsThrough(t *testing.T) {
	gw := gwfix.WithDeps(&GatewayConfig{
		DisableLogging: true,
	}, gwfix.Deps{STS: &flexMockSTSService{
		assumeWebIdentityFn: func(*sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
			t.Fatal("signed request must not reach the anonymous STS dispatcher")
			return nil, nil
		},
	}})

	gw.PreAuth = []dispatch.PreAuth{stsawsapi.NewPreAuth(stsawsapi.Deps{STS: gwfix.DepsOf(gw).STS})}
	nextCalled := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true })

	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader("Action=AssumeRoleWithWebIdentity&WebIdentityToken=x"))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=...")

	rec := httptest.NewRecorder()
	gw.preAuthInterceptor(next).ServeHTTP(rec, req)
	assert.True(t, nextCalled, "signed request must pass through to next handler")
}
