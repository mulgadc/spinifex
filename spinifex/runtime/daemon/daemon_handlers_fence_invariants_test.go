package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fenceDaemon is a daemon running one guest on volumeID, answering fence
// notices on a real NATS server as it does in production.
func fenceDaemon(t *testing.T, volumeID string) (*Daemon, *vm.VM, *nats.Conn) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	_, nc := testutil.StartTestNATS(t)
	d := &Daemon{node: "node-a", vmMgr: vm.NewManagerWithDeps(vm.Deps{NodeID: "node-a"})}
	guest := &vm.VM{
		ID:          "i-fencedguest",
		Status:      vm.StateRunning,
		Instance:    &ec2.Instance{},
		EBSRequests: vm.EBSRequests{Requests: []vm.EBSRequest{{Name: volumeID}}},
	}
	d.vmMgr.Insert(guest)

	_, err := nc.Subscribe(viperblocklegacyv1.VolumeFencedSubject(d.node), func(msg *nats.Msg) { d.handleVolumeFenced(msg) })
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	return d, guest, nc
}

// TestS4_FenceNoticeIsAcknowledgedOnlyOnceTheGuestIsStopped enforces ADR-0007:S4
// on the daemon's half of the fence. viperblockd repeats the notice until it is
// answered, so the answer is what ends delivery, and it must not come before the
// guest is out of running, or a lost notice is acknowledged for a guest left up.
func TestS4_FenceNoticeIsAcknowledgedOnlyOnceTheGuestIsStopped(t *testing.T) {
	d, guest, nc := fenceDaemon(t, "vol-fencedack")

	payload, err := json.Marshal(viperblocklegacyv1.VolumeFencedEvent{
		Volume: "vol-fencedack", Node: "node-a", Winner: "node-b", Reason: "volume lease moved to node-b",
	})
	require.NoError(t, err)

	reply, err := nc.Request(viperblocklegacyv1.VolumeFencedSubject(d.node), payload, 2*time.Second)
	require.NoErrorf(t, err,
		"ADR-0007:S4: the daemon did not answer a fence notice, so viperblockd can never tell it was heard. "+
			"Fix: respond in handleVolumeFenced (daemon_handlers_fence.go)")

	var ack viperblocklegacyv1.VolumeFencedAck
	require.NoError(t, json.Unmarshal(reply.Data, &ack))
	assert.Equal(t, guest.ID, ack.Instance, "the ack names the guest the fence stopped")
	assert.Empty(t, ack.Error)
	assert.NotEqual(t, vm.StateRunning, d.vmMgr.Status(guest),
		"ADR-0007:S4: the fence was acknowledged while the guest was still running")
}

// TestS4_RepeatedFenceNoticeIsHarmless covers the cost of the retry: the notice
// can arrive more than once, and a second copy must leave the fenced guest as the
// first did rather than re-driving it or failing.
func TestS4_RepeatedFenceNoticeIsHarmless(t *testing.T) {
	d, guest, nc := fenceDaemon(t, "vol-fencedtwice")

	payload, err := json.Marshal(viperblocklegacyv1.VolumeFencedEvent{Volume: "vol-fencedtwice", Winner: "node-b"})
	require.NoError(t, err)

	for range 2 {
		reply, err := nc.Request(viperblocklegacyv1.VolumeFencedSubject(d.node), payload, 2*time.Second)
		require.NoError(t, err)
		var ack viperblocklegacyv1.VolumeFencedAck
		require.NoError(t, json.Unmarshal(reply.Data, &ack))
		assert.Empty(t, ack.Error, "a repeated notice is not an error")
	}
	assert.Equal(t, vm.StateError, d.vmMgr.Status(guest))
}

// TestVolumeFenced_BadEventIsAnsweredWithAnError stops a notice that can never be
// acted on from being repeated forever.
func TestVolumeFenced_BadEventIsAnsweredWithAnError(t *testing.T) {
	d, _, nc := fenceDaemon(t, "vol-unused")

	reply, err := nc.Request(viperblocklegacyv1.VolumeFencedSubject(d.node), []byte(`{"volume":""}`), 2*time.Second)
	require.NoError(t, err)
	var ack viperblocklegacyv1.VolumeFencedAck
	require.NoError(t, json.Unmarshal(reply.Data, &ack))
	assert.NotEmpty(t, ack.Error)
}
