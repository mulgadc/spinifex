package viperblock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit/deadpid"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProviderHandlers_RepliesInRequestVersion holds every subject to the
// rule a mixed-version rollout depends on: a caller at version N must get a
// reply at N, success or error, or it may not be able to parse it.
func TestProviderHandlers_RepliesInRequestVersion(t *testing.T) {
	prevProbe := ownerProbeTimeout
	ownerProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { ownerProbeTimeout = prevProbe })

	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	for _, version := range ebsprovider.SupportedSchemaVersions() {
		for _, tc := range providerSubjectCases() {
			t.Run(fmt.Sprintf("v%d/%s", version, tc.name), func(t *testing.T) {
				body := map[string]any{"schema_version": version}
				maps.Copy(body, tc.body)
				msg := requestProvider(t, nc, tc.subject, marshalRequest(t, body))

				var resp versionedErrorResponse
				require.NoError(t, json.Unmarshal(msg.Data, &resp))
				assert.Equal(t, version, resp.SchemaVersion)
			})
		}
	}
}

// TestProviderHandlers_UnpublishNoSealNeverSeals is the reason no_seal exists:
// a node whose copy is behind a newer owner's must drop it, and a seal would
// write that stale copy over the owner's block map.
func TestProviderHandlers_UnpublishNoSealNeverSeals(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-unpubnoseal01"
	sealed := false
	cfg := fencedConfig(t, natsURL, "test-node", volumeName)
	cfg.sealVolume = func(context.Context, string) error { sealed = true; return nil }
	nc := startProviderSubjects(t, cfg, natsURL)

	subject, err := ebsprovider.UnpublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.NoSealSchemaVersion,
		"volume_id":      volumeName,
		"node_id":        cfg.NodeName,
		"no_seal":        true,
	})
	msg := requestProvider(t, nc, subject, body)

	var resp ebsprovider.UnpublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.Nil(t, resp.Error)
	assert.True(t, resp.TornDown, "the volume was mounted, so the export was torn down")
	assert.False(t, sealed, "a no_seal unpublish must never seal")
	_, mounted := findMountedVolume(cfg, volumeName)
	assert.False(t, mounted, "the entry must go with the export")
}

// TestProviderHandlers_UnpublishNoSealBelowV3Refused covers the other half: a
// caller below version 3 cannot ask for no_seal, so the flag is refused rather
// than ignored, and the export is left alone.
func TestProviderHandlers_UnpublishNoSealBelowV3Refused(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)

	const volumeName = "vol-unpubnosealv2"
	sealed := false
	cfg := fencedConfig(t, natsURL, "test-node", volumeName)
	cfg.sealVolume = func(context.Context, string) error { sealed = true; return nil }
	nc := startProviderSubjects(t, cfg, natsURL)

	subject, err := ebsprovider.UnpublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.DescriptorSchemaVersion,
		"volume_id":      volumeName,
		"node_id":        cfg.NodeName,
		"no_seal":        true,
	})
	msg := requestProvider(t, nc, subject, body)

	var resp ebsprovider.UnpublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, resp.Error.Code)
	assert.False(t, sealed)
	_, mounted := findMountedVolume(cfg, volumeName)
	assert.True(t, mounted, "a refused unpublish must leave the export running")
}

// TestProviderHandlers_PublishVolume_DeadEntryIsNotHandedBack pins the removed
// early return: an entry whose nbdkit has died must be dropped and remounted,
// never answered with a URI nothing serves.
func TestProviderHandlers_PublishVolume_DeadEntryIsNotHandedBack(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-pubdeadentry1"
	const deadURI = "nbd://127.0.0.1:1"
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
		Name: volumeName, VB: createTestVBWithState(t, volumeName), PID: deadpid.PID, NBDURI: deadURI,
	})

	subject, err := ebsprovider.PublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"node_id":        cfg.NodeName,
	})
	msg := requestProvider(t, nc, subject, body)

	var resp ebsprovider.PublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	if resp.Published != nil {
		assert.NotEqual(t, ebsprovider.NewNBDExport(deadURI), resp.Published.Export, "the dead entry's URI was handed back")
	}
	if mv, ok := findMountedVolume(cfg, volumeName); ok {
		assert.NotEqual(t, deadURI, mv.NBDURI, "the dead entry must not survive the republish")
	}
}

// TestPublishError_Codes pins the code each mount failure reaches a caller as.
// held_elsewhere is gated on the request version because v1.23.0 callers only
// know volume_in_use, and a mode mismatch is never held_elsewhere.
func TestPublishError_Codes(t *testing.T) {
	leaseHeld := fmt.Errorf("acquire: %w", errVolumeLeaseHeld)
	cases := []struct {
		name    string
		version uint16
		err     error
		want    ebsprovider.ErrorCode
	}{
		{"absent volume", ebsprovider.SchemaVersion, fmt.Errorf("load: %w", viperblock.ErrStateNotFound), ebsprovider.ErrorCodeNotFound},
		{"lease held at v1", ebsprovider.LegacySchemaVersion, leaseHeld, ebsprovider.ErrorCodeVolumeInUse},
		{"lease held at v2", ebsprovider.DescriptorSchemaVersion, leaseHeld, ebsprovider.ErrorCodeVolumeInUse},
		{"lease held at v3", ebsprovider.NoSealSchemaVersion, leaseHeld, ebsprovider.ErrorCodeVolumeHeldElsewhere},
		{"mode mismatch at v3", ebsprovider.NoSealSchemaVersion, fmt.Errorf("mounted: %w", errReadOnlyMismatch), ebsprovider.ErrorCodeVolumeInUse},
		{"backing store not ready", ebsprovider.SchemaVersion, fmt.Errorf("load: %w", viperblock.ErrStateBackendUnavailable), ebsprovider.ErrorCodeUnavailable},
		{"lease store not ready", ebsprovider.SchemaVersion, fmt.Errorf("open: %w", ErrLeaseStoreUnavailable), ebsprovider.ErrorCodeUnavailable},
		{"anything else", ebsprovider.SchemaVersion, errors.New("nbdkit exited"), ebsprovider.ErrorCodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := publishError(tc.version, "vol-publisherror01", tc.err)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, got.Code)
		})
	}
	assert.True(t, ebsprovider.ErrorCodeUnavailable.Retryable(), "a backing store that is not ready must read as retry-later")
}

// TestProviderHandlers_CreateSnapshot_QueueRefusesWhileLeaseHeldElsewhere is
// what makes a snapshot from a non-owner safe: whichever node answers the queue
// subject must not snapshot a checkpoint the holder may still be writing.
func TestProviderHandlers_CreateSnapshot_QueueRefusesWhileLeaseHeldElsewhere(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-snapqueueheld1"
	const snapshotID = "snap-queueheld00001"
	holder := newTestLeases(t, natsURL, "node-other")
	_, err := holder.acquire(t.Context(), volumeName)
	require.NoError(t, err)

	completionSubject, err := ebsprovider.SnapshotCompletionSubject(snapshotID)
	require.NoError(t, err)
	completions := make(chan *nats.Msg, 1)
	sub, err := nc.ChanSubscribe(completionSubject, completions)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"snapshot_id":    snapshotID,
	})
	accepted := requestProvider(t, nc, ebsprovider.SnapshotCreateSubjectPrefix+volumeName, body)
	var acceptance ebsprovider.CreateSnapshotResponse
	require.NoError(t, json.Unmarshal(accepted.Data, &acceptance))
	require.Nil(t, acceptance.Error)

	select {
	case msg := <-completions:
		var completion ebsprovider.CreateSnapshotResponse
		require.NoError(t, json.Unmarshal(msg.Data, &completion))
		require.NotNil(t, completion.Error, "a snapshot under another node's lease must be refused")
		assert.Equal(t, ebsprovider.ErrorCodeUnavailable, completion.Error.Code)
		assert.Nil(t, completion.Snapshot)
	case <-time.After(10 * time.Second):
		t.Fatal("no completion published")
	}
}
