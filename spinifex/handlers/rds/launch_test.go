package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testCustomerAccount = "123456789012"
	testDBSubnet        = "subnet-customer-db"
	testEngineAMI       = "ami-rds-postgres-18"

	// The persisted endpoint a replace re-attaches: its address is the DNS
	// target and its MAC is what the replacement VM's NIC has to come up with.
	testEndpointIP  = "10.20.30.40"
	testEndpointMAC = "02:00:00:00:aa:01"
)

// --- Fakes ---

// fakeSystemVPC is the whole EC2 VPC family as the system-VPC builder sees it:
// every describe is empty and every create succeeds. The builder's own rules are
// covered in its package; here it only yields a private subnet for the DB VM.
type fakeSystemVPC struct {
	seq int
	// igwCreated flips the IGW describe from "none attached" (the pre-create
	// lookup) to "attached" (the post-attach re-read).
	igwCreated bool
}

var (
	_ handlers_systemvpc.VPCProvisioner        = (*fakeSystemVPC)(nil)
	_ handlers_systemvpc.RouteTableProvisioner = (*fakeSystemVPC)(nil)
	_ handlers_systemvpc.NATGatewayProvisioner = (*fakeSystemVPC)(nil)
	_ handlers_systemvpc.EIPProvisioner        = (*fakeSystemVPC)(nil)
	_ handlers_systemvpc.IGWProvisioner        = (*fakeSystemVPC)(nil)
)

func (f *fakeSystemVPC) id(prefix string) *string {
	f.seq++
	return aws.String(fmt.Sprintf("%s-%04d", prefix, f.seq))
}

func (f *fakeSystemVPC) deps() handlers_systemvpc.Deps {
	return handlers_systemvpc.Deps{VPC: f, IGW: f, RT: f, NGW: f, EIP: f}
}

func (f *fakeSystemVPC) CreateVpc(context.Context, *ec2.CreateVpcInput, string) (*ec2.CreateVpcOutput, error) {
	return &ec2.CreateVpcOutput{Vpc: &ec2.Vpc{VpcId: f.id("vpc")}}, nil
}

func (f *fakeSystemVPC) DeleteVpc(context.Context, *ec2.DeleteVpcInput, string) (*ec2.DeleteVpcOutput, error) {
	return &ec2.DeleteVpcOutput{}, nil
}

func (f *fakeSystemVPC) DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, string) (*ec2.DescribeVpcsOutput, error) {
	return &ec2.DescribeVpcsOutput{}, nil
}

func (f *fakeSystemVPC) CreateSubnet(_ context.Context, in *ec2.CreateSubnetInput, _ string) (*ec2.CreateSubnetOutput, error) {
	return &ec2.CreateSubnetOutput{Subnet: &ec2.Subnet{SubnetId: f.id("subnet-rdssys"), CidrBlock: in.CidrBlock}}, nil
}

func (f *fakeSystemVPC) DeleteSubnet(context.Context, *ec2.DeleteSubnetInput, string) (*ec2.DeleteSubnetOutput, error) {
	return &ec2.DeleteSubnetOutput{}, nil
}

func (f *fakeSystemVPC) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, string) (*ec2.DescribeSubnetsOutput, error) {
	return &ec2.DescribeSubnetsOutput{}, nil
}

func (f *fakeSystemVPC) CreateRouteTable(context.Context, *ec2.CreateRouteTableInput, string) (*ec2.CreateRouteTableOutput, error) {
	return &ec2.CreateRouteTableOutput{RouteTable: &ec2.RouteTable{RouteTableId: f.id("rtb")}}, nil
}

func (f *fakeSystemVPC) DeleteRouteTable(context.Context, *ec2.DeleteRouteTableInput, string) (*ec2.DeleteRouteTableOutput, error) {
	return &ec2.DeleteRouteTableOutput{}, nil
}

func (f *fakeSystemVPC) DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, string) (*ec2.DescribeRouteTablesOutput, error) {
	return &ec2.DescribeRouteTablesOutput{}, nil
}

func (f *fakeSystemVPC) CreateRoute(context.Context, *ec2.CreateRouteInput, string) (*ec2.CreateRouteOutput, error) {
	return &ec2.CreateRouteOutput{}, nil
}

func (f *fakeSystemVPC) AssociateRouteTable(context.Context, *ec2.AssociateRouteTableInput, string) (*ec2.AssociateRouteTableOutput, error) {
	return &ec2.AssociateRouteTableOutput{AssociationId: f.id("rtbassoc")}, nil
}

func (f *fakeSystemVPC) DisassociateRouteTable(context.Context, *ec2.DisassociateRouteTableInput, string) (*ec2.DisassociateRouteTableOutput, error) {
	return &ec2.DisassociateRouteTableOutput{}, nil
}

func (f *fakeSystemVPC) CreateNatGateway(context.Context, *ec2.CreateNatGatewayInput, string) (*ec2.CreateNatGatewayOutput, error) {
	return &ec2.CreateNatGatewayOutput{NatGateway: &ec2.NatGateway{NatGatewayId: f.id("nat")}}, nil
}

func (f *fakeSystemVPC) DeleteNatGateway(context.Context, *ec2.DeleteNatGatewayInput, string) (*ec2.DeleteNatGatewayOutput, error) {
	return &ec2.DeleteNatGatewayOutput{}, nil
}

func (f *fakeSystemVPC) DescribeNatGateways(context.Context, *ec2.DescribeNatGatewaysInput, string) (*ec2.DescribeNatGatewaysOutput, error) {
	return &ec2.DescribeNatGatewaysOutput{}, nil
}

func (f *fakeSystemVPC) AllocateAddress(context.Context, *ec2.AllocateAddressInput, string) (*ec2.AllocateAddressOutput, error) {
	return &ec2.AllocateAddressOutput{AllocationId: f.id("eipalloc"), PublicIp: aws.String("198.51.100.7")}, nil
}

func (f *fakeSystemVPC) ReleaseAddress(context.Context, *ec2.ReleaseAddressInput, string) (*ec2.ReleaseAddressOutput, error) {
	return &ec2.ReleaseAddressOutput{}, nil
}

func (f *fakeSystemVPC) CreateInternetGateway(context.Context, *ec2.CreateInternetGatewayInput, string) (*ec2.CreateInternetGatewayOutput, error) {
	f.igwCreated = true
	return &ec2.CreateInternetGatewayOutput{InternetGateway: &ec2.InternetGateway{InternetGatewayId: aws.String("igw-rdssys")}}, nil
}

func (f *fakeSystemVPC) AttachInternetGateway(context.Context, *ec2.AttachInternetGatewayInput, string) (*ec2.AttachInternetGatewayOutput, error) {
	return &ec2.AttachInternetGatewayOutput{}, nil
}

func (f *fakeSystemVPC) DetachInternetGateway(context.Context, *ec2.DetachInternetGatewayInput, string) (*ec2.DetachInternetGatewayOutput, error) {
	return &ec2.DetachInternetGatewayOutput{}, nil
}

func (f *fakeSystemVPC) DeleteInternetGateway(context.Context, *ec2.DeleteInternetGatewayInput, string) (*ec2.DeleteInternetGatewayOutput, error) {
	return &ec2.DeleteInternetGatewayOutput{}, nil
}

// DescribeInternetGateways answers the post-attach lookup: the builder attaches
// an IGW and then re-reads it, so this reports one attached to any VPC asked
// about.
func (f *fakeSystemVPC) DescribeInternetGateways(_ context.Context, in *ec2.DescribeInternetGatewaysInput, _ string) (*ec2.DescribeInternetGatewaysOutput, error) {
	// The pre-create lookup must find nothing, or no IGW is ever created; only
	// the post-attach one is answered. Distinguish them by whether an IGW has
	// been created yet.
	if !f.igwCreated {
		return &ec2.DescribeInternetGatewaysOutput{}, nil
	}
	return &ec2.DescribeInternetGatewaysOutput{InternetGateways: []*ec2.InternetGateway{{
		InternetGatewayId: aws.String("igw-rdssys"),
		Attachments:       []*ec2.InternetGatewayAttachment{{VpcId: in.Filters[0].Values[0]}},
	}}}, nil
}

// fakeENIs records the ENIs the launch helper created and deleted, per account,
// which is where the dual-NIC/cross-account wiring is observable.
type fakeENIs struct {
	created []*ec2.CreateNetworkInterfaceInput
	accts   []string
	deleted []string
	seq     int

	// createErrOn fails the nth (1-based) create, to open the rollback window
	// after the system NIC exists.
	createErrOn int

	// unwind is the harness-wide teardown log, and deleteCtxErr the state of the
	// context the delete was handed — both are how the rollback's ordering and
	// its independence from the caller's context become observable.
	unwind       *[]string
	deleteCtxErr error

	// A replace re-attaches the persisted endpoint ENI rather than minting one,
	// so what it detached, read back and re-associated is where that becomes
	// observable.
	detached  []string
	described []string
	modified  []*ec2.ModifyNetworkInterfaceAttributeInput

	// describeMissing makes the endpoint ENI read as gone, which is the state a
	// replace must refuse to mint a replacement address for.
	describeMissing bool
	modifyErr       error
}

var _ launchVPCProvisioner = (*fakeENIs)(nil)

func (f *fakeENIs) CreateNetworkInterface(_ context.Context, in *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error) {
	f.seq++
	if f.createErrOn == f.seq {
		return nil, errors.New("subnet has no free addresses")
	}
	f.created = append(f.created, in)
	f.accts = append(f.accts, accountID)
	return &ec2.CreateNetworkInterfaceOutput{NetworkInterface: &ec2.NetworkInterface{
		NetworkInterfaceId: aws.String(fmt.Sprintf("eni-%04d", f.seq)),
		PrivateIpAddress:   aws.String(fmt.Sprintf("10.0.0.%d", f.seq)),
		MacAddress:         aws.String(fmt.Sprintf("02:00:00:00:00:%02d", f.seq)),
		SubnetId:           in.SubnetId,
	}}, nil
}

func (f *fakeENIs) DeleteNetworkInterface(ctx context.Context, in *ec2.DeleteNetworkInterfaceInput, _ string) (*ec2.DeleteNetworkInterfaceOutput, error) {
	f.deleted = append(f.deleted, aws.StringValue(in.NetworkInterfaceId))
	f.deleteCtxErr = ctx.Err()
	if f.unwind != nil {
		*f.unwind = append(*f.unwind, "delete-eni")
	}
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

func (f *fakeENIs) DetachENI(_ context.Context, _, eniID string) error {
	f.detached = append(f.detached, eniID)
	return nil
}

// Answers for whatever was asked about, since the launch only ever reads back
// an ENI it or a previous launch created. The address is derived from the ID so
// a re-attach observably keeps the endpoint it already had.
func (f *fakeENIs) DescribeNetworkInterfaces(_ context.Context, in *ec2.DescribeNetworkInterfacesInput, _ string) (*ec2.DescribeNetworkInterfacesOutput, error) {
	out := &ec2.DescribeNetworkInterfacesOutput{}
	for _, id := range aws.StringValueSlice(in.NetworkInterfaceIds) {
		f.described = append(f.described, id)
		if f.describeMissing {
			continue
		}
		out.NetworkInterfaces = append(out.NetworkInterfaces, &ec2.NetworkInterface{
			NetworkInterfaceId: aws.String(id),
			PrivateIpAddress:   aws.String(testEndpointIP),
			MacAddress:         aws.String(testEndpointMAC),
			SubnetId:           aws.String(testDBSubnet),
		})
	}
	return out, nil
}

func (f *fakeENIs) ModifyNetworkInterfaceAttribute(_ context.Context, in *ec2.ModifyNetworkInterfaceAttributeInput, _ string) (*ec2.ModifyNetworkInterfaceAttributeOutput, error) {
	if f.modifyErr != nil {
		return nil, f.modifyErr
	}
	f.modified = append(f.modified, in)
	return &ec2.ModifyNetworkInterfaceAttributeOutput{}, nil
}

// fakeLauncher stands in for the system-instance launcher.
type fakeLauncher struct {
	input      *sysinstance.SystemInstanceInput
	err        error
	terminated []string
	unwind     *[]string

	// terminateErr fails the teardown of the superseded VM, which is the step a
	// replace cannot proceed past: the volume is still attached to it.
	terminateErr error

	// onLaunch runs once the VM exists, for tests that need to disturb state
	// after the launch has committed resources but before the caller records it.
	onLaunch func()
}

var _ launchInstanceLauncher = (*fakeLauncher)(nil)

func (f *fakeLauncher) LaunchSystemInstance(in *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error) {
	f.input = in
	if f.err != nil {
		return nil, f.err
	}
	if f.onLaunch != nil {
		f.onLaunch()
	}
	return &sysinstance.SystemInstanceOutput{InstanceID: "i-rds0001"}, nil
}

func (f *fakeLauncher) TerminateSystemInstance(instanceID string) error {
	f.terminated = append(f.terminated, instanceID)
	if f.unwind != nil {
		*f.unwind = append(*f.unwind, "terminate")
	}
	return f.terminateErr
}

// fakeImages answers the engine-AMI lookup from a canned image list.
type fakeImages struct {
	images  []*ec2.Image
	filters []*ec2.Filter
	err     error
}

var _ launchAMIResolver = (*fakeImages)(nil)

func (f *fakeImages) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ string) (*ec2.DescribeImagesOutput, error) {
	f.filters = in.Filters
	if f.err != nil {
		return nil, f.err
	}
	return &ec2.DescribeImagesOutput{Images: f.images}, nil
}

// fakeVolumes records the data volume's create and delete.
type fakeVolumes struct {
	created []*ec2.CreateVolumeInput
	accts   []string
	deleted []string
	err     error

	// encrypted is what the created volume reports about itself, which is what
	// the launch's encrypted-storage guard reads — not the request's echo.
	encrypted bool

	unwind       *[]string
	deleteCtxErr error

	// What DeleteVolume refuses with, which is how the volume store's own
	// snapshot index is made to disagree with the snapshot enumeration.
	deleteErr error
}

var _ launchVolumeProvisioner = (*fakeVolumes)(nil)

func (f *fakeVolumes) CreateVolume(_ context.Context, in *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.created = append(f.created, in)
	f.accts = append(f.accts, accountID)
	return &ec2.Volume{VolumeId: aws.String("vol-rdsdata01"), Encrypted: aws.Bool(f.encrypted)}, nil
}

func (f *fakeVolumes) DeleteVolume(ctx context.Context, in *ec2.DeleteVolumeInput, _ string) (*ec2.DeleteVolumeOutput, error) {
	f.deleted = append(f.deleted, aws.StringValue(in.VolumeId))
	f.deleteCtxErr = ctx.Err()
	if f.unwind != nil {
		*f.unwind = append(*f.unwind, "delete-volume")
	}
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &ec2.DeleteVolumeOutput{}, nil
}

// fakeAttacher records the hot-plug attach request.
type fakeAttacher struct {
	accountID, instanceID, volumeID, device string
	err                                     error

	// onCall fires before the attach returns, so a case can cancel the caller's
	// context at the moment the step that fails is running.
	onCall func()
}

var _ volumeAttacher = (*fakeAttacher)(nil)

func (f *fakeAttacher) AttachVolume(_ context.Context, accountID, instanceID, volumeID, device string) (string, error) {
	f.accountID, f.instanceID, f.volumeID, f.device = accountID, instanceID, volumeID, device
	if f.onCall != nil {
		f.onCall()
	}
	if f.err != nil {
		return "", f.err
	}
	return device, nil
}

// launchHarness bundles the fakes so a case can reach into any of them.
type launchHarness struct {
	sysvpc   *fakeSystemVPC
	enis     *fakeENIs
	launcher *fakeLauncher
	images   *fakeImages
	volumes  *fakeVolumes
	attacher *fakeAttacher

	// unwind is every teardown call in the order it happened, shared by the
	// fakes that perform one.
	unwind []string
}

func newLaunchHarness() *launchHarness {
	h := &launchHarness{
		sysvpc:   &fakeSystemVPC{},
		enis:     &fakeENIs{},
		launcher: &fakeLauncher{},
		images: &fakeImages{images: []*ec2.Image{
			{ImageId: aws.String(testEngineAMI), CreationDate: aws.String("2026-01-02T00:00:00Z")},
		}},
		volumes:  &fakeVolumes{encrypted: true},
		attacher: &fakeAttacher{},
	}
	h.enis.unwind, h.launcher.unwind, h.volumes.unwind = &h.unwind, &h.unwind, &h.unwind
	return h
}

func (h *launchHarness) deps() LaunchDeps {
	return LaunchDeps{
		Config:    &config.Config{Region: "ap-southeast-2", AZ: "ap-southeast-2a"},
		SystemVPC: h.sysvpc.deps(),
		VPC:       h.enis,
		Instance:  h.launcher,
		Image:     h.images,
		Volume:    h.volumes,
		Attacher:  h.attacher,
	}
}

func testLaunchInput() LaunchInput {
	return LaunchInput{
		DBInstanceIdentifier:  "mydb",
		AccountID:             testCustomerAccount,
		SubnetID:              testDBSubnet,
		SecurityGroupIDs:      []string{"sg-customer"},
		Engine:                "postgres",
		EngineVersion:         "18",
		InstanceType:          "t3.medium",
		AllocatedStorage:      20,
		UserData:              "#cloud-config\n",
		IamInstanceProfileArn: "arn:aws:iam::000000000000:instance-profile/rdsInstanceRole",
	}
}

// tagOf reads one tag out of a create request's tag specification.
func tagOf(specs []*ec2.TagSpecification, key string) string {
	for _, s := range specs {
		for _, t := range s.Tags {
			if aws.StringValue(t.Key) == key {
				return aws.StringValue(t.Value)
			}
		}
	}
	return ""
}

// --- Tests ---

func TestLaunchDBInstanceVMWiresBothNICs(t *testing.T) {
	h := newLaunchHarness()

	out, err := LaunchDBInstanceVM(t.Context(), h.deps(), testLaunchInput())
	require.NoError(t, err)

	require.Len(t, h.enis.created, 2, "a DB VM is dual-NIC: a system NIC for the agent and a customer-facing endpoint")
	sysENI, custENI := h.enis.created[0], h.enis.created[1]

	// The system NIC sits in the RDS system VPC's private subnet under the
	// system account. That is the NIC with NAT egress, which is how the in-guest
	// agent reaches the gateway from a customer DB subnet that has none.
	assert.Equal(t, utils.GlobalAccountID, h.enis.accts[0])
	assert.True(t, strings.HasPrefix(aws.StringValue(sysENI.SubnetId), "subnet-rdssys"),
		"the primary NIC must land in the RDS system VPC, got %s", aws.StringValue(sysENI.SubnetId))
	assert.Empty(t, sysENI.Groups, "the system NIC is unreachable from any customer VPC and needs no security group")

	// The customer-facing ENI is created in the customer's account and subnet,
	// with the customer's security groups: it is the only ingress path.
	assert.Equal(t, testCustomerAccount, h.enis.accts[1])
	assert.Equal(t, testDBSubnet, aws.StringValue(custENI.SubnetId))
	assert.Equal(t, []string{"sg-customer"}, aws.StringValueSlice(custENI.Groups))

	for _, eni := range h.enis.created {
		assert.Equal(t, tags.ManagedByRDS, tagOf(eni.TagSpecifications, tags.ManagedByKey))
		assert.Equal(t, "mydb", tagOf(eni.TagSpecifications, rdsInstanceTagKey))
	}

	// The VM itself: a system-account instance off the engine AMI, carrying the
	// managed-by tag that hides it from the customer's EC2 API, with the
	// customer ENI injected cross-account as an extra NIC.
	in := h.launcher.input
	require.NotNil(t, in)
	assert.Equal(t, sysinstance.BootAMI, in.BootMode)
	assert.Equal(t, tags.ManagedByRDS, in.ManagedBy)
	assert.Equal(t, testEngineAMI, in.ImageID)
	assert.Equal(t, utils.GlobalAccountID, in.AccountID)
	assert.Equal(t, aws.StringValue(sysENI.SubnetId), in.SubnetID)
	assert.Equal(t, out.SystemENIID, in.ENIID)
	require.Len(t, in.ExtraENIs, 1)
	assert.Equal(t, sysinstance.ExtraENIInput{
		ENIID:     out.CustomerENIID,
		ENIMac:    "02:00:00:00:00:02",
		ENIIP:     out.CustomerENIIP,
		SubnetID:  testDBSubnet,
		AccountID: testCustomerAccount,
	}, in.ExtraENIs[0], "the extra NIC must carry the customer account, or the daemon updates the wrong ENI record")
	assert.Equal(t, "#cloud-config\n", in.UserData)
	assert.Equal(t, "arn:aws:iam::000000000000:instance-profile/rdsInstanceRole", in.IamInstanceProfileArn)

	assert.Equal(t, "i-rds0001", out.InstanceID)
}

func TestLaunchDBInstanceVMAttachesTheDataVolume(t *testing.T) {
	h := newLaunchHarness()

	out, err := LaunchDBInstanceVM(t.Context(), h.deps(), testLaunchInput())
	require.NoError(t, err)

	// The data volume is created after the VM is running and hot-plugged onto
	// it, so it is decoupled from the boot volume a VM replace throws away.
	require.Len(t, h.volumes.created, 1)
	vol := h.volumes.created[0]
	assert.Equal(t, int64(20), aws.Int64Value(vol.Size))
	assert.Equal(t, "gp3", aws.StringValue(vol.VolumeType))
	assert.Equal(t, "ap-southeast-2a", aws.StringValue(vol.AvailabilityZone))
	assert.Equal(t, tags.ManagedByRDS, tagOf(vol.TagSpecifications, tags.ManagedByKey))
	assert.Equal(t, "mydb", tagOf(vol.TagSpecifications, rdsInstanceTagKey))
	assert.Equal(t, utils.GlobalAccountID, h.volumes.accts[0], "the data volume belongs to the system account, like the VM it serves")

	assert.Equal(t, "i-rds0001", h.attacher.instanceID)
	assert.Equal(t, "vol-rdsdata01", h.attacher.volumeID)
	assert.Equal(t, dataVolumeDevice, h.attacher.device)
	assert.Equal(t, utils.GlobalAccountID, h.attacher.accountID)

	assert.Equal(t, "vol-rdsdata01", out.DataVolumeID)
}

func TestLaunchDBInstanceVMRollsBackEverythingItCreated(t *testing.T) {
	// Each case fails one step; everything created before it must be gone, so a
	// caller retrying a failed CreateDBInstance does not accumulate orphans.
	t.Run("customer ENI", func(t *testing.T) {
		h := newLaunchHarness()
		h.enis.createErrOn = 2

		_, err := LaunchDBInstanceVM(t.Context(), h.deps(), testLaunchInput())
		require.Error(t, err)
		assert.Equal(t, []string{"eni-0001"}, h.enis.deleted, "the system NIC must not outlive the failed launch")
		assert.Nil(t, h.launcher.input, "no VM may be launched once a NIC is missing")
	})

	t.Run("VM launch", func(t *testing.T) {
		h := newLaunchHarness()
		h.launcher.err = errors.New("no capacity")

		_, err := LaunchDBInstanceVM(t.Context(), h.deps(), testLaunchInput())
		require.Error(t, err)
		assert.ElementsMatch(t, []string{"eni-0001", "eni-0002"}, h.enis.deleted)
		assert.Empty(t, h.volumes.created)
	})

	t.Run("volume create", func(t *testing.T) {
		h := newLaunchHarness()
		h.volumes.err = errors.New("storage pool exhausted")

		_, err := LaunchDBInstanceVM(t.Context(), h.deps(), testLaunchInput())
		require.Error(t, err)
		assert.Equal(t, []string{"i-rds0001"}, h.launcher.terminated)
		assert.ElementsMatch(t, []string{"eni-0001", "eni-0002"}, h.enis.deleted)
	})

	t.Run("volume attach", func(t *testing.T) {
		h := newLaunchHarness()
		h.attacher.err = errors.New("no free hot-plug port")

		_, err := LaunchDBInstanceVM(t.Context(), h.deps(), testLaunchInput())
		require.Error(t, err)
		// A volume left behind here is a billable orphan the customer never
		// asked for and cannot see.
		assert.Equal(t, []string{"vol-rdsdata01"}, h.volumes.deleted)
		assert.Equal(t, []string{"i-rds0001"}, h.launcher.terminated)
		assert.ElementsMatch(t, []string{"eni-0001", "eni-0002"}, h.enis.deleted)
	})
}

func TestLaunchDBInstanceVMTerminatesTheVMBeforeReleasingWhatIsAttachedToIt(t *testing.T) {
	h := newLaunchHarness()
	h.attacher.err = errors.New("no free hot-plug port")

	_, err := LaunchDBInstanceVM(t.Context(), h.deps(), testLaunchInput())
	require.Error(t, err)

	// The data volume and both ENIs are attached to a running VM. Releasing one
	// before the VM is gone is rejected as in-use, which the unwind can only log
	// — so the resource survives as a billable orphan the customer cannot see.
	require.Equal(t, []string{"terminate", "delete-volume", "delete-eni", "delete-eni"}, h.unwind)
}

func TestLaunchDBInstanceVMRollsBackAfterTheCallersContextIsDone(t *testing.T) {
	h := newLaunchHarness()
	ctx, cancel := context.WithCancel(t.Context())

	// The most likely reason a late step fails is the caller's own deadline, so
	// the unwind has to run on a context that outlives it. On the request's own
	// context every delete below returns immediately and every resource leaks.
	h.attacher.err = errors.New("deadline exceeded")
	h.attacher.onCall = cancel

	_, err := LaunchDBInstanceVM(ctx, h.deps(), testLaunchInput())
	require.Error(t, err)

	assert.Equal(t, []string{"vol-rdsdata01"}, h.volumes.deleted)
	assert.ElementsMatch(t, []string{"eni-0001", "eni-0002"}, h.enis.deleted)
	assert.NoError(t, h.volumes.deleteCtxErr, "the volume delete ran on the cancelled request context")
	assert.NoError(t, h.enis.deleteCtxErr, "the ENI delete ran on the cancelled request context")
}

func TestLaunchDBInstanceVMRejectsIncompleteInput(t *testing.T) {
	for name, mutate := range map[string]func(*LaunchInput){
		"no identifier":    func(in *LaunchInput) { in.DBInstanceIdentifier = "" },
		"no account":       func(in *LaunchInput) { in.AccountID = "" },
		"no subnet":        func(in *LaunchInput) { in.SubnetID = "" },
		"no instance type": func(in *LaunchInput) { in.InstanceType = "" },
		"no storage":       func(in *LaunchInput) { in.AllocatedStorage = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			h := newLaunchHarness()
			in := testLaunchInput()
			mutate(&in)

			_, err := LaunchDBInstanceVM(t.Context(), h.deps(), in)
			require.Error(t, err)
			assert.Empty(t, h.enis.created, "validation must run before anything is provisioned")
			assert.Nil(t, h.launcher.input)
		})
	}
}

func TestResolveEngineAMIMatchesOnManifestTags(t *testing.T) {
	h := newLaunchHarness()

	amiID, err := resolveEngineAMI(t.Context(), h.images, "postgres", "18")
	require.NoError(t, err)
	assert.Equal(t, testEngineAMI, amiID)

	// The filters are what stop a customer's own image, or another engine's,
	// from being booted as a DB instance.
	got := map[string]string{}
	for _, f := range h.images.filters {
		got[aws.StringValue(f.Name)] = aws.StringValue(f.Values[0])
	}
	assert.Equal(t, map[string]string{
		"tag:" + tags.ManagedByKey: tags.ManagedByRDS,
		"tag:engine":               "postgres",
		"tag:engine-version":       "18",
	}, got)
}

func TestResolveEngineAMIOmitsTheVersionFilterWhenUnset(t *testing.T) {
	h := newLaunchHarness()

	_, err := resolveEngineAMI(t.Context(), h.images, "postgres", "")
	require.NoError(t, err)
	for _, f := range h.images.filters {
		assert.NotEqual(t, "tag:engine-version", aws.StringValue(f.Name),
			"an unset EngineVersion must match any version, not the empty one")
	}
}

func TestResolveEngineAMIPicksTheNewestBuild(t *testing.T) {
	h := newLaunchHarness()
	h.images.images = []*ec2.Image{
		{ImageId: aws.String("ami-old"), CreationDate: aws.String("2025-06-01T00:00:00Z")},
		{ImageId: aws.String("ami-new"), CreationDate: aws.String("2026-03-01T00:00:00Z")},
		{ImageId: aws.String("ami-mid"), CreationDate: aws.String("2026-01-01T00:00:00Z")},
	}

	amiID, err := resolveEngineAMI(t.Context(), h.images, "postgres", "18")
	require.NoError(t, err)
	assert.Equal(t, "ami-new", amiID)
}

func TestResolveEngineAMISkipsMalformedCatalogEntries(t *testing.T) {
	h := newLaunchHarness()
	h.images.images = []*ec2.Image{
		nil,
		{ImageId: aws.String(""), CreationDate: aws.String("2026-04-01T00:00:00Z")},
		{CreationDate: aws.String("2026-05-01T00:00:00Z")},
		{ImageId: aws.String("ami-valid"), CreationDate: aws.String("2026-03-01T00:00:00Z")},
	}

	amiID, err := resolveEngineAMI(t.Context(), h.images, "postgres", "18")
	require.NoError(t, err)
	assert.Equal(t, "ami-valid", amiID)
}

func TestResolveEngineAMIExcludesGPUTaggedBuilds(t *testing.T) {
	h := newLaunchHarness()
	h.images.images = []*ec2.Image{
		{ImageId: aws.String(testEngineAMI), CreationDate: aws.String("2026-01-02T00:00:00Z")},
		{
			ImageId:      aws.String("ami-rds-postgres-18-gpu"),
			CreationDate: aws.String("2026-05-01T00:00:00Z"),
			Tags:         []*ec2.Tag{{Key: aws.String(tags.GPUVendorKey), Value: aws.String("nvidia")}},
		},
	}

	amiID, err := resolveEngineAMI(t.Context(), h.images, "postgres", "18")
	require.NoError(t, err)
	assert.Equal(t, testEngineAMI, amiID,
		"a newer GPU build carries the same engine tags, so newest-wins alone would boot it for an ordinary instance")
}

func TestResolveEngineAMIFailsWhenOnlyGPUBuildsMatch(t *testing.T) {
	h := newLaunchHarness()
	h.images.images = []*ec2.Image{{
		ImageId:      aws.String("ami-rds-postgres-18-gpu"),
		CreationDate: aws.String("2026-05-01T00:00:00Z"),
		Tags:         []*ec2.Tag{{Key: aws.String(tags.GPUVendorKey), Value: aws.String("nvidia")}},
	}}

	_, err := resolveEngineAMI(t.Context(), h.images, "postgres", "18")
	require.ErrorIs(t, err, ErrEngineAMINotFound,
		"excluding the GPU build must fail the lookup, not fall through to it")
}

func TestLaunchDBInstanceVMFailsWithoutAnEngineAMI(t *testing.T) {
	h := newLaunchHarness()
	h.images.images = nil

	_, err := LaunchDBInstanceVM(t.Context(), h.deps(), testLaunchInput())
	require.ErrorIs(t, err, ErrEngineAMINotFound,
		"a missing engine image must be a named failure, never a fallback to some other AMI")
	assert.Empty(t, h.enis.created)
}
