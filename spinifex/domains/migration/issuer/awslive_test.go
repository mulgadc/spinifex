//go:build awslive

package issuer

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	liveClusterID = "awslive"
	liveAccountID = "744090693897"
)

func liveEnv(t *testing.T) (certPath, keyPath string, req SessionRequest) {
	t.Helper()
	certPath = os.Getenv("SPX_AWSLIVE_CA_CERT")
	keyPath = os.Getenv("SPX_AWSLIVE_CA_KEY")
	req = SessionRequest{
		TrustAnchorARN: os.Getenv("SPX_AWSLIVE_TRUST_ANCHOR_ARN"),
		ProfileARN:     os.Getenv("SPX_AWSLIVE_PROFILE_ARN"),
		RoleARN:        os.Getenv("SPX_AWSLIVE_ROLE_ARN"),
	}
	if certPath == "" || keyPath == "" || req.TrustAnchorARN == "" || req.ProfileARN == "" || req.RoleARN == "" {
		t.Skip("SPX_AWSLIVE_* environment not set")
	}
	return certPath, keyPath, req
}

func TestLiveCreateSession(t *testing.T) {
	certPath, keyPath, req := liveEnv(t)
	iss, err := Load(certPath, keyPath, liveClusterID, nil)
	require.NoError(t, err)
	client := &Client{}
	ctx := context.Background()

	t.Run("accepted", func(t *testing.T) {
		cert, err := iss.Issue(liveAccountID, "conn-live1")
		require.NoError(t, err)
		sess, err := client.CreateSession(ctx, cert, req)
		require.NoError(t, err)

		cfg := aws.Config{
			Region:      "ap-southeast-2",
			Credentials: credentials.NewStaticCredentialsProvider(sess.AccessKeyID, sess.SecretAccessKey, sess.SessionToken),
		}
		out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		require.NoError(t, err)
		arn := aws.ToString(out.Arn)
		roleName := req.RoleARN[strings.LastIndex(req.RoleARN, "/")+1:]
		assert.Contains(t, arn, ":assumed-role/"+roleName+"/")
		t.Logf("assumed identity: %s", arn)
	})

	t.Run("different connection refused", func(t *testing.T) {
		cert, err := iss.Issue(liveAccountID, "conn-other")
		require.NoError(t, err)
		_, err = client.CreateSession(ctx, cert, req)
		require.Error(t, err)
		var apiErr *APIError
		require.True(t, errors.As(err, &apiErr), "got %v", err)
		assert.True(t, apiErr.Refused())
		t.Logf("refusal: %v", apiErr)
	})
}
