package natgw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// setupSubnetForNatGateway starts a VPC service with one subnet and an
// allocated EIP, returning the subnet ID.
func setupSubnetForNatGateway(t *testing.T) (*ec2vpc.VPCServiceImpl, *nats.Conn, string) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)
	vpcSvc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	vpcOut, err := vpcSvc.CreateVpc(t.Context(), &ec2.CreateVpcInput{CidrBlock: aws.String("10.70.0.0/16")}, testAccountID)
	require.NoError(t, err)
	subnetOut, err := vpcSvc.CreateSubnet(t.Context(), &ec2.CreateSubnetInput{VpcId: vpcOut.Vpc.VpcId, CidrBlock: aws.String("10.70.1.0/24")}, testAccountID)
	require.NoError(t, err)
	testutil.SeedKV(t, testutil.NewJetStream(t, nc), ec2eip.KVBucketEIPs, map[string][]byte{
		kvstore.AccountKey(testAccountID, "eipalloc-test1"): []byte(`{"allocation_id":"eipalloc-test1","public_ip":"203.0.113.50","state":"allocated"}`),
	})
	return vpcSvc, nc, *subnetOut.Subnet.SubnetId
}

func requireSubnetPinned(t *testing.T, vpcSvc *ec2vpc.VPCServiceImpl, subnetID string) {
	t.Helper()
	_, err := vpcSvc.DeleteSubnet(t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}, testAccountID)
	testutil.RequireAWSError(t, err, awserrors.ErrorDependencyViolation, "The subnet '"+subnetID+"' has dependencies and cannot be deleted.")
}

// TestNatGatewayPinsSubnetUntilDeleted asserts CreateNatGateway indexes the
// gateway so DeleteSubnet refuses, and DeleteNatGateway releases the subnet.
func TestNatGatewayPinsSubnetUntilDeleted(t *testing.T) {
	t.Parallel()
	vpcSvc, nc, subnetID := setupSubnetForNatGateway(t)
	svc, err := NewNatGatewayServiceImplWithNATS(t.Context(), nc)
	require.NoError(t, err)

	out, err := svc.CreateNatGateway(t.Context(), &ec2.CreateNatGatewayInput{
		SubnetId:     aws.String(subnetID),
		AllocationId: aws.String("eipalloc-test1"),
	}, testAccountID)
	require.NoError(t, err)
	requireSubnetPinned(t, vpcSvc, subnetID)

	_, err = svc.DeleteNatGateway(t.Context(), &ec2.DeleteNatGatewayInput{NatGatewayId: out.NatGateway.NatGatewayId}, testAccountID)
	require.NoError(t, err)
	_, err = svc.natgwIdxKV.Get(t.Context(), ec2vpc.SubnetNatGatewayKey(testAccountID, subnetID, *out.NatGateway.NatGatewayId))
	require.Error(t, err, "DeleteNatGateway must remove the subnet index key")
	_, err = vpcSvc.DeleteSubnet(t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(subnetID)}, testAccountID)
	require.NoError(t, err)
}

// TestNatGatewayStoredBeforeIndexPinsSubnet asserts service start indexes a
// gateway stored without an index key, so it still pins its subnet.
func TestNatGatewayStoredBeforeIndexPinsSubnet(t *testing.T) {
	t.Parallel()
	vpcSvc, nc, subnetID := setupSubnetForNatGateway(t)
	testutil.SeedKV(t, testutil.NewJetStream(t, nc), KVBucketNatGateways, map[string][]byte{
		kvstore.AccountKey(testAccountID, "nat-0legacy"):  []byte(`{"nat_gateway_id":"nat-0legacy","subnet_id":"` + subnetID + `","state":"available"}`),
		kvstore.AccountKey(testAccountID, "nat-0corrupt"): []byte(`{not json`),
	})

	_, err := NewNatGatewayServiceImplWithNATS(t.Context(), nc)
	require.NoError(t, err, "an undecodable record must not stop the service starting")
	requireSubnetPinned(t, vpcSvc, subnetID)
}
