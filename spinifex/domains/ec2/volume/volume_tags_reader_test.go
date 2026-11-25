package volume

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// describeVolumeRows returns DescribeTags' volume rows for account, read by a
// tags service wired to svc as the daemon wires it.
func describeVolumeRows(t *testing.T, svc *VolumeServiceImpl, cfg *config.Config, store objectstore.ObjectStore, account string, filters ...*ec2.Filter) map[string]map[string]string {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := ec2tags.GetOrCreateTagsBucket(t.Context(), js)
	require.NoError(t, err)
	tags := ec2tags.NewTagsServiceImplWithStore(cfg, store, kv)
	tags.SetVolumeTagReader(svc)

	out, err := tags.DescribeTags(t.Context(), &ec2.DescribeTagsInput{Filters: filters}, account)
	require.NoError(t, err)
	rows := map[string]map[string]string{}
	for _, tag := range out.Tags {
		id := aws.StringValue(tag.ResourceId)
		if rows[id] == nil {
			rows[id] = map[string]string{}
		}
		rows[id][aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	return rows
}

// TestVolumeTags_SnapshotBackedCreateVolumeTagsReachDescribeTags covers the
// snapshot path of CreateVolume, and that another account reads no row.
func TestVolumeTags_SnapshotBackedCreateVolumeTagsReachDescribeTags(t *testing.T) {
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{AZ: providerTestAZ, Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewVolumeServiceImplWithStore(cfg, store, nil)
	svc.SetEBSProvider(newSnapshotSourceProvider())
	writeSnapshotMetadata(t, store, "snap-src", "vol-origin", "000000000001", 8)

	created, err := svc.CreateVolume(context.Background(), &ec2.CreateVolumeInput{
		Size: aws.Int64(8), AvailabilityZone: aws.String(providerTestAZ), SnapshotId: aws.String("snap-src"),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String(ec2.ResourceTypeVolume),
			Tags:         []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("restored")}},
		}},
	}, "000000000001")
	require.NoError(t, err)
	volumeID := aws.StringValue(created.VolumeId)
	byID := &ec2.Filter{Name: aws.String("resource-id"), Values: []*string{created.VolumeId}}

	want := map[string]map[string]string{volumeID: {"Name": "restored"}}
	assert.Equal(t, want, describeVolumeRows(t, svc, cfg, store, "000000000001"))
	assert.Equal(t, want, describeVolumeRows(t, svc, cfg, store, "000000000001", byID))

	assert.Empty(t, describeVolumeRows(t, svc, cfg, store, "000000000002"))
	assert.Empty(t, describeVolumeRows(t, svc, cfg, store, "000000000002", byID))
}
