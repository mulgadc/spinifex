package viperblockd

// Tests proving a mounted volume's snapshot handlers reuse the live VB via
// findMountedVolume rather than open a second engine, pinned on the same
// "viperblock volume opened" log record a production alarm would watch.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// volumeOpenCount counts "viperblock volume opened" records in logs
// (captureLogs' output) naming volumeID via a "volume=<id>" token, so a
// volume ID that is a prefix of another one never falsely matches.
func volumeOpenCount(logs, volumeID string) int {
	count := 0
	for line := range strings.SplitSeq(logs, "\n") {
		if !strings.Contains(line, `msg="viperblock volume opened"`) {
			continue
		}
		if slices.Contains(strings.Fields(line), "volume="+volumeID) {
			count++
		}
	}
	return count
}

// fastFailingS3Host starts a local server answering every request with 400
// Bad Request, for use as cfg.S3Host. A plain 4xx isn't retried by the AWS
// SDK's default retryer, so a forced engine construction fails immediately.
func fastFailingS3Host(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// chunkUploaderFrames counts live stack frames belonging to viperblock's chunk
// uploader. A running uploader contributes a fixed number of them, so a
// before/after comparison detects one that was never stopped.
func chunkUploaderFrames(t *testing.T) int {
	t.Helper()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "viperblock.(*VB).StartChunkUploader")
}

// TestOpenVolumeVB_FailedOpenLeavesNoChunkUploader pins the release half of
// the same invariant: viperblock.New starts an uploader before the backend is
// touched, so an open that fails afterwards must not abandon it.
func TestOpenVolumeVB_FailedOpenLeavesNoChunkUploader(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = fastFailingS3Host(t)

	before := chunkUploaderFrames(t)

	vb, err := openVolumeVB(cfg, "vol-invariantleak001")
	require.Error(t, err, "the fast-failing backend must fail the open, or this test proves nothing")
	require.Nil(t, vb)

	assert.Equal(t, before, chunkUploaderFrames(t),
		"a failed open must stop the uploader viperblock.New started; the caller gets no handle to stop it with")
}

// TestProviderHandlers_SnapshotPath_MountedVolumeDoesNotOpenEngine drives
// handleCreateSnapshot and handleCopySnapshot against a live mounted VB and
// asserts zero "viperblock volume opened" records name that volume.
func TestProviderHandlers_SnapshotPath_MountedVolumeDoesNotOpenEngine(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-invariantmount01"
	const srcSnapshotID = "snap-invariantsrc001"
	const dstSnapshotID = "snap-invariantdst001"

	vb := createTestVBWithState(t, volumeName)
	t.Cleanup(func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	})
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{Name: volumeName, VB: vb})

	logs := captureLogs(t)

	wantCompletionSubject, err := ebsprovider.SnapshotCompletionSubject(srcSnapshotID)
	require.NoError(t, err)
	completionSub, err := nc.SubscribeSync(wantCompletionSubject)
	require.NoError(t, err)
	require.NoError(t, nc.Flush())

	createBody := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"snapshot_id":    srcSnapshotID,
	})
	requestProvider(t, nc, ebsprovider.SnapshotCreateSubjectPrefix+volumeName, createBody)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completionMsg, err := completionSub.NextMsgWithContext(ctx)
	require.NoError(t, err)
	var completed ebsprovider.CreateSnapshotResponse
	require.NoError(t, json.Unmarshal(completionMsg.Data, &completed))
	require.Nil(t, completed.Error, "snapshot create on a mounted volume must succeed with no S3 backend involved")

	copyBody := marshalRequest(t, map[string]any{
		"schema_version":          ebsprovider.SchemaVersion,
		"source_snapshot_id":      srcSnapshotID,
		"destination_snapshot_id": dstSnapshotID,
		"volume_id":               volumeName,
	})
	copyMsg := requestProvider(t, nc, ebsprovider.CopySnapshotSubject, copyBody)
	var copyResp ebsprovider.CopySnapshotResponse
	require.NoError(t, json.Unmarshal(copyMsg.Data, &copyResp))
	require.Nil(t, copyResp.Error, "snapshot copy on a mounted volume must succeed with no S3 backend involved")

	assert.Equal(t, 0, volumeOpenCount(logs.String(), volumeName),
		"snapshot create+copy on an already-mounted volume must reuse the live VB, never open a second engine")
}

// TestProviderHandlers_CreateSnapshot_UnmountedVolumeOpensEngine is the
// contrast case: with no mounted VB, handleCreateSnapshot must construct its
// own engine (one log record), even though the fast-failing backend then fails it.
func TestProviderHandlers_CreateSnapshot_UnmountedVolumeOpensEngine(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = fastFailingS3Host(t)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-invariantopen001"
	const snapshotID = "snap-invariantopen01"

	logs := captureLogs(t)

	wantCompletionSubject, err := ebsprovider.SnapshotCompletionSubject(snapshotID)
	require.NoError(t, err)
	completionSub, err := nc.SubscribeSync(wantCompletionSubject)
	require.NoError(t, err)
	require.NoError(t, nc.Flush())

	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"snapshot_id":    snapshotID,
	})
	requestProvider(t, nc, ebsprovider.SnapshotCreateSubjectPrefix+volumeName, body)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completionMsg, err := completionSub.NextMsgWithContext(ctx)
	require.NoError(t, err)
	var completed ebsprovider.CreateSnapshotResponse
	require.NoError(t, json.Unmarshal(completionMsg.Data, &completed))
	require.NotNil(t, completed.Error,
		"the fast-failing backend must fail the snapshot, proving this really took the construct-a-new-engine path rather than silently reusing something")

	assert.Equal(t, 1, volumeOpenCount(logs.String(), volumeName),
		"snapshot create on an unmounted volume must construct exactly one engine")
}

// TestProviderHandlers_SnapshotPath_DifferentNodeConfigDoesNotOpenEngine puts
// the mount on cfgOwner, reachable only by owner subject, and makes cfgOther
// the queue group's sole member. Zero opens proves the owner subject won.
func TestProviderHandlers_SnapshotPath_DifferentNodeConfigDoesNotOpenEngine(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	cfgOther := setupTestConfig(t, natsURL)
	cfgOther.S3Host = fastFailingS3Host(t)
	startProviderSubjects(t, cfgOther, natsURL)

	cfgOwner := setupTestConfig(t, natsURL)
	ncOwner, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(ncOwner.Close)

	const volumeName = "vol-diffnodeowner01"
	const srcSnapshotID = "snap-diffnodesrc001"
	const dstSnapshotID = "snap-diffnodedst001"

	vb := createTestVBWithState(t, volumeName)
	t.Cleanup(func() {
		vb.StopChunkUploader()
		vb.StopWALSyncer()
	})

	ownerSubs := subscribeOwnerSubjects(context.Background(), cfgOwner, ncOwner, volumeName)
	require.Len(t, ownerSubs, 4, "all four owner verbs must subscribe cleanly")
	require.NoError(t, ncOwner.Flush())

	cfgOwner.MountedVolumes = append(cfgOwner.MountedVolumes, MountedVolume{Name: volumeName, VB: vb, OwnerSubs: ownerSubs})

	logs := captureLogs(t)

	// The client connects through cfgOther's connection: with no owner
	// routing this would have no bearing on which node answers (NATS
	// subjects are server-wide, not connection-scoped), so this alone does
	// not bias the result toward the fix passing.
	ncClient, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(ncClient.Close)
	provider := ebsprovider.NewNATSProvider(ncClient, 5*time.Second)

	snap, err := provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{
		Versioned:  ebsprovider.NewVersioned(),
		SnapshotID: srcSnapshotID,
		VolumeID:   volumeName,
	})
	require.NoError(t, err, "the mounting node's owner subject must serve this, not the other node's fast-failing queue handler")
	require.NotNil(t, snap)

	_, err = provider.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned:             ebsprovider.NewVersioned(),
		SourceSnapshotID:      srcSnapshotID,
		DestinationSnapshotID: dstSnapshotID,
		VolumeID:              volumeName,
	})
	require.NoError(t, err)

	assert.Equal(t, 0, volumeOpenCount(logs.String(), volumeName),
		"a snapshot request for a volume mounted by a different node's config must never open a second engine")
}

// TestProviderHandlers_CopySnapshot_UnmountedVolumeOpensEngine is
// handleCopySnapshot's counterpart: with no mounted VB, copySnapshotOnVolume
// falls through to openVolumeVB, which calls viperblock.New() exactly once.
func TestProviderHandlers_CopySnapshot_UnmountedVolumeOpensEngine(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.S3Host = fastFailingS3Host(t)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-invariantcopy001"

	logs := captureLogs(t)

	body := marshalRequest(t, map[string]any{
		"schema_version":          ebsprovider.SchemaVersion,
		"source_snapshot_id":      "snap-invariantcopysrc",
		"destination_snapshot_id": "snap-invariantcopydst",
		"volume_id":               volumeName,
	})
	msg := requestProvider(t, nc, ebsprovider.CopySnapshotSubject, body)
	var resp ebsprovider.CopySnapshotResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.NotNil(t, resp.Error,
		"the fast-failing backend must fail the copy, proving this really took the construct-a-new-engine path rather than silently reusing something")

	assert.Equal(t, 1, volumeOpenCount(logs.String(), volumeName),
		"snapshot copy on an unmounted volume must construct exactly one engine")
}
