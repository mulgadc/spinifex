package handlers_ec2_snapshot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/handlers/ec2/volumestate"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const drainInstanceID = "i-drain-host"

// setupDrainService returns a snapshot service configured like a daemon that
// answers ec2.CreateSnapshot: a reachable Predastore host (so drains are not
// skipped as metadata-only), a data dir with no drain socket in it, and a NATS
// connection. The Predastore stub rejects everything, so a snapshot that gets
// past the drain fails fast rather than hanging on a dead address.
func setupDrainService(t *testing.T) (*SnapshotServiceImpl, *objectstore.MemoryObjectStore, *nats.Conn) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	_, nc := testutil.StartTestNATS(t)
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{
		DataDir: testutil.SocketTempDir(t),
		Predastore: config.PredastoreConfig{
			Host:   srv.URL,
			Bucket: "test-bucket",
		},
	}
	return NewSnapshotServiceImplWithStore(cfg, store, nc), store, nc
}

// seedVolumeAttachment writes the control-plane state.json that decides whether
// a snapshot must drain.
func seedVolumeAttachment(t *testing.T, store *objectstore.MemoryObjectStore, volumeID, state, instanceID string) {
	t.Helper()
	require.NoError(t, volumestate.Write(context.Background(), store, "test-bucket", volumeID, volumestate.Record{
		State:            state,
		AttachedInstance: instanceID,
	}))
}

// drainResponder answers the per-instance command subject the way the node
// hosting the instance does, and hands each request it received to the test.
func drainResponder(t *testing.T, nc *nats.Conn, instanceID string, reply []byte) chan *nats.Msg {
	t.Helper()

	got := make(chan *nats.Msg, 4)
	sub, err := nc.Subscribe("ec2.cmd."+instanceID, func(msg *nats.Msg) {
		got <- msg
		_ = msg.Respond(reply)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	return got
}

// drainedAck is the reply of a node whose drain reached S3.
func drainedAck(t *testing.T, volumeID string) []byte {
	t.Helper()
	data, err := json.Marshal(types.DrainVolumeResponse{VolumeID: volumeID, Status: types.DrainVolumeStatusDrained})
	require.NoError(t, err)
	return data
}

// requireReturnsWithin fails the test if fn has not returned successfully by
// limit, so a drain that blocks on a socket it should not have dialed reports
// as a failure rather than as a slow test.
func requireReturnsWithin(t *testing.T, limit time.Duration, fn func() error) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- fn() }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(limit):
		t.Fatalf("drain did not return within %s", limit)
	}
}

// awaitDrainCommand returns the command the hosting node received, failing the
// test if none arrives.
func awaitDrainCommand(t *testing.T, got chan *nats.Msg) types.EC2InstanceCommand {
	t.Helper()
	select {
	case msg := <-got:
		var command types.EC2InstanceCommand
		require.NoError(t, json.Unmarshal(msg.Data, &command))
		assert.Equal(t, testAccountID, msg.Header.Get(utils.AccountIDHeader))
		return command
	case <-time.After(2 * time.Second):
		t.Fatal("the node hosting the volume never received a drain command")
		return types.EC2InstanceCommand{}
	}
}

// An attached volume is drained on the node hosting its instance, which is the
// only node with the socket — the node answering CreateSnapshot usually is not.
func TestDrainVolume_AttachedRoutesToHostingNode(t *testing.T) {
	svc, store, nc := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-attached", "in-use", drainInstanceID)
	got := drainResponder(t, nc, drainInstanceID, drainedAck(t, "vol-attached"))

	require.NoError(t, svc.drainVolume(context.Background(), "vol-attached", "", "", testAccountID))

	command := awaitDrainCommand(t, got)
	assert.True(t, command.Attributes.DrainVolume)
	assert.Equal(t, drainInstanceID, command.ID)
	require.NotNil(t, command.DrainVolumeData)
	assert.Equal(t, "vol-attached", command.DrainVolumeData.VolumeID)
}

// No responder on the command subject means no node is running the instance,
// which is what a stopped instance looks like: stop tears down both the plugin
// and the subscription but deliberately leaves a boot volume attached. Failing
// here would make every stopped instance's root volume unsnapshottable.
func TestDrainVolume_AttachedToStoppedInstanceTakesStoppedPath(t *testing.T) {
	svc, store, _ := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-no-host", "in-use", drainInstanceID)

	require.NoError(t, svc.drainVolume(context.Background(), "vol-no-host", "", "", testAccountID))
}

// A host that still holds the instance but reports it not running has nothing
// to flush, so the sealed checkpoint stands. This is the host-drain stop, which
// unmounts the volumes but keeps the subscription.
func TestDrainVolume_NotRunningAckTakesStoppedPath(t *testing.T) {
	svc, store, nc := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-not-running", "in-use", drainInstanceID)
	ack, err := json.Marshal(types.DrainVolumeResponse{
		VolumeID: "vol-not-running", Status: types.DrainVolumeStatusNotRunning,
	})
	require.NoError(t, err)
	got := drainResponder(t, nc, drainInstanceID, ack)

	require.NoError(t, svc.drainVolume(context.Background(), "vol-not-running", "", "", testAccountID))
	assert.True(t, awaitDrainCommand(t, got).Attributes.DrainVolume)
}

// A hosting node that reports the drain failed (wedged plugin, socket gone
// mid-restart) fails the snapshot rather than falling through.
func TestDrainVolume_AttachedWithFailedAckFails(t *testing.T) {
	svc, store, nc := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-drain-err", "in-use", drainInstanceID)
	drainResponder(t, nc, drainInstanceID,
		[]byte(`{"Code":"`+awserrors.ErrorServerInternal+`","Message":"drain failed"}`))

	err := svc.drainVolume(context.Background(), "vol-drain-err", "", "", testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drain failed")
}

// An ack that is not the drained ack is not evidence the writes reached S3.
func TestDrainVolume_AttachedWithUnexpectedAckFails(t *testing.T) {
	svc, store, nc := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-odd-ack", "in-use", drainInstanceID)
	drainResponder(t, nc, drainInstanceID, []byte(`{}`))

	err := svc.drainVolume(context.Background(), "vol-odd-ack", "", "", testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected ack")
}

// Nothing writes to an unattached volume, so the checkpoint its Close() left
// behind is current and no drain is attempted — not even locally. Dialing the
// socket is not a probe: it makes the plugin run a full flush, so an available
// volume must be decided from the record without touching it.
func TestDrainVolume_AvailableDoesNotDialLocalSocket(t *testing.T) {
	svc, store, _ := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-available", "available", "")

	// A socket that never answers: a dial would park here until the read
	// deadline, so returning promptly is what proves none was made.
	testutil.StartBlockingDrainSocket(t, svc.config.DataDir, "vol-available", "OK\n")

	// A nil connection proves no command was issued: routing one would error.
	svc.natsConn = nil

	requireReturnsWithin(t, 2*time.Second, func() error {
		return svc.drainVolume(context.Background(), "vol-available", "", "", testAccountID)
	})
}

// A volume recorded as in-use but with no instance (drifted state) has no node
// to address, so it takes the stopped path rather than failing every snapshot.
func TestDrainVolume_InUseWithoutInstanceTakesStoppedPath(t *testing.T) {
	svc, store, _ := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-no-instance", "in-use", "")
	svc.natsConn = nil

	require.NoError(t, svc.drainVolume(context.Background(), "vol-no-instance", "", "", testAccountID))
}

// When the volume is served by this node the socket is local, so the drain
// short-circuits without a hop. This is every single-node deployment.
func TestDrainVolume_LocalSocketShortCircuits(t *testing.T) {
	svc, store, _ := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-local", "in-use", drainInstanceID)
	testutil.StartDrainSocket(t, svc.config.DataDir, "vol-local", "OK\n")

	// A nil connection proves the local ack was enough: no responder exists for
	// drainInstanceID, so a routed command would fail.
	svc.natsConn = nil

	require.NoError(t, svc.drainVolume(context.Background(), "vol-local", "", "", testAccountID))
}

// A socket that answers without acking means this node does serve the volume
// and the flush failed. Routing that onward would come back to this same node
// and re-run the drain that has already failed, so it fails here instead.
func TestDrainVolume_LocalSocketErrorAckFailsWithoutRouting(t *testing.T) {
	svc, store, nc := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-local-err", "in-use", drainInstanceID)
	testutil.StartDrainSocket(t, svc.config.DataDir, "vol-local-err", "ERR\n")
	got := drainResponder(t, nc, drainInstanceID, drainedAck(t, "vol-local-err"))

	err := svc.drainVolume(context.Background(), "vol-local-err", "", "", testAccountID)
	require.Error(t, err)

	assert.Empty(t, got, "a local drain that failed must not be re-issued to the host node")
}

// state.json is the control-plane-owned attachment record; the copy inside
// config.json is rewritten by the live NBD plugin from its stale in-memory
// state, so a volume reading "available" there can still be attached.
func TestDrainVolume_StateJSONOverridesConfigJSON(t *testing.T) {
	svc, store, nc := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-stale-config", "in-use", drainInstanceID)
	got := drainResponder(t, nc, drainInstanceID, drainedAck(t, "vol-stale-config"))

	stale := viperblock.VolumeMetadata{State: "available"}
	require.NoError(t, svc.drainVolume(context.Background(), "vol-stale-config", stale.State, stale.AttachedInstance, testAccountID))

	assert.True(t, awaitDrainCommand(t, got).Attributes.DrainVolume)
}

// Volumes predating the state.json split have no state object, so the state
// embedded in config.json is all there is to decide from.
func TestDrainVolume_FallsBackToConfigJSONWhenNoStateObject(t *testing.T) {
	svc, _, nc := setupDrainService(t)
	got := drainResponder(t, nc, drainInstanceID, drainedAck(t, "vol-legacy"))

	legacy := viperblock.VolumeMetadata{State: "in-use", AttachedInstance: drainInstanceID}
	require.NoError(t, svc.drainVolume(context.Background(), "vol-legacy", legacy.State, legacy.AttachedInstance, testAccountID))

	assert.True(t, awaitDrainCommand(t, got).Attributes.DrainVolume)
}

// Without Predastore the snapshot is metadata-only and never reads a live
// checkpoint, so there is nothing for a drain to make current.
func TestDrainVolume_MetadataOnlySkipsDrain(t *testing.T) {
	svc, store, _ := setupDrainService(t)
	seedVolumeAttachment(t, store, "vol-meta-only", "in-use", drainInstanceID)
	svc.config.Predastore.Host = ""
	svc.natsConn = nil

	require.NoError(t, svc.drainVolume(context.Background(), "vol-meta-only", "", "", testAccountID))
}

// CreateSnapshot drains before it reads the checkpoint. The snapshot itself
// then fails against the rejecting Predastore stub; what is asserted here is
// that the drain was routed to the volume's host first.
func TestCreateSnapshot_DrainsAttachedVolume(t *testing.T) {
	svc, store, nc := setupDrainService(t)
	createTestVolume(t, store, "vol-snap-drain", 10)
	seedVolumeAttachment(t, store, "vol-snap-drain", "in-use", drainInstanceID)
	got := drainResponder(t, nc, drainInstanceID, drainedAck(t, "vol-snap-drain"))

	_, err := svc.CreateSnapshot(context.Background(),
		&ec2.CreateSnapshotInput{VolumeId: aws.String("vol-snap-drain")}, testAccountID)
	require.Error(t, err)

	command := awaitDrainCommand(t, got)
	assert.True(t, command.Attributes.DrainVolume)
	assert.Equal(t, "vol-snap-drain", command.DrainVolumeData.VolumeID)
}

// An attached volume whose host reports the drain failed fails the snapshot: a
// retryable error is strictly better than a successful snapshot of stale data.
func TestCreateSnapshot_UndrainableAttachedVolumeFails(t *testing.T) {
	svc, store, nc := setupDrainService(t)
	createTestVolume(t, store, "vol-snap-nodrain", 10)
	seedVolumeAttachment(t, store, "vol-snap-nodrain", "in-use", drainInstanceID)
	got := drainResponder(t, nc, drainInstanceID,
		[]byte(`{"Code":"`+awserrors.ErrorServerInternal+`","Message":"drain failed"}`))

	_, err := svc.CreateSnapshot(context.Background(),
		&ec2.CreateSnapshotInput{VolumeId: aws.String("vol-snap-nodrain")}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())

	assert.True(t, awaitDrainCommand(t, got).Attributes.DrainVolume)
}

// putProviderVolume seeds both the control-plane document CreateSnapshot's
// provider branch reads for attachment state, and the provider's own record,
// mirroring what CreateVolume's provider branch leaves behind.
func putProviderVolume(t *testing.T, svc *SnapshotServiceImpl, provider ebsprovider.EBSProvider, volumeID, state, instanceID string) {
	t.Helper()
	require.NoError(t, svc.metadata.PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: testAccountID, CapacityGiB: 4,
		State: state, AttachedInstance: instanceID, AvailabilityZone: "us-east-1a",
		ProviderHandle: "memory://volume/" + volumeID,
	}))
	_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
		CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: 4 * 1024 * 1024 * 1024},
		AvailabilityZone: "us-east-1a",
	})
	require.NoError(t, err)
}

// The provider branch used to skip the drain entirely, silently snapshotting a
// stale checkpoint of an attached, actively-written volume. It must drain the
// same way the legacy branch does.
func TestCreateSnapshot_Provider_DrainsAttachedVolume(t *testing.T) {
	svc, _, nc := setupDrainService(t)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc.SetEBSProvider(provider)
	putProviderVolume(t, svc, provider, "vol-provider-drain", "in-use", drainInstanceID)

	got := drainResponder(t, nc, drainInstanceID, drainedAck(t, "vol-provider-drain"))

	_, err := svc.CreateSnapshot(context.Background(),
		&ec2.CreateSnapshotInput{VolumeId: aws.String("vol-provider-drain")}, testAccountID)
	require.NoError(t, err)

	command := awaitDrainCommand(t, got)
	assert.True(t, command.Attributes.DrainVolume)
	assert.Equal(t, "vol-provider-drain", command.DrainVolumeData.VolumeID)
}

// A provider-branch snapshot whose drain fails must fail closed rather than
// return a snapshot of stale data: the provider must never be asked to
// snapshot, and no snapshot metadata may be left behind.
func TestCreateSnapshot_Provider_UndrainableAttachedVolumeFails(t *testing.T) {
	svc, _, nc := setupDrainService(t)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc.SetEBSProvider(provider)
	putProviderVolume(t, svc, provider, "vol-provider-nodrain", "in-use", drainInstanceID)

	got := drainResponder(t, nc, drainInstanceID,
		[]byte(`{"Code":"`+awserrors.ErrorServerInternal+`","Message":"drain failed"}`))

	_, err := svc.CreateSnapshot(context.Background(),
		&ec2.CreateSnapshotInput{VolumeId: aws.String("vol-provider-nodrain")}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.True(t, awaitDrainCommand(t, got).Attributes.DrainVolume)

	out, err := svc.DescribeSnapshots(context.Background(), &ec2.DescribeSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Snapshots, "a failed drain must not leave a snapshot behind")
}
