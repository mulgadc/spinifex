package handlers_ec2_vpc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateVpc_ProjectsTagsToCentralStore(t *testing.T) {
	t.Parallel()
	svc := handlers_ec2_vpc.SetupTestVPCService(t)
	writer := &handlers_ec2_vpc.FakeCentralTagStore{}
	svc.SetCentralTagStore(writer)

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
	}, handlers_ec2_vpc.TestAccountID)
	require.NoError(t, err)

	vpcID := *out.Vpc.VpcId
	require.Contains(t, writer.Calls, vpcID)
	assert.Equal(t, map[string]string{"Name": "my-vpc"}, writer.Calls[vpcID])
}

func TestCreateSubnet_ProjectsTagsToCentralStore(t *testing.T) {
	t.Parallel()
	svc := handlers_ec2_vpc.SetupTestVPCService(t)
	writer := &handlers_ec2_vpc.FakeCentralTagStore{}
	svc.SetCentralTagStore(writer)
	vpcID := handlers_ec2_vpc.CreateTestVPC(t, svc, "10.0.0.0/16")

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
	}, handlers_ec2_vpc.TestAccountID)
	require.NoError(t, err)

	subnetID := *out.Subnet.SubnetId
	require.Contains(t, writer.Calls, subnetID)
	assert.Equal(t, map[string]string{"Name": "my-subnet"}, writer.Calls[subnetID])
}

func TestCreateSecurityGroup_ProjectsTagsToCentralStore(t *testing.T) {
	t.Parallel()
	svc := handlers_ec2_vpc.SetupTestVPCService(t)
	writer := &handlers_ec2_vpc.FakeCentralTagStore{}
	svc.SetCentralTagStore(writer)
	vpcID := handlers_ec2_vpc.CreateTestVPC(t, svc, "10.0.0.0/16")

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
	}, handlers_ec2_vpc.TestAccountID)
	require.NoError(t, err)

	groupID := *out.GroupId
	require.Contains(t, writer.Calls, groupID)
	assert.Equal(t, map[string]string{"Name": "my-sg"}, writer.Calls[groupID])
}

func TestCreateVpc_NoTags_SkipsCentralStoreWrite(t *testing.T) {
	t.Parallel()
	svc := handlers_ec2_vpc.SetupTestVPCService(t)
	writer := &handlers_ec2_vpc.FakeCentralTagStore{}
	svc.SetCentralTagStore(writer)

	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, handlers_ec2_vpc.TestAccountID)
	require.NoError(t, err)

	assert.NotContains(t, writer.Calls, *out.Vpc.VpcId)
}

func TestCreateVpc_NilCentralTagStore_CreateSucceeds(t *testing.T) {
	t.Parallel()
	svc := handlers_ec2_vpc.SetupTestVPCService(t)
	// No SetCentralTagStore call: centralTags stays nil.

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
	}, handlers_ec2_vpc.TestAccountID)
	require.NoError(t, err)
	require.Len(t, out.Vpc.Tags, 1)
}

func TestCreateVpc_CentralTagStoreError_DoesNotFailCreate(t *testing.T) {
	t.Parallel()
	svc := handlers_ec2_vpc.SetupTestVPCService(t)
	writer := &handlers_ec2_vpc.FakeCentralTagStore{Err: errors.New("central store unavailable")}
	svc.SetCentralTagStore(writer)

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
	}, handlers_ec2_vpc.TestAccountID)
	require.NoError(t, err)
	require.Len(t, out.Vpc.Tags, 1)
}

// Deleting a resource must retire its central tag entry, whatever the type.
// The table drives each delete path through the same store so the assertion is
// about the shared teardown rather than one handler: a type added to the
// package without a clear call fails here rather than leaking quietly.
func TestDelete_ClearsCentralTagStore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// create returns the id of a resource of this type, already tagged.
		create func(t *testing.T, svc *handlers_ec2_vpc.VPCServiceImpl) string
		del    func(t *testing.T, svc *handlers_ec2_vpc.VPCServiceImpl, id string)
	}{
		{
			name: "security group",
			create: func(t *testing.T, svc *handlers_ec2_vpc.VPCServiceImpl) string {
				vpcID := handlers_ec2_vpc.CreateTestVPC(t, svc, "10.1.0.0/16")
				out, err := svc.CreateSecurityGroup(context.Background(), &ec2.CreateSecurityGroupInput{
					GroupName:   aws.String("doomed-sg"),
					Description: aws.String("doomed"),
					VpcId:       aws.String(vpcID),
					TagSpecifications: []*ec2.TagSpecification{
						{ResourceType: aws.String("security-group"), Tags: []*ec2.Tag{
							{Key: aws.String("Name"), Value: aws.String("doomed")},
						}},
					},
				}, handlers_ec2_vpc.TestAccountID)
				require.NoError(t, err)
				return *out.GroupId
			},
			del: func(t *testing.T, svc *handlers_ec2_vpc.VPCServiceImpl, id string) {
				_, err := svc.DeleteSecurityGroup(context.Background(),
					&ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)}, handlers_ec2_vpc.TestAccountID)
				require.NoError(t, err)
			},
		},
		{
			name: "subnet",
			create: func(t *testing.T, svc *handlers_ec2_vpc.VPCServiceImpl) string {
				vpcID := handlers_ec2_vpc.CreateTestVPC(t, svc, "10.2.0.0/16")
				out, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
					VpcId:     aws.String(vpcID),
					CidrBlock: aws.String("10.2.1.0/24"),
					TagSpecifications: []*ec2.TagSpecification{
						{ResourceType: aws.String("subnet"), Tags: []*ec2.Tag{
							{Key: aws.String("Name"), Value: aws.String("doomed")},
						}},
					},
				}, handlers_ec2_vpc.TestAccountID)
				require.NoError(t, err)
				return *out.Subnet.SubnetId
			},
			del: func(t *testing.T, svc *handlers_ec2_vpc.VPCServiceImpl, id string) {
				_, err := svc.DeleteSubnet(context.Background(),
					&ec2.DeleteSubnetInput{SubnetId: aws.String(id)}, handlers_ec2_vpc.TestAccountID)
				require.NoError(t, err)
			},
		},
		{
			// The interface that first exposed this gap. Both the public delete
			// and the forced instance teardown run the same path, so covering
			// one covers the ENI reclaimed by a task going away.
			name: "network interface",
			create: func(t *testing.T, svc *handlers_ec2_vpc.VPCServiceImpl) string {
				vpcID := handlers_ec2_vpc.CreateTestVPC(t, svc, "10.7.0.0/16")
				subnetID := handlers_ec2_vpc.CreateTestSubnet(t, svc, vpcID, "10.7.1.0/24")
				out, err := svc.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
					SubnetId: aws.String(subnetID),
					TagSpecifications: []*ec2.TagSpecification{
						{ResourceType: aws.String("network-interface"), Tags: []*ec2.Tag{
							{Key: aws.String("Name"), Value: aws.String("doomed")},
						}},
					},
				}, handlers_ec2_vpc.TestAccountID)
				require.NoError(t, err)
				return *out.NetworkInterface.NetworkInterfaceId
			},
			del: func(t *testing.T, svc *handlers_ec2_vpc.VPCServiceImpl, id string) {
				_, err := svc.DeleteNetworkInterface(context.Background(),
					&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(id)}, handlers_ec2_vpc.TestAccountID)
				require.NoError(t, err)
			},
		},
		{
			name: "vpc",
			create: func(t *testing.T, svc *handlers_ec2_vpc.VPCServiceImpl) string {
				out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
					CidrBlock: aws.String("10.3.0.0/16"),
					TagSpecifications: []*ec2.TagSpecification{
						{ResourceType: aws.String("vpc"), Tags: []*ec2.Tag{
							{Key: aws.String("Name"), Value: aws.String("doomed")},
						}},
					},
				}, handlers_ec2_vpc.TestAccountID)
				require.NoError(t, err)
				return *out.Vpc.VpcId
			},
			del: func(t *testing.T, svc *handlers_ec2_vpc.VPCServiceImpl, id string) {
				_, err := svc.DeleteVpc(context.Background(),
					&ec2.DeleteVpcInput{VpcId: aws.String(id)}, handlers_ec2_vpc.TestAccountID)
				require.NoError(t, err)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := handlers_ec2_vpc.SetupTestVPCService(t)
			store := &handlers_ec2_vpc.FakeCentralTagStore{}
			svc.SetCentralTagStore(store)

			id := tc.create(t, svc)
			require.Contains(t, store.Calls, id, "precondition: the create must have tagged it")

			tc.del(t, svc, id)
			assert.Contains(t, store.Deleted, id, "the deleted resource's tags must be retired")
		})
	}
}

// DeleteVpc cascades the default security group, which no caller ever deletes
// by hand. Asserted separately because the id is not the one under test.
func TestDeleteVpc_ClearsTheCascadedDefaultSecurityGroup(t *testing.T) {
	t.Parallel()
	svc := handlers_ec2_vpc.SetupTestVPCService(t)
	store := &handlers_ec2_vpc.FakeCentralTagStore{}
	svc.SetCentralTagStore(store)

	vpcID := handlers_ec2_vpc.CreateTestVPC(t, svc, "10.4.0.0/16")
	defaultSG, err := svc.FindDefaultSGForVPC(handlers_ec2_vpc.TestAccountID, vpcID)
	require.NoError(t, err)
	require.NotEmpty(t, defaultSG, "precondition: CreateVpc makes a default SG")

	_, err = svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, handlers_ec2_vpc.TestAccountID)
	require.NoError(t, err)

	assert.Contains(t, store.Deleted, defaultSG, "the cascaded default SG must not keep its tags")
}

func TestDelete_NilCentralTagStore_DeleteSucceeds(t *testing.T) {
	t.Parallel()
	svc := handlers_ec2_vpc.SetupTestVPCService(t)
	// No SetCentralTagStore call: centralTags stays nil.

	vpcID := handlers_ec2_vpc.CreateTestVPC(t, svc, "10.5.0.0/16")
	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, handlers_ec2_vpc.TestAccountID)
	require.NoError(t, err)
}

// The resource is already gone by the time the store is called, so a store
// failure must not fail the delete: the caller cannot retry something that has
// happened, and would be left believing the resource survived.
func TestDelete_CentralTagStoreError_DoesNotFailDelete(t *testing.T) {
	t.Parallel()
	svc := handlers_ec2_vpc.SetupTestVPCService(t)
	store := &handlers_ec2_vpc.FakeCentralTagStore{Err: errors.New("central store unavailable")}
	svc.SetCentralTagStore(store)

	vpcID := handlers_ec2_vpc.CreateTestVPC(t, svc, "10.6.0.0/16")
	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, handlers_ec2_vpc.TestAccountID)
	require.NoError(t, err)
	assert.Contains(t, store.Deleted, vpcID, "the clear must still have been attempted")
}
