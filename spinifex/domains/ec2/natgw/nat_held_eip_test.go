package natgw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/igw"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const natHeldPool = "nat-held-pool"

func setupNatHeldEIP(t *testing.T) (*NatGatewayServiceImpl, *ec2eip.EIPServiceImpl, *ec2vpc.ExternalIPAM, *nats.Conn, jetstream.JetStream) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	testutil.SeedKV(t, js, ec2vpc.KVBucketSubnets, map[string][]byte{
		kvstore.AccountKey(testAccountID, "subnet-pub1"): []byte(`{"subnet_id":"subnet-pub1","vpc_id":"vpc-test1","cidr_block":"10.0.1.0/24","state":"available"}`),
	})
	ipam, err := ec2vpc.NewExternalIPAM(t.Context(), js, []external.ExternalPoolConfig{{
		Name: natHeldPool, RangeStart: "198.51.100.10", RangeEnd: "198.51.100.20", Gateway: "198.51.100.1", PrefixLen: 24,
	}})
	require.NoError(t, err)
	eipSvc, err := ec2eip.NewEIPServiceImpl(t.Context(), nc, ipam, nil)
	require.NoError(t, err)
	natSvc, err := NewNatGatewayServiceImplWithNATS(t.Context(), nc)
	require.NoError(t, err)
	return natSvc, eipSvc, ipam, nc, js
}

func requireAuthFailure(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "error must carry a registered code: %v", err)
	assert.Equal(t, awserrors.ErrorAuthFailure, code)
	assert.Equal(t, "You do not have permission to access the specified resource.", message)
}

// AWS refuses DisassociateAddress and ReleaseAddress on a NAT gateway's address
// with AuthFailure and leaves the gateway, its association and the pool lease as
// they were, so the address it translates to is never handed back to the pool.
func TestNatHeldEIP_DisassociateAndReleaseRefused(t *testing.T) {
	natSvc, eipSvc, ipam, nc, js := setupNatHeldEIP(t)
	ctx := t.Context()

	alloc, err := eipSvc.AllocateAddress(ctx, &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	created, err := natSvc.CreateNatGateway(ctx, &ec2.CreateNatGatewayInput{
		SubnetId: aws.String("subnet-pub1"), AllocationId: alloc.AllocationId,
	}, testAccountID)
	require.NoError(t, err)
	assocID := created.NatGateway.NatGatewayAddresses[0].AssociationId

	_, err = eipSvc.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{AssociationId: assocID}, testAccountID)
	requireAuthFailure(t, err)
	_, err = eipSvc.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: alloc.AllocationId}, testAccountID)
	requireAuthFailure(t, err)

	addrs, err := eipSvc.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{AllocationIds: []*string{alloc.AllocationId}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, addrs.Addresses, 1)
	assert.Equal(t, aws.StringValue(assocID), aws.StringValue(addrs.Addresses[0].AssociationId))

	pool, err := ipam.GetPoolRecord(natHeldPool)
	require.NoError(t, err)
	assert.Contains(t, pool.Allocated, aws.StringValue(alloc.PublicIp), "the gateway's address must stay leased")

	// The address stays mapped, so the VPC's internet gateway cannot be detached.
	igwSvc, err := igw.NewIGWServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	igwKV, err := js.KeyValue(ctx, igw.KVBucketIGW)
	require.NoError(t, err)
	_, err = igwKV.Put(ctx, kvstore.AccountKey(testAccountID, "igw-test1"), []byte(`{"internet_gateway_id":"igw-test1","vpc_id":"vpc-test1","state":"available"}`))
	require.NoError(t, err)
	_, err = igwSvc.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String("igw-test1"), VpcId: aws.String("vpc-test1"),
	}, testAccountID)
	code, msg, _ := awserrors.ResolveErrorDetail(err)
	assert.Equal(t, awserrors.ErrorDependencyViolation, code)
	assert.Equal(t, "Network vpc-test1 has some mapped public address(es). Please unmap those public address(es) before detaching the gateway.", msg)

	// Deleting the gateway frees the address for the customer to release.
	_, err = natSvc.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{NatGatewayId: created.NatGateway.NatGatewayId}, testAccountID)
	require.NoError(t, err)
	_, err = eipSvc.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: alloc.AllocationId}, testAccountID)
	require.NoError(t, err)
}
