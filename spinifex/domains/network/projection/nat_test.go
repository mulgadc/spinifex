package projection

import (
	"encoding/json"
	"testing"
	"time"

	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func startTestNATSServer(t *testing.T) *server.Server {
	t.Helper()
	ns, _ := testutil.StartTestNATS(t)
	return ns
}

// TestAddNAT_Success pins that AddNAT returns nil only when vpcd acks the
// add-nat request with {"success":true}. The wire payload must match the
// networkv1.NATEvent shape vpcd unmarshals on the other end.
func TestAddNAT_Success(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	var got networkv1.NATEvent
	_, err = nc.Subscribe("vpc.add-nat", func(msg *nats.Msg) {
		_ = json.Unmarshal(msg.Data, &got)
		_ = msg.Respond([]byte(`{"success":true}`))
	})
	require.NoError(t, err)

	err = New(nc).AddNAT("vpc-1", "203.0.113.5", "10.0.0.5", "port-eni-1", "02:00:00:00:00:01")
	require.NoError(t, err)
	assert.Equal(t, "vpc-1", got.VpcId)
	assert.Equal(t, "203.0.113.5", got.ExternalIP)
	assert.Equal(t, "10.0.0.5", got.LogicalIP)
	assert.Equal(t, "port-eni-1", got.PortName)
	assert.Equal(t, "02:00:00:00:00:01", got.MAC)
}

// TestAddNAT_NACK is the regression for the silent-corruption bug: a vpcd failure must return a non-nil error
// so callers can roll back IPAM and ENI public IP state (previously the helper only logged a warning).
func TestAddNAT_NACK(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	_, err = nc.Subscribe("vpc.add-nat", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"success":false,"error":"northd unavailable"}`))
	})
	require.NoError(t, err)

	err = New(nc).AddNAT("vpc-1", "203.0.113.5", "10.0.0.5", "port-eni-1", "02:00:00:00:00:01")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "northd unavailable")
}

// TestAddNAT_NoResponders ensures a vpcd outage (no subscriber on the topic)
// surfaces as an error rather than a swallowed warning.
func TestAddNAT_NoResponders(t *testing.T) {
	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	err = New(nc).AddNAT("vpc-1", "203.0.113.5", "10.0.0.5", "port-eni-1", "02:00:00:00:00:01")
	require.Error(t, err)
}

// A teardown published into the gap where vpcd has unsubscribed but its
// replacement has not yet subscribed must be retried, not dropped: the address
// is released while its host route still delivers to the guest that held it.
func TestRemoveNAT_DeleteRetriesAcrossSubscriberGap(t *testing.T) {
	shortenDeleteNATRetryDelay(t)
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	// Subscribe only after the first attempt would have found nobody home.
	got := make(chan string, 1)
	time.AfterFunc(deleteNATRetryDelay/2, func() {
		_, serr := nc.Subscribe("vpc.delete-nat", func(msg *nats.Msg) {
			var evt networkv1.NATEvent
			_ = json.Unmarshal(msg.Data, &evt)
			got <- evt.ExternalIP
			_ = msg.Respond([]byte(`{"success":true}`))
		})
		assert.NoError(t, serr)
	})

	New(nc).RemoveNAT("vpc-a", "192.168.0.73", "172.31.0.4", "port-eni-a", "")

	select {
	case eip := <-got:
		assert.Equal(t, "192.168.0.73", eip)
	case <-time.After(5 * time.Second):
		t.Fatal("vpc.delete-nat was never delivered")
	}
}

// The retry is bounded: with nothing ever subscribing it gives up rather than
// blocking the API call that issued the disassociate.
func TestRemoveNAT_DeleteGivesUpWithNoSubscriber(t *testing.T) {
	shortenDeleteNATRetryDelay(t)
	ns := startTestNATSServer(t)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	start := time.Now()
	New(nc).RemoveNAT("vpc-a", "192.168.0.73", "172.31.0.4", "port-eni-a", "")
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, time.Duration(deleteNATRetries-1)*deleteNATRetryDelay,
		"every retry must be attempted before giving up")
	assert.Less(t, elapsed, deleteNATTimeout,
		"no-responders must fail fast rather than burn the reply timeout")
}

// shortenDeleteNATRetryDelay keeps the gap test's delay/2 subscribe well clear
// of the first retry while not paying the production delay.
func shortenDeleteNATRetryDelay(t *testing.T) {
	t.Helper()
	prev := deleteNATRetryDelay
	deleteNATRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() { deleteNATRetryDelay = prev })
}
