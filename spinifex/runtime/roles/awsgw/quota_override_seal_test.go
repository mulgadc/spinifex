package awsgw

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	admissionv1 "github.com/mulgadc/spinifex/contracts/admission/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubChecker answers AccountTerminating from a fixed set, standing in for
// iamdomain.AccountStatusReader so this test does not need a real IAM
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

func newQuotaSealCapabilityConns(t *testing.T) (*nats.Conn, *nats.Conn) {
	t.Helper()
	ns, nc := testutil.StartTestNATS(t)
	caller, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(caller.Close)
	return nc, caller
}

func quotaSealCapabilityRequest(t *testing.T, caller *nats.Conn, subjectNode, bodyNode string) *nats.Msg {
	t.Helper()
	data, err := json.Marshal(admissionv1.AccountQuotaOverrideSealCapabilityRequest{Node: bodyNode, CorrelationRef: "req-1"})
	require.NoError(t, err)
	reply, err := caller.Request(admissionv1.AccountQuotaOverrideSealCapabilitySubject(subjectNode), data, 2*time.Second)
	require.NoError(t, err)
	return reply
}

func TestQuotaOverrideSealCapability_AnswersOwnNode(t *testing.T) {
	nc, caller := newQuotaSealCapabilityConns(t)
	sub, err := registerQuotaOverrideSealCapability(nc, "node-1")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()
	require.NoError(t, nc.Flush())

	raw := quotaSealCapabilityRequest(t, caller, "node-1", "node-1")
	assert.Equal(t, "node-1", raw.Header.Get(natsmsg.NodeIDHeader))

	var reply admissionv1.AccountQuotaOverrideSealCapabilityReply
	require.NoError(t, json.Unmarshal(raw.Data, &reply))
	assert.Equal(t, "node-1", reply.Node)
	assert.Equal(t, admissionv1.AccountQuotaOverrideSealCapabilityVersion, reply.CapabilityVersion)
	assert.Equal(t, "req-1", reply.CorrelationRef)
}

func TestQuotaOverrideSealCapability_RefusesOtherNode(t *testing.T) {
	nc, caller := newQuotaSealCapabilityConns(t)
	sub, err := registerQuotaOverrideSealCapability(nc, "node-1")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()
	require.NoError(t, nc.Flush())

	raw := quotaSealCapabilityRequest(t, caller, "node-1", "node-2")
	var re struct {
		Code string `json:"Code"`
	}
	require.NoError(t, json.Unmarshal(raw.Data, &re))
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, re.Code)
}

// Two gateways configured with one node ID both answer: the subject has no
// queue group, which is how the probe catches a duplicate.
func TestQuotaOverrideSealCapability_HasNoQueueGroup(t *testing.T) {
	nc, caller := newQuotaSealCapabilityConns(t)
	for range 2 {
		sub, err := registerQuotaOverrideSealCapability(nc, "node-1")
		require.NoError(t, err)
		defer func() { _ = sub.Unsubscribe() }()
	}
	require.NoError(t, nc.Flush())

	inbox := caller.NewRespInbox()
	replies, err := caller.SubscribeSync(inbox)
	require.NoError(t, err)
	data, err := json.Marshal(admissionv1.AccountQuotaOverrideSealCapabilityRequest{Node: "node-1"})
	require.NoError(t, err)
	require.NoError(t, caller.PublishRequest(admissionv1.AccountQuotaOverrideSealCapabilitySubject("node-1"), inbox, data))

	for range 2 {
		_, err := replies.NextMsg(2 * time.Second)
		require.NoError(t, err)
	}
}

func TestQuotaOverrideSealCapability_InvalidNodeServesNothing(t *testing.T) {
	nc, caller := newQuotaSealCapabilityConns(t)
	for _, node := range []string{"", "a.b", "node >"} {
		sub, err := registerQuotaOverrideSealCapability(nc, node)
		require.NoError(t, err)
		assert.Nil(t, sub, "%q", node)
	}
	require.NoError(t, nc.Flush())

	_, err := caller.Request(admissionv1.AccountQuotaOverrideSealCapabilitySubject("a"), []byte(`{"node":"a"}`), 200*time.Millisecond)
	assert.ErrorIs(t, err, nats.ErrNoResponders)
}
