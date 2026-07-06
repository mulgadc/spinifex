package handlers_ec2_image

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTagsInput(imageID string, tags map[string]string) *ec2.CreateTagsInput {
	in := &ec2.CreateTagsInput{Resources: []*string{aws.String(imageID)}}
	for k, v := range tags {
		in.Tags = append(in.Tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return in
}

func TestApplyRecordTags_OwnedAMI(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-owned", "img")

	err := svc.ApplyRecordTags(createTagsInput("ami-owned", map[string]string{"env": "prod"}), testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(context.Background(), "ami-owned")
	require.NoError(t, err)
	assert.Equal(t, "prod", meta.Tags["env"])
}

func TestApplyRecordTags_NotOwnedSkipped(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithOwner(t, store, "ami-foreign", "img", "999999999999")

	err := svc.ApplyRecordTags(createTagsInput("ami-foreign", map[string]string{"env": "prod"}), testAccountID)
	require.NoError(t, err)

	meta, err := svc.GetAMIConfig(context.Background(), "ami-foreign")
	require.NoError(t, err)
	assert.NotContains(t, meta.Tags, "env")
}

func TestApplyRecordTags_AbsentAMINoop(t *testing.T) {
	svc, _ := setupTestImageService(t)
	err := svc.ApplyRecordTags(createTagsInput("ami-missing", map[string]string{"env": "prod"}), testAccountID)
	require.NoError(t, err)
}

func TestApplyRecordTags_NonAMIResourceSkipped(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-owned", "img")

	in := &ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-123"), aws.String("ami-owned")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}
	require.NoError(t, svc.ApplyRecordTags(in, testAccountID))

	meta, err := svc.GetAMIConfig(context.Background(), "ami-owned")
	require.NoError(t, err)
	assert.Equal(t, "v", meta.Tags["k"])
}

func TestRemoveRecordTags_ValueMatchAndMismatch(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-owned", "img")
	require.NoError(t, svc.ApplyRecordTags(createTagsInput("ami-owned", map[string]string{"a": "1", "b": "2"}), testAccountID))

	// value mismatch keeps; value match deletes
	in := &ec2.DeleteTagsInput{
		Resources: []*string{aws.String("ami-owned")},
		Tags: []*ec2.Tag{
			{Key: aws.String("a"), Value: aws.String("wrong")},
			{Key: aws.String("b"), Value: aws.String("2")},
		},
	}
	require.NoError(t, svc.RemoveRecordTags(in, testAccountID))

	meta, err := svc.GetAMIConfig(context.Background(), "ami-owned")
	require.NoError(t, err)
	assert.Equal(t, "1", meta.Tags["a"])
	assert.NotContains(t, meta.Tags, "b")
}

func TestRemoveRecordTags_NilValueUnconditional(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-owned", "img")
	require.NoError(t, svc.ApplyRecordTags(createTagsInput("ami-owned", map[string]string{"a": "1"}), testAccountID))

	in := &ec2.DeleteTagsInput{
		Resources: []*string{aws.String("ami-owned")},
		Tags:      []*ec2.Tag{{Key: aws.String("a")}},
	}
	require.NoError(t, svc.RemoveRecordTags(in, testAccountID))

	meta, err := svc.GetAMIConfig(context.Background(), "ami-owned")
	require.NoError(t, err)
	assert.NotContains(t, meta.Tags, "a")
}

func TestRemoveRecordTags_EmptyClearsAll(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithName(t, store, "ami-owned", "img")
	require.NoError(t, svc.ApplyRecordTags(createTagsInput("ami-owned", map[string]string{"a": "1", "b": "2"}), testAccountID))

	in := &ec2.DeleteTagsInput{Resources: []*string{aws.String("ami-owned")}}
	require.NoError(t, svc.RemoveRecordTags(in, testAccountID))

	meta, err := svc.GetAMIConfig(context.Background(), "ami-owned")
	require.NoError(t, err)
	assert.Empty(t, meta.Tags)
}

func TestRemoveRecordTags_NotOwnedSkipped(t *testing.T) {
	svc, store := setupTestImageService(t)
	createTestAMIConfigWithOwner(t, store, "ami-foreign", "img", "999999999999")

	in := &ec2.DeleteTagsInput{Resources: []*string{aws.String("ami-foreign")}}
	require.NoError(t, svc.RemoveRecordTags(in, testAccountID))
	// foreign AMI untouched (still readable, no panic / error)
	_, err := svc.GetAMIConfig(context.Background(), "ami-foreign")
	require.NoError(t, err)
}
