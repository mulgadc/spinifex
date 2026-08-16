package accountteardown

//test:in-package — the reaper is unexported, and ACM has no service adapter to
// fake, so the subjects it speaks are what has to be asserted.

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replyWith answers subject with payload once subscribed, recording the
// account header every request carried.
func replyWith(t *testing.T, nc *nats.Conn, subject string, payload []byte, accounts *[]string) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		*accounts = append(*accounts, msg.Header.Get(utils.AccountIDHeader))
		require.NoError(t, msg.Respond(payload))
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func TestCertificateReaperListsAndDeletesByARN(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	listing, err := json.Marshal(acm.ListCertificatesOutput{CertificateSummaryList: []*acm.CertificateSummary{
		{CertificateArn: aws.String("arn:acm/one"), DomainName: aws.String("api.example.com")},
		{DomainName: aws.String("no-arn.example.com")},
	}})
	require.NoError(t, err)

	var accounts []string
	replyWith(t, nc, "acm.ListCertificates", listing, &accounts)
	replyWith(t, nc, "acm.DeleteCertificate", []byte(`{}`), &accounts)

	reaper := &certificateReaper{nc: nc}
	assert.Equal(t, StagePlatform, reaper.Stage())

	found, err := reaper.List(t.Context(), "000000000002")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "arn:acm/one", found[0].ID)
	assert.Equal(t, "api.example.com", found[0].Detail)

	require.NoError(t, reaper.Delete(t.Context(), "000000000002", found[0], false))

	// Both calls have to carry the tenant, or they read another account's
	// certificates and delete from it.
	assert.Equal(t, []string{"000000000002", "000000000002"}, accounts)
}

func TestCertificateReaperTreatsAMissingCertificateAsDeleted(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	var accounts []string
	replyWith(t, nc, "acm.DeleteCertificate", utils.GenerateErrorPayload("ResourceNotFound"), &accounts)

	reaper := &certificateReaper{nc: nc}
	assert.NoError(t, reaper.Delete(t.Context(), "000000000002", Resource{ID: "arn:acm/gone"}, false))
}
