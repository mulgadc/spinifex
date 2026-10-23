package awsgw

import (
	"context"
	"testing"
	"time"

	admissionv1 "github.com/mulgadc/spinifex/contracts/admission/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubChecker answers AccountTerminating from a fixed set, standing in for
// handlers_iam.AccountStatusReader so this test does not need a real IAM
// bucket.
type stubChecker struct {
	terminating map[string]bool
}

func (c *stubChecker) AccountTerminating(_ context.Context, accountID string) (bool, error) {
	return c.terminating[accountID], nil
}

func newQuotaSealService(t *testing.T) (*admissionquota.Service, jetstream.JetStream) {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	quota := admissionquota.New(admissionquota.Limits{}, nil)
	quota.SetJetStream(js)
	return quota, js
}

func TestQuotaOverrideSeal_SealsTerminatingAccount(t *testing.T) {
	quota, js := newQuotaSealService(t)
	_, nc := testutil.StartTestNATS(t)
	quota.SetAccountTerminationChecker(&stubChecker{terminating: map[string]bool{"111122223333": true}})

	sub, err := registerQuotaOverrideSeal(nc, quota)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	req := admissionv1.SealAccountQuotaOverrideRequest{AccountID: "111122223333", CorrelationRef: "req-1"}
	resp, err := natsmsg.NATSRequest[admissionv1.SealAccountQuotaOverrideReply](context.Background(),
		nc, admissionv1.SealAccountQuotaOverrideSubject, req, 2*time.Second, "")
	require.NoError(t, err)

	assert.Equal(t, "111122223333", resp.AccountID)
	assert.Equal(t, "req-1", resp.CorrelationRef)
	assert.Equal(t, admissionv1.AccountQuotaOverrideDispositionSealed, resp.Disposition)
	assert.NotZero(t, resp.Revision)

	kv, err := js.KeyValue(context.Background(), admissionquota.KVBucketAccountQuota)
	require.NoError(t, err)
	entry, err := kv.Get(context.Background(), "111122223333")
	require.NoError(t, err)
	assert.Equal(t, resp.Revision, entry.Revision())
}

func TestQuotaOverrideSeal_RefusesActiveAccount(t *testing.T) {
	quota, _ := newQuotaSealService(t)
	_, nc := testutil.StartTestNATS(t)
	quota.SetAccountTerminationChecker(&stubChecker{terminating: map[string]bool{}})

	sub, err := registerQuotaOverrideSeal(nc, quota)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	req := admissionv1.SealAccountQuotaOverrideRequest{AccountID: "111122223333"}
	_, err = natsmsg.NATSRequest[admissionv1.SealAccountQuotaOverrideReply](context.Background(),
		nc, admissionv1.SealAccountQuotaOverrideSubject, req, 2*time.Second, "")
	require.Error(t, err, "a non-TERMINATING account must be refused")
}

func TestQuotaOverrideSeal_NoResponderWithoutGateway(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	req := admissionv1.SealAccountQuotaOverrideRequest{AccountID: "111122223333"}
	_, err := natsmsg.NATSRequest[admissionv1.SealAccountQuotaOverrideReply](context.Background(),
		nc, admissionv1.SealAccountQuotaOverrideSubject, req, 500*time.Millisecond, "")
	require.Error(t, err, "no subscriber must surface as a no-responders error")
}
