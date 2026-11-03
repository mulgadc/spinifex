package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fenceDaemon is a daemon running one guest on volumeID, subscribed to fence
// notices on a real NATS server through the subscription it makes in production.
func fenceDaemon(t *testing.T, volumeID string) (*Daemon, *vm.VM, *nats.Conn) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	_, nc := testutil.StartTestNATS(t)
	d := &Daemon{
		node:              "node-a",
		vmMgr:             vm.NewManagerWithDeps(vm.Deps{NodeID: "node-a"}),
		natsConn:          nc,
		natsSubscriptions: map[string]*nats.Subscription{},
	}
	guest := &vm.VM{
		ID:          "i-fencedguest",
		Status:      vm.StateRunning,
		Instance:    &ec2.Instance{},
		EBSRequests: vm.EBSRequests{Requests: []vm.EBSRequest{{Name: volumeID}}},
	}
	d.vmMgr.Insert(guest)

	require.NoError(t, d.registerNatsSubs(d.fenceNoticeSubs()))
	require.NoError(t, nc.Flush())
	return d, guest, nc
}

// requestFenceNotice sends raw to node-a's fence-notice subject and decodes the ack.
func requestFenceNotice(t *testing.T, nc *nats.Conn, raw []byte) ebsprovider.FenceNoticeAck {
	t.Helper()
	subject, err := ebsprovider.FenceNoticeSubject("node-a")
	require.NoError(t, err)
	reply, err := nc.Request(subject, raw, 2*time.Second)
	require.NoErrorf(t, err,
		"ADR-0007:S4: the daemon did not answer a fence notice, so the provider can never tell it was heard. "+
			"Fix: subscribe fenceNoticeSubs and respond in handleFenceNotice (daemon_handlers_fence.go)")
	var ack ebsprovider.FenceNoticeAck
	require.NoError(t, json.Unmarshal(reply.Data, &ack))
	return ack
}

func fenceNoticePayload(t *testing.T, notice ebsprovider.FenceNotice) []byte {
	t.Helper()
	payload, err := json.Marshal(notice)
	require.NoError(t, err)
	return payload
}

// memoryProviderPublishing is a non-viperblock provider holding volumeID
// published to node-a, announcing fences on nc.
func memoryProviderPublishing(t *testing.T, nc *nats.Conn, volumeID string) *ebsprovider.MemoryProvider {
	t.Helper()
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	provider.NotifyFencesOn(nc)
	_, err := provider.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	_, err = provider.PublishVolume(t.Context(), ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: "node-a",
	})
	require.NoError(t, err)
	return provider
}

// TestS4_FenceNoticeIsAcknowledgedOnlyOnceTheGuestIsStopped enforces ADR-0007:S4
// on the daemon's half of the fence. The provider repeats the notice until it is
// answered, so the answer is what ends delivery, and it must not come before the
// guest is out of running, or a lost notice is acknowledged for a guest left up.
func TestS4_FenceNoticeIsAcknowledgedOnlyOnceTheGuestIsStopped(t *testing.T) {
	d, guest, nc := fenceDaemon(t, "vol-fencedack")

	ack := requestFenceNotice(t, nc, fenceNoticePayload(t, ebsprovider.FenceNotice{
		Versioned: ebsprovider.NewVersioned(),
		VolumeID:  "vol-fencedack", NodeID: "node-a", Winner: "node-b", Reason: "volume lease moved to node-b",
	}))
	assert.Equal(t, guest.ID, ack.InstanceID, "the ack names the guest the fence stopped")
	assert.Nil(t, ack.Error)
	assert.NotEqual(t, vm.StateRunning, d.vmMgr.Status(guest),
		"ADR-0007:S4: the fence was acknowledged while the guest was still running")
}

// TestS4_FenceNoticeFromAnyProviderStopsTheGuest holds the daemon to the
// contract rather than to viperblockd: a provider that is not viperblock
// revoking the export a local guest runs on gets that guest stopped.
func TestS4_FenceNoticeFromAnyProviderStopsTheGuest(t *testing.T) {
	d, guest, nc := fenceDaemon(t, "vol-memoryfenced")
	provider := memoryProviderPublishing(t, nc, "vol-memoryfenced")

	stopped, err := provider.RevokePublication(t.Context(), "vol-memoryfenced", "node-b")
	require.NoErrorf(t, err,
		"ADR-0007:S4: the daemon did not take a fence notice from a non-viperblock provider. "+
			"Fix: subscribe the contract fence-notice subject in fenceNoticeSubs (daemon_handlers_fence.go)")
	assert.Equal(t, guest.ID, stopped)
	assert.Equal(t, vm.StateError, d.vmMgr.Status(guest),
		"ADR-0007:S4: the guest runs on against an export its provider revoked")
}

// TestFenceNotice_VolumeNoLocalGuestUsesChangesNothing is the ordinary case: a
// volume fenced after its guest has gone leaves every local guest as it was.
func TestFenceNotice_VolumeNoLocalGuestUsesChangesNothing(t *testing.T) {
	d, guest, nc := fenceDaemon(t, "vol-inuse")
	provider := memoryProviderPublishing(t, nc, "vol-noguest")

	stopped, err := provider.RevokePublication(t.Context(), "vol-noguest", "node-b")
	require.NoError(t, err, "a fence with nothing to stop is not an error")
	assert.Empty(t, stopped)
	assert.Equal(t, vm.StateRunning, d.vmMgr.Status(guest), "a guest on another volume must not be stopped")
}

// TestS4_RepeatedFenceNoticeIsHarmless covers the cost of the retry: the notice
// can arrive more than once, and a second copy must leave the fenced guest as the
// first did rather than re-driving it or failing.
func TestS4_RepeatedFenceNoticeIsHarmless(t *testing.T) {
	d, guest, nc := fenceDaemon(t, "vol-fencedtwice")

	payload := fenceNoticePayload(t, ebsprovider.FenceNotice{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-fencedtwice", NodeID: "node-a", Winner: "node-b",
	})
	for range 2 {
		ack := requestFenceNotice(t, nc, payload)
		assert.Nil(t, ack.Error, "a repeated notice is not an error")
	}
	assert.Equal(t, vm.StateError, d.vmMgr.Status(guest))
}

// TestFenceNotice_UnactionableNoticeIsAnsweredWithAnError stops a notice that
// can never be acted on from being repeated forever, and leaves the guest alone.
func TestFenceNotice_UnactionableNoticeIsAnsweredWithAnError(t *testing.T) {
	cases := map[string]struct {
		raw  []byte
		code ebsprovider.ErrorCode
	}{
		"undecodable":     {[]byte(`{`), ebsprovider.ErrorCodeInvalidArgument},
		"no volume":       {[]byte(`{"schema_version":1,"volume_id":""}`), ebsprovider.ErrorCodeInvalidArgument},
		"unknown version": {[]byte(`{"schema_version":0,"volume_id":"vol-unused"}`), ebsprovider.ErrorCodeUnsupportedVersion},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d, guest, nc := fenceDaemon(t, "vol-unused")
			ack := requestFenceNotice(t, nc, tc.raw)
			require.NotNil(t, ack.Error)
			assert.Equal(t, tc.code, ack.Error.Code)
			assert.Equal(t, vm.StateRunning, d.vmMgr.Status(guest))
		})
	}
}
