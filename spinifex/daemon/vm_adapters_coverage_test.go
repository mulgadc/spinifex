package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/gpu"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/services/viperblockd/vbwire"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uniqueAdapterID returns an ID no other test in the binary uses, for keys
// written to the shared JetStream buckets.
func uniqueAdapterID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// connectAdapterTestNATS opens a connection to url that closes with the test.
func connectAdapterTestNATS(t *testing.T, url string) *nats.Conn {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

// closedAdapterTestConn returns a connection that is already closed, so every
// request and subscribe on it fails immediately.
func closedAdapterTestConn(t *testing.T) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	nc.Close()
	return nc
}

// respondJSON subscribes subject on nc and answers every request with reply.
func respondJSON(t *testing.T, nc *nats.Conn, subject string, reply any) {
	t.Helper()
	data, err := json.Marshal(reply)
	require.NoError(t, err)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) { _ = msg.Respond(data) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// --- stateStoreAdapter ---

func TestStateStoreAdapter_DelegatesToJetStream(t *testing.T) {
	nc := connectAdapterTestNATS(t, sharedJSNATSURL)
	jsm, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, jsm.InitKVBucket())
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	var persistedNode string
	adapter := newStateStoreAdapter(jsm, func(nodeID string, _ map[string]*vm.VM) error {
		persistedNode = nodeID
		return nil
	})

	t.Run("running set goes through persist", func(t *testing.T) {
		require.NoError(t, adapter.SaveRunningState("node-persist", map[string]*vm.VM{}))
		assert.Equal(t, "node-persist", persistedNode)

		vms, found, err := adapter.LoadRunningState(uniqueAdapterID("node-none"))
		require.NoError(t, err)
		assert.False(t, found, "a node that never wrote state has none")
		assert.Empty(t, vms)
	})

	t.Run("stopped instance lifecycle", func(t *testing.T) {
		id := uniqueAdapterID("i-stopped")
		t.Cleanup(func() { _ = jsm.DeleteStoppedInstance(id) })

		require.NoError(t, adapter.WriteStoppedInstance(id, &vm.VM{ID: id, InstanceType: "t3.nano"}))

		got, err := adapter.LoadStoppedInstance(id)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "t3.nano", got.InstanceType)

		updated, err := adapter.UpdateStoppedInstance(id, func(v *vm.VM) { v.InstanceType = "t3.micro" })
		require.NoError(t, err)
		assert.Equal(t, "t3.micro", updated.InstanceType)

		stopped, err := adapter.ListStoppedInstances()
		require.NoError(t, err)
		assert.True(t, slices.ContainsFunc(stopped, func(v *vm.VM) bool { return v.ID == id }))

		require.NoError(t, adapter.DeleteStoppedInstance(id))
		got, err = adapter.LoadStoppedInstance(id)
		require.NoError(t, err)
		assert.Nil(t, got, "a deleted stopped instance is gone")
	})

	t.Run("a stopped instance is claimed once", func(t *testing.T) {
		id := uniqueAdapterID("i-claim")
		t.Cleanup(func() { _ = jsm.DeleteStoppedInstance(id) })
		require.NoError(t, adapter.WriteStoppedInstance(id, &vm.VM{ID: id, InstanceType: "t3.nano"}))

		claimed, err := adapter.ClaimStoppedInstance(id)
		require.NoError(t, err)
		assert.Equal(t, id, claimed.ID)

		_, err = adapter.ClaimStoppedInstance(id)
		assert.ErrorIs(t, err, vm.ErrStoppedInstanceClaimed)
	})

	t.Run("terminated instance lifecycle", func(t *testing.T) {
		id := uniqueAdapterID("i-term")
		require.NoError(t, adapter.WriteTerminatedInstance(id, &vm.VM{ID: id, InstanceType: "t3.nano"}))

		updated, err := adapter.UpdateTerminatedInstance(id, func(v *vm.VM) { v.InstanceType = "t3.small" })
		require.NoError(t, err)
		assert.Equal(t, "t3.small", updated.InstanceType)

		hasID := func(v *vm.VM) bool { return v.ID == id }
		terminated, err := adapter.ListTerminatedInstances()
		require.NoError(t, err)
		assert.True(t, slices.ContainsFunc(terminated, hasID))

		require.NoError(t, adapter.DeleteTerminatedInstance(id))
		terminated, err = adapter.ListTerminatedInstances()
		require.NoError(t, err)
		assert.False(t, slices.ContainsFunc(terminated, hasID))
	})
}

// --- consumeCleanShutdownMarker ---

func TestConsumeCleanShutdownMarker(t *testing.T) {
	assert.False(t, (&Daemon{}).consumeCleanShutdownMarker()(),
		"without JetStream there is no marker to trust")

	nc := connectAdapterTestNATS(t, sharedJSNATSURL)
	jsm, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, jsm.InitClusterStateBucket())

	node := uniqueAdapterID("node-marker")
	d := &Daemon{node: node, jsManager: jsm}
	consume := d.consumeCleanShutdownMarker()

	assert.False(t, consume(), "no marker means the shutdown was not clean")

	require.NoError(t, jsm.WriteShutdownMarker(node))
	assert.True(t, consume(), "a recorded clean shutdown is trusted")

	present, err := jsm.ReadShutdownMarker(node)
	require.NoError(t, err)
	assert.False(t, present, "the marker is consumed so a later crash is not mistaken for a clean stop")
	assert.False(t, consume())
}

func TestBuildVMManagerDeps_BackingStoreReady(t *testing.T) {
	d := &Daemon{config: &config.Config{}, vmMgr: vm.NewManager()}
	assert.False(t, d.buildVMManagerDeps().BackingStoreReady(),
		"no NATS connection means viperblock is unreachable")

	d.natsConn = connectAdapterTestNATS(t, sharedJSNATSURL)
	assert.True(t, d.buildVMManagerDeps().BackingStoreReady(),
		"no predastore configured and JetStream answering is ready")
}

// --- volumeMounterAdapter ---

func TestVolumeMounterAdapter_Mount_ClosedConnIsNotRetryable(t *testing.T) {
	adapter := newVolumeMounterAdapter(closedAdapterTestConn(t), "node-closed", nil)
	instance := &vm.VM{ID: "i-mount-closed"}
	instance.EBSRequests.Requests = []viperblocklegacyv1.EBSRequest{{Name: "vol-closed"}}

	err := adapter.Mount(t.Context(), instance)
	require.ErrorIs(t, err, nats.ErrConnectionClosed)
	assert.NotErrorIs(t, err, vm.ErrMountRetryable,
		"only a missing responder means viperblockd is still starting")
}

// recordingVolumeState records UpdateVolumeState calls and returns err.
type recordingVolumeState struct {
	err     error
	updated []string
}

func (r *recordingVolumeState) UpdateVolumeState(_, volumeID, state, _, _ string) error {
	r.updated = append(r.updated, volumeID+"="+state)
	return r.err
}

func TestVolumeMounterAdapter_Unmount_Errors(t *testing.T) {
	nc := connectAdapterTestNATS(t, sharedNATSURL)

	t.Run("no responder fails the seal and leaves the volume", func(t *testing.T) {
		volState := &recordingVolumeState{}
		adapter := newVolumeMounterAdapter(nc, uniqueAdapterID("node-unmount"), volState)
		instance := &vm.VM{ID: "i-unmount-none"}
		instance.EBSRequests.Requests = []viperblocklegacyv1.EBSRequest{{Name: "vol-a"}, {Name: "vol-b"}}

		err := adapter.Unmount(t.Context(), instance)
		require.ErrorIs(t, err, nats.ErrNoResponders)
		assert.Contains(t, err.Error(), "vol-a")
		assert.Contains(t, err.Error(), "vol-b", "every volume is attempted")
		assert.Empty(t, volState.updated, "an unsealed volume must not go available")
	})

	t.Run("state update failure does not fail the unmount", func(t *testing.T) {
		node := uniqueAdapterID("node-unmount")
		respondJSON(t, nc, "ebs."+node+".unmount", viperblocklegacyv1.EBSUnMountResponse{Volume: "vol-a"})
		volState := &recordingVolumeState{err: errInjected}
		adapter := newVolumeMounterAdapter(nc, node, volState)
		instance := &vm.VM{ID: "i-unmount-state"}
		instance.EBSRequests.Requests = []viperblocklegacyv1.EBSRequest{{Name: "vol-a"}}

		require.NoError(t, adapter.Unmount(t.Context(), instance))
		assert.Equal(t, []string{"vol-a=available"}, volState.updated)
	})
}

func TestVolumeMounterAdapter_Abandon(t *testing.T) {
	nc := connectAdapterTestNATS(t, sharedNATSURL)

	// abandonResponder answers per volume so one request can mix outcomes.
	abandonResponder := func(t *testing.T, node string, replies map[string][]byte) *[]vbwire.VolumeAbandonRequest {
		t.Helper()
		var (
			mu   sync.Mutex
			seen []vbwire.VolumeAbandonRequest
		)
		sub, err := nc.Subscribe(vbwire.VolumeAbandonSubject(node), func(msg *nats.Msg) {
			var req vbwire.VolumeAbandonRequest
			_ = json.Unmarshal(msg.Data, &req)
			mu.Lock()
			seen = append(seen, req)
			mu.Unlock()
			_ = msg.Respond(replies[req.Volume])
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
		return &seen
	}
	instanceWith := func(volumes ...string) *vm.VM {
		v := &vm.VM{ID: "i-abandon", AccountID: testAccountID}
		for _, name := range volumes {
			v.EBSRequests.Requests = append(v.EBSRequests.Requests, viperblocklegacyv1.EBSRequest{Name: name})
		}
		return v
	}

	t.Run("no responder", func(t *testing.T) {
		adapter := newVolumeMounterAdapter(nc, uniqueAdapterID("node-abandon"), nil)
		err := adapter.Abandon(t.Context(), instanceWith("vol-1"), "superseded")
		assert.ErrorIs(t, err, nats.ErrNoResponders)
	})

	t.Run("outcomes per volume", func(t *testing.T) {
		node := uniqueAdapterID("node-abandon")
		seen := abandonResponder(t, node, map[string][]byte{
			"vol-abandoned": []byte(`{"abandoned":true}`),
			"vol-absent":    []byte(`{"abandoned":false}`),
		})
		adapter := newVolumeMounterAdapter(nc, node, nil)

		require.NoError(t, adapter.Abandon(t.Context(), instanceWith("vol-abandoned", "vol-absent"), "superseded"),
			"a volume not exported here is the ordinary case, not a failure")
		require.Len(t, *seen, 2)
		assert.Equal(t, vbwire.VolumeAbandonRequest{Volume: "vol-abandoned", Reason: "superseded"}, (*seen)[0])
	})

	t.Run("failures are joined and do not stop the sweep", func(t *testing.T) {
		node := uniqueAdapterID("node-abandon")
		seen := abandonResponder(t, node, map[string][]byte{
			"vol-garbled": []byte(`not json`),
			"vol-refused": []byte(`{"error":"export busy"}`),
			"vol-ok":      []byte(`{"abandoned":true}`),
		})
		adapter := newVolumeMounterAdapter(nc, node, nil)

		err := adapter.Abandon(t.Context(), instanceWith("vol-garbled", "vol-refused", "vol-ok"), "fenced")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unmarshal abandon response for vol-garbled")
		assert.Contains(t, err.Error(), "abandon vol-refused: export busy")
		assert.NotContains(t, err.Error(), "vol-ok")
		assert.Len(t, *seen, 3, "every volume is attempted")
	})
}

// --- resourceControllerAdapter ---

func TestResourceControllerAdapter_UnknownAndKnownType(t *testing.T) {
	rm := newReservationTestRM()
	a := newResourceControllerAdapter(rm)

	t.Run("unknown type", func(t *testing.T) {
		require.ErrorContains(t, a.Allocate("zz9.none"), "instance type zz9.none not found")
		assert.Zero(t, a.CanAllocate("zz9.none", 4))
		a.Deallocate("zz9.none")
		a.ReleaseToReservation("cr-none", "zz9.none")
		assert.Zero(t, rm.allocatedVCPU, "an unknown type must not move the books")
	})

	t.Run("known type", func(t *testing.T) {
		assert.Equal(t, 4, a.CanAllocate("t3.micro", 10))
		require.NoError(t, a.Allocate("t3.micro"))
		assert.Equal(t, 2, rm.allocatedVCPU)
		assert.Equal(t, 3, a.CanAllocate("t3.micro", 10))

		a.Deallocate("t3.micro")
		assert.Zero(t, rm.allocatedVCPU)

		require.NoError(t, a.Allocate("t3.micro"))
		a.ReleaseToReservation("cr-gone", "t3.micro")
		assert.Zero(t, rm.allocatedVCPU, "a slot from a vanished reservation returns to the general pool")
	})
}

// --- per-instance subscription hooks ---

func TestOnInstanceRecoveringHook(t *testing.T) {
	t.Run("subscribes the command topic once", func(t *testing.T) {
		d, _ := newHookTestDaemon(t)
		hook := d.onInstanceRecoveringHook()
		instance := &vm.VM{ID: "i-recovering"}

		hook(instance)
		first, ok := d.natsSubscriptions[instance.ID]
		require.True(t, ok)
		assert.Equal(t, "ec2.cmd.i-recovering", first.Subject)

		hook(instance)
		assert.Same(t, first, d.natsSubscriptions[instance.ID], "an existing subscription is kept, not replaced")
	})

	t.Run("subscribe failure leaves no entry", func(t *testing.T) {
		d := &Daemon{natsConn: closedAdapterTestConn(t), natsSubscriptions: map[string]*nats.Subscription{}}
		d.onInstanceRecoveringHook()(&vm.VM{ID: "i-recovering-closed"})
		assert.Empty(t, d.natsSubscriptions)
	})
}

func TestOnInstanceUpHook_SubscribeFailureReturnsError(t *testing.T) {
	d := &Daemon{natsConn: closedAdapterTestConn(t), natsSubscriptions: map[string]*nats.Subscription{}}

	err := d.onInstanceUpHook()(&vm.VM{ID: "i-up-closed"})
	require.ErrorIs(t, err, nats.ErrConnectionClosed)
	assert.Empty(t, d.natsSubscriptions, "no topic may stay bound when the set fails")
}

// natEIPResolver answers the associated-EIP lookup the up hook makes.
type natEIPResolver struct {
	ec2eip.EIPService

	ip string
}

func (r *natEIPResolver) AssociatedPublicIPForInstance(context.Context, string, string) (string, bool) {
	return r.ip, r.ip != ""
}

// recordNATEvents answers subject with success and records each event whose
// external IP is externalIP.
func recordNATEvents(t *testing.T, nc *nats.Conn, subject, externalIP string) func() []map[string]string {
	t.Helper()
	var (
		mu     sync.Mutex
		events []map[string]string
	)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		var evt map[string]string
		_ = json.Unmarshal(msg.Data, &evt)
		if evt["external_ip"] == externalIP {
			mu.Lock()
			events = append(events, evt)
			mu.Unlock()
		}
		_ = msg.Respond([]byte(`{"success":true}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return func() []map[string]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events)
	}
}

func TestOnInstanceUpHook_RepublishesNAT(t *testing.T) {
	d, nc := newHookTestDaemon(t)
	upInstance := func(id, publicIP string) *vm.VM {
		return &vm.VM{
			ID:       id,
			ENIId:    "eni-" + id,
			ENIMac:   "02:00:00:00:00:01",
			PublicIP: publicIP,
			Instance: &ec2.Instance{VpcId: aws.String("vpc-nat"), PrivateIpAddress: aws.String("10.0.1.5")},
		}
	}

	t.Run("elastic IP resolved from the EIP store", func(t *testing.T) {
		events := recordNATEvents(t, nc, "vpc.add-nat", "198.51.100.77")
		d.eipService = &natEIPResolver{ip: "198.51.100.77"}

		require.NoError(t, d.onInstanceUpHook()(upInstance("i-nat-eip", "")))
		got := events()
		require.Len(t, got, 1, "the EIP's NAT must be re-announced after a reboot")
		assert.Equal(t, "vpc-nat", got[0]["vpc_id"])
		assert.Equal(t, "10.0.1.5", got[0]["logical_ip"])
		assert.Equal(t, "port-eni-i-nat-eip", got[0]["port_name"])
		assert.Equal(t, "02:00:00:00:00:01", got[0]["mac"])
	})

	t.Run("auto-assigned IP wins over the EIP store", func(t *testing.T) {
		events := recordNATEvents(t, nc, "vpc.add-nat", "198.51.100.78")
		d.eipService = &natEIPResolver{ip: "198.51.100.200"}

		require.NoError(t, d.onInstanceUpHook()(upInstance("i-nat-auto", "198.51.100.78")))
		assert.Len(t, events(), 1)
	})
}

func TestOnInstanceUpHook_ReclaimsMIGSlice(t *testing.T) {
	mdev := filepath.Join(t.TempDir(), "mdev-uuid")
	require.NoError(t, os.WriteFile(mdev, nil, 0o600))

	mgr := gpu.NewManager(nil)
	mgr.AddMIGInstances(gpu.GPUDevice{PCIAddress: "0000:41:00.0"}, []gpu.MIGInstance{{MdevPath: mdev}})
	d, _ := newHookTestDaemon(t)
	d.gpuManager = mgr

	instance := &vm.VM{
		ID: "i-mig",
		GPUAttachments: []gpu.GPUAttachment{
			{MdevPath: mdev},
			{MdevPath: "/nonexistent/mdev"},
		},
	}
	require.NoError(t, d.onInstanceUpHook()(instance), "a slice that cannot be re-claimed only warns")
	assert.Zero(t, mgr.Available(), "the surviving slice is claimed again after a restart")

	require.NoError(t, newInstanceCleanerAdapter(d).ReleaseGPU(instance))
	assert.Equal(t, 1, mgr.Available(), "release returns the slice to the pool")
}

func TestOnInstanceDownHook_UnsubscribeErrors(t *testing.T) {
	d, nc := newHookTestDaemon(t)
	id := "i-down-stale"
	keys := []string{id, id + ".console", id + ".password", "system.TerminateInstance." + id}
	for _, key := range keys {
		sub, err := nc.SubscribeSync("test.stale." + key)
		require.NoError(t, err)
		require.NoError(t, sub.Unsubscribe())
		d.natsSubscriptions[key] = sub
	}

	d.onInstanceDownHook()(id)

	assert.Empty(t, d.natsSubscriptions, "a failed unsubscribe still drops the entry")
}

// --- instanceCleanerAdapter ---

// getFailingStore fails every metadata read with a generic error.
type getFailingStore struct {
	objectstore.ObjectStore
}

func (getFailingStore) GetObject(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	return nil, errInjected
}

func TestInstanceCleaner_DeleteVolumes_Errors(t *testing.T) {
	instanceWith := func(reqs ...viperblocklegacyv1.EBSRequest) *vm.VM {
		v := &vm.VM{ID: "i-delvol", AccountID: testAccountID}
		v.EBSRequests.Requests = reqs
		return v
	}

	t.Run("EFI delete with no responder", func(t *testing.T) {
		a := newInstanceCleanerAdapter(&Daemon{natsConn: connectAdapterTestNATS(t, sharedNATSURL)})
		err := a.DeleteVolumes(instanceWith(viperblocklegacyv1.EBSRequest{Name: "vol-efi-" + uniqueAdapterID("x"), EFI: true}))
		assert.ErrorIs(t, err, nats.ErrNoResponders)
	})

	t.Run("no volume service skips both kinds", func(t *testing.T) {
		a := newInstanceCleanerAdapter(&Daemon{})
		require.NoError(t, a.DeleteVolumes(instanceWith(
			viperblocklegacyv1.EBSRequest{Name: "vol-keep", DeleteOnTermination: false},
			viperblocklegacyv1.EBSRequest{Name: "vol-delete", DeleteOnTermination: true},
		)))
	})

	t.Run("volume service errors are reported", func(t *testing.T) {
		svc := ec2volume.NewVolumeServiceImplWithStore(&config.Config{}, getFailingStore{objectstore.NewMemoryObjectStore()}, nil)
		a := newInstanceCleanerAdapter(&Daemon{volumeService: svc})

		err := a.DeleteVolumes(instanceWith(viperblocklegacyv1.EBSRequest{Name: "vol-keep", DeleteOnTermination: false}))
		require.ErrorIs(t, err, errInjected, "a failed detach must surface")

		err = a.DeleteVolumes(instanceWith(viperblocklegacyv1.EBSRequest{Name: "vol-delete", DeleteOnTermination: true}))
		require.ErrorIs(t, err, errInjected, "a failed delete must surface")
	})
}

// cleanupFailingPlumber fails CleanupTap and records the tap it was given.
type cleanupFailingPlumber struct {
	recordingPlumber
}

func (p *cleanupFailingPlumber) CleanupTap(name string) error {
	p.cleanedTaps = append(p.cleanedTaps, name)
	return errInjected
}

func TestInstanceCleaner_CleanupMgmtNetwork_TapFailureStillReleases(t *testing.T) {
	flushed, _ := captureNeighHooks(t)
	alloc, err := NewMgmtIPAllocator("10.15.9.1")
	require.NoError(t, err)
	const ip = "10.15.9.20"
	alloc.Claim("i-tapfail", ip)
	require.Equal(t, 1, alloc.AllocatedCount())

	plumber := &cleanupFailingPlumber{}
	d := &Daemon{networkPlumber: plumber, mgmtIPAllocator: alloc, mgmtBridgeIP: "10.15.9.1"}
	newInstanceCleanerAdapter(d).CleanupMgmtNetwork(&vm.VM{ID: "i-tapfail", MgmtIP: ip})

	assert.Len(t, plumber.cleanedTaps, 1)
	assert.Len(t, *flushed, 1, "the neighbour entry is invalidated even when the tap teardown fails")
	assert.Zero(t, alloc.AllocatedCount(), "the address goes back to the pool")
}

func TestInstanceCleaner_ReleasePublicIP(t *testing.T) {
	const pool = "wan-fake"
	newCleaner := func(t *testing.T, releaseErr error, nc *nats.Conn) (*instanceCleanerAdapter, *fakeAllocator) {
		t.Helper()
		ipam := ec2vpc.NewExternalIPAMWithKV(nil, nil)
		alloc := &fakeAllocator{releaseErr: releaseErr}
		require.NoError(t, ipam.InstallAllocator(pool, alloc))
		return newInstanceCleanerAdapter(&Daemon{externalIPAM: ipam, natsConn: nc}), alloc
	}
	withIP := func(ip string) *vm.VM {
		return &vm.VM{ID: "i-pub", ENIId: "eni-pub", PublicIP: ip, PublicIPPool: pool}
	}

	t.Run("nothing to release", func(t *testing.T) {
		a, alloc := newCleaner(t, nil, nil)
		require.NoError(t, a.ReleasePublicIP(&vm.VM{ID: "i-pub", PublicIPPool: pool}))
		require.NoError(t, a.ReleasePublicIP(&vm.VM{ID: "i-pub", PublicIP: "203.0.113.5"}))
		require.NoError(t, newInstanceCleanerAdapter(&Daemon{}).ReleasePublicIP(withIP("203.0.113.5")))
		assert.Empty(t, alloc.releases)
	})

	t.Run("success tears down NAT and releases", func(t *testing.T) {
		nc := connectAdapterTestNATS(t, sharedNATSURL)
		events := recordNATEvents(t, nc, "vpc.delete-nat", "203.0.113.6")
		a, alloc := newCleaner(t, nil, nc)
		inst := withIP("203.0.113.6")
		inst.Instance = &ec2.Instance{VpcId: aws.String("vpc-pub"), PrivateIpAddress: aws.String("10.0.0.6")}

		require.NoError(t, a.ReleasePublicIP(inst))
		assert.Equal(t, []netip.Addr{netip.MustParseAddr("203.0.113.6")}, alloc.releases)
		got := events()
		require.Len(t, got, 1)
		assert.Equal(t, "vpc-pub", got[0]["vpc_id"])
		assert.Equal(t, "10.0.0.6", got[0]["logical_ip"])
		assert.Equal(t, "port-eni-pub", got[0]["port_name"])
	})

	t.Run("untracked lease is terminal success", func(t *testing.T) {
		a, alloc := newCleaner(t, fmt.Errorf("release: %w", dhcp.ErrLeaseNotTracked), nil)
		require.NoError(t, a.ReleasePublicIP(withIP("203.0.113.7")))
		assert.Len(t, alloc.releases, 1)
	})

	t.Run("release failure is returned", func(t *testing.T) {
		a, _ := newCleaner(t, errInjected, nil)
		assert.ErrorIs(t, a.ReleasePublicIP(withIP("203.0.113.8")), errInjected)
	})

	t.Run("unparseable address", func(t *testing.T) {
		a, alloc := newCleaner(t, nil, nil)
		assert.ErrorContains(t, a.ReleasePublicIP(withIP("not-an-ip")), "parse release IP")
		assert.Empty(t, alloc.releases)
	})
}

// adapterVPCFixture is a VPC service on a private JetStream with one ENI.
type adapterVPCFixture struct {
	url   string
	nc    *nats.Conn
	js    jetstream.JetStream
	vpc   *ec2vpc.VPCServiceImpl
	eniID string
}

func newAdapterVPCFixture(t *testing.T) *adapterVPCFixture {
	t.Helper()
	ns, nc, js := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	vpcSvc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), &config.Config{}, nc)
	require.NoError(t, err)
	vpcOut, err := vpcSvc.CreateVpc(t.Context(), &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")}, testAccountID)
	require.NoError(t, err)
	subnetOut, err := vpcSvc.CreateSubnet(t.Context(), &ec2.CreateSubnetInput{
		VpcId: vpcOut.Vpc.VpcId, CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	require.NoError(t, err)
	eniOut, err := vpcSvc.CreateNetworkInterface(t.Context(), &ec2.CreateNetworkInterfaceInput{
		SubnetId: subnetOut.Subnet.SubnetId,
	}, testAccountID)
	require.NoError(t, err)

	return &adapterVPCFixture{
		url: ns.ClientURL(), nc: nc, js: js, vpc: vpcSvc,
		eniID: *eniOut.NetworkInterface.NetworkInterfaceId,
	}
}

func TestInstanceCleaner_ReleaseAutoAssignedPublicIP(t *testing.T) {
	const pool = "wan-auto"
	f := newAdapterVPCFixture(t)

	eipKV, err := f.js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "test-adapter-eips"})
	require.NoError(t, err)
	_, err = eipKV.Put(t.Context(), testAccountID+".eipalloc-owned", []byte(`{"eni_id":"eni-eip-owned"}`))
	require.NoError(t, err)
	f.vpc.SetExternalIPAM(nil, eipKV)

	newCleaner := func(t *testing.T, releaseErr error) (*instanceCleanerAdapter, *fakeAllocator) {
		t.Helper()
		ipam := ec2vpc.NewExternalIPAMWithKV(nil, nil)
		alloc := &fakeAllocator{releaseErr: releaseErr}
		require.NoError(t, ipam.InstallAllocator(pool, alloc))
		return newInstanceCleanerAdapter(&Daemon{externalIPAM: ipam, vpcService: f.vpc}), alloc
	}
	stopping := func(eniID string) *vm.VM {
		return &vm.VM{ID: "i-auto", AccountID: testAccountID, ENIId: eniID, PublicIP: "203.0.113.20", PublicIPPool: pool}
	}

	t.Run("nothing to release", func(t *testing.T) {
		a, alloc := newCleaner(t, nil)
		released, err := a.ReleaseAutoAssignedPublicIP(&vm.VM{ID: "i-auto", PublicIPPool: pool})
		require.NoError(t, err)
		assert.False(t, released)

		inst := stopping(f.eniID)
		inst.PublicIPAllocID = "eipalloc-launch"
		released, err = a.ReleaseAutoAssignedPublicIP(inst)
		require.NoError(t, err)
		assert.False(t, released, "an address allocated through the EIP service is the customer's")
		assert.Empty(t, alloc.releases)
	})

	t.Run("address held by an elastic IP stays", func(t *testing.T) {
		a, alloc := newCleaner(t, nil)
		released, err := a.ReleaseAutoAssignedPublicIP(stopping("eni-eip-owned"))
		require.NoError(t, err)
		assert.False(t, released)
		assert.Empty(t, alloc.releases)
	})

	t.Run("EIP lookup failure keeps the address", func(t *testing.T) {
		failing := newFailingKV(eipKV, "Keys")
		f.vpc.SetExternalIPAM(nil, failing)
		t.Cleanup(func() { f.vpc.SetExternalIPAM(nil, eipKV) })

		a, alloc := newCleaner(t, nil)
		released, err := a.ReleaseAutoAssignedPublicIP(stopping(f.eniID))
		require.ErrorIs(t, err, errInjected)
		assert.False(t, released)
		assert.Empty(t, alloc.releases, "an address not proven ours is never released")
	})

	t.Run("release failure", func(t *testing.T) {
		a, _ := newCleaner(t, errInjected)
		released, err := a.ReleaseAutoAssignedPublicIP(stopping(f.eniID))
		require.ErrorIs(t, err, errInjected)
		assert.False(t, released)
	})

	t.Run("missing ENI only warns", func(t *testing.T) {
		a, alloc := newCleaner(t, nil)
		released, err := a.ReleaseAutoAssignedPublicIP(stopping("eni-vanished"))
		require.NoError(t, err)
		assert.True(t, released)
		assert.Len(t, alloc.releases, 1)
	})

	t.Run("success clears the ENI public IP", func(t *testing.T) {
		require.NoError(t, f.vpc.UpdateENIPublicIP(testAccountID, f.eniID, "203.0.113.20", pool))
		a, alloc := newCleaner(t, nil)

		released, err := a.ReleaseAutoAssignedPublicIP(stopping(f.eniID))
		require.NoError(t, err)
		assert.True(t, released)
		assert.Equal(t, []netip.Addr{netip.MustParseAddr("203.0.113.20")}, alloc.releases)

		rec, err := f.vpc.GetENIRecord(testAccountID, f.eniID)
		require.NoError(t, err)
		assert.Empty(t, rec.PublicIpAddress, "a stale address would let the reconciler restore the NAT rule")
		assert.Empty(t, rec.PublicIpPool)
	})

	t.Run("primary ENI delete failure is returned", func(t *testing.T) {
		nc, err := nats.Connect(f.url)
		require.NoError(t, err)
		vpcSvc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), &config.Config{}, nc)
		require.NoError(t, err)
		nc.Close()

		a := newInstanceCleanerAdapter(&Daemon{vpcService: vpcSvc})
		err = a.DetachAndDeleteENI(&vm.VM{ID: "i-auto", AccountID: testAccountID, ENIId: f.eniID})
		require.Error(t, err, "a primary ENI that could not be deleted must fail terminate")
	})

	t.Run("spot close failure is returned", func(t *testing.T) {
		nc, err := nats.Connect(f.url)
		require.NoError(t, err)
		spotSvc, err := ec2spotinstance.NewSpotInstanceServiceImplWithNATS(t.Context(), &config.Config{}, nc)
		require.NoError(t, err)

		a := newInstanceCleanerAdapter(&Daemon{spotInstanceService: spotSvc})
		require.NoError(t, a.RemoveFromSpotRequest(&vm.VM{ID: "i-auto", AccountID: testAccountID}),
			"an instance no spot request names is a no-op")

		nc.Close()
		assert.ErrorContains(t, a.RemoveFromSpotRequest(&vm.VM{ID: "i-auto", AccountID: testAccountID}),
			awserrors.ErrorServerInternal)
	})
}

// failingEIPDisassociator fails every disassociation.
type failingEIPDisassociator struct {
	ec2eip.EIPService

	calls int
}

func (s *failingEIPDisassociator) DisassociateByENI(context.Context, string, string) (bool, error) {
	s.calls++
	return false, errInjected
}

func TestInstanceCleaner_DisassociateEIPFailureDoesNotFailTerminate(t *testing.T) {
	f := newENIHotPlugFixture(t)
	f.vmInst.AccountID = testAccountID
	f.vmInst.ENIId = f.eniID
	eip := &failingEIPDisassociator{}
	f.daemon.eipService = eip

	require.NoError(t, newInstanceCleanerAdapter(f.daemon).DetachAndDeleteENI(f.vmInst))
	assert.Equal(t, 1, eip.calls)
}

func TestInstanceCleaner_RemoveFromPlacementGroup(t *testing.T) {
	inGroup := &vm.VM{ID: "i-pg", AccountID: testAccountID, PlacementGroupName: "pg-spread"}

	require.NoError(t, newInstanceCleanerAdapter(&Daemon{}).RemoveFromPlacementGroup(inGroup),
		"no placement group service is a no-op")

	a := newInstanceCleanerAdapter(&Daemon{placementGroupService: &ec2placementgroup.PlacementGroupServiceImpl{}})
	require.NoError(t, a.RemoveFromPlacementGroup(&vm.VM{ID: "i-pg"}), "an ungrouped instance is a no-op")

	assert.EqualError(t, a.RemoveFromPlacementGroup(inGroup), awserrors.ErrorMissingParameter,
		"a group member with no node recorded cannot be removed")
}

func TestInstanceCleaner_DetachAndDeleteENI_NoVPCServiceIsNoOp(t *testing.T) {
	a := newInstanceCleanerAdapter(&Daemon{})
	require.NoError(t, a.DetachAndDeleteENI(&vm.VM{ID: "i-novpc", ENIId: "eni-novpc"}))
}
