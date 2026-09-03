package handlers_ec2_snapshot_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/testutil/pagedstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "111122223333"

// TestDeleteSnapshot_RemovesEveryObjectAcrossMultiplePages proves DeleteSnapshot
// follows ListObjectsV2's continuation token to exhaustion. Before this fix it
// read only the first page: above the page size it deleted a partial set of
// objects, reported success, and left the rest orphaned under the snapshot
// prefix.
func TestDeleteSnapshot_RemovesEveryObjectAcrossMultiplePages(t *testing.T) {
	const pageSize = 3
	store := pagedstore.New(pageSize)
	cfg := &config.Config{
		Predastore: config.PredastoreConfig{
			Bucket:    "test-bucket",
			AccessKey: "test-owner-123",
		},
	}
	svc := handlers_ec2_snapshot.NewSnapshotServiceImplWithStore(cfg, store, nil)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc.SetEBSProvider(provider)

	_, err := provider.CreateVolume(context.Background(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-1",
		CapacityRange:    ebsprovider.CapacityRange{RequiredBytes: 10 * 1024 * 1024 * 1024},
		AvailabilityZone: "us-east-1a",
	})
	require.NoError(t, err)
	require.NoError(t, ebsmetadata.NewStore(store, "test-bucket").PutVolume(context.Background(), ebsmetadata.Volume{
		VolumeID: "vol-1", TenantID: testAccountID, CapacityGiB: 10, AvailabilityZone: "us-east-1a",
	}))

	snap, err := svc.CreateSnapshot(context.Background(), &ec2.CreateSnapshotInput{
		VolumeId: aws.String("vol-1"),
	}, testAccountID)
	require.NoError(t, err)
	snapshotID := *snap.SnapshotId

	// Inflate the snapshot's object prefix well past one page: metadata.json
	// (written by CreateSnapshot) plus 10 synthetic block objects.
	const extraObjects = 10
	for i := range extraObjects {
		_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String("test-bucket"),
			Key:    aws.String(fmt.Sprintf("%s/block-%03d", snapshotID, i)),
			Body:   strings.NewReader("x"),
		})
		require.NoError(t, err)
	}

	store.Calls = 0
	_, err = svc.DeleteSnapshot(context.Background(), &ec2.DeleteSnapshotInput{
		SnapshotId: snap.SnapshotId,
	}, testAccountID)
	require.NoError(t, err)

	wantPages := (extraObjects + 1 + pageSize - 1) / pageSize
	assert.GreaterOrEqual(t, store.Calls, wantPages, "the listing must span more than one page for this test to prove anything")

	remaining, err := store.MemoryObjectStore.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String("test-bucket"), Prefix: aws.String(snapshotID + "/"),
	})
	require.NoError(t, err)
	assert.Empty(t, remaining.Contents, "every object under the snapshot prefix must be deleted, not just the first page")
}
