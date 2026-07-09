package awsgw

import (
	"context"
	"errors"
	"testing"
	"time"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeVerifier struct {
	ident   *handlers_sts.PresignedCallerIdentity
	err     error
	gotURL  string
	gotName string
}

func (f *fakeVerifier) VerifyPresignedGetCallerIdentity(presignedURL, expectedClusterName string) (*handlers_sts.PresignedCallerIdentity, error) {
	f.gotURL = presignedURL
	f.gotName = expectedClusterName
	return f.ident, f.err
}

func TestEKSTokenVerify_ResolvesPrincipal(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	fake := &fakeVerifier{ident: &handlers_sts.PresignedCallerIdentity{
		AccountID:     "111122223333",
		ARN:           "arn:aws:iam::111122223333:role/admin",
		UserID:        "AROAEXAMPLE:sess",
		PrincipalType: "AssumedRole",
	}}
	sub, err := registerEKSTokenVerify(nc, fake)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	req := handlers_eks.TokenVerifyRequest{PresignedURL: "https://sts/?x=1", ClusterName: "alpha"}
	resp, err := utils.NatsRequest[handlers_eks.TokenVerifyResponse](context.Background(),
		nc, handlers_eks.TokenVerifySubject, req, 2*time.Second, "")
	require.NoError(t, err)

	assert.Equal(t, "arn:aws:iam::111122223333:role/admin", resp.ARN)
	assert.Equal(t, "AROAEXAMPLE:sess", resp.UserID)
	assert.Equal(t, "AssumedRole", resp.PrincipalType)
	// Cluster name is forwarded for the STS anti-replay pin.
	assert.Equal(t, "alpha", fake.gotName)
}

func TestEKSTokenVerify_RejectedTokenIsError(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	fake := &fakeVerifier{err: errors.New("signature mismatch (cluster name replay)")}
	sub, err := registerEKSTokenVerify(nc, fake)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	req := handlers_eks.TokenVerifyRequest{PresignedURL: "https://sts/?x=1", ClusterName: "alpha"}
	_, err = utils.NatsRequest[handlers_eks.TokenVerifyResponse](context.Background(),
		nc, handlers_eks.TokenVerifySubject, req, 2*time.Second, "")
	require.Error(t, err, "a verify failure must surface as a NATS error payload")
}

func TestEKSTokenVerify_RejectsEmptyFields(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	fake := &fakeVerifier{ident: &handlers_sts.PresignedCallerIdentity{ARN: "arn:..."}}
	sub, err := registerEKSTokenVerify(nc, fake)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	// Missing ClusterName — responder rejects before calling the verifier.
	req := handlers_eks.TokenVerifyRequest{PresignedURL: "https://sts/?x=1"}
	_, err = utils.NatsRequest[handlers_eks.TokenVerifyResponse](context.Background(),
		nc, handlers_eks.TokenVerifySubject, req, 2*time.Second, "")
	require.Error(t, err)
	assert.Empty(t, fake.gotURL, "verifier must not run on an invalid request")
}
