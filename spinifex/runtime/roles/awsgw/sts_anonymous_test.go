package awsgw_test

import (
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
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

// TestAnonymousSTS_WebIdentityNoAuthReachesHandler proves an unsigned
// AssumeRoleWithWebIdentity is routed to the STS dispatcher before SigV4, not rejected.
func TestAnonymousSTS_WebIdentityNoAuthReachesHandler(t *testing.T) {
	var seenToken string
	gw := withSTS(withVerifier(gwfix.WithDeps(&pipeline.GatewayConfig{
		DisableLogging: true,
	}, gwfix.Deps{IAM: &flexMockIAMService{}, STS: &flexMockSTSService{
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
	}})))
	handler := gw.SetupRoutes()

	body := "Action=AssumeRoleWithWebIdentity&Version=2011-06-15" +
		"&RoleArn=arn:aws:iam::000000000001:role/demo&RoleSessionName=s1" +
		"&WebIdentityToken=header.payload.sig"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// No Authorization header — unsigned bootstrap path.

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	b, _ := io.ReadAll(rec.Body)
	assert.Contains(t, string(b), "AssumeRoleWithWebIdentityResult")
	assert.Contains(t, string(b), "ASIAIRSA")
	assert.Equal(t, "header.payload.sig", seenToken)
}
