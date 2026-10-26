package daemon

import (
	"encoding/json"
	"testing"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCapabilityTestDaemon builds a daemon serving its real subscription table
// with node as its configured node ID.
func newCapabilityTestDaemon(t *testing.T, node string) (*nats.Conn, *nats.Conn) {
	t.Helper()
	ns, nc, _ := testutil.StartTestJetStream(t)

	d := newSubscribeTestDaemon(t, nc, "", "")
	d.node = node
	require.NoError(t, d.subscribeAll())
	require.NoError(t, nc.Flush())

	caller, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(caller.Close)
	return nc, caller
}

func capabilityRequest(t *testing.T, nc *nats.Conn, node string) *nats.Msg {
	t.Helper()
	data, err := json.Marshal(ec2v1.AccountSettingsSealCapabilityRequest{Node: node, CorrelationRef: "req-1"})
	require.NoError(t, err)
	reply, err := nc.Request(ec2v1.AccountSettingsSealCapabilitySubject(node), data, 5*time.Second)
	require.NoError(t, err)
	return reply
}

// The responder answers its own node with version 1, the configured node and
// no KV access (no settings service is wired into the test daemon at all).
func TestAccountSettingsSealCapabilityAnswersOwnNode(t *testing.T) {
	_, caller := newCapabilityTestDaemon(t, "node-1")

	raw := capabilityRequest(t, caller, "node-1")
	assert.Equal(t, "node-1", raw.Header.Get("X-Node-ID"))

	var reply ec2v1.AccountSettingsSealCapabilityReply
	require.NoError(t, json.Unmarshal(raw.Data, &reply))
	assert.Equal(t, "node-1", reply.Node)
	assert.Equal(t, ec2v1.AccountSettingsSealCapabilityVersion, reply.CapabilityVersion)
	assert.Equal(t, "req-1", reply.CorrelationRef)
}

// A request naming a node other than this daemon's own is refused, not
// answered as if it were this node.
func TestAccountSettingsSealCapabilityRefusesOtherNode(t *testing.T) {
	_, caller := newCapabilityTestDaemon(t, "node-1")

	data, err := json.Marshal(ec2v1.AccountSettingsSealCapabilityRequest{Node: "node-2"})
	require.NoError(t, err)
	raw, err := caller.Request(ec2v1.AccountSettingsSealCapabilitySubject("node-1"), data, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, replyErrCode(t, raw.Data))
}

// The subject is per node with no queue group, so two daemons configured
// with the same node ID both answer a request addressed to that node.
func TestAccountSettingsSealCapabilityHasNoQueueGroup(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	first := newSubscribeTestDaemon(t, nc, "", "")
	first.node = "node-1"
	require.NoError(t, first.subscribeAll())

	second := newSubscribeTestDaemon(t, nc, "", "")
	second.node = "node-1"
	require.NoError(t, second.subscribeAll())
	require.NoError(t, nc.Flush())

	sub, err := nc.SubscribeSync(nats.NewInbox())
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	data, err := json.Marshal(ec2v1.AccountSettingsSealCapabilityRequest{Node: "node-1"})
	require.NoError(t, err)
	msg := nats.NewMsg(ec2v1.AccountSettingsSealCapabilitySubject("node-1"))
	msg.Data = data
	msg.Reply = sub.Subject
	require.NoError(t, nc.PublishMsg(msg))
	require.NoError(t, nc.Flush())

	seen := 0
	for {
		_, err := sub.NextMsg(300 * time.Millisecond)
		if err != nil {
			break
		}
		seen++
	}
	assert.Equal(t, 2, seen)
}

// A wildcard node ID must not become a wildcard subscription: built unchecked,
// "node-*" would answer probes addressed to node-1 on node-1's behalf.
func TestAccountSettingsSealCapabilityNotServedForInvalidNodeID(t *testing.T) {
	_, caller := newCapabilityTestDaemon(t, "node-*")

	data, err := json.Marshal(ec2v1.AccountSettingsSealCapabilityRequest{Node: "node-1"})
	require.NoError(t, err)
	_, err = caller.Request(ec2v1.AccountSettingsSealCapabilitySubject("node-1"), data, time.Second)
	require.ErrorIs(t, err, nats.ErrNoResponders)
}
