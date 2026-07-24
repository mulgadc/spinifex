package handlers_ec2_routetable

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rtbTagVal(tags []*ec2.Tag, key string) string {
	for _, tg := range tags {
		if tg.Key != nil && *tg.Key == key {
			return aws.StringValue(tg.Value)
		}
	}
	return ""
}

func TestRouteTableRecordTagsMirror(t *testing.T) {
	svc := setupTestService(t)
	rtbID := createTestRtb(t, svc)

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(rtbID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("yes")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err := svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{
		RouteTableIds: []*string{aws.String(rtbID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.RouteTables, 1)
	assert.Equal(t, "yes", rtbTagVal(out.RouteTables[0].Tags, "keep"))
	assert.Equal(t, "v", rtbTagVal(out.RouteTables[0].Tags, "drop"))

	// Value-mismatched delete is a no-op; matched delete removes.
	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(rtbID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err = svc.DescribeRouteTables(t.Context(), &ec2.DescribeRouteTablesInput{
		RouteTableIds: []*string{aws.String(rtbID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.RouteTables, 1)
	assert.Equal(t, "yes", rtbTagVal(out.RouteTables[0].Tags, "keep"))
	assert.Empty(t, rtbTagVal(out.RouteTables[0].Tags, "drop"))
}

func TestRouteTableRecordTagsMirror_UnownedNoError(t *testing.T) {
	svc := setupTestService(t)
	// Absent rtb + non-rtb id: both skipped without error.
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("rtb-missing"), aws.String("vpc-other")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID))
}
