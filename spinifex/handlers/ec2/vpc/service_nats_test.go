package handlers_ec2_vpc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupNATSVPCServiceTest creates a NATSVPCService client connected to a
// VPCServiceImpl backend via NATS. It returns the client, backend, and
// NATS connection used by the backend.
func setupNATSVPCServiceTest(t *testing.T) (VPCService, *VPCServiceImpl, *nats.Conn) {
	t.Helper()

	backend, nc := setupTestVPCServiceWithNC(t)

	// Subscribe the backend handlers on NATS topics
	topics := map[string]func(*nats.Msg){
		"ec2.CreateVpc":                 func(msg *nats.Msg) { handleNATSMsg(msg, backend.CreateVpc) },
		"ec2.DeleteVpc":                 func(msg *nats.Msg) { handleNATSMsg(msg, backend.DeleteVpc) },
		"ec2.DescribeVpcs":              func(msg *nats.Msg) { handleNATSMsg(msg, backend.DescribeVpcs) },
		"ec2.CreateSubnet":              func(msg *nats.Msg) { handleNATSMsg(msg, backend.CreateSubnet) },
		"ec2.DeleteSubnet":              func(msg *nats.Msg) { handleNATSMsg(msg, backend.DeleteSubnet) },
		"ec2.DescribeSubnets":           func(msg *nats.Msg) { handleNATSMsg(msg, backend.DescribeSubnets) },
		"ec2.CreateNetworkInterface":    func(msg *nats.Msg) { handleNATSMsg(msg, backend.CreateNetworkInterface) },
		"ec2.DeleteNetworkInterface":    func(msg *nats.Msg) { handleNATSMsg(msg, backend.DeleteNetworkInterface) },
		"ec2.DescribeNetworkInterfaces": func(msg *nats.Msg) { handleNATSMsg(msg, backend.DescribeNetworkInterfaces) },
	}

	for topic, handler := range topics {
		sub, err := nc.Subscribe(topic, handler)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}

	client := NewNATSVPCService(nc)
	return client, backend, nc
}

// handleNATSMsg is a generic NATS request handler that unmarshals the request,
// extracts the account ID from the header, calls the handler, and responds with the result.
func handleNATSMsg[In any, Out any](msg *nats.Msg, fn func(context.Context, *In, string) (*Out, error)) {
	var input In
	if err := json.Unmarshal(msg.Data, &input); err != nil {
		_ = msg.Respond([]byte(`{"error":"unmarshal"}`))
		return
	}
	accountID := msg.Header.Get("X-Account-ID")
	result, err := fn(context.Background(), &input, accountID)
	if err != nil {
		errResp, _ := json.Marshal(map[string]string{"error": err.Error()})
		_ = msg.Respond(errResp)
		return
	}
	data, _ := json.Marshal(result)
	_ = msg.Respond(data)
}

func TestNATSVPCService_CreateVpc(t *testing.T) {
	client, _, _ := setupNATSVPCServiceTest(t)

	out, err := client.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.Vpc)
	assert.NotEmpty(t, *out.Vpc.VpcId)
	assert.Equal(t, "10.0.0.0/16", *out.Vpc.CidrBlock)
}

func TestNATSVPCService_DescribeVpcs(t *testing.T) {
	client, _, _ := setupNATSVPCServiceTest(t)

	// Create a VPC first
	createOut, err := client.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	// Describe
	out, err := client.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{}, testAccountID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(out.Vpcs), 1)

	found := false
	for _, vpc := range out.Vpcs {
		if *vpc.VpcId == *createOut.Vpc.VpcId {
			found = true
		}
	}
	assert.True(t, found)
}

func TestNATSVPCService_DeleteVpc(t *testing.T) {
	client, _, _ := setupNATSVPCServiceTest(t)

	createOut, err := client.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = client.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{
		VpcId: createOut.Vpc.VpcId,
	}, testAccountID)
	require.NoError(t, err)
}

func TestNATSVPCService_CreateAndDeleteSubnet(t *testing.T) {
	client, _, _ := setupNATSVPCServiceTest(t)

	vpcOut, err := client.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	subnetOut, err := client.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     vpcOut.Vpc.VpcId,
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, subnetOut.Subnet)
	assert.Equal(t, *vpcOut.Vpc.VpcId, *subnetOut.Subnet.VpcId)

	_, err = client.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{
		SubnetId: subnetOut.Subnet.SubnetId,
	}, testAccountID)
	require.NoError(t, err)
}

func TestNATSVPCService_DescribeSubnets(t *testing.T) {
	client, _, _ := setupNATSVPCServiceTest(t)

	vpcOut, err := client.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = client.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     vpcOut.Vpc.VpcId,
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	require.NoError(t, err)

	out, err := client.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{}, testAccountID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(out.Subnets), 1)
}

func TestNATSVPCService_CreateAndDeleteENI(t *testing.T) {
	client, _, _ := setupNATSVPCServiceTest(t)

	vpcOut, err := client.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	subnetOut, err := client.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     vpcOut.Vpc.VpcId,
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	require.NoError(t, err)

	eniOut, err := client.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
		SubnetId: subnetOut.Subnet.SubnetId,
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, eniOut.NetworkInterface)

	_, err = client.DeleteNetworkInterface(context.Background(), &ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: eniOut.NetworkInterface.NetworkInterfaceId,
	}, testAccountID)
	require.NoError(t, err)
}

func TestNATSVPCService_DescribeNetworkInterfaces(t *testing.T) {
	client, _, _ := setupNATSVPCServiceTest(t)

	vpcOut, err := client.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)

	subnetOut, err := client.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId:     vpcOut.Vpc.VpcId,
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = client.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
		SubnetId: subnetOut.Subnet.SubnetId,
	}, testAccountID)
	require.NoError(t, err)

	out, err := client.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(out.NetworkInterfaces), 1)
}
