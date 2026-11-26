package snapshot_test

import (
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit/recordingstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A walk of five-item pages returns each of the caller's matching snapshots
// once, and nothing the owner check or a filter excludes.
func TestDescribeSnapshots_PageWalkSeesEachSnapshotOnce(t *testing.T) {
	t.Parallel()
	store := recordingstore.New()
	svc := newByIDService(store)
	want := map[string]bool{}
	for i := range 12 {
		id := "snap-page" + strconv.Itoa(i)
		putByIDSnapshot(t, store, byIDSnapshotDoc(id, byIDAccount))
		want[id] = true
	}
	excluded := byIDSnapshotDoc("snap-excluded", byIDAccount)
	excluded.VolumeID = "vol-other"
	putByIDSnapshot(t, store, excluded)
	putByIDSnapshot(t, store, byIDSnapshotDoc("snap-foreign", byIDOther))
	filters := []*ec2.Filter{{Name: aws.String("volume-id"), Values: aws.StringSlice([]string{"vol-source"})}}

	all, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{Filters: filters}, byIDAccount)
	require.NoError(t, err)
	assert.Len(t, all.Snapshots, len(want), "no MaxResults returns every match")
	assert.Nil(t, all.NextToken)

	seen := map[string]int{}
	var token *string
	pages := 0
	for {
		out, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
			Filters: filters, MaxResults: aws.Int64(5), NextToken: token,
		}, byIDAccount)
		require.NoError(t, err)
		require.LessOrEqual(t, len(out.Snapshots), 5)
		for _, snap := range out.Snapshots {
			seen[aws.StringValue(snap.SnapshotId)]++
		}
		pages++
		if out.NextToken == nil {
			break
		}
		token = out.NextToken
	}
	assert.Equal(t, 3, pages)
	require.Len(t, seen, len(want))
	for id, n := range seen {
		assert.True(t, want[id], "unexpected snapshot %s", id)
		assert.Equal(t, 1, n, "snapshot %s seen more than once", id)
	}
}

func TestDescribeSnapshots_PagingValidation(t *testing.T) {
	t.Parallel()
	store := recordingstore.New()
	svc := newByIDService(store)
	putByIDSnapshot(t, store, byIDSnapshotDoc(byIDSnapshot, byIDAccount))

	_, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
		NextToken: aws.String("garbage"),
	}, byIDAccount)
	code, _, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, code, "a token the service did not issue is refused")

	_, err = svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
		MaxResults: aws.Int64(5), SnapshotIds: aws.StringSlice([]string{byIDSnapshot}),
	}, byIDAccount)
	code, _, ok = awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameterCombination, code)

	// AWS caps a MaxResults above 1000 at one full page rather than refusing it.
	out, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{MaxResults: aws.Int64(5000)}, byIDAccount)
	require.NoError(t, err)
	assert.Len(t, out.Snapshots, 1)
}
