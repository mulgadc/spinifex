//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil/vbscan"
	testpredastore "github.com/mulgadc/spinifex/tests/fixtures/predastore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateVolume_PersistsToRealPredastore is the storage-touching proof
// case for the real-predastore fixture (StartVolumeDaemonLite): it drives
// ec2.CreateVolume through the real gateway and a real VolumeServiceImpl,
// which constructs viperblock.New and calls Backend.Init()/SaveState()
// against an actual predastore daemon rather than an unreachable host. The
// previous unit-test equivalent (handlers/ec2/volume's
// TestCreateVolume_PassesValidation) could only assert the returned error
// wasn't a validation error, because nothing past validation could execute
// without a live backend. Here CreateVolume is expected to fully succeed,
// and DescribeVolumes reloads the volume from that same real backend
// (config.json read back over S3) rather than from any in-memory state the
// handler might be holding, so this proves the volume was actually
// persisted.
//
// The DescribeVolumes reload only proves the handler's own read path agrees
// with the handler's own write path — both go through the same
// getVolumeConfig deserialization, so a bug that corrupts a field neither
// path happens to look at, or that miscomputes a value DescribeVolumes never
// re-derives (VolumeMetadata.SizeGiB is copied from the request directly, not
// recomputed from vb.VolumeSize), would round-trip clean. vbscan reads the
// same config.json independently of any spinifex handler code, and reports
// the real chunk objects present under the volume's prefix, so it catches
// drift between what the handler claims and what viperblock actually wrote.
func TestCreateVolume_PersistsToRealPredastore(t *testing.T) {
	gw := StartGateway(t)
	StartVolumeDaemonLite(t, gw)

	client := gw.EC2Client(t)

	out, err := client.CreateVolume(&ec2.CreateVolumeInput{
		Size:             aws.Int64(8),
		AvailabilityZone: aws.String(testAZ),
	})
	require.NoError(t, err, "CreateVolume")
	require.NotNil(t, out.VolumeId)

	desc, err := client.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: []*string{out.VolumeId},
	})
	require.NoError(t, err, "DescribeVolumes")
	require.Len(t, desc.Volumes, 1, "volume should reload from the real predastore backend")
	assert.Equal(t, aws.StringValue(out.VolumeId), aws.StringValue(desc.Volumes[0].VolumeId))
	assert.Equal(t, int64(8), aws.Int64Value(desc.Volumes[0].Size))
	assert.Equal(t, "gp3", aws.StringValue(desc.Volumes[0].VolumeType))
	assert.Equal(t, "available", aws.StringValue(desc.Volumes[0].State))

	// Independent storage-state oracle: read the real bytes viperblock
	// persisted, bypassing DescribeVolumes' deserialization entirely.
	fixture := testpredastore.Start(t)
	store := objectstore.NewS3ObjectStoreFromConfig(fixture.Host, fixture.Region, fixture.AccessKey, fixture.SecretKey)
	scanner := vbscan.NewScanner(store, testVolumeBucket)

	rep, err := scanner.Inspect(context.Background(), aws.StringValue(out.VolumeId))
	require.NoError(t, err, "vbscan.Inspect")

	meta := rep.State.VolumeConfig.VolumeMetadata
	assert.Equal(t, aws.StringValue(out.VolumeId), meta.VolumeID, "persisted VolumeID")
	assert.Equal(t, uint64(8), meta.SizeGiB, "persisted VolumeMetadata.SizeGiB")
	assert.Equal(t, "available", meta.State, "persisted VolumeMetadata.State")

	// Tenancy, volume type and placement are control-plane facts, held in the
	// ebsmetadata document rather than storage state: the provider writes only
	// what it owns. DescribeVolumes above is what checks them, and its answer
	// is account-scoped, so returning this volume at all proves the tenant was
	// recorded.
	assert.Empty(t, meta.TenantID, "tenancy belongs to ebsmetadata, not viperblock state")
	assert.Empty(t, meta.VolumeType, "volume type belongs to ebsmetadata, not viperblock state")
	assert.Empty(t, meta.AvailabilityZone, "placement belongs to ebsmetadata, not viperblock state")
	assert.Equal(t, testAZ, aws.StringValue(desc.Volumes[0].AvailabilityZone), "reported AvailabilityZone")

	// The on-disk VolumeSize is bytes, derived independently of
	// VolumeMetadata.SizeGiB (the field DescribeVolumes actually reports) —
	// a unit-conversion bug here would round-trip clean through the assertions
	// above but not through this one.
	assert.Equal(t, uint64(8)*1024*1024*1024, rep.State.VolumeSize, "persisted VBState.VolumeSize (bytes)")
	assert.NotZero(t, rep.State.BlockSize, "persisted VBState.BlockSize")

	// No I/O has happened yet, so no chunk should exist under the volume's
	// prefix -- a real, listing-derived claim about the bucket, not an
	// inference from ObjectNum or any in-memory count.
	assert.Zero(t, rep.LiveChunkCount, "fresh volume should have no live chunks")
	assert.Zero(t, rep.LiveChunkBytes, "fresh volume should have no live chunk bytes")
}
