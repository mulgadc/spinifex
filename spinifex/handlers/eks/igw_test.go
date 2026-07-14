package handlers_eks

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeIGWProvisioner struct {
	createCalls   []*ec2.CreateInternetGatewayInput
	attachCalls   []*ec2.AttachInternetGatewayInput
	detachCalls   []*ec2.DetachInternetGatewayInput
	deleteCalls   []*ec2.DeleteInternetGatewayInput
	describeCalls []*ec2.DescribeInternetGatewaysInput

	// gateways are returned by DescribeInternetGateways (filtered on
	// attachment.vpc-id), keyed by IGW id.
	gateways  map[string]*ec2.InternetGateway
	nextID    string
	createErr error
}

var _ igwProvisioner = (*fakeIGWProvisioner)(nil)

func newFakeIGWProvisioner() *fakeIGWProvisioner {
	return &fakeIGWProvisioner{gateways: map[string]*ec2.InternetGateway{}, nextID: "igw-fake0001"}
}

func (f *fakeIGWProvisioner) DescribeInternetGateways(_ context.Context, input *ec2.DescribeInternetGatewaysInput, _ string) (*ec2.DescribeInternetGatewaysOutput, error) {
	f.describeCalls = append(f.describeCalls, input)
	var wantVPC string
	for _, flt := range input.Filters {
		if aws.StringValue(flt.Name) == "attachment.vpc-id" && len(flt.Values) > 0 {
			wantVPC = aws.StringValue(flt.Values[0])
		}
	}
	var out []*ec2.InternetGateway
	for _, igw := range f.gateways {
		for _, att := range igw.Attachments {
			if aws.StringValue(att.VpcId) == wantVPC {
				out = append(out, igw)
			}
		}
	}
	return &ec2.DescribeInternetGatewaysOutput{InternetGateways: out}, nil
}

func (f *fakeIGWProvisioner) CreateInternetGateway(_ context.Context, input *ec2.CreateInternetGatewayInput, _ string) (*ec2.CreateInternetGatewayOutput, error) {
	f.createCalls = append(f.createCalls, input)
	if f.createErr != nil {
		return nil, f.createErr
	}
	igw := &ec2.InternetGateway{
		InternetGatewayId: aws.String(f.nextID),
		Tags:              utils.MapToEC2Tags(utils.ExtractTags(input.TagSpecifications, "internet-gateway")),
	}
	f.gateways[f.nextID] = igw
	return &ec2.CreateInternetGatewayOutput{InternetGateway: igw}, nil
}

func (f *fakeIGWProvisioner) AttachInternetGateway(_ context.Context, input *ec2.AttachInternetGatewayInput, _ string) (*ec2.AttachInternetGatewayOutput, error) {
	f.attachCalls = append(f.attachCalls, input)
	if igw, ok := f.gateways[aws.StringValue(input.InternetGatewayId)]; ok {
		igw.Attachments = append(igw.Attachments, &ec2.InternetGatewayAttachment{
			VpcId: input.VpcId,
			State: aws.String("available"),
		})
	}
	return &ec2.AttachInternetGatewayOutput{}, nil
}

func (f *fakeIGWProvisioner) DetachInternetGateway(_ context.Context, input *ec2.DetachInternetGatewayInput, _ string) (*ec2.DetachInternetGatewayOutput, error) {
	f.detachCalls = append(f.detachCalls, input)
	if igw, ok := f.gateways[aws.StringValue(input.InternetGatewayId)]; ok {
		igw.Attachments = nil
	}
	return &ec2.DetachInternetGatewayOutput{}, nil
}

func (f *fakeIGWProvisioner) DeleteInternetGateway(_ context.Context, input *ec2.DeleteInternetGatewayInput, _ string) (*ec2.DeleteInternetGatewayOutput, error) {
	f.deleteCalls = append(f.deleteCalls, input)
	delete(f.gateways, aws.StringValue(input.InternetGatewayId))
	return &ec2.DeleteInternetGatewayOutput{}, nil
}

// seedAttached registers an IGW already attached to vpcID with the given tags.
func (f *fakeIGWProvisioner) seedAttached(igwID, vpcID string, tagKV ...string) {
	igw := &ec2.InternetGateway{
		InternetGatewayId: aws.String(igwID),
		Attachments:       []*ec2.InternetGatewayAttachment{{VpcId: aws.String(vpcID), State: aws.String("available")}},
	}
	for i := 0; i+1 < len(tagKV); i += 2 {
		igw.Tags = append(igw.Tags, &ec2.Tag{Key: aws.String(tagKV[i]), Value: aws.String(tagKV[i+1])})
	}
	f.gateways[igwID] = igw
}

func TestEnsureClusterIGW_FreshCreatesAndAttaches(t *testing.T) {
	f := newFakeIGWProvisioner()

	require.NoError(t, EnsureClusterIGW(context.Background(), f, "acct", "vpc-1", "demo"))

	require.Len(t, f.createCalls, 1)
	require.Len(t, f.attachCalls, 1)
	assert.Equal(t, "vpc-1", aws.StringValue(f.attachCalls[0].VpcId))
	assert.Equal(t, "igw-fake0001", aws.StringValue(f.attachCalls[0].InternetGatewayId))

	igw := f.gateways["igw-fake0001"]
	require.NotNil(t, igw)
	assert.True(t, ownedByCluster(igw, "demo"), "created IGW must carry cluster ownership tags")
}

func TestEnsureClusterIGW_ExistingIsReusedNotRecreated(t *testing.T) {
	f := newFakeIGWProvisioner()
	f.seedAttached("igw-customer", "vpc-1") // untagged, customer-provisioned

	require.NoError(t, EnsureClusterIGW(context.Background(), f, "acct", "vpc-1", "demo"))

	assert.Empty(t, f.createCalls, "must not create when VPC already has an attached IGW")
	assert.Empty(t, f.attachCalls)
}

func TestEnsureClusterIGW_Idempotent(t *testing.T) {
	f := newFakeIGWProvisioner()
	require.NoError(t, EnsureClusterIGW(context.Background(), f, "acct", "vpc-1", "demo"))
	require.NoError(t, EnsureClusterIGW(context.Background(), f, "acct", "vpc-1", "demo"))
	assert.Len(t, f.createCalls, 1, "second call reuses the IGW created by the first")
}

func TestDeleteClusterIGW_RemovesOnlyOwned(t *testing.T) {
	f := newFakeIGWProvisioner()
	f.seedAttached("igw-owned", "vpc-1", tags.ManagedByKey, tags.ManagedByEKS, clusterEKSClusterTagKey, "demo")

	require.NoError(t, DeleteClusterIGW(context.Background(), f, "acct", "vpc-1", "demo"))

	require.Len(t, f.detachCalls, 1)
	require.Len(t, f.deleteCalls, 1)
	assert.Equal(t, "igw-owned", aws.StringValue(f.deleteCalls[0].InternetGatewayId))
	assert.NotContains(t, f.gateways, "igw-owned")
}

func TestDeleteClusterIGW_LeavesCustomerIGW(t *testing.T) {
	f := newFakeIGWProvisioner()
	f.seedAttached("igw-customer", "vpc-1") // untagged

	require.NoError(t, DeleteClusterIGW(context.Background(), f, "acct", "vpc-1", "demo"))

	assert.Empty(t, f.detachCalls, "must not touch a customer-provisioned IGW")
	assert.Empty(t, f.deleteCalls)
	assert.Contains(t, f.gateways, "igw-customer")
}

func TestDeleteClusterIGW_WrongClusterTagNotOwned(t *testing.T) {
	f := newFakeIGWProvisioner()
	f.seedAttached("igw-other", "vpc-1", tags.ManagedByKey, tags.ManagedByEKS, clusterEKSClusterTagKey, "other-cluster")

	require.NoError(t, DeleteClusterIGW(context.Background(), f, "acct", "vpc-1", "demo"))

	assert.Empty(t, f.deleteCalls, "must not delete another cluster's IGW")
}

func TestEnsureClusterIGW_CreateErrorPropagates(t *testing.T) {
	f := newFakeIGWProvisioner()
	f.createErr = errors.New("boom")
	err := EnsureClusterIGW(context.Background(), f, "acct", "vpc-1", "demo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create cluster IGW")
}
