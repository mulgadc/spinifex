package gateway_eks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testARN = "arn:aws:iam::111122223333:role/admin"

// validToken builds a k8s-aws-v1 bearer token (the `aws eks get-token` shape):
// the literal prefix + base64 raw-url of the presigned GetCallerIdentity URL.
func validToken(url string) string {
	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(url))
}

func TestWebhookTokenReview_Rejects(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	// nil conn → ServerInternal
	_, err := WebhookTokenReview(context.Background(), nil, "alpha", []byte(`{"accountId":"1","token":"t"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())

	cases := []struct {
		name, cluster, body, want string
	}{
		{"empty cluster", "", `{"accountId":"1","token":"t"}`, awserrors.ErrorInvalidParameterValue},
		{"malformed json", "alpha", `{not-json`, awserrors.ErrorInvalidParameterValue},
		{"empty account", "alpha", `{"accountId":"","token":"t"}`, awserrors.ErrorInvalidParameterValue},
		{"empty token", "alpha", `{"accountId":"1","token":""}`, awserrors.ErrorInvalidParameterValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := WebhookTokenReview(context.Background(), nc, tc.cluster, []byte(tc.body))
			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
		})
	}
}

// A genuine infra fault (account bucket absent) maps to ServerInternal.
func TestWebhookTokenReview_InfraFaultIsServerInternal(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := WebhookTokenReview(context.Background(), nc, "alpha", []byte(`{"accountId":"111122223333","token":"`+validToken("https://sts/?x=1")+`"}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestWebhookTokenReview_ResolvesIdentity(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	kv := testutil.SeedKV(t, js, handlers_eks.AccountBucketName("111122223333"), nil)
	require.NoError(t, handlers_eks.PutAccessEntryRecord(kv, &handlers_eks.AccessEntryRecord{
		ClusterName:        "alpha",
		PrincipalARN:       testARN,
		KubernetesUsername: testARN,
		KubernetesGroups:   []string{"system:masters"},
		Type:               handlers_eks.AccessEntryTypeStandard,
	}))

	sub, err := nc.Subscribe(handlers_eks.TokenVerifySubject, func(m *nats.Msg) {
		resp, _ := json.Marshal(handlers_eks.TokenVerifyResponse{
			AccountID:     "111122223333",
			ARN:           testARN,
			UserID:        "AROAEXAMPLE:session",
			PrincipalType: "AssumedRole",
		})
		_ = m.Respond(resp)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out, err := WebhookTokenReview(context.Background(), nc, "alpha", []byte(`{"accountId":"111122223333","token":"`+validToken("https://sts/?Action=GetCallerIdentity")+`"}`))
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.True(t, out.Authenticated)
	assert.Equal(t, testARN, out.Username)
	assert.Equal(t, []string{"system:masters"}, out.Groups)
}
