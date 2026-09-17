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

// fakeCentralTagStore records the tag map passed to PutResourceTags per
// resource and the ids passed to DeleteAllTags, and can be made to fail so
// tests can prove a central-store error never fails the operation it tracks.
type fakeCentralTagStore struct {
	calls   map[string]map[string]string
	deleted []string
	err     error
}

func (f *fakeCentralTagStore) PutResourceTags(_ context.Context, _, resourceID string, tags map[string]string) error {
	if f.calls == nil {
		f.calls = map[string]map[string]string{}
	}
	f.calls[resourceID] = tags
	return f.err
}

func (f *fakeCentralTagStore) DeleteAllTags(_ context.Context, _, resourceID string) error {
	f.deleted = append(f.deleted, resourceID)
	return f.err
}

func TestCreateVpc_ProjectsTagsToCentralStore(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	writer := &fakeCentralTagStore{}
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
	}, testAccountID)
	require.NoError(t, err)

	vpcID := *out.Vpc.VpcId
	require.Contains(t, writer.calls, vpcID)
	assert.Equal(t, map[string]string{"Name": "my-vpc"}, writer.calls[vpcID])
}

func TestCreateSubnet_ProjectsTagsToCentralStore(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	writer := &fakeCentralTagStore{}
	svc.SetCentralTagStore(writer)
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
	writer := &fakeCentralTagStore{}
	svc.SetCentralTagStore(writer)
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
	writer := &fakeCentralTagStore{}
	svc.SetCentralTagStore(writer)

	out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	assert.NotContains(t, writer.calls, *out.Vpc.VpcId)
}

func TestCreateVpc_NilCentralTagStore_CreateSucceeds(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
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
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Vpc.Tags, 1)
}

func TestCreateVpc_CentralTagStoreError_DoesNotFailCreate(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	writer := &fakeCentralTagStore{err: errors.New("central store unavailable")}
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
	}, testAccountID)
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
		create func(t *testing.T, svc *VPCServiceImpl) string
		del    func(t *testing.T, svc *VPCServiceImpl, id string)
	}{
		{
			name: "security group",
			create: func(t *testing.T, svc *VPCServiceImpl) string {
				vpcID := createTestVPC(t, svc, "10.1.0.0/16")
				out, err := svc.CreateSecurityGroup(context.Background(), &ec2.CreateSecurityGroupInput{
					GroupName:   aws.String("doomed-sg"),
					Description: aws.String("doomed"),
					VpcId:       aws.String(vpcID),
					TagSpecifications: []*ec2.TagSpecification{
						{ResourceType: aws.String("security-group"), Tags: []*ec2.Tag{
							{Key: aws.String("Name"), Value: aws.String("doomed")},
						}},
					},
				}, testAccountID)
				require.NoError(t, err)
				return *out.GroupId
			},
			del: func(t *testing.T, svc *VPCServiceImpl, id string) {
				_, err := svc.DeleteSecurityGroup(context.Background(),
					&ec2.DeleteSecurityGroupInput{GroupId: aws.String(id)}, testAccountID)
				require.NoError(t, err)
			},
		},
		{
			name: "subnet",
			create: func(t *testing.T, svc *VPCServiceImpl) string {
				vpcID := createTestVPC(t, svc, "10.2.0.0/16")
				out, err := svc.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
					VpcId:     aws.String(vpcID),
					CidrBlock: aws.String("10.2.1.0/24"),
					TagSpecifications: []*ec2.TagSpecification{
						{ResourceType: aws.String("subnet"), Tags: []*ec2.Tag{
							{Key: aws.String("Name"), Value: aws.String("doomed")},
						}},
					},
				}, testAccountID)
				require.NoError(t, err)
				return *out.Subnet.SubnetId
			},
			del: func(t *testing.T, svc *VPCServiceImpl, id string) {
				_, err := svc.DeleteSubnet(context.Background(),
					&ec2.DeleteSubnetInput{SubnetId: aws.String(id)}, testAccountID)
				require.NoError(t, err)
			},
		},
		{
			// The interface that first exposed this gap. Both the public delete
			// and the forced instance teardown run the same path, so covering
			// one covers the ENI reclaimed by a task going away.
			name: "network interface",
			create: func(t *testing.T, svc *VPCServiceImpl) string {
				vpcID := createTestVPC(t, svc, "10.7.0.0/16")
				subnetID := createTestSubnet(t, svc, vpcID, "10.7.1.0/24")
				out, err := svc.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
					SubnetId: aws.String(subnetID),
					TagSpecifications: []*ec2.TagSpecification{
						{ResourceType: aws.String("network-interface"), Tags: []*ec2.Tag{
							{Key: aws.String("Name"), Value: aws.String("doomed")},
						}},
					},
				}, testAccountID)
				require.NoError(t, err)
				return *out.NetworkInterface.NetworkInterfaceId
			},
			del: func(t *testing.T, svc *VPCServiceImpl, id string) {
				_, err := svc.DeleteNetworkInterface(context.Background(),
					&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(id)}, testAccountID)
				require.NoError(t, err)
			},
		},
		{
			name: "vpc",
			create: func(t *testing.T, svc *VPCServiceImpl) string {
				out, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{
					CidrBlock: aws.String("10.3.0.0/16"),
					TagSpecifications: []*ec2.TagSpecification{
						{ResourceType: aws.String("vpc"), Tags: []*ec2.Tag{
							{Key: aws.String("Name"), Value: aws.String("doomed")},
						}},
					},
				}, testAccountID)
				require.NoError(t, err)
				return *out.Vpc.VpcId
			},
			del: func(t *testing.T, svc *VPCServiceImpl, id string) {
				_, err := svc.DeleteVpc(context.Background(),
					&ec2.DeleteVpcInput{VpcId: aws.String(id)}, testAccountID)
				require.NoError(t, err)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := setupTestVPCService(t)
			store := &fakeCentralTagStore{}
			svc.SetCentralTagStore(store)

			id := tc.create(t, svc)
			require.Contains(t, store.calls, id, "precondition: the create must have tagged it")

			tc.del(t, svc, id)
			assert.Contains(t, store.deleted, id, "the deleted resource's tags must be retired")
		})
	}
}

// DeleteVpc cascades the default security group, which no caller ever deletes
// by hand. Asserted separately because the id is not the one under test.
func TestDeleteVpc_ClearsTheCascadedDefaultSecurityGroup(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	store := &fakeCentralTagStore{}
	svc.SetCentralTagStore(store)

	vpcID := createTestVPC(t, svc, "10.4.0.0/16")
	defaultSG, err := svc.FindDefaultSGForVPC(testAccountID, vpcID)
	require.NoError(t, err)
	require.NotEmpty(t, defaultSG, "precondition: CreateVpc makes a default SG")

	_, err = svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)

	assert.Contains(t, store.deleted, defaultSG, "the cascaded default SG must not keep its tags")
}

func TestDelete_NilCentralTagStore_DeleteSucceeds(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	// No SetCentralTagStore call: centralTags stays nil.

	vpcID := createTestVPC(t, svc, "10.5.0.0/16")
	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)
}

// The resource is already gone by the time the store is called, so a store
// failure must not fail the delete: the caller cannot retry something that has
// happened, and would be left believing the resource survived.
func TestDelete_CentralTagStoreError_DoesNotFailDelete(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	store := &fakeCentralTagStore{err: errors.New("central store unavailable")}
	svc.SetCentralTagStore(store)

	vpcID := createTestVPC(t, svc, "10.6.0.0/16")
	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)
	assert.Contains(t, store.deleted, vpcID, "the clear must still have been attempted")
}
