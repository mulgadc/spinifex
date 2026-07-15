package handlers_ec2_eip

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func eipTagVal(tags []*ec2.Tag, key string) string {
	for _, tg := range tags {
		if tg.Key != nil && *tg.Key == key {
			return aws.StringValue(tg.Value)
		}
	}
	return ""
}

func TestEIPRecordTagsMirror(t *testing.T) {
	svc, _ := setupTestEIP(t)
	alloc, err := svc.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	allocID := *alloc.AllocationId

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(allocID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("yes")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err := svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		AllocationIds: []*string{aws.String(allocID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Addresses, 1)
	assert.Equal(t, "yes", eipTagVal(out.Addresses[0].Tags, "keep"))
	assert.Equal(t, "v", eipTagVal(out.Addresses[0].Tags, "drop"))

	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(allocID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err = svc.DescribeAddresses(context.Background(), &ec2.DescribeAddressesInput{
		AllocationIds: []*string{aws.String(allocID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Addresses, 1)
	assert.Equal(t, "yes", eipTagVal(out.Addresses[0].Tags, "keep"))
	assert.Empty(t, eipTagVal(out.Addresses[0].Tags, "drop"))
}

func TestEIPRecordTagsMirror_UnownedNoError(t *testing.T) {
	svc, _ := setupTestEIP(t)
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("eipalloc-missing"), aws.String("nat-other")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID))
}
