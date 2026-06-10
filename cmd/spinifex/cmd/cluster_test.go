package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDrainResponder subscribes a stand-in vpcd to vpc.dhcp.drain that
// replies with the given released count (or error), mirroring
// Manager.handleDrainMsg without a real lease store.
func fakeDrainResponder(t *testing.T, nc *nats.Conn, released int, errMsg string) {
	t.Helper()
	sub, err := nc.Subscribe(dhcp.TopicDrain, func(msg *nats.Msg) {
		body, _ := json.Marshal(struct {
			Released int    `json:"released"`
			Error    string `json:"error,omitempty"`
		}{Released: released, Error: errMsg})
		_ = msg.Respond(body)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func TestDrainDHCPLeasesSumsReleased(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	fakeDrainResponder(t, nc, 7, "")

	released, responders := drainDHCPLeases(nc, 2*time.Second)
	assert.Equal(t, 7, released)
	assert.Equal(t, 1, responders)
}

func TestDrainDHCPLeasesReportsResponderError(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	fakeDrainResponder(t, nc, 0, "list leases for drain: boom")

	// An erroring responder still counts as a responder but contributes
	// zero released leases.
	released, responders := drainDHCPLeases(nc, 2*time.Second)
	assert.Equal(t, 0, released)
	assert.Equal(t, 1, responders)
}

func TestDrainDHCPLeasesNoResponders(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	// No vpcd subscribed: the collection window elapses with zero replies.
	released, responders := drainDHCPLeases(nc, 200*time.Millisecond)
	assert.Equal(t, 0, released)
	assert.Equal(t, 0, responders)
}
