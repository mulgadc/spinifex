package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reaperTestAccountID = "123456789012"

// TestENIOrphanReaperScopeIsClusterWide pins the scope. The record it reaps has
// no instance, so no node owns it; running the sweep node-locally would have
// every node racing to delete the same records.
func TestENIOrphanReaperScopeIsClusterWide(t *testing.T) {
	r := &eniOrphanReaper{}
	assert.Equal(t, vm.ScopeClusterWide, r.Scope())
	assert.Equal(t, "eni-orphan", r.Class())
}

func TestENIOrphanReaperSweep(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	testutil.StubVpcdSGResponder(t, nc)

	ctx := context.Background()
	vpcOut, err := vpcSvc.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, reaperTestAccountID)
	require.NoError(t, err)
	subnetOut, err := vpcSvc.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:     vpcOut.Vpc.VpcId,
		CidrBlock: aws.String("10.0.1.0/24"),
	}, reaperTestAccountID)
	require.NoError(t, err)

	eniOut, err := vpcSvc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    subnetOut.Subnet.SubnetId,
		Description: aws.String(handlers_ec2_vpc.AutoENIDescriptionPrefix + "i-abandoned"),
	}, reaperTestAccountID)
	require.NoError(t, err)
	eniID := *eniOut.NetworkInterface.NetworkInterfaceId
	require.NoError(t, vpcSvc.UpdateENI(reaperTestAccountID, eniID, func(rec *handlers_ec2_vpc.ENIRecord) {
		rec.CreatedAt = time.Now().Add(-time.Hour)
	}))

	r := &eniOrphanReaper{vpc: vpcSvc, minAge: 15 * time.Minute}

	reaped, err := r.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)

	_, err = vpcSvc.GetENIRecord(reaperTestAccountID, eniID)
	require.Error(t, err, "the record must be gone, or it keeps blocking its security group")

	// A second pass has nothing left and must not report work or fail.
	reaped, err = r.Sweep(ctx)
	require.NoError(t, err)
	assert.Zero(t, reaped)
}
