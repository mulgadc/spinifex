package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAnonymousSTS_WebIdentityNoAuthReachesHandler proves the IRSA bootstrap
// call — AssumeRoleWithWebIdentity carrying no Authorization header — is routed
// to the STS dispatcher ahead of the SigV4 middleware, rather than rejected as
// unauthenticated. The mock asserts the parsed token reached the handler.
func TestAnonymousSTS_WebIdentityNoAuthReachesHandler(t *testing.T) {
	var seenToken string
	gw := &GatewayConfig{
		DisableLogging: true,
		IAMService:     &flexMockIAMService{},
		STSService: &flexMockSTSService{
			assumeWebIdentityFn: func(in *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
				seenToken = aws.StringValue(in.WebIdentityToken)
				return &sts.AssumeRoleWithWebIdentityOutput{
					Credentials: &sts.Credentials{
						AccessKeyId:     aws.String("ASIAIRSA"),
						SecretAccessKey: aws.String("secret"),
						SessionToken:    aws.String("token"),
					},
				}, nil
			},
		},
	}
	handler := gw.SetupRoutes()

	body := "Action=AssumeRoleWithWebIdentity&Version=2011-06-15" +
		"&RoleArn=arn:aws:iam::000000000001:role/demo&RoleSessionName=s1" +
		"&WebIdentityToken=header.payload.sig"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// No Authorization header — this is the unsigned bootstrap path.

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	b, _ := io.ReadAll(rec.Body)
	assert.Contains(t, string(b), "AssumeRoleWithWebIdentityResult")
	assert.Contains(t, string(b), "ASIAIRSA")
	assert.Equal(t, "header.payload.sig", seenToken)
}

// TestAnonymousSTS_SignedRequestFallsThrough confirms a request bearing an
// Authorization header is NOT intercepted — it flows to the signed surface, so
// the anonymous dispatcher never runs and the body is left for the SigV4 path.
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

// TestAnonymousSTSArgs_Classification covers the body-peek helper: an anonymous
// action is recognised and the body is restored for the downstream handler; a
// non-anonymous action is rejected and the body left intact for SigV4.
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
