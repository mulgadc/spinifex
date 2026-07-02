package handlers_ec2_eigw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func eigwTagVal(tags []*ec2.Tag, key string) string {
	for _, tg := range tags {
		if tg.Key != nil && *tg.Key == key {
			return aws.StringValue(tg.Value)
		}
	}
	return ""
}

func TestEIGWRecordTagsMirror(t *testing.T) {
	svc := setupTestEIGWService(t)
	eigwID := createTestEIGW(t, svc)

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(eigwID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("yes")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err := svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		EgressOnlyInternetGatewayIds: []*string{aws.String(eigwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.EgressOnlyInternetGateways, 1)
	assert.Equal(t, "yes", eigwTagVal(out.EgressOnlyInternetGateways[0].Tags, "keep"))
	assert.Equal(t, "v", eigwTagVal(out.EgressOnlyInternetGateways[0].Tags, "drop"))

	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(eigwID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err = svc.DescribeEgressOnlyInternetGateways(&ec2.DescribeEgressOnlyInternetGatewaysInput{
		EgressOnlyInternetGatewayIds: []*string{aws.String(eigwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.EgressOnlyInternetGateways, 1)
	assert.Equal(t, "yes", eigwTagVal(out.EgressOnlyInternetGateways[0].Tags, "keep"))
	assert.Equal(t, "", eigwTagVal(out.EgressOnlyInternetGateways[0].Tags, "drop"))
}

func TestEIGWRecordTagsMirror_UnownedNoError(t *testing.T) {
	svc := setupTestEIGWService(t)
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("eigw-missing"), aws.String("igw-other")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID))
}
