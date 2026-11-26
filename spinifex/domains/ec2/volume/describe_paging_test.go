package volume

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pagingVolumeCount = 12

// newPagingVolumeService seeds pagingVolumeCount modified volumes.
func newPagingVolumeService(t *testing.T) (*VolumeServiceImpl, []string) {
	t.Helper()
	store := objectstore.NewMemoryObjectStore()
	svc := newTestVolumeServiceWithStore("ap-southeast-2a", store)
	ids := make([]string, 0, pagingVolumeCount)
	for i := range pagingVolumeCount {
		id := fmt.Sprintf("vol-%017d", i)
		ids = append(ids, id)
		seedVolumeDocument(t, store, ebsmetadata.Volume{
			VolumeID: id, CapacityGiB: 10, State: "available", VolumeType: "gp3",
			Modification: &ebsmetadata.VolumeModification{ModificationState: "completed", OriginalSize: 5, TargetSize: 10},
		})
	}
	return svc, ids
}

// walkPages calls describe until NextToken is nil and returns every ID seen.
func walkPages(t *testing.T, describe func(token *string) ([]string, *string, error)) []string {
	t.Helper()
	var seen []string
	var token *string
	for range pagingVolumeCount + 1 {
		ids, next, err := describe(token)
		require.NoError(t, err)
		require.LessOrEqual(t, len(ids), 5)
		seen = append(seen, ids...)
		if next == nil {
			return seen
		}
		token = next
	}
	t.Fatal("paging did not end")
	return nil
}

func assertAWSError(t *testing.T, err error, code string) {
	t.Helper()
	got, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "not an AWS error: %v", err)
	assert.Equal(t, code, got)
}

func TestDescribeVolumes_Paging(t *testing.T) {
	svc, ids := newPagingVolumeService(t)
	ctx := context.Background()

	seen := walkPages(t, func(token *string) ([]string, *string, error) {
		out, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{MaxResults: aws.Int64(5), NextToken: token}, testVolAccountID)
		if err != nil {
			return nil, nil, err
		}
		got := make([]string, 0, len(out.Volumes))
		for _, v := range out.Volumes {
			got = append(got, *v.VolumeId)
		}
		return got, out.NextToken, nil
	})
	assert.Equal(t, ids, seen)

	all, err := svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, all.Volumes, pagingVolumeCount)
	assert.Nil(t, all.NextToken)

	for _, in := range []*ec2.DescribeVolumesInput{
		{NextToken: aws.String("garbage")},
		{MaxResults: aws.Int64(4)},
		{MaxResults: aws.Int64(501)},
	} {
		_, err := svc.DescribeVolumes(ctx, in, testVolAccountID)
		assertAWSError(t, err, "InvalidParameterValue")
	}
	_, err = svc.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{MaxResults: aws.Int64(5), VolumeIds: []*string{aws.String(ids[0])}}, testVolAccountID)
	assertAWSError(t, err, "InvalidParameterCombination")
}

func TestDescribeVolumeStatus_Paging(t *testing.T) {
	svc, ids := newPagingVolumeService(t)
	ctx := context.Background()

	seen := walkPages(t, func(token *string) ([]string, *string, error) {
		out, err := svc.DescribeVolumeStatus(ctx, &ec2.DescribeVolumeStatusInput{MaxResults: aws.Int64(5), NextToken: token}, testVolAccountID)
		if err != nil {
			return nil, nil, err
		}
		got := make([]string, 0, len(out.VolumeStatuses))
		for _, v := range out.VolumeStatuses {
			got = append(got, *v.VolumeId)
		}
		return got, out.NextToken, nil
	})
	assert.Equal(t, ids, seen)

	all, err := svc.DescribeVolumeStatus(ctx, &ec2.DescribeVolumeStatusInput{}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, all.VolumeStatuses, pagingVolumeCount)
	assert.Nil(t, all.NextToken)

	for _, in := range []*ec2.DescribeVolumeStatusInput{
		{NextToken: aws.String("garbage")},
		{MaxResults: aws.Int64(4)},
		{MaxResults: aws.Int64(1001)},
	} {
		_, err := svc.DescribeVolumeStatus(ctx, in, testVolAccountID)
		assertAWSError(t, err, "InvalidParameterValue")
	}
	_, err = svc.DescribeVolumeStatus(ctx, &ec2.DescribeVolumeStatusInput{MaxResults: aws.Int64(1000)}, testVolAccountID)
	require.NoError(t, err)
	_, err = svc.DescribeVolumeStatus(ctx, &ec2.DescribeVolumeStatusInput{MaxResults: aws.Int64(5), VolumeIds: []*string{aws.String(ids[0])}}, testVolAccountID)
	assertAWSError(t, err, "InvalidParameterCombination")
}

func TestDescribeVolumesModifications_Paging(t *testing.T) {
	svc, ids := newPagingVolumeService(t)
	ctx := context.Background()

	walk := func(volumeIDs []*string) []string {
		return walkPages(t, func(token *string) ([]string, *string, error) {
			out, err := svc.DescribeVolumesModifications(ctx, &ec2.DescribeVolumesModificationsInput{
				MaxResults: aws.Int64(5), NextToken: token, VolumeIds: volumeIDs,
			}, testVolAccountID)
			if err != nil {
				return nil, nil, err
			}
			got := make([]string, 0, len(out.VolumesModifications))
			for _, m := range out.VolumesModifications {
				got = append(got, *m.VolumeId)
			}
			return got, out.NextToken, nil
		})
	}
	assert.Equal(t, ids, walk(nil))
	// MaxResults with explicit IDs pages them, and a repeated ID is listed once.
	assert.Equal(t, ids, walk(aws.StringSlice(append(ids, ids...))))

	all, err := svc.DescribeVolumesModifications(ctx, &ec2.DescribeVolumesModificationsInput{}, testVolAccountID)
	require.NoError(t, err)
	assert.Len(t, all.VolumesModifications, pagingVolumeCount)
	assert.Nil(t, all.NextToken)

	for _, in := range []*ec2.DescribeVolumesModificationsInput{
		{NextToken: aws.String("garbage")},
		{MaxResults: aws.Int64(4)},
		{MaxResults: aws.Int64(501)},
	} {
		_, err := svc.DescribeVolumesModifications(ctx, in, testVolAccountID)
		assertAWSError(t, err, "InvalidParameterValue")
	}
}
