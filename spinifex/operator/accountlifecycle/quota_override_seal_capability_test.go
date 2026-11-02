package accountlifecycle

//test:in-package — the probe client and its reasons are unexported; this pins that the
// quota probe addresses the admission subjects and reuses the shared classification.

import (
	"encoding/json"
	"testing"
	"time"

	admissionv1 "github.com/mulgadc/spinifex/contracts/admission/v1"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testQuotaCapabilityClient(nc *nats.Conn, nodeIDs []string) *sealCapabilityClient {
	c := newQuotaSealCapabilityClient(nc, nodeIDs)
	c.wait = 200 * time.Millisecond
	c.grace = 30 * time.Millisecond
	return c
}

func quotaCapabilityReply(replyNode, headerNode string, version int) nats.MsgHandler {
	return func(msg *nats.Msg) {
		data, _ := json.Marshal(admissionv1.AccountQuotaOverrideSealCapabilityReply{Node: replyNode, CapabilityVersion: version})
		reply := nats.NewMsg(msg.Reply)
		reply.Data = data
		if headerNode != "" {
			reply.Header.Set(natsmsg.NodeIDHeader, headerNode)
		}
		_ = msg.RespondMsg(reply)
	}
}

func TestQuotaSealCapabilityComplete(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	for _, node := range []string{"gw-1", "gw-2"} {
		serveCapability(t, nc, admissionv1.AccountQuotaOverrideSealCapabilitySubject(node), quotaCapabilityReply(node, node, 1))
	}
	report := testQuotaCapabilityClient(nc, []string{"gw-2", "gw-1"}).CheckSealCapability(testCtx(t), "req")
	assert.True(t, report.Complete)
	assert.Equal(t, 1, nodeResult(t, report, "gw-1").Version)
	assert.Equal(t, 1, nodeResult(t, report, "gw-2").Version)
}

func TestQuotaSealCapabilityIncomplete(t *testing.T) {
	cases := []struct {
		name   string
		serve  func(t *testing.T, nc *nats.Conn)
		nodes  []string
		reason string
	}{
		{"missing", func(*testing.T, *nats.Conn) {}, []string{"gw-1"}, sealCapabilityReasonNoReply},
		{"duplicate", func(t *testing.T, nc *nats.Conn) {
			subject := admissionv1.AccountQuotaOverrideSealCapabilitySubject("gw-1")
			serveCapability(t, nc, subject, quotaCapabilityReply("gw-1", "gw-1", 1))
			serveCapability(t, nc, subject, quotaCapabilityReply("gw-1", "gw-1", 1))
		}, []string{"gw-1"}, sealCapabilityReasonDuplicate},
		{"no node header", func(t *testing.T, nc *nats.Conn) {
			serveCapability(t, nc, admissionv1.AccountQuotaOverrideSealCapabilitySubject("gw-1"), quotaCapabilityReply("gw-1", "", 1))
		}, []string{"gw-1"}, sealCapabilityReasonWrongIdentity},
		{"body names another node", func(t *testing.T, nc *nats.Conn) {
			serveCapability(t, nc, admissionv1.AccountQuotaOverrideSealCapabilitySubject("gw-1"), quotaCapabilityReply("gw-2", "gw-1", 1))
		}, []string{"gw-1"}, sealCapabilityReasonWrongIdentity},
		{"version zero", func(t *testing.T, nc *nats.Conn) {
			serveCapability(t, nc, admissionv1.AccountQuotaOverrideSealCapabilitySubject("gw-1"), quotaCapabilityReply("gw-1", "gw-1", 0))
		}, []string{"gw-1"}, sealCapabilityReasonOldVersion},
		{"invalid node id", func(*testing.T, *nats.Conn) {}, []string{"gw.1"}, sealCapabilityReasonInvalidNodeID},
		// The EC2 responder on a daemon is not the gateway's quota capability.
		{"only the ec2 subject answers", func(t *testing.T, nc *nats.Conn) {
			serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("gw-1"), capabilityReply("gw-1", "gw-1", 1))
		}, []string{"gw-1"}, sealCapabilityReasonNoReply},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, nc, _ := testutil.StartTestJetStream(t)
			tc.serve(t, nc)
			report := testQuotaCapabilityClient(nc, tc.nodes).CheckSealCapability(testCtx(t), "req")
			assert.False(t, report.Complete)
			assert.Equal(t, tc.reason, nodeResult(t, report, tc.nodes[0]).Reason)
		})
	}
}

func TestQuotaSealCapabilityEmptySetIsIncomplete(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	report := testQuotaCapabilityClient(nc, nil).CheckSealCapability(testCtx(t), "req")
	assert.False(t, report.Complete)
	assert.Contains(t, errQuotaSealCapabilityIncomplete(report).Error(), "quota override seal capability incomplete: "+sealCapabilityReasonSetUnknown)
}

func TestQuotaSealCapabilityIncompleteErrorNamesNodeAndReason(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	report := testQuotaCapabilityClient(nc, []string{"gw-1"}).CheckSealCapability(testCtx(t), "req")
	err := errQuotaSealCapabilityIncomplete(report)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quota override seal capability incomplete: gw-1="+sealCapabilityReasonNoReply)
}
