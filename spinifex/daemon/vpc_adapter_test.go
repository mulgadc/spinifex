package daemon

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- daemonENICreator.GetENI: DeleteOnTermination mapping ---

// TestDaemonENICreator_GetENI_DeleteOnTerminationMapping locks the nil/true/false
// mapping onto ENIInfo.DeleteOnTermination: a nil ENIRecord flag (never attached)
// must read as true, matching what DescribeNetworkInterfaces already advertises.
func TestDaemonENICreator_GetENI_DeleteOnTerminationMapping(t *testing.T) {
	tests := []struct {
		name string
		dot  *bool
		want bool
	}{
		{"NilDefaultsTrue", nil, true},
		{"ExplicitTrue", aws.Bool(true), true},
		{"ExplicitFalse", aws.Bool(false), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := createVPCTestDaemon(t)
			vpcOut, err := d.vpcService.CreateVpc(t.Context(), &ec2.CreateVpcInput{
				CidrBlock: aws.String("10.0.0.0/16"),
			}, testAccountID)
			require.NoError(t, err)
			subnetOut, err := d.vpcService.CreateSubnet(t.Context(), &ec2.CreateSubnetInput{
				VpcId:     vpcOut.Vpc.VpcId,
				CidrBlock: aws.String("10.0.1.0/24"),
			}, testAccountID)
			require.NoError(t, err)
			eniOut, err := d.vpcService.CreateNetworkInterface(t.Context(), &ec2.CreateNetworkInterfaceInput{
				SubnetId: subnetOut.Subnet.SubnetId,
			}, testAccountID)
			require.NoError(t, err)
			eniID := *eniOut.NetworkInterface.NetworkInterfaceId

			if tt.dot != nil {
				require.NoError(t, d.vpcService.UpdateENI(testAccountID, eniID, func(r *handlers_ec2_vpc.ENIRecord) {
					r.DeleteOnTermination = tt.dot
				}))
			}

			creator := &daemonENICreator{d: d}
			info, err := creator.GetENI(context.Background(), testAccountID, eniID)
			require.NoError(t, err)
			assert.Equal(t, tt.want, info.DeleteOnTermination)
		})
	}
}

// --- daemonENICreator.ListInstanceENIs ---

// TestDaemonENICreator_ListInstanceENIs_HappyPath confirms the adapter maps every
// field off ENIRecord, including the DeleteOnTermination default AttachENI stamps
// on first attach — this is what the terminate sweep relies on.
func TestDaemonENICreator_ListInstanceENIs_HappyPath(t *testing.T) {
	f := newENIHotPlugFixture(t)
	f.vmInst.AccountID = testAccountID

	_, err := f.daemon.vpcService.AttachENI(testAccountID, f.eniID, f.vmInst.ID, 2)
	require.NoError(t, err)

	creator := &daemonENICreator{d: f.daemon}
	infos, err := creator.ListInstanceENIs(context.Background(), testAccountID, f.vmInst.ID)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, f.eniID, infos[0].NetworkInterfaceID)
	assert.Equal(t, f.subnetID, infos[0].SubnetID)
	assert.Equal(t, f.vpcID, infos[0].VpcID)
	assert.Equal(t, f.mac, infos[0].MacAddress)
	assert.True(t, infos[0].DeleteOnTermination, "attach defaults DeleteOnTermination to true")
}

// TestDaemonENICreator_ListInstanceENIs_NoAttachments confirms an instance with no
// KV-recorded ENIs returns an empty, non-error result.
func TestDaemonENICreator_ListInstanceENIs_NoAttachments(t *testing.T) {
	d := createVPCTestDaemon(t)
	creator := &daemonENICreator{d: d}

	infos, err := creator.ListInstanceENIs(context.Background(), testAccountID, "i-none")
	require.NoError(t, err)
	assert.Empty(t, infos)
}

// TestDaemonENICreator_ListInstanceENIs_KVError exercises the error-mapping branch:
// the connection backing vpcService's KV is closed before enumerating, so Keys()
// fails with a real connection error rather than jetstream.ErrNoKeysFound.
func TestDaemonENICreator_ListInstanceENIs_KVError(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	_, nc, _ := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(t.Context(), daemon.config, nc)
	require.NoError(t, err)
	daemon.vpcService = vpcSvc

	nc.Close()

	creator := &daemonENICreator{d: daemon}
	_, err = creator.ListInstanceENIs(context.Background(), testAccountID, "i-any")
	require.Error(t, err)
}
