package handlers_ec2_snapshot

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotRecordTagsMirror(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	snapID := createTestSnapshot(t, svc, store, "vol-tagmirror", 10, nil)

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(snapID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("yes")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	cfg, err := svc.getSnapshotConfig(snapID)
	require.NoError(t, err)
	assert.Equal(t, "yes", cfg.Tags["keep"])
	assert.Equal(t, "v", cfg.Tags["drop"])

	// Value-mismatched delete is a no-op; matched delete removes.
	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(snapID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	cfg, err = svc.getSnapshotConfig(snapID)
	require.NoError(t, err)
	assert.Equal(t, "yes", cfg.Tags["keep"])
	_, ok := cfg.Tags["drop"]
	assert.False(t, ok)
}

func TestSnapshotRecordTagsMirror_CrossTenantNoMutation(t *testing.T) {
	svc, store := setupTestSnapshotService(t)
	snapID := createTestSnapshot(t, svc, store, "vol-tenantguard", 10, map[string]string{"orig": "1"})

	// A different account tagging this snapshot must not mutate the record.
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(snapID)},
		Tags:      []*ec2.Tag{{Key: aws.String("evil"), Value: aws.String("1")}},
	}, "999999999999"))

	cfg, err := svc.getSnapshotConfig(snapID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"orig": "1"}, cfg.Tags)
}

func TestSnapshotRecordTagsMirror_UnownedNoError(t *testing.T) {
	svc, _ := setupTestSnapshotService(t)
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("snap-missing"), aws.String("vol-other")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID))
}
