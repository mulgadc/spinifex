package vpc

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func describeOneENI(t *testing.T, svc *VPCServiceImpl, eniID string) *ec2.NetworkInterface {
	t.Helper()
	out, err := svc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.NetworkInterfaces, 1)
	require.NotNil(t, out.NetworkInterfaces[0].Attachment)
	return out.NetworkInterfaces[0]
}

func TestDescribeNetworkInterfaces_CustomerInstanceAttachmentOwnerIsTheAccount(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)

	_, err := svc.AttachENI(testAccountID, eniID, "i-customer", 0)
	require.NoError(t, err)

	eni := describeOneENI(t, svc, eniID)
	assert.Equal(t, testAccountID, aws.StringValue(eni.Attachment.InstanceOwnerId))
}

func TestDescribeNetworkInterfaces_LBAttachmentOwnerIsAmazonELB(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	eniID := createTestENI(t, svc, subnetID)
	require.NoError(t, svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(eniID)},
		Tags:      []*ec2.Tag{{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByELBv2)}},
	}, testAccountID))

	_, err := svc.AttachENIWithOwner(testAccountID, eniID, "i-lb", 0, "000000000000")
	require.NoError(t, err)

	eni := describeOneENI(t, svc, eniID)
	assert.Equal(t, "amazon-elb", aws.StringValue(eni.Attachment.InstanceOwnerId))
	assert.True(t, aws.BoolValue(eni.RequesterManaged))
}
