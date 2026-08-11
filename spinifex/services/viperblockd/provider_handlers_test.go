package viperblockd

// Tests for the ebs.provider.v1.* handlers registered by registerProviderSubjects.
// Engine-backed cases (CreateVolume idempotency, ExpandVolume in-use, CreateSnapshot)
// use a live mounted VB on the file backend (createTestVBWithState from
// viperblockd_handlers_test.go), matching this package's existing convention for
// tests that need a real viperblock engine but not real predastore. Success paths
// that require opening a DETACHED engine against S3 (CreateVolume of a fresh
// volume, GetVolume/ExpandVolume of an unmounted one) are out of scope here for
// the same reason handlers/ec2/volume's unit tests stop at validation: nothing
// past Backend.Init() can execute without a live backend, and this file must not
// add a network dependency to a unit test.

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionedErrorResponse decodes the common shape every ebs.provider.v1.*
// response carries: an embedded Versioned and an optional ProviderError.
// Every concrete response type marshals a superset of these fields, so
// decoding into this narrower struct works regardless of which handler
// answered.
type versionedErrorResponse struct {
	ebsprovider.Versioned

	Error *ebsprovider.ProviderError `json:"error,omitempty"`
}

// startProviderSubjects wires registerProviderSubjects onto a fresh NATS
// connection for cfg, without launching the rest of launchService (mount,
// unmount, sync, config, delete), which are unrelated to the ebs.provider.v1.*
// contract under test here.
func startProviderSubjects(t *testing.T, cfg *Config, natsURL string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	require.NoError(t, registerProviderSubjects(cfg, nc))
	return nc
}

func requestProvider(t *testing.T, nc *nats.Conn, subject string, payload []byte) *nats.Msg {
	t.Helper()
	msg, err := nc.Request(subject, payload, 5*time.Second)
	require.NoError(t, err)
	return msg
}

func marshalRequest(t *testing.T, req map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(req)
	require.NoError(t, err)
	return data
}

// providerSubjectCase is a subject plus a validly-shaped request body (minus
// schema_version) for the generic malformed-JSON / wrong-version table below.
type providerSubjectCase struct {
	name    string
	subject string
	body    map[string]any
}

func providerSubjectCases() []providerSubjectCase {
	return []providerSubjectCase{
		{"capabilities", ebsprovider.CapabilitiesSubject, map[string]any{}},
		{"create_volume", ebsprovider.CreateVolumeSubject, map[string]any{
			"volume_id":      "vol-testcreate0001",
			"capacity_range": map[string]any{"required_bytes": 1073741824},
		}},
		{"get_volume", ebsprovider.GetVolumeSubject, map[string]any{"volume_id": "vol-testget00001"}},
		{"expand_volume", ebsprovider.ExpandVolumeSubject, map[string]any{
			"volume_id":      "vol-testexpand001",
			"capacity_range": map[string]any{"required_bytes": 1073741824},
		}},
		{"delete_volume", ebsprovider.DeleteVolumeSubject, map[string]any{"volume_id": "vol-testdelete001"}},
		{"delete_snapshot", ebsprovider.DeleteSnapshotSubject, map[string]any{"snapshot_id": "snap-testdelete01"}},
		{"copy_snapshot", ebsprovider.CopySnapshotSubject, map[string]any{
			"source_snapshot_id":      "snap-testcopysrc01",
			"destination_snapshot_id": "snap-testcopydst01",
			"volume_id":               "vol-testcopysnap01",
		}},
		{"create_snapshot", ebsprovider.SnapshotCreateSubjectPrefix + "vol-testsnap0001", map[string]any{
			"volume_id":   "vol-testsnap0001",
			"snapshot_id": "snap-testsnap0001",
		}},
		// publish_volume/unpublish_volume are node-addressed; the subject
		// below must match setupTestConfig's NodeName ("test-node").
		{"publish_volume", "ebs.provider.v1.test-node.mount", map[string]any{
			"volume_id": "vol-testpublish001",
			"node_id":   "test-node",
		}},
		{"unpublish_volume", "ebs.provider.v1.test-node.unmount", map[string]any{
			"volume_id": "vol-testunpublish01",
			"node_id":   "test-node",
		}},
	}
}

func TestProviderHandlers_UnsupportedSchemaVersion(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	for _, tc := range providerSubjectCases() {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"schema_version": ebsprovider.SchemaVersion + 1}
			maps.Copy(body, tc.body)
			msg := requestProvider(t, nc, tc.subject, marshalRequest(t, body))

			var resp versionedErrorResponse
			require.NoError(t, json.Unmarshal(msg.Data, &resp))
			require.NotNil(t, resp.Error, "expected an error for unsupported schema version")
			assert.Equal(t, ebsprovider.ErrorCodeUnsupportedVersion, resp.Error.Code)
			// The zero-version guard: even an error response must carry a
			// valid SchemaVersion, or NATSProvider's responseError would
			// reject it before ever inspecting Error.
			assert.Equal(t, ebsprovider.SchemaVersion, resp.SchemaVersion)
		})
	}
}

func TestProviderHandlers_MalformedJSON(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	for _, tc := range providerSubjectCases() {
		t.Run(tc.name, func(t *testing.T) {
			msg := requestProvider(t, nc, tc.subject, []byte("{not valid json"))

			var resp versionedErrorResponse
			require.NoError(t, json.Unmarshal(msg.Data, &resp))
			require.NotNil(t, resp.Error, "expected an error for malformed JSON")
			assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, resp.Error.Code)
			assert.Equal(t, ebsprovider.SchemaVersion, resp.SchemaVersion)
		})
	}
}

// TestProviderHandlers_InvalidVolumeName covers the validVolumeName rejection
// path shared by every handler that takes a volume ID: empty, ".", "..", and
// path traversal must all come back invalid_argument, never reach the
// filesystem or a backend call.
func TestProviderHandlers_InvalidVolumeName(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	invalidNames := []string{"", ".", "..", "../..", "a/b", "/etc/passwd", "vol/../.."}

	for _, name := range invalidNames {
		t.Run("delete_volume/"+name, func(t *testing.T) {
			body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "volume_id": name})
			msg := requestProvider(t, nc, ebsprovider.DeleteVolumeSubject, body)

			var resp ebsprovider.DeleteVolumeResponse
			require.NoError(t, json.Unmarshal(msg.Data, &resp))
			require.NotNil(t, resp.Error)
			assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, resp.Error.Code)
		})

		t.Run("get_volume/"+name, func(t *testing.T) {
			body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "volume_id": name})
			msg := requestProvider(t, nc, ebsprovider.GetVolumeSubject, body)

			var resp ebsprovider.GetVolumeResponse
			require.NoError(t, json.Unmarshal(msg.Data, &resp))
			require.NotNil(t, resp.Error)
			assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, resp.Error.Code)
		})

		t.Run("delete_snapshot/"+name, func(t *testing.T) {
			body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "snapshot_id": name})
			msg := requestProvider(t, nc, ebsprovider.DeleteSnapshotSubject, body)

			var resp ebsprovider.DeleteSnapshotResponse
			require.NoError(t, json.Unmarshal(msg.Data, &resp))
			require.NotNil(t, resp.Error)
			assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, resp.Error.Code)
		})

		t.Run("copy_snapshot/"+name, func(t *testing.T) {
			body := marshalRequest(t, map[string]any{
				"schema_version":          ebsprovider.SchemaVersion,
				"source_snapshot_id":      name,
				"destination_snapshot_id": "snap-testcopyvalid1",
				"volume_id":               "vol-testcopyvalid01",
			})
			msg := requestProvider(t, nc, ebsprovider.CopySnapshotSubject, body)

			var resp ebsprovider.CopySnapshotResponse
			require.NoError(t, json.Unmarshal(msg.Data, &resp))
			require.NotNil(t, resp.Error)
			assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, resp.Error.Code)
		})

		t.Run("publish_volume/"+name, func(t *testing.T) {
			publishSubject, err := ebsprovider.PublishSubject(cfg.NodeName)
			require.NoError(t, err)
			body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "volume_id": name, "node_id": cfg.NodeName})
			msg := requestProvider(t, nc, publishSubject, body)

			var resp ebsprovider.PublishVolumeResponse
			require.NoError(t, json.Unmarshal(msg.Data, &resp))
			require.NotNil(t, resp.Error)
			assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, resp.Error.Code)
		})

		t.Run("unpublish_volume/"+name, func(t *testing.T) {
			unpublishSubject, err := ebsprovider.UnpublishSubject(cfg.NodeName)
			require.NoError(t, err)
			body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "volume_id": name, "node_id": cfg.NodeName})
			msg := requestProvider(t, nc, unpublishSubject, body)

			var resp ebsprovider.UnpublishVolumeResponse
			require.NoError(t, json.Unmarshal(msg.Data, &resp))
			require.NotNil(t, resp.Error)
			assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, resp.Error.Code)
		})
	}
}

func TestProviderHandlers_DeleteVolume_AbsentIsIdempotent(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	store := objectstore.NewMemoryObjectStore()
	prev := providerObjectStoreFactory
	providerObjectStoreFactory = func(*Config) objectstore.ObjectStore { return store }
	t.Cleanup(func() { providerObjectStoreFactory = prev })

	body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "volume_id": "vol-neverexisted1"})
	msg := requestProvider(t, nc, ebsprovider.DeleteVolumeSubject, body)

	var resp ebsprovider.DeleteVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Nil(t, resp.Error, "deleting an absent volume must succeed")
}

func TestProviderHandlers_DeleteSnapshot_AbsentIsIdempotent(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	store := objectstore.NewMemoryObjectStore()
	prev := providerObjectStoreFactory
	providerObjectStoreFactory = func(*Config) objectstore.ObjectStore { return store }
	t.Cleanup(func() { providerObjectStoreFactory = prev })

	body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "snapshot_id": "snap-neverexisted1"})
	msg := requestProvider(t, nc, ebsprovider.DeleteSnapshotSubject, body)

	var resp ebsprovider.DeleteSnapshotResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Nil(t, resp.Error, "deleting an absent snapshot must succeed")
}

// TestProviderHandlers_CreateVolume_IdempotentOnLiveVB exercises CreateVolume's
// idempotency check (describeVolumeEngine) against a live mounted VB, which
// needs no S3 backend at all: an identical repeat must return the existing
// volume, and a conflicting capacity must be rejected as already_exists.
func TestProviderHandlers_CreateVolume_IdempotentOnLiveVB(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-liveidempotent"
	vb := createTestVBWithState(t, volumeName)
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{Name: volumeName, VB: vb})
	size := int64(vb.GetVolumeSize())

	t.Run("identical repeat returns existing volume", func(t *testing.T) {
		body := marshalRequest(t, map[string]any{
			"schema_version": ebsprovider.SchemaVersion,
			"volume_id":      volumeName,
			"capacity_range": map[string]any{"required_bytes": size},
		})
		msg := requestProvider(t, nc, ebsprovider.CreateVolumeSubject, body)

		var resp ebsprovider.CreateVolumeResponse
		require.NoError(t, json.Unmarshal(msg.Data, &resp))
		require.Nil(t, resp.Error)
		require.NotNil(t, resp.Volume)
		assert.Equal(t, volumeName, resp.Volume.ID)
		assert.Equal(t, size, resp.Volume.CapacityBytes)
	})

	t.Run("conflicting capacity is already_exists", func(t *testing.T) {
		body := marshalRequest(t, map[string]any{
			"schema_version": ebsprovider.SchemaVersion,
			"volume_id":      volumeName,
			"capacity_range": map[string]any{"required_bytes": size + bytesPerGiB},
		})
		msg := requestProvider(t, nc, ebsprovider.CreateVolumeSubject, body)

		var resp ebsprovider.CreateVolumeResponse
		require.NoError(t, json.Unmarshal(msg.Data, &resp))
		require.NotNil(t, resp.Error)
		assert.Equal(t, ebsprovider.ErrorCodeAlreadyExists, resp.Error.Code)
	})
}

// TestProviderHandlers_ExpandVolume_MountedIsVolumeInUse confirms ExpandVolume
// refuses a volume that is mounted with a live VB, matching the capability
// advertised (OnlineExpansion: false).
func TestProviderHandlers_ExpandVolume_MountedIsVolumeInUse(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-expandmounted1"
	vb := createTestVBWithState(t, volumeName)
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{Name: volumeName, VB: vb})

	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"capacity_range": map[string]any{"required_bytes": int64(vb.GetVolumeSize()) + bytesPerGiB},
	})
	msg := requestProvider(t, nc, ebsprovider.ExpandVolumeSubject, body)

	var resp ebsprovider.ExpandVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ebsprovider.ErrorCodeVolumeInUse, resp.Error.Code)
}

// TestProviderHandlers_CreateSnapshot_AcceptThenPublish exercises the accept
// reply directly (non-empty OperationID, pending state, correct completion
// subject) against a live mounted VB, which lets completeCreateSnapshot's
// background work finish with no S3 backend involved.
func TestProviderHandlers_CreateSnapshot_AcceptThenPublish(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-snapaccept0001"
	const snapshotID = "snap-accept00000001"
	vb := createTestVBWithState(t, volumeName)
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{Name: volumeName, VB: vb})

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
	msg := requestProvider(t, nc, ebsprovider.SnapshotCreateSubjectPrefix+volumeName, body)

	var accepted ebsprovider.CreateSnapshotResponse
	require.NoError(t, json.Unmarshal(msg.Data, &accepted))
	require.Nil(t, accepted.Error)
	assert.NotEmpty(t, accepted.OperationID)
	assert.Equal(t, wantCompletionSubject, accepted.CompletionSubject)
	require.NotNil(t, accepted.Snapshot)
	assert.Equal(t, ebsprovider.SnapshotStatePending, accepted.Snapshot.State)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completionMsg, err := completionSub.NextMsgWithContext(ctx)
	require.NoError(t, err)

	var completed ebsprovider.CreateSnapshotResponse
	require.NoError(t, json.Unmarshal(completionMsg.Data, &completed))
	require.Nil(t, completed.Error)
	assert.Equal(t, accepted.OperationID, completed.OperationID)
	require.NotNil(t, completed.Snapshot)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, completed.Snapshot.State)
	assert.Equal(t, snapshotID, completed.Snapshot.ID)
}

// TestProviderHandlers_CreateSnapshot_FullRoundTripViaNATSProvider drives the
// same flow through ebsprovider.NewNATSProvider, the real client this server
// must satisfy, rather than raw NATS requests.
func TestProviderHandlers_CreateSnapshot_FullRoundTripViaNATSProvider(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-snaproundtrip1"
	vb := createTestVBWithState(t, volumeName)
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{Name: volumeName, VB: vb})

	provider := ebsprovider.NewNATSProvider(nc, 10*time.Second)
	snap, err := provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{
		Versioned:  ebsprovider.NewVersioned(),
		SnapshotID: "snap-roundtrip00001",
		VolumeID:   volumeName,
	})
	require.NoError(t, err)
	require.NotNil(t, snap)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, snap.State)
	assert.Equal(t, "snap-roundtrip00001", snap.ID)
	assert.Equal(t, volumeName, snap.SourceVolumeID)
}

// TestProviderHandlers_CopySnapshot_FullRoundTripViaNATSProvider drives
// CopySnapshot through ebsprovider.NewNATSProvider against a live mounted VB
// on the file backend: CreateSnapshot seeds a real source snapshot, then
// CopySnapshot must produce a second, independently readable one.
func TestProviderHandlers_CopySnapshot_FullRoundTripViaNATSProvider(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-copysnaproundtr1"
	vb := createTestVBWithState(t, volumeName)
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{Name: volumeName, VB: vb})

	provider := ebsprovider.NewNATSProvider(nc, 10*time.Second)
	_, err := provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{
		Versioned:  ebsprovider.NewVersioned(),
		SnapshotID: "snap-copysrc0000001",
		VolumeID:   volumeName,
	})
	require.NoError(t, err)

	copied, err := provider.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned:             ebsprovider.NewVersioned(),
		SourceSnapshotID:      "snap-copysrc0000001",
		DestinationSnapshotID: "snap-copydst0000001",
		VolumeID:              volumeName,
	})
	require.NoError(t, err)
	require.NotNil(t, copied)
	assert.Equal(t, ebsprovider.SnapshotStateCompleted, copied.State)
	assert.Equal(t, "snap-copydst0000001", copied.ID)
	assert.Equal(t, volumeName, copied.SourceVolumeID)
	assert.NotEmpty(t, copied.Handle)

	// The checkpoint really landed on the backend under the new ID: a second
	// copy from the destination must itself succeed, proving snap-copydst is a
	// real, independently readable snapshot rather than a synthesized answer.
	_, err = provider.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned:             ebsprovider.NewVersioned(),
		SourceSnapshotID:      "snap-copydst0000001",
		DestinationSnapshotID: "snap-copydst0000002",
		VolumeID:              volumeName,
	})
	require.NoError(t, err)
}

// TestProviderHandlers_CopySnapshot_MissingSourceIsNotFound proves the
// missing-source guard: copying a snapshot that was never created on the
// volume must fail as not_found, not internal.
func TestProviderHandlers_CopySnapshot_MissingSourceIsNotFound(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-copysnapmissing1"
	vb := createTestVBWithState(t, volumeName)
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{Name: volumeName, VB: vb})

	provider := ebsprovider.NewNATSProvider(nc, 10*time.Second)
	_, err := provider.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned:             ebsprovider.NewVersioned(),
		SourceSnapshotID:      "snap-nevercreated001",
		DestinationSnapshotID: "snap-copydstmissing1",
		VolumeID:              volumeName,
	})
	require.Error(t, err)
	var providerErr *ebsprovider.ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, ebsprovider.ErrorCodeNotFound, providerErr.Code)
}

// TestProviderHandlers_CopySnapshot_ExistingDestinationIsAlreadyExists proves
// the destination-exists guard: copying onto an ID that already has a
// snapshot must refuse as already_exists, matching viperblock.CopySnapshotMeta
// refusing to clobber a committed destination.
func TestProviderHandlers_CopySnapshot_ExistingDestinationIsAlreadyExists(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-copysnapexists01"
	vb := createTestVBWithState(t, volumeName)
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{Name: volumeName, VB: vb})

	provider := ebsprovider.NewNATSProvider(nc, 10*time.Second)
	_, err := provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{
		Versioned:  ebsprovider.NewVersioned(),
		SnapshotID: "snap-copyexists-a001",
		VolumeID:   volumeName,
	})
	require.NoError(t, err)
	_, err = provider.CreateSnapshot(context.Background(), ebsprovider.CreateSnapshotRequest{
		Versioned:  ebsprovider.NewVersioned(),
		SnapshotID: "snap-copyexists-b001",
		VolumeID:   volumeName,
	})
	require.NoError(t, err)

	_, err = provider.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned:             ebsprovider.NewVersioned(),
		SourceSnapshotID:      "snap-copyexists-a001",
		DestinationSnapshotID: "snap-copyexists-b001",
		VolumeID:              volumeName,
	})
	require.Error(t, err)
	var providerErr *ebsprovider.ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, ebsprovider.ErrorCodeAlreadyExists, providerErr.Code)
}

// TestProviderHandlers_CopySnapshot_SameIDIsInvalidArgument proves
// handleCopySnapshot's own source-equals-destination guard fires before any
// engine work happens (no volume needs to be mounted for this to fail).
func TestProviderHandlers_CopySnapshot_SameIDIsInvalidArgument(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	provider := ebsprovider.NewNATSProvider(nc, 10*time.Second)
	_, err := provider.CopySnapshot(context.Background(), ebsprovider.CopySnapshotRequest{
		Versioned:             ebsprovider.NewVersioned(),
		SourceSnapshotID:      "snap-sameid00000001",
		DestinationSnapshotID: "snap-sameid00000001",
		VolumeID:              "vol-copysnapsameid01",
	})
	require.Error(t, err)
	var providerErr *ebsprovider.ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, providerErr.Code)
}

// TestProviderHandlers_SnapshotCreateWildcard_DoesNotCaptureDelete guards the
// SnapshotCreateSubjectPrefix wildcard subscription against ever matching
// ebs.provider.v1.snapshot.delete: a DeleteSnapshot request must come back as
// a DeleteSnapshotResponse, never a CreateSnapshotResponse (which carries a
// non-empty operation_id).
func TestProviderHandlers_SnapshotCreateWildcard_DoesNotCaptureDelete(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	store := objectstore.NewMemoryObjectStore()
	prev := providerObjectStoreFactory
	providerObjectStoreFactory = func(*Config) objectstore.ObjectStore { return store }
	t.Cleanup(func() { providerObjectStoreFactory = prev })

	body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "snapshot_id": "snap-notcreate0001"})
	msg := requestProvider(t, nc, ebsprovider.DeleteSnapshotSubject, body)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(msg.Data, &raw))
	_, hasOperationID := raw["operation_id"]
	assert.False(t, hasOperationID, "DeleteSnapshot response must not look like a CreateSnapshot accept")
}

// TestProviderHandlers_DeleteVolume_RemovesExistingObjects covers
// deleteObjectPrefix's non-empty loop body (DeleteVolume's absent-volume test
// only exercises the immediate zero-objects return): objects under both the
// volume's main prefix and its -efi/ auxiliary prefix must be removed.
func TestProviderHandlers_DeleteVolume_RemovesExistingObjects(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	store := objectstore.NewMemoryObjectStore()
	prev := providerObjectStoreFactory
	providerObjectStoreFactory = func(*Config) objectstore.ObjectStore { return store }
	t.Cleanup(func() { providerObjectStoreFactory = prev })

	const volumeName = "vol-deleteobjects01"
	putTestObject(t, store, cfg.Bucket, volumeName+"/config.json")
	putTestObject(t, store, cfg.Bucket, volumeName+"/chunk-0000.bin")
	putTestObject(t, store, cfg.Bucket, volumeName+"-efi/config.json")

	body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "volume_id": volumeName})
	msg := requestProvider(t, nc, ebsprovider.DeleteVolumeSubject, body)

	var resp ebsprovider.DeleteVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Nil(t, resp.Error)
	assert.Equal(t, 0, store.Count(), "every object under both prefixes must be deleted")
}

// TestProviderHandlers_DeleteSnapshot_RemovesExistingObjects is
// DeleteVolume's counterpart for DeleteSnapshot's deleteObjectPrefix call.
func TestProviderHandlers_DeleteSnapshot_RemovesExistingObjects(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	store := objectstore.NewMemoryObjectStore()
	prev := providerObjectStoreFactory
	providerObjectStoreFactory = func(*Config) objectstore.ObjectStore { return store }
	t.Cleanup(func() { providerObjectStoreFactory = prev })

	const snapshotName = "snap-deleteobjects01"
	putTestObject(t, store, cfg.Bucket, snapshotName+"/metadata.json")
	putTestObject(t, store, cfg.Bucket, snapshotName+"/chunk-0000.bin")

	body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "snapshot_id": snapshotName})
	msg := requestProvider(t, nc, ebsprovider.DeleteSnapshotSubject, body)

	var resp ebsprovider.DeleteSnapshotResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Nil(t, resp.Error)
	assert.Equal(t, 0, store.Count(), "every object under the snapshot prefix must be deleted")
}

func putTestObject(t *testing.T, store objectstore.ObjectStore, bucket, key string) {
	t.Helper()
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: awssdk.String(bucket), Key: awssdk.String(key), Body: bytes.NewReader([]byte("data")),
	})
	require.NoError(t, err)
}

// TestProviderErrorConstructors covers the notFoundError and internalError
// helpers directly: every other *Error constructor in this file is already
// exercised via a handler that returns it, but nothing in this package's
// current test set reaches a not_found or internal response without a live
// S3/viperblock backend.
func TestProviderErrorConstructors(t *testing.T) {
	notFound := notFoundError("volume %s not found", "vol-1")
	assert.Equal(t, ebsprovider.ErrorCodeNotFound, notFound.Code)
	assert.Equal(t, "volume vol-1 not found", notFound.Message)

	internal := internalError("backend init: %v", assert.AnError)
	assert.Equal(t, ebsprovider.ErrorCodeInternal, internal.Code)
	assert.Contains(t, internal.Message, assert.AnError.Error())
}

// TestBuildProviderVBConfig covers buildProviderVBConfig's field wiring: the
// GCEnabled/MasterKey passthrough this file's own comment calls out as
// control-plane-owned (TenantID, Tags, VolumeType, IOPS, Throughput,
// AvailabilityZone) staying out of VolumeMetadata, and the SnapshotID/
// SourceVolumeName fields only being set when a source snapshot is given.
func TestBuildProviderVBConfig(t *testing.T) {
	cfg := &Config{BaseDir: "/tmp/vb-base", GCEnabled: true}

	vbconfig := buildProviderVBConfig(cfg, "vol-1", 8<<30, "", "")
	assert.Equal(t, "vol-1", vbconfig.VolumeName)
	assert.Equal(t, uint64(8<<30), vbconfig.VolumeSize)
	assert.Equal(t, "/tmp/vb-base", vbconfig.BaseDir)
	assert.True(t, vbconfig.GCEnabled)
	assert.Empty(t, vbconfig.VolumeConfig.VolumeMetadata.TenantID, "control-plane facts must not be set here")
	assert.Empty(t, vbconfig.SnapshotID)
	assert.Empty(t, vbconfig.SourceVolumeName)

	fromSnapshot := buildProviderVBConfig(cfg, "vol-2", 8<<30, "snap-1", "vol-src")
	assert.Equal(t, "snap-1", fromSnapshot.SnapshotID)
	assert.Equal(t, "vol-src", fromSnapshot.SourceVolumeName)
}

// TestProviderHandlers_DeleteVolume_MountedVolumeStopsResources covers
// handleDeleteVolume's mounted-volume path end to end: stopMountedVolumeForDelete
// must remove the entry from cfg.MountedVolumes, stop the VB's background
// goroutines, and delete the NBD socket file, all without touching an S3
// backend (providerObjectStoreFactory is overridden to a MemoryObjectStore).
func TestProviderHandlers_DeleteVolume_MountedVolumeStopsResources(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	store := objectstore.NewMemoryObjectStore()
	prev := providerObjectStoreFactory
	providerObjectStoreFactory = func(*Config) objectstore.ObjectStore { return store }
	t.Cleanup(func() { providerObjectStoreFactory = prev })

	const volumeName = "vol-deletemounted01"
	vb := createTestVBWithState(t, volumeName)
	socketPath := filepath.Join(t.TempDir(), volumeName+".sock")
	require.NoError(t, os.WriteFile(socketPath, []byte("fake-socket"), 0600))

	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
		Name: volumeName, VB: vb, Socket: socketPath,
		// A PID that (almost certainly) does not exist: KillProcess must fail
		// gracefully (logged, not fatal) rather than block or panic.
		PID: 999999,
	})

	body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion, "volume_id": volumeName})
	msg := requestProvider(t, nc, ebsprovider.DeleteVolumeSubject, body)

	var resp ebsprovider.DeleteVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Nil(t, resp.Error)

	cfg.mu.Lock()
	remaining := cfg.MountedVolumes
	cfg.mu.Unlock()
	for _, v := range remaining {
		assert.NotEqual(t, volumeName, v.Name, "deleted volume must be removed from MountedVolumes")
	}

	_, statErr := os.Stat(socketPath)
	assert.True(t, os.IsNotExist(statErr), "the NBD socket file must be removed")
}

// TestProviderHandlers_PublishVolume_IdempotentRepublish covers the double-
// writer hazard this provider boundary exists to prevent: republishing a
// volume this node already has mounted must return the existing attachment
// unchanged rather than mounting it a second time. The mounted-volume count
// and PID (not just the returned URI) prove no second nbdkit was started.
func TestProviderHandlers_PublishVolume_IdempotentRepublish(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-pubidempotent1"
	const existingURI = "nbd:unix:/fake/vol-pubidempotent1.sock"
	vb := createTestVBWithState(t, volumeName)
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
		Name:   volumeName,
		VB:     vb,
		PID:    424242,
		NBDURI: existingURI,
	})

	publishSubject, err := ebsprovider.PublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"node_id":        cfg.NodeName,
	})
	msg := requestProvider(t, nc, publishSubject, body)

	var resp ebsprovider.PublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.Nil(t, resp.Error)
	require.NotNil(t, resp.Published)
	assert.Equal(t, volumeName, resp.Published.VolumeID)
	assert.Equal(t, cfg.NodeName, resp.Published.NodeID)
	assert.Equal(t, existingURI, resp.Published.NBDURI)

	cfg.mu.Lock()
	count := len(cfg.MountedVolumes)
	pid := cfg.MountedVolumes[0].PID
	cfg.mu.Unlock()
	assert.Equal(t, 1, count, "republish of an already-mounted volume must not add a second entry")
	assert.Equal(t, 424242, pid, "republish must not spawn a second nbdkit (PID must be unchanged)")
}

// TestProviderHandlers_PublishVolume_ReadOnlyRejected covers the ReadOnly
// field against a provider that does not advertise ReadOnlyPublish: the
// request must be refused as unsupported, never silently attached read-write.
func TestProviderHandlers_PublishVolume_ReadOnlyRejected(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	publishSubject, err := ebsprovider.PublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      "vol-readonlypub001",
		"node_id":        cfg.NodeName,
		"read_only":      true,
	})
	msg := requestProvider(t, nc, publishSubject, body)

	var resp ebsprovider.PublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ebsprovider.ErrorCodeUnsupportedCap, resp.Error.Code)
}

// TestProviderHandlers_UnpublishVolume_RemovesMountedVolume mirrors
// TestIntegration_EBSUnmountRequest (the legacy ebs.unmount handler's happy
// path) but drives it through the provider subject instead, confirming the
// shared unmountVolume extraction behaves the same way from either front.
func TestProviderHandlers_UnpublishVolume_RemovesMountedVolume(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-unpubremoved01"
	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
		Name: volumeName,
		PID:  999999, // Fake PID that doesn't exist; KillProcess must fail gracefully.
	})

	unpublishSubject, err := ebsprovider.UnpublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      volumeName,
		"node_id":        cfg.NodeName,
	})
	msg := requestProvider(t, nc, unpublishSubject, body)

	var resp ebsprovider.UnpublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Nil(t, resp.Error)

	cfg.mu.Lock()
	remaining := cfg.MountedVolumes
	cfg.mu.Unlock()
	for _, v := range remaining {
		assert.NotEqual(t, volumeName, v.Name, "unpublished volume must be removed from MountedVolumes")
	}
}

// TestProviderHandlers_UnpublishVolume_NotFound mirrors
// TestIntegration_EBSUnmountNonExistentVolume: unpublishing a volume this
// node never mounted must come back not_found, matching the legacy handler's
// NotFound flag rather than a generic internal error.
func TestProviderHandlers_UnpublishVolume_NotFound(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	unpublishSubject, err := ebsprovider.UnpublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
		"volume_id":      "vol-neverpublished1",
		"node_id":        cfg.NodeName,
	})
	msg := requestProvider(t, nc, unpublishSubject, body)

	var resp ebsprovider.UnpublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.NotNil(t, resp.Error)
	assert.Equal(t, ebsprovider.ErrorCodeNotFound, resp.Error.Code)
}

// TestProviderHandlers_PublishUnpublish_RequireNodeName covers
// registerProviderSubjects' NodeName gate: PublishVolume/UnpublishVolume must
// not be registered when cfg.NodeName is empty (PublishSubject has no node to
// address), while every other ebs.provider.v1.* subject still is.
func TestProviderHandlers_PublishUnpublish_RequireNodeName(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfgNoNode := setupTestConfig(t, natsURL)
	cfgNoNode.NodeName = ""
	ncNoNode := startProviderSubjects(t, cfgNoNode, natsURL)

	_, natsURL2 := setupEmbeddedNATS(t)
	cfgNamed := setupTestConfig(t, natsURL2)
	ncNamed := startProviderSubjects(t, cfgNamed, natsURL2)

	assert.Equal(t, ncNamed.NumSubscriptions()-2, ncNoNode.NumSubscriptions(),
		"an empty NodeName must skip exactly the publish and unpublish subscriptions")

	// The other provider subjects must still work without a node name.
	body := marshalRequest(t, map[string]any{"schema_version": ebsprovider.SchemaVersion})
	msg := requestProvider(t, ncNoNode, ebsprovider.CapabilitiesSubject, body)
	var resp ebsprovider.GetCapabilitiesResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	assert.Nil(t, resp.Error)
}

// TestProviderHandlers_CreateVolume_SeedValidation covers the guards that must
// reject a seed before any engine is built. The bound exists because a []byte
// is base64-encoded into the JSON request and has to clear NATS max_payload.
func TestProviderHandlers_CreateVolume_SeedValidation(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	requestError := func(t *testing.T, body []byte) *ebsprovider.ProviderError {
		t.Helper()
		msg := requestProvider(t, nc, ebsprovider.CreateVolumeSubject, body)
		var resp ebsprovider.CreateVolumeResponse
		require.NoError(t, json.Unmarshal(msg.Data, &resp))
		require.NotNil(t, resp.Error)
		require.Nil(t, resp.Volume)
		return resp.Error
	}

	t.Run("seed above MaxSeedBytes is invalid_argument", func(t *testing.T) {
		err := requestError(t, marshalRequest(t, map[string]any{
			"schema_version": ebsprovider.SchemaVersion,
			"volume_id":      "vol-seedtoobig0001",
			"capacity_range": map[string]any{"required_bytes": int64(bytesPerGiB)},
			"seed_data":      make([]byte, ebsprovider.MaxSeedBytes+1),
		}))
		assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, err.Code)
	})

	t.Run("seed larger than capacity is invalid_argument", func(t *testing.T) {
		err := requestError(t, marshalRequest(t, map[string]any{
			"schema_version": ebsprovider.SchemaVersion,
			"volume_id":      "vol-seedovercap001",
			"capacity_range": map[string]any{"required_bytes": 512},
			"seed_data":      make([]byte, 4096),
		}))
		assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, err.Code)
	})

	// A snapshot clone already fills the volume, so seeding on top of one would
	// overwrite the cloned image rather than initialise a blank volume.
	t.Run("seed with a source snapshot is invalid_argument", func(t *testing.T) {
		err := requestError(t, marshalRequest(t, map[string]any{
			"schema_version":     ebsprovider.SchemaVersion,
			"volume_id":          "vol-seedwithsnap01",
			"capacity_range":     map[string]any{"required_bytes": int64(bytesPerGiB)},
			"source_snapshot_id": "snap-0000000000001",
			"source_volume_id":   "vol-source00000001",
			"seed_data":          make([]byte, 16),
		}))
		assert.Equal(t, ebsprovider.ErrorCodeInvalidArgument, err.Code)
	})
}

// TestProviderHandlers_OwnerSubscription_UnmountRemovesItAndFallsBack proves
// the owner subscription's lifecycle: live while a volume is mounted (a raw
// request to its owner subject gets a responder), gone once unmountVolume
// runs (the same request comes back nats.ErrNoResponders), at which point
// NATSProvider.GetVolume falls back to the queue-group subject and takes the
// fresh-engine path (the fast-failing backend then fails it, proving this
// really happened rather than silently reusing the removed VB).
func TestProviderHandlers_OwnerSubscription_UnmountRemovesItAndFallsBack(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	const volumeName = "vol-ownerunmount001"
	vb := createTestVBWithState(t, volumeName)

	ownerSubs := subscribeOwnerSubjects(context.Background(), cfg, nc, volumeName)
	require.Len(t, ownerSubs, 4, "all four owner verbs must subscribe cleanly")
	require.NoError(t, nc.Flush())

	cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
		Name: volumeName, VB: vb, OwnerSubs: ownerSubs,
		// A PID that (almost certainly) does not exist: KillProcess must fail
		// gracefully during unmountVolume rather than block or panic.
		PID: 999999,
	})

	ownerSubject, err := ebsprovider.GetVolumeOwnerSubject(volumeName)
	require.NoError(t, err)
	getVolumeBody := marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion, "volume_id": volumeName,
	})

	_, err = nc.Request(ownerSubject, getVolumeBody, 2*time.Second)
	require.NoError(t, err, "owner subject must have a live responder while the volume is mounted")

	_, err = unmountVolume(context.Background(), cfg, volumeName)
	require.NoError(t, err)

	_, err = nc.Request(ownerSubject, getVolumeBody, 2*time.Second)
	require.ErrorIs(t, err, nats.ErrNoResponders, "unmount must drop the owner subscription")

	cfg.S3Host = fastFailingS3Host(t)
	provider := ebsprovider.NewNATSProvider(nc, 5*time.Second)
	_, err = provider.GetVolume(context.Background(), ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeName,
	})
	require.Error(t, err, "with no owner and a broken S3 backend, the queue fallback's fresh engine must fail")
}

// The EFI variable store depends on CreateVolume writing a seed, so the daemon
// must advertise that capability; a caller branching on it would otherwise fall
// back to building its own engine.
func TestProviderHandlers_Capabilities_AdvertisesVolumeSeeding(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	nc := startProviderSubjects(t, cfg, natsURL)

	msg := requestProvider(t, nc, ebsprovider.CapabilitiesSubject, marshalRequest(t, map[string]any{
		"schema_version": ebsprovider.SchemaVersion,
	}))
	var resp ebsprovider.GetCapabilitiesResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	require.Nil(t, resp.Error)
	assert.True(t, resp.Capabilities.VolumeSeeding)
}
