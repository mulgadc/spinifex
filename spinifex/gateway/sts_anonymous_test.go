package gateway

import (
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAnonymousSTSArgs_Classification covers the body-peek helper for anonymous action detection.
func TestAnonymousSTSArgs_Classification(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true}

	t.Run("anonymous action recognised, body restored", func(t *testing.T) {
		const body = "Action=AssumeRoleWithWebIdentity&WebIdentityToken=tok"
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		args, ok := gw.anonymousSTSArgs(req)
		require.True(t, ok)
		assert.Equal(t, "AssumeRoleWithWebIdentity", args["Action"])

		restored, _ := io.ReadAll(req.Body)
		assert.Equal(t, body, string(restored), "body must be restored for downstream read")
	})

	t.Run("non-anonymous action rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=GetCallerIdentity"))
		_, ok := gw.anonymousSTSArgs(req)
		assert.False(t, ok)
	})
}

// TestAnonymousSTS_SignedRequestFallsThrough confirms a request with an Authorization
// header is not intercepted — it passes through to the signed surface.
func TestAnonymousSTS_SignedRequestFallsThrough(t *testing.T) {
	gw := &GatewayConfig{
		DisableLogging: true,
		STSService: &flexMockSTSService{
			assumeWebIdentityFn: func(*sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
				t.Fatal("signed request must not reach the anonymous STS dispatcher")
				return nil, nil
			},
		},
	}

	nextCalled := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true })

	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader("Action=AssumeRoleWithWebIdentity&WebIdentityToken=x"))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=...")

	rec := httptest.NewRecorder()
	gw.anonymousSTSInterceptor(next).ServeHTTP(rec, req)
	assert.True(t, nextCalled, "signed request must pass through to next handler")
}
