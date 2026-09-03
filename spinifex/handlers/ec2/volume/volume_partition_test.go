// These tests reach svc.metadata and the unexported describes, all
// package-internal.
//
//test:in-package
package handlers_ec2_volume

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/testutil/recordingstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const otherVolAccountID = "210987654321"

// volumePartitionService returns a service over a recording store holding one
// volume for each of two accounts, with the seeding writes already forgotten.
func volumePartitionService(t *testing.T) (*VolumeServiceImpl, *recordingstore.Store) {
	t.Helper()
	objects := recordingstore.New()
	cfg := &config.Config{AZ: "ap-southeast-2a", Predastore: config.PredastoreConfig{Bucket: "test-bucket"}}
	svc := NewVolumeServiceImplWithStore(cfg, objects, nil)

	ctx := context.Background()
	for _, volume := range []ebsmetadata.Volume{
		{VolumeID: "vol-mine", TenantID: testVolAccountID, CapacityGiB: 8, State: "available",
			AvailabilityZone: "ap-southeast-2a"},
		{VolumeID: "vol-theirs", TenantID: otherVolAccountID, CapacityGiB: 16, State: "available",
			AvailabilityZone: "ap-southeast-2a"},
	} {
		require.NoError(t, svc.metadata.PutVolume(ctx, volume))
	}
	objects.Reset()
	return svc, objects
}

// otherAccountPrefix is spelled out rather than derived: it is the layout claim
// itself, and deriving it from the code under test would assert nothing.
const otherAccountPrefix = "spinifex/ebsmetadata/v2/volumes/" + otherVolAccountID + "/"

// Isolation must be enforced *by* the read, not by a filter after it. A
// whole-tree listing plus a correct in-memory filter would return the same
// volumes, so this asserts the access pattern: the listing prefix is the
// caller's own, and no key under another account's prefix is fetched.
func TestDescribeVolumes_TouchesNoOtherAccountsPrefix(t *testing.T) {
	svc, objects := volumePartitionService(t)

	listed, err := svc.DescribeVolumes(context.Background(), &ec2.DescribeVolumesInput{}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, listed.Volumes, 1)
	assert.Equal(t, "vol-mine", aws.StringValue(listed.Volumes[0].VolumeId))

	assert.False(t, objects.TouchedPrefix(otherAccountPrefix),
		"a describe must not read or list another account's prefix: %v %v", objects.ListPrefixes(), objects.Gets())
}

func TestDescribeVolumeStatus_TouchesNoOtherAccountsPrefix(t *testing.T) {
	svc, objects := volumePartitionService(t)

	listed, err := svc.DescribeVolumeStatus(context.Background(), &ec2.DescribeVolumeStatusInput{}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, listed.VolumeStatuses, 1)
	assert.Equal(t, "vol-mine", aws.StringValue(listed.VolumeStatuses[0].VolumeId))

	assert.False(t, objects.TouchedPrefix(otherAccountPrefix),
		"a status describe must not read or list another account's prefix: %v %v", objects.ListPrefixes(), objects.Gets())
}

func TestDescribeVolumesModifications_TouchesNoOtherAccountsPrefix(t *testing.T) {
	svc, objects := volumePartitionService(t)

	_, err := svc.DescribeVolumesModifications(context.Background(), &ec2.DescribeVolumesModificationsInput{}, testVolAccountID)
	require.NoError(t, err)

	assert.False(t, objects.TouchedPrefix(otherAccountPrefix),
		"a modifications describe must not read or list another account's prefix: %v %v", objects.ListPrefixes(), objects.Gets())
}

// Given explicit IDs the status describe fetches each document, exactly as
// DescribeVolumes does. Enumerating the prefix to answer for one volume is the
// bug this fast path exists to remove, so the assertion is that nothing lists.
func TestDescribeVolumeStatus_ByIDDoesNotList(t *testing.T) {
	svc, objects := volumePartitionService(t)

	listed, err := svc.DescribeVolumeStatus(context.Background(),
		&ec2.DescribeVolumeStatusInput{VolumeIds: []*string{aws.String("vol-mine")}}, testVolAccountID)
	require.NoError(t, err)
	require.Len(t, listed.VolumeStatuses, 1)
	assert.Equal(t, "vol-mine", aws.StringValue(listed.VolumeStatuses[0].VolumeId))

	assert.Empty(t, objects.ListPrefixes(), "a by-ID status describe must fetch documents, not enumerate a prefix")
}

// A volume outside the caller's prefix is absent rather than denied: the key is
// the ownership check, and the by-ID path reports it as not-found.
func TestDescribeVolumeStatus_ByIDOtherAccountIsNotFound(t *testing.T) {
	svc, _ := volumePartitionService(t)

	_, err := svc.DescribeVolumeStatus(context.Background(),
		&ec2.DescribeVolumeStatusInput{VolumeIds: []*string{aws.String("vol-theirs")}}, testVolAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidVolume.NotFound")
}

// A caller with no account cannot name a prefix, so the listing fails rather
// than widening to every account's volumes.
func TestVolumeDescribes_RefuseACallerWithNoAccount(t *testing.T) {
	svc, _ := volumePartitionService(t)
	ctx := context.Background()

	for name, describe := range map[string]func() error{
		"DescribeVolumes": func() error {
			_, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{}, "")
			return err
		},
		"DescribeVolumeStatus": func() error {
			_, err := svc.DescribeVolumeStatus(ctx, &ec2.DescribeVolumeStatusInput{}, "")
			return err
		},
		"DescribeVolumesModifications": func() error {
			_, err := svc.DescribeVolumesModifications(ctx, &ec2.DescribeVolumesModificationsInput{}, "")
			return err
		},
	} {
		require.Error(t, describe(), name)
	}
}

// The whole-cluster verb is what the leak reaper and the admin scans want, and
// it must reach every account's documents rather than the caller's alone.
func TestListAllVolumes_ReachesEveryAccount(t *testing.T) {
	svc, objects := volumePartitionService(t)

	all, err := svc.metadata.ListAllVolumes(context.Background())
	require.NoError(t, err)

	ids := make([]string, 0, len(all))
	for _, volume := range all {
		ids = append(ids, volume.VolumeID)
	}
	assert.ElementsMatch(t, []string{"vol-mine", "vol-theirs"}, ids)
	assert.Equal(t, []string{"spinifex/ebsmetadata/v2/volumes/"}, objects.ListPrefixes(),
		"the whole-cluster verb lists across accounts, not under one")
}
