package daemon

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
)

// fenceDaemon is a daemon running one guest on volumeID, subscribed to fence
// notices on a real NATS server through the subscription it makes in production.
func fenceDaemon(t *testing.T, volumeID string) (*Daemon, *vm.VM, *nats.Conn) {
	t.Helper()
	return fenceDaemonWithManager(t, volumeID, vm.NewManagerWithDeps(vm.Deps{NodeID: "node-a"}))
}

func fenceDaemonWithManager(t *testing.T, volumeID string, mgr *vm.Manager) (*Daemon, *vm.VM, *nats.Conn) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	_, nc := testutil.StartTestNATS(t)
	d := &Daemon{
		node:              "node-a",
		vmMgr:             mgr,
		natsConn:          nc,
		natsSubscriptions: map[string]*nats.Subscription{},
	}
	guest := fencedGuest(volumeID)
	d.vmMgr.Insert(guest)

	require.NoError(t, d.registerNatsSubs(d.fenceNoticeSubs()))
	require.NoError(t, nc.Flush())
	return d, guest, nc
}

func fencedGuest(volumeID string) *vm.VM {
	return &vm.VM{
		ID:          "i-fencedguest",
		Status:      vm.StateRunning,
		Instance:    &ec2.Instance{},
		EBSRequests: vm.EBSRequests{Requests: []vm.EBSRequest{{Name: volumeID}}},
	}
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

// TestS4_FenceNoticeIsNotAcknowledgedWhileTheStopIsInProgress enforces
// ADR-0007:S4 on the order of the stop and the ack: with the stop held open,
// no answer may reach the provider until it completes.
func TestS4_FenceNoticeIsNotAcknowledgedWhileTheStopIsInProgress(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	var mgr *vm.Manager
	mgr = vm.NewManagerWithDeps(vm.Deps{NodeID: "node-a", TransitionState: func(v *vm.VM, target vm.InstanceState) error {
		close(entered)
		<-release
		mgr.Inspect(v, func(v *vm.VM) { v.Status = target })
		return nil
	}})
	_, guest, nc := fenceDaemonWithManager(t, "vol-fenceheld", mgr)

	subject, err := ebsprovider.FenceNoticeSubject("node-a")
	require.NoError(t, err)
	type answer struct {
		msg *nats.Msg
		err error
	}
	answers := make(chan answer, 1)
	payload := fenceNoticePayload(t, ebsprovider.FenceNotice{Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-fenceheld", NodeID: "node-a"})
	go func() {
		msg, err := nc.Request(subject, payload, 10*time.Second)
		answers <- answer{msg, err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the fence never reached the guest's stop, so this test proved nothing about the ack order")
	}
	select {
	case got := <-answers:
		t.Fatalf("ADR-0007:S4: the fence was acknowledged (%v) before the guest's stop finished. "+
			"Fix: ack only after FenceVolume returns in handleFenceNotice (daemon_handlers_fence.go)", got.err)
	case <-time.After(300 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(release) })
	var got answer
	select {
	case got = <-answers:
	case <-time.After(5 * time.Second):
		t.Fatal("the fence was never acknowledged once the stop finished")
	}
	require.NoError(t, got.err)
	var ack ebsprovider.FenceNoticeAck
	require.NoError(t, json.Unmarshal(got.msg.Data, &ack))
	assert.Equal(t, guest.ID, ack.InstanceID)
}

// TestS4_FenceNoticeIsServedByTheProductionSubscriptionTable keeps the
// subscription in subscribeAll: a handler nothing subscribes is a fence no
// daemon hears.
func TestS4_FenceNoticeIsServedByTheProductionSubscriptionTable(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	ns, nc, _ := testutil.StartTestJetStream(t)

	d := newSubscribeTestDaemon(t, nc, "", "")
	d.node = "node-a"
	d.vmMgr = vm.NewManagerWithDeps(vm.Deps{NodeID: "node-a"})
	guest := fencedGuest("vol-fencedsubscribed")
	d.vmMgr.Insert(guest)
	require.NoError(t, d.subscribeAll())
	require.NoError(t, nc.Flush())

	caller, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(caller.Close)
	subject, err := ebsprovider.FenceNoticeSubject("node-a")
	require.NoError(t, err)
	reply, err := caller.Request(subject, fenceNoticePayload(t, ebsprovider.FenceNotice{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-fencedsubscribed", NodeID: "node-a",
	}), 2*time.Second)
	require.NoErrorf(t, err,
		"ADR-0007:S4: the running daemon does not serve fence notices. Fix: append fenceNoticeSubs in subscribeAll (daemon.go)")
	var ack ebsprovider.FenceNoticeAck
	require.NoError(t, json.Unmarshal(reply.Data, &ack))
	assert.Nil(t, ack.Error)
	assert.Equal(t, guest.ID, ack.InstanceID)
}

// TestNewDaemon_RefusesANodeTheFenceNoticeCannotAddress keeps a daemon from
// starting where no fence notice can reach it, which would leave a guest
// running on a revoked export with nothing but a log line to say so.
func TestNewDaemon_RefusesANodeTheFenceNoticeCannotAddress(t *testing.T) {
	for _, node := range []string{"", "node.a", "node-*", "node>"} {
		t.Run(node, func(t *testing.T) {
			d, err := NewDaemon(&config.ClusterConfig{Node: node, Nodes: map[string]config.Config{node: {}}})
			require.ErrorIs(t, err, ebsprovider.ErrInvalidArgument)
			assert.Contains(t, err.Error(), "cluster config node")
			assert.Nil(t, d)
		})
	}
}

// TestFenceNotice_SpanAttributesTheAccountAndTheRefusal reads the daemon's
// span for a refused notice: a server span for the fenced verb, carrying the
// sender's account and the refusal's code rather than a successful round trip.
func TestFenceNotice_SpanAttributesTheAccountAndTheRefusal(t *testing.T) {
	recorder := testutil.RecordSpans(t)
	_, _, nc := fenceDaemon(t, "vol-unused")

	ctx := ebsprovider.WithAccountID(t.Context(), "123456789012")
	_, err := ebsprovider.NotifyFenced(ctx, nc, ebsprovider.FenceNotice{
		Versioned: ebsprovider.Versioned{SchemaVersion: ebsprovider.SchemaVersion + 1}, VolumeID: "vol-unused", NodeID: "node-a",
	}, 2*time.Second)
	require.ErrorIs(t, err, ebsprovider.ErrUnsupportedVersion)

	_, server := testutil.SpansByKind(t, recorder)
	assert.Equal(t, "ebs.volume.fenced", server.Name())
	assert.Equal(t, codes.Error, server.Status().Code)
	assert.Equal(t, string(ebsprovider.ErrorCodeUnsupportedVersion), testutil.SpanAttribute(t, server, "ebs.provider.error_code").AsString())
	assert.Equal(t, "123456789012", testutil.SpanAttribute(t, server, "aws.account_id").AsString())
}

// TestFenceNotice_AckEchoesTheNoticeVersion keeps the answer readable by the
// sender: an ack in a version other than the notice's is one it may refuse.
func TestFenceNotice_AckEchoesTheNoticeVersion(t *testing.T) {
	for _, version := range ebsprovider.SupportedSchemaVersions() {
		_, _, nc := fenceDaemon(t, "vol-versioned")
		ack := requestFenceNotice(t, nc, fenceNoticePayload(t, ebsprovider.FenceNotice{
			Versioned: ebsprovider.Versioned{SchemaVersion: version}, VolumeID: "vol-versioned", NodeID: "node-a",
		}))
		assert.Nil(t, ack.Error)
		assert.Equalf(t, version, ack.SchemaVersion, "a notice at version %d was answered at another", version)
	}
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
		"future version":  {fmt.Appendf(nil, `{"schema_version":%d,"volume_id":"vol-unused"}`, ebsprovider.SchemaVersion+1), ebsprovider.ErrorCodeUnsupportedVersion},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d, guest, nc := fenceDaemon(t, "vol-unused")
			ack := requestFenceNotice(t, nc, tc.raw)
			require.NotNil(t, ack.Error)
			assert.Equal(t, tc.code, ack.Error.Code)
			if tc.code == ebsprovider.ErrorCodeUnsupportedVersion {
				assert.Contains(t, ack.Error.Message, "unsupported schema version ")
				assert.Contains(t, ack.Error.Message, ebsprovider.SupportedVersionsText())
			}
			assert.Equal(t, vm.StateRunning, d.vmMgr.Status(guest))
		})
	}
}
