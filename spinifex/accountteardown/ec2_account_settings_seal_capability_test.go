package accountteardown

//test:in-package — the probe client, its report types and reason constants
// are unexported, and what they guarantee (incomplete never seals, complete
// does) is only observable from inside the package.

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCapabilityClient builds a probe client with test-sized timeouts so the
// incomplete cases below do not each pay the production 3 s wait.
func testCapabilityClient(nc *nats.Conn, nodeIDs []string) *sealCapabilityClient {
	c := newSealCapabilityClient(nc, nodeIDs)
	c.wait = 200 * time.Millisecond
	c.grace = 30 * time.Millisecond
	return c
}

// serveCapability subscribes reply on subject with no queue group, which is
// how the real responder is addressed: per node, so two subscribers on the
// same node's subject both answer.
func serveCapability(t *testing.T, nc *nats.Conn, subject string, reply nats.MsgHandler) *nats.Subscription {
	t.Helper()
	sub, err := nc.Subscribe(subject, reply)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())
	return sub
}

// capabilityReply answers with a well-formed reply naming replyNode (which
// may deliberately differ from the subject's node) and headerNode as the
// X-Node-ID ("" omits the header).
func capabilityReply(replyNode, headerNode string, version int) nats.MsgHandler {
	return func(msg *nats.Msg) {
		data, _ := json.Marshal(ec2v1.AccountSettingsSealCapabilityReply{Node: replyNode, CapabilityVersion: version})
		reply := nats.NewMsg(msg.Reply)
		reply.Data = data
		if headerNode != "" {
			reply.Header.Set(natsmsg.NodeIDHeader, headerNode)
		}
		_ = msg.RespondMsg(reply)
	}
}

func nodeResult(t *testing.T, report sealCapabilityReport, node string) nodeSealCapability {
	t.Helper()
	for _, n := range report.Nodes {
		if n.Node == node {
			return n
		}
	}
	t.Fatalf("no result for node %q in %+v", node, report)
	return nodeSealCapability{}
}

// Each sub-case names exactly one way a node fails to prove capability
// version 1, and that failure alone makes the whole probe incomplete.
func TestSealCapabilityIncomplete(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		report := testCapabilityClient(nc, []string{"node-1"}).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonNoReply, nodeResult(t, report, "node-1").Reason)
	})

	t.Run("duplicate", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), capabilityReply("node-1", "node-1", 1))
		serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), capabilityReply("node-1", "node-1", 1))
		report := testCapabilityClient(nc, []string{"node-1"}).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonDuplicate, nodeResult(t, report, "node-1").Reason)
	})

	t.Run("unreachable timeout", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), func(msg *nats.Msg) {
			time.Sleep(time.Second)
			_ = msg.Respond(nil)
		})
		client := testCapabilityClient(nc, []string{"node-1"})
		report := client.CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonNoReply, nodeResult(t, report, "node-1").Reason)
	})

	t.Run("transport closed connection", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		nc.Close()
		report := testCapabilityClient(nc, []string{"node-1"}).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonTransport, nodeResult(t, report, "node-1").Reason)
	})

	t.Run("malformed body", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), func(msg *nats.Msg) {
			reply := nats.NewMsg(msg.Reply)
			reply.Data = []byte("not json")
			reply.Header.Set(natsmsg.NodeIDHeader, "node-1")
			_ = msg.RespondMsg(reply)
		})
		report := testCapabilityClient(nc, []string{"node-1"}).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonMalformed, nodeResult(t, report, "node-1").Reason)
	})

	t.Run("error reply", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), func(msg *nats.Msg) {
			reply := nats.NewMsg(msg.Reply)
			reply.Data = awserrors.GenerateErrorPayload(awserrors.ErrorServerInternal)
			reply.Header.Set(natsmsg.NodeIDHeader, "node-1")
			_ = msg.RespondMsg(reply)
		})
		report := testCapabilityClient(nc, []string{"node-1"}).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonErrorReply, nodeResult(t, report, "node-1").Reason)
	})

	t.Run("wrong identity no header", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), capabilityReply("node-1", "", 1))
		report := testCapabilityClient(nc, []string{"node-1"}).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonWrongIdentity, nodeResult(t, report, "node-1").Reason)
	})

	t.Run("wrong identity another node header", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), capabilityReply("node-1", "node-9", 1))
		report := testCapabilityClient(nc, []string{"node-1"}).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonWrongIdentity, nodeResult(t, report, "node-1").Reason)
	})

	t.Run("wrong identity body node differs", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), capabilityReply("node-9", "node-1", 1))
		report := testCapabilityClient(nc, []string{"node-1"}).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonWrongIdentity, nodeResult(t, report, "node-1").Reason)
	})

	t.Run("lower version", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), capabilityReply("node-1", "node-1", 0))
		report := testCapabilityClient(nc, []string{"node-1"}).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonOldVersion, nodeResult(t, report, "node-1").Reason)
	})

	t.Run("configured set empty", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		report := testCapabilityClient(nc, nil).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Empty(t, report.Nodes)
	})

	t.Run("invalid node id", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		report := testCapabilityClient(nc, []string{"bad.node"}).CheckSealCapability(testCtx(t), "req")
		assert.False(t, report.Complete)
		assert.Equal(t, sealCapabilityReasonInvalidNodeID, nodeResult(t, report, "bad.node").Reason)
	})
}

// Every configured node answering version 1 or later makes the probe
// complete, and the reaper sends exactly one seal request.
func TestSealCapabilityCompleteAllowsSeal(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), capabilityReply("node-1", "node-1", 1))
	serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-2"), capabilityReply("node-2", "node-2", 2))

	client := testCapabilityClient(nc, []string{"node-1", "node-2"})
	report := client.CheckSealCapability(testCtx(t), "req")
	require.True(t, report.Complete)
	assert.Equal(t, 1, nodeResult(t, report, "node-1").Version)
	assert.Equal(t, 2, nodeResult(t, report, "node-2").Version)

	sealer := &fakeSealer{}
	reaper := &accountSettingsSealReaper{sealer: sealer, capability: client}
	require.NoError(t, reaper.Delete(testCtx(t), testAccountID, Resource{Kind: reaper.Kind(), ID: testAccountID}, false))
	assert.Equal(t, []string{testAccountID}, sealer.calls)
}

// An incomplete probe keeps the account TERMINATING through the full engine,
// names the silent node and its reason, sends no seal request, and leaves the
// settings record untouched (the fake sealer records no call).
func TestSealCapabilityIncompleteKeepsAccountTerminating(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), capabilityReply("node-1", "node-1", 1))
	// node-2 has no subscriber.

	sealer := &fakeSealer{}
	reaper := &accountSettingsSealReaper{sealer: sealer, capability: testCapabilityClient(nc, []string{"node-1", "node-2"})}
	accounts := newFakeAccounts()
	engine := quietEngine(accounts, reaper)

	result, err := engine.Teardown(testCtx(t), request())
	require.ErrorIs(t, err, ErrResourcesStuck)
	assert.False(t, result.AccountDeleted)
	assert.Equal(t, AccountStatusTerminating, accounts.account.Status)
	assert.Empty(t, sealer.calls)

	var reason string
	for _, stage := range result.Stages {
		for _, stuck := range stage.Stuck {
			if stuck.Resource.Kind == "ec2-account-settings" {
				reason = stuck.Reason
			}
		}
	}
	assert.Contains(t, reason, "node-2="+sealCapabilityReasonNoReply)
}

// The silent node starting to answer during the drain makes the very next
// Delete re-probe and succeed, which proves the result is retryable and the
// probe is never cached across attempts.
func TestSealCapabilityRetriesThenSeals(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), capabilityReply("node-1", "node-1", 1))

	var node2Up atomic.Bool
	sub, err := nc.Subscribe(ec2v1.AccountSettingsSealCapabilitySubject("node-2"), func(msg *nats.Msg) {
		if !node2Up.Load() {
			return
		}
		capabilityReply("node-2", "node-2", 1)(msg)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	sealer := &fakeSealer{}
	reaper := &accountSettingsSealReaper{sealer: sealer, capability: testCapabilityClient(nc, []string{"node-1", "node-2"})}
	accounts := newFakeAccounts()
	engine := quietEngine(accounts, reaper)
	engine.Timeouts = Timeouts{StageDrain: time.Second, DrainPoll: 5 * time.Millisecond}

	go func() {
		time.Sleep(80 * time.Millisecond)
		node2Up.Store(true)
	}()

	result, err := engine.Teardown(testCtx(t), request())
	require.NoError(t, err)
	assert.True(t, result.AccountDeleted)
	assert.Equal(t, []string{testAccountID}, sealer.calls)
}

// Two Delete calls on one reaper probe twice: the probe runs in Delete
// itself, never cached from a previous attempt.
func TestSealCapabilityProbedBeforeEverySeal(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	var probes int
	serveCapability(t, nc, ec2v1.AccountSettingsSealCapabilitySubject("node-1"), func(msg *nats.Msg) {
		probes++
		capabilityReply("node-1", "node-1", 1)(msg)
	})

	sealer := &fakeSealer{}
	client := testCapabilityClient(nc, []string{"node-1"})
	reaper := &accountSettingsSealReaper{sealer: sealer, capability: client}

	require.NoError(t, reaper.Delete(testCtx(t), testAccountID, Resource{Kind: reaper.Kind(), ID: testAccountID}, false))
	// alreadySealed now makes List empty, but Delete itself does not consult
	// it, so a second direct call still probes and seals again.
	require.NoError(t, reaper.Delete(testCtx(t), testAccountID, Resource{Kind: reaper.Kind(), ID: testAccountID}, false))
	assert.Equal(t, 2, probes)
	assert.Len(t, sealer.calls, 2)
}

// Five silent nodes still return within one deadline, not five: every node
// is probed in parallel against the same wait.
func TestSealCapabilityBoundedByOneDeadline(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	client := testCapabilityClient(nc, []string{"node-1", "node-2", "node-3", "node-4", "node-5"})

	started := time.Now()
	report := client.CheckSealCapability(testCtx(t), "req")
	elapsed := time.Since(started)

	assert.False(t, report.Complete)
	assert.Less(t, elapsed, client.wait+time.Second, "five silent nodes must not cost five deadlines")
}
