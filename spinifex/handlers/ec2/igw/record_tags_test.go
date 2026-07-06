package handlers_ec2_igw

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func igwTagVal(tags []*ec2.Tag, key string) string {
	for _, tg := range tags {
		if tg.Key != nil && *tg.Key == key {
			return aws.StringValue(tg.Value)
		}
	}
	return ""
}

func TestIGWRecordTagsMirror(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	igwID := createTestIGW(t, svc)

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(igwID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("yes")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err := svc.DescribeInternetGateways(context.Background(), &ec2.DescribeInternetGatewaysInput{
		InternetGatewayIds: []*string{aws.String(igwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.InternetGateways, 1)
	assert.Equal(t, "yes", igwTagVal(out.InternetGateways[0].Tags, "keep"))
	assert.Equal(t, "v", igwTagVal(out.InternetGateways[0].Tags, "drop"))

	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(igwID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err = svc.DescribeInternetGateways(context.Background(), &ec2.DescribeInternetGatewaysInput{
		InternetGatewayIds: []*string{aws.String(igwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.InternetGateways, 1)
	assert.Equal(t, "yes", igwTagVal(out.InternetGateways[0].Tags, "keep"))
	assert.Equal(t, "", igwTagVal(out.InternetGateways[0].Tags, "drop"))
}

func TestIGWRecordTagsMirror_UnownedNoError(t *testing.T) {
	svc, _ := setupTestIGWService(t)
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("igw-missing"), aws.String("eigw-other")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID))
}
