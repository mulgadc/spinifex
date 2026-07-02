package handlers_ec2_natgw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func natgwTagVal(tags []*ec2.Tag, key string) string {
	for _, tg := range tags {
		if tg.Key != nil && *tg.Key == key {
			return aws.StringValue(tg.Value)
		}
	}
	return ""
}

func TestNatGatewayRecordTagsMirror(t *testing.T) {
	svc := setupTestService(t)
	natgwID := createTestNatGateway(t, svc)

	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(natgwID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("yes")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err := svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		NatGatewayIds: []*string{aws.String(natgwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NatGateways, 1)
	assert.Equal(t, "yes", natgwTagVal(out.NatGateways[0].Tags, "keep"))
	assert.Equal(t, "v", natgwTagVal(out.NatGateways[0].Tags, "drop"))

	require.NoError(t, svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(natgwID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("keep"), Value: aws.String("wrong")},
			{Key: aws.String("drop"), Value: aws.String("v")},
		},
	}, testAccountID))

	out, err = svc.DescribeNatGateways(&ec2.DescribeNatGatewaysInput{
		NatGatewayIds: []*string{aws.String(natgwID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NatGateways, 1)
	assert.Equal(t, "yes", natgwTagVal(out.NatGateways[0].Tags, "keep"))
	assert.Equal(t, "", natgwTagVal(out.NatGateways[0].Tags, "drop"))
}

func TestNatGatewayRecordTagsMirror_UnownedNoError(t *testing.T) {
	svc := setupTestService(t)
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String("nat-missing"), aws.String("igw-other")},
		Tags:      []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID))
}
