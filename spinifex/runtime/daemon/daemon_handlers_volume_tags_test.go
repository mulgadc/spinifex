package daemon

//test:in-package — the subjects are served from the daemon's own subscription
// table, registered by the unexported subscribeAll.

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2tagsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/tags"
	ec2volumeapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/volume"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	volTagsAccount = "111122223333"
	volTagsBucket  = "test-bucket"
	volTagsAZ      = "ap-southeast-2a"
)

// volumeTagsCluster serves the daemon's subscription table with real volume
// and tag services, and returns a separate client connection that issues
// requests through the same NATS clients the gateway's EC2 dispatch uses.
func volumeTagsCluster(t *testing.T) (*nats.Conn, *ebsmetadata.Store) {
	t.Helper()
	ns, nc, js := testutil.StartTestJetStream(t)
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{AZ: volTagsAZ, Predastore: config.PredastoreConfig{Bucket: volTagsBucket}}

	tagsKV, err := ec2tags.GetOrCreateTagsBucket(t.Context(), js)
	require.NoError(t, err)
	snapshotKV, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "volume-tags-snapshots"})
	require.NoError(t, err)

	d := newSubscribeTestDaemon(t, nc, "", "")
	d.tagsService = ec2tags.NewTagsServiceImplWithStore(cfg, store, tagsKV)
	d.volumeService = ec2volume.NewVolumeServiceImplWithStore(cfg, store, nc, snapshotKV)
	d.volumeService.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	d.tagsService.SetVolumeTagReader(d.volumeService)
	require.NoError(t, d.subscribeAll())
	require.NoError(t, nc.Flush())

	caller, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	t.Cleanup(caller.Close)
	return caller, ebsmetadata.NewStore(store, volTagsBucket)
}

func describeVolumeTags(t *testing.T, caller *nats.Conn, volumeID string) map[string]string {
	t.Helper()
	out, err := ec2tagsapi.DescribeTags(t.Context(), &ec2.DescribeTagsInput{Filters: []*ec2.Filter{{
		Name: aws.String("resource-id"), Values: []*string{aws.String(volumeID)},
	}}}, caller, volTagsAccount)
	require.NoError(t, err)
	got := map[string]string{}
	for _, tag := range out.Tags {
		assert.Equal(t, "volume", aws.StringValue(tag.ResourceType))
		got[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	return got
}

func TestDescribeTags_ReturnsCreateVolumeCreationTags(t *testing.T) {
	caller, _ := volumeTagsCluster(t)

	created, err := ec2volumeapi.CreateVolume(t.Context(), &ec2.CreateVolumeInput{
		Size: aws.Int64(8), AvailabilityZone: aws.String(volTagsAZ),
		TagSpecifications: []*ec2.TagSpecification{{ResourceType: aws.String(ec2.ResourceTypeVolume),
			Tags: []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("data")}}}},
	}, caller, volTagsAccount)
	require.NoError(t, err)

	described, err := ec2volumeapi.DescribeVolumes(t.Context(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{created.VolumeId},
	}, caller, volTagsAccount)
	require.NoError(t, err)
	require.Len(t, described.Volumes, 1)
	require.Len(t, described.Volumes[0].Tags, 1, "DescribeVolumes is the owner's view of the creation tags")

	assert.Equal(t, map[string]string{"Name": "data"}, describeVolumeTags(t, caller, aws.StringValue(created.VolumeId)))
}

// The RunInstances root volume and every volume created before a fix reach
// DescribeTags only through their metadata record, so seed one directly.
func TestDescribeTags_ReturnsTagsOfAVolumeRecord(t *testing.T) {
	caller, metadata := volumeTagsCluster(t)
	const volumeID = "vol-0root0000000000a"
	require.NoError(t, metadata.PutVolume(t.Context(), ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: volTagsAccount, CapacityGiB: 8, State: "in-use",
		AvailabilityZone: volTagsAZ, Tags: map[string]string{"Name": "root-disk"},
	}))

	assert.Equal(t, map[string]string{"Name": "root-disk"}, describeVolumeTags(t, caller, volumeID))
}

func TestDescribeTags_CreateTagsKeepsVolumeCreationTags(t *testing.T) {
	caller, _ := volumeTagsCluster(t)

	created, err := ec2volumeapi.CreateVolume(t.Context(), &ec2.CreateVolumeInput{
		Size: aws.Int64(8), AvailabilityZone: aws.String(volTagsAZ),
		TagSpecifications: []*ec2.TagSpecification{{ResourceType: aws.String(ec2.ResourceTypeVolume),
			Tags: []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("data")}}}},
	}, caller, volTagsAccount)
	require.NoError(t, err)
	_, err = ec2tagsapi.CreateTags(t.Context(), &ec2.CreateTagsInput{
		Resources: []*string{created.VolumeId},
		Tags:      []*ec2.Tag{{Key: aws.String("Team"), Value: aws.String("storage")}},
	}, caller, volTagsAccount)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"Name": "data", "Team": "storage"},
		describeVolumeTags(t, caller, aws.StringValue(created.VolumeId)))
}
