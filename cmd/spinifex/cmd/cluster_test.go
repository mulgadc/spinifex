package cmd

import (
	"encoding/json"
	"errors"
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

// TestHostIsStopping covers every systemctl is-system-running outcome the
// shutdown-drain gate must distinguish: a real shutdown/reboot ("stopping"),
// ordinary steady states that must never trigger a drain, unrecognized
// output, and a fully unreachable systemctl.
func TestHostIsStopping(t *testing.T) {
	orig := systemIsSystemRunning
	t.Cleanup(func() { systemIsSystemRunning = orig })

	cases := []struct {
		name         string
		out          string
		err          error
		wantStopping bool
		wantErr      bool
	}{
		{"stopping", "stopping", errors.New("exit status 1"), true, false},
		{"running", "running", nil, false, false},
		{"degraded", "degraded", errors.New("exit status 1"), false, false},
		{"maintenance", "maintenance", errors.New("exit status 1"), false, false},
		{"unknown output", "banana", nil, false, true},
		{"systemctl unreachable", "", errors.New("exec: \"systemctl\": executable file not found in $PATH"), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			systemIsSystemRunning = func() (string, error) { return tc.out, tc.err }
			stopping, err := hostIsStopping()
			assert.Equal(t, tc.wantStopping, stopping)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
