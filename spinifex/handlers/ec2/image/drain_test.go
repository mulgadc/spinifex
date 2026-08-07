package handlers_ec2_image

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/handlers/ec2/volumestate"
	"github.com/mulgadc/spinifex/spinifex/migrate/ebsmetadatabackfill"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const drainImageInstanceID = "i-image-drain-host"

// setupDrainImageService mirrors handlers/ec2/snapshot's setupDrainService: a
// reachable (but rejecting) Predastore host so a drained volume falls through
// into the real snapshot path and fails fast, a data dir with no local drain
// socket, and a NATS connection to route the drain to the hosting node.
func setupDrainImageService(t *testing.T) (*ImageServiceImpl, *objectstore.MemoryObjectStore, *nats.Conn) {
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
			Bucket: testBucket,
		},
	}
	svc := NewImageServiceImplWithConfig(cfg, store, nc)
	svc.metadata.SetLegacyVolumeFallback(ebsmetadatabackfill.LegacyVolumeFromLegacyState)
	return svc, store, nc
}

func seedImageVolumeAttachment(t *testing.T, store *objectstore.MemoryObjectStore, volumeID, state, instanceID string) {
	t.Helper()
	require.NoError(t, volumestate.Write(context.Background(), store, testBucket, volumeID, volumestate.Record{
		State:            state,
		AttachedInstance: instanceID,
	}))
}

func drainImageResponder(t *testing.T, nc *nats.Conn, instanceID string, reply []byte) chan *nats.Msg {
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

func drainedImageAck(t *testing.T, volumeID string) []byte {
	t.Helper()
	data, err := json.Marshal(types.DrainVolumeResponse{VolumeID: volumeID, Status: types.DrainVolumeStatusDrained})
	require.NoError(t, err)
	return data
}

func awaitImageDrainCommand(t *testing.T, got chan *nats.Msg) types.EC2InstanceCommand {
	t.Helper()
	select {
	case msg := <-got:
		var command types.EC2InstanceCommand
		require.NoError(t, json.Unmarshal(msg.Data, &command))
		return command
	case <-time.After(2 * time.Second):
		t.Fatal("the node hosting the volume never received a drain command")
		return types.EC2InstanceCommand{}
	}
}

// CreateImageFromInstance's IsRunning path must drain the volume's hosting node
// before it reads the live checkpoint — the same guarantee ec2.CreateSnapshot
// has. Without this, a checkpoint predating a guest-triggered GPT rewrite
// (e.g. growpart on first boot) can be captured silently.
func TestSnapshotRunningVolume_DrainsAttachedVolume(t *testing.T) {
	svc, store, nc := setupDrainImageService(t)
	createTestVolumeConfig(t, store, "vol-image-drain", 10)
	seedImageVolumeAttachment(t, store, "vol-image-drain", "in-use", drainImageInstanceID)
	got := drainImageResponder(t, nc, drainImageInstanceID, drainedImageAck(t, "vol-image-drain"))

	err := svc.snapshotRunningVolume("vol-image-drain", "snap-image-drain", testAccountID)
	// The rejecting Predastore stub fails the snapshot itself; what matters
	// here is that the drain reached the hosting node first.
	require.Error(t, err)

	command := awaitImageDrainCommand(t, got)
	assert.True(t, command.Attributes.DrainVolume)
	assert.Equal(t, drainImageInstanceID, command.ID)
	require.NotNil(t, command.DrainVolumeData)
	assert.Equal(t, "vol-image-drain", command.DrainVolumeData.VolumeID)
}

// An attached volume whose host reports the drain failed must fail
// CreateImage rather than register an AMI built from a stale checkpoint.
func TestSnapshotRunningVolume_UndrainableAttachedVolumeFails(t *testing.T) {
	svc, store, nc := setupDrainImageService(t)
	createTestVolumeConfig(t, store, "vol-image-nodrain", 10)
	seedImageVolumeAttachment(t, store, "vol-image-nodrain", "in-use", drainImageInstanceID)
	drainImageResponder(t, nc, drainImageInstanceID,
		[]byte(`{"Code":"`+awserrors.ErrorServerInternal+`","Message":"drain failed"}`))

	err := svc.snapshotRunningVolume("vol-image-nodrain", "snap-image-nodrain", testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// An unattached (stopped-instance) volume takes the no-drain path: the
// checkpoint Close() left behind is already current.
func TestSnapshotRunningVolume_AvailableSkipsDrain(t *testing.T) {
	svc, store, _ := setupDrainImageService(t)
	createTestVolumeConfig(t, store, "vol-image-available", 10)
	seedImageVolumeAttachment(t, store, "vol-image-available", "available", "")
	svc.natsConn = nil

	// No responder exists and the socket is never dialled: proceeding straight
	// to the (rejecting) snapshot path proves the drain was skipped, not stuck.
	err := svc.snapshotRunningVolume("vol-image-available", "snap-image-available", testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}
