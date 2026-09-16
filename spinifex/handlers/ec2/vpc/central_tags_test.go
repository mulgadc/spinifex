package handlers_ec2_vpc

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCentralTagWriter records the tag map passed to PutResourceTags per
// resource, and can be made to fail so tests can prove a central-store error
// never fails the create it is projecting from.
type fakeCentralTagWriter struct {
	calls map[string]map[string]string
	err   error
}

func (f *fakeCentralTagWriter) PutResourceTags(_ context.Context, _, resourceID string, tags map[string]string) error {
	if f.calls == nil {
		f.calls = map[string]map[string]string{}
	}
	f.calls[resourceID] = tags
	return f.err
}

func TestCreateVpc_ProjectsTagsToCentralStore(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	writer := &fakeCentralTagWriter{}
	svc.SetCentralTagWriter(writer)

	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("vpc"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("my-vpc")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)

	vpcID := *out.Vpc.VpcId
	require.Contains(t, writer.calls, vpcID)
	assert.Equal(t, map[string]string{"Name": "my-vpc"}, writer.calls[vpcID])
}

func TestCreateSubnet_ProjectsTagsToCentralStore(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	writer := &fakeCentralTagWriter{}
	svc.SetCentralTagWriter(writer)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     aws.String(vpcID),
		CidrBlock: aws.String("10.0.1.0/24"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("subnet"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("my-subnet")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)

	subnetID := *out.Subnet.SubnetId
	require.Contains(t, writer.calls, subnetID)
	assert.Equal(t, map[string]string{"Name": "my-subnet"}, writer.calls[subnetID])
}

func TestCreateSecurityGroup_ProjectsTagsToCentralStore(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	writer := &fakeCentralTagWriter{}
	svc.SetCentralTagWriter(writer)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.CreateSecurityGroup(context.Background(), &ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("web-sg"),
		Description: aws.String("web sg"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("security-group"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("my-sg")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)

	groupID := *out.GroupId
	require.Contains(t, writer.calls, groupID)
	assert.Equal(t, map[string]string{"Name": "my-sg"}, writer.calls[groupID])
}

func TestCreateVpc_NoTags_SkipsCentralStoreWrite(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	writer := &fakeCentralTagWriter{}
	svc.SetCentralTagWriter(writer)

	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	assert.NotContains(t, writer.calls, *out.Vpc.VpcId)
}

func TestCreateVpc_NilCentralTagWriter_CreateSucceeds(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	// No SetCentralTagWriter call: centralTags stays nil.

	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("vpc"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("my-vpc")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Vpc.Tags, 1)
}

func TestCreateVpc_CentralTagWriterError_DoesNotFailCreate(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	writer := &fakeCentralTagWriter{err: errors.New("central store unavailable")}
	svc.SetCentralTagWriter(writer)

	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("vpc"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("my-vpc")},
				},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Vpc.Tags, 1)
}
