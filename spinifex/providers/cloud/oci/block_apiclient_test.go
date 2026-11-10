package oci_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
)

func testBlockClient(t *testing.T, h http.HandlerFunc) (oci.BlockClient, *[]capture) {
	t.Helper()
	provider, endpoint, hc, seen := testEndpoint(t, h)
	c, err := oci.NewTestBlockClient(provider, endpoint, hc)
	require.NoError(t, err)
	return c, seen
}

const volumeJSON = `{"id":"ocid1.volume.oc1..v1","compartmentId":"c1","availabilityDomain":"AD-1",
"displayName":"vol-1","sizeInGBs":50,"sizeInMBs":51200,"vpusPerGB":20,"lifecycleState":"PROVISIONING",
"timeCreated":"2026-11-10T03:00:00Z","freeformTags":{"spinifex-volume-id":"vol-1"},
"sourceDetails":{"type":"volumeBackup","id":"ocid1.volumebackup.oc1..b1"}}`

// The retry token is what makes a create after a lost reply return the first
// volume instead of a second one, so it has to reach OCI as the header, with
// the tags and the restore source in the body.
func TestBlockCreateVolumeSendsRetryTokenTagsAndSource(t *testing.T) {
	c, seen := testBlockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(volumeJSON))
	})

	got, err := c.CreateVolume(context.Background(), oci.CreateVolumeInput{
		CompartmentID: "c1", AvailabilityDomain: "AD-1", DisplayName: "vol-1",
		SizeGB: 50, VPUsPerGB: 20, SourceBackupID: "ocid1.volumebackup.oc1..b1",
		FreeformTags: map[string]string{"spinifex-volume-id": "vol-1"},
		RetryToken:   "token-1",
	})
	require.NoError(t, err)
	assert.Equal(t, oci.BlockVolume{
		ID: "ocid1.volume.oc1..v1", CompartmentID: "c1", AvailabilityDomain: "AD-1", DisplayName: "vol-1",
		SizeGB: 50, VPUsPerGB: 20, State: oci.VolumeProvisioning, SourceBackupID: "ocid1.volumebackup.oc1..b1",
		FreeformTags: map[string]string{"spinifex-volume-id": "vol-1"}, TimeCreated: got.TimeCreated,
	}, got)
	assert.False(t, got.TimeCreated.IsZero())

	require.Len(t, *seen, 1)
	req := (*seen)[0]
	assert.Equal(t, http.MethodPost, req.method)
	assert.Equal(t, "/20160918/volumes", req.path)
	assert.Equal(t, "token-1", req.header.Get("opc-retry-token"))
	assert.Equal(t, "c1", req.body["compartmentId"])
	assert.Equal(t, "AD-1", req.body["availabilityDomain"])
	assert.Equal(t, "vol-1", req.body["displayName"])
	assert.EqualValues(t, 50, req.body["sizeInGBs"])
	assert.EqualValues(t, 20, req.body["vpusPerGB"])
	assert.Equal(t, map[string]any{"spinifex-volume-id": "vol-1"}, req.body["freeformTags"])
	assert.Equal(t, map[string]any{"type": "volumeBackup", "id": "ocid1.volumebackup.oc1..b1"}, req.body["sourceDetails"])
}

// A zero tier is OCI's Lower Cost tier, so the client must leave the field out
// rather than send a zero nobody asked for.
func TestBlockCreateVolumeOmitsUnsetTierAndSource(t *testing.T) {
	c, seen := testBlockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"v1","lifecycleState":"AVAILABLE"}`))
	})

	_, err := c.CreateVolume(context.Background(), oci.CreateVolumeInput{CompartmentID: "c1", AvailabilityDomain: "AD-1", SizeGB: 50})
	require.NoError(t, err)
	body := (*seen)[0].body
	assert.NotContains(t, body, "vpusPerGB")
	assert.NotContains(t, body, "sourceDetails")
	assert.NotContains(t, body, "displayName")
}

// OCI's list call has no tag filter, so the client filters every page; a
// volume carrying a different cluster's tag must not be returned.
func TestBlockListVolumesFiltersTagsAcrossPages(t *testing.T) {
	c, seen := testBlockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Opc-Next-Page", "page2")
			_, _ = w.Write([]byte(`[{"id":"v1","freeformTags":{"spinifex-cluster":"a"}},{"id":"v2","freeformTags":{"spinifex-cluster":"b"}}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"v3","freeformTags":{"spinifex-cluster":"a","other":"x"}},{"id":"v4"}]`))
	})

	got, err := c.ListVolumes(context.Background(), oci.VolumeFilter{
		CompartmentID: "c1", DisplayName: "vol-1", Tags: map[string]string{"spinifex-cluster": "a"},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "v1", got[0].ID)
	assert.Equal(t, "v3", got[1].ID)
	require.Len(t, *seen, 2)
	assert.Equal(t, "c1", (*seen)[0].query.Get("compartmentId"))
	assert.Equal(t, "vol-1", (*seen)[0].query.Get("displayName"))
	assert.False(t, (*seen)[0].query.Has("availabilityDomain"), "an unset filter must not be sent")
}

func TestBlockUpdateVolumeSendsOnlyWhatChanges(t *testing.T) {
	c, seen := testBlockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"v1","sizeInGBs":60,"lifecycleState":"AVAILABLE"}`))
	})

	got, err := c.UpdateVolume(context.Background(), "v1", oci.UpdateVolumeInput{FreeformTags: map[string]string{"k": "v"}})
	require.NoError(t, err)
	assert.Equal(t, int64(60), got.SizeGB)
	_, err = c.UpdateVolume(context.Background(), "v1", oci.UpdateVolumeInput{SizeGB: 60})
	require.NoError(t, err)

	require.Len(t, *seen, 2)
	assert.Equal(t, http.MethodPut, (*seen)[0].method)
	assert.Equal(t, "/20160918/volumes/v1", (*seen)[0].path)
	assert.NotContains(t, (*seen)[0].body, "sizeInGBs", "a tag update must not resize the volume")
	assert.Equal(t, map[string]any{"k": "v"}, (*seen)[0].body["freeformTags"])
	assert.EqualValues(t, 60, (*seen)[1].body["sizeInGBs"])
	assert.NotContains(t, (*seen)[1].body, "freeformTags", "a resize must not clear the tags")
}

// The attachment is never shareable: that is what makes OCI refuse a second
// writer anywhere, which the provider's cluster-wide exclusion rests on.
func TestBlockAttachVolumeIsISCSIAndNotShareable(t *testing.T) {
	c, seen := testBlockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"attachmentType":"iscsi","id":"a1","instanceId":"i1","volumeId":"v1",
"availabilityDomain":"AD-1","lifecycleState":"ATTACHING","isReadOnly":true,
"iqn":"iqn.2015-12.com.oracleiaas:x","ipv4":"169.254.2.2","port":3260,"compartmentId":"c1","timeCreated":"2026-11-10T03:00:00Z"}`))
	})

	got, err := c.AttachVolume(context.Background(), oci.AttachVolumeInput{InstanceID: "i1", VolumeID: "v1", DisplayName: "vol-1", ReadOnly: true})
	require.NoError(t, err)
	assert.Equal(t, oci.VolumeAttachment{
		ID: "a1", InstanceID: "i1", VolumeID: "v1", AvailabilityDomain: "AD-1", State: oci.AttachmentAttaching,
		ReadOnly: true, IQN: "iqn.2015-12.com.oracleiaas:x", IPv4: "169.254.2.2", Port: 3260,
	}, got)

	body := (*seen)[0].body
	assert.Equal(t, "/20160918/volumeAttachments", (*seen)[0].path)
	assert.Equal(t, "iscsi", body["type"])
	assert.Equal(t, true, body["isReadOnly"])
	assert.Equal(t, false, body["isShareable"])
	assert.Equal(t, "i1", body["instanceId"])
	assert.Equal(t, "v1", body["volumeId"])
}

// A paravirtualized attachment has no iSCSI target, and must read back with
// none rather than fail the whole list.
func TestBlockListVolumeAttachmentsReadsEveryType(t *testing.T) {
	c, seen := testBlockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
{"attachmentType":"iscsi","id":"a1","instanceId":"i1","volumeId":"v1","availabilityDomain":"AD-1","lifecycleState":"ATTACHED","iqn":"iqn.x","ipv4":"169.254.2.2","port":3260,"compartmentId":"c1","timeCreated":"2026-11-10T03:00:00Z"},
{"attachmentType":"paravirtualized","id":"a2","instanceId":"i2","volumeId":"v1","availabilityDomain":"AD-1","lifecycleState":"DETACHED","compartmentId":"c1","timeCreated":"2026-11-10T03:00:00Z"}]`))
	})

	got, err := c.ListVolumeAttachments(context.Background(), oci.AttachmentFilter{CompartmentID: "c1", VolumeID: "v1"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "iqn.x", got[0].IQN)
	assert.Equal(t, 3260, got[0].Port)
	assert.Equal(t, oci.AttachmentDetached, got[1].State)
	assert.Empty(t, got[1].IQN)
	assert.Equal(t, "v1", (*seen)[0].query.Get("volumeId"))
	assert.False(t, (*seen)[0].query.Has("instanceId"))
}

func TestBlockCreateVolumeBackupIsIncrementalWithRetryToken(t *testing.T) {
	c, seen := testBlockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"b1","compartmentId":"c1","volumeId":"v1","displayName":"snap-1","sizeInGBs":50,
"uniqueSizeInGBs":2,"lifecycleState":"CREATING","type":"INCREMENTAL","timeCreated":"2026-11-10T03:00:00Z","freeformTags":{"spinifex-snapshot-id":"snap-1"}}`))
	})

	got, err := c.CreateVolumeBackup(context.Background(), oci.CreateVolumeBackupInput{
		VolumeID: "v1", DisplayName: "snap-1", FreeformTags: map[string]string{"spinifex-snapshot-id": "snap-1"}, RetryToken: "token-2",
	})
	require.NoError(t, err)
	assert.Equal(t, oci.BackupCreating, got.State)
	assert.Equal(t, int64(2), got.UniqueSizeGB)
	assert.Equal(t, "v1", got.VolumeID)

	req := (*seen)[0]
	assert.Equal(t, "/20160918/volumeBackups", req.path)
	assert.Equal(t, "token-2", req.header.Get("opc-retry-token"))
	assert.Equal(t, "INCREMENTAL", req.body["type"])
	assert.Equal(t, map[string]any{"spinifex-snapshot-id": "snap-1"}, req.body["freeformTags"])
}

func TestBlockListVolumeBackupsFiltersTags(t *testing.T) {
	c, _ := testBlockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"b1","freeformTags":{"spinifex-cluster":"a"}},{"id":"b2","freeformTags":{"spinifex-cluster":"b"}}]`))
	})

	got, err := c.ListVolumeBackups(context.Background(), oci.BackupFilter{CompartmentID: "c1", Tags: map[string]string{"spinifex-cluster": "a"}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "b1", got[0].ID)
}

// OCI listed one new backup twice in a live run, as it left REQUEST_RECEIVED,
// and a provider counting the entries saw two backups for one snapshot.
func TestBlockListsAnswerEachResourceOnce(t *testing.T) {
	c, _ := testBlockClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Opc-Next-Page", "page2")
			_, _ = w.Write([]byte(`[{"id":"x1","lifecycleState":"REQUEST_RECEIVED"},{"id":"x1","lifecycleState":"CREATING"},{"id":"x2"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":"x2"},{"id":"x3"}]`))
	})

	backups, err := c.ListVolumeBackups(context.Background(), oci.BackupFilter{CompartmentID: "c1"})
	require.NoError(t, err)
	ids := make([]string, 0, len(backups))
	for _, b := range backups {
		ids = append(ids, b.ID)
	}
	assert.Equal(t, []string{"x1", "x2", "x3"}, ids)

	volumes, err := c.ListVolumes(context.Background(), oci.VolumeFilter{CompartmentID: "c1"})
	require.NoError(t, err)
	ids = ids[:0]
	for _, v := range volumes {
		ids = append(ids, v.ID)
	}
	assert.Equal(t, []string{"x1", "x2", "x3"}, ids)
}

// The two 409s OCI answers while a backup is in progress are what the provider
// maps to volume_in_use and unavailable, so they must stay classified.
func TestBlockConflictsAreClassified(t *testing.T) {
	c, _ := testBlockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"Conflict","message":"There is currently a backup operation in progress"}`))
	})

	for name, call := range map[string]func() error{
		"DeleteVolume":       func() error { return c.DeleteVolume(context.Background(), "v1") },
		"DeleteVolumeBackup": func() error { return c.DeleteVolumeBackup(context.Background(), "b1") },
		"DetachVolume":       func() error { return c.DetachVolume(context.Background(), "a1") },
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			require.ErrorIs(t, err, oci.ErrConflict)
			var api *oci.APIError
			require.ErrorAs(t, err, &api)
			assert.Equal(t, name, api.Op)
		})
	}
}

func TestBlockNotFoundAndLimitAreClassified(t *testing.T) {
	var status int
	var code string
	c, _ := testBlockClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"code":"` + code + `","message":"m"}`))
	})

	ctx := context.Background()
	for name, tc := range map[string]struct {
		status int
		code   string
		want   error
		call   func() error
	}{
		"GetVolume": {http.StatusNotFound, "NotAuthorizedOrNotFound", oci.ErrNotFound, func() error {
			_, err := c.GetVolume(ctx, "v1")
			return err
		}},
		"GetVolumeBackup": {http.StatusNotFound, "NotAuthorizedOrNotFound", oci.ErrNotFound, func() error {
			_, err := c.GetVolumeBackup(ctx, "b1")
			return err
		}},
		"GetVolumeAttachment": {http.StatusNotFound, "NotAuthorizedOrNotFound", oci.ErrNotFound, func() error {
			_, err := c.GetVolumeAttachment(ctx, "a1")
			return err
		}},
		"AttachVolume": {http.StatusBadRequest, "LimitExceeded", oci.ErrLimitExceeded, func() error {
			_, err := c.AttachVolume(ctx, oci.AttachVolumeInput{InstanceID: "i1", VolumeID: "v1"})
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			status, code = tc.status, tc.code
			err := tc.call()
			require.ErrorIs(t, err, tc.want)
			var api *oci.APIError
			require.ErrorAs(t, err, &api)
			assert.Equal(t, name, api.Op)
		})
	}
}
