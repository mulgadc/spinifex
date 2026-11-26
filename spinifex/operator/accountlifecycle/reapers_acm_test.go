package accountlifecycle

//test:in-package — the reaper is unexported, and ACM has no service adapter to
// fake, so the subjects it speaks are what has to be asserted.

import (
	"encoding/json"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replyWith answers subject with payload once subscribed, recording the
// account header every request carried.
func replyWith(t *testing.T, nc *nats.Conn, subject string, payload []byte, accounts *[]string) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		*accounts = append(*accounts, msg.Header.Get(natsmsg.AccountIDHeader))
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
	replyWith(t, nc, "acm.DeleteCertificate", awserrors.GenerateErrorPayload("ResourceNotFound"), &accounts)

	reaper := &certificateReaper{nc: nc}
	assert.NoError(t, reaper.Delete(t.Context(), "000000000002", Resource{ID: "arn:acm/gone"}, false))
}

// ListCertificates lists only RSA_1024 and RSA_2048 by default, so the reaper
// must name every key type or it leaves EC and larger RSA certificates behind.
func TestCertificateReaperListsEveryKeyType(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	var input acm.ListCertificatesInput
	sub, err := nc.Subscribe("acm.ListCertificates", func(msg *nats.Msg) {
		assert.NoError(t, json.Unmarshal(msg.Data, &input))
		assert.NoError(t, msg.Respond([]byte(`{"CertificateSummaryList":[]}`)))
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	_, err = (&certificateReaper{nc: nc}).List(t.Context(), "000000000002")
	require.NoError(t, err)
	require.NotNil(t, input.Includes)
	assert.ElementsMatch(t, acm.KeyAlgorithm_Values(), aws.StringValueSlice(input.Includes.KeyTypes))
	assert.Nil(t, input.MaxItems, "an unpaged request returns every certificate in one answer")
}
