package daemon

import (
	"context"
	"errors"
	"fmt"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sysInstFixture is a VPC test daemon with one subnet in the customer account
// and one in the system account, so launches can use real ENIs in either.
type sysInstFixture struct {
	d           *Daemon
	js          jetstream.JetStream
	subnetID    string
	sysSubnetID string
	itype       string
}

func newSysInstFixture(t *testing.T) *sysInstFixture {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	d := createTestDaemon(t, sharedNATSURL)
	_, nc, js := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)
	vpcSvc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), d.config, nc)
	require.NoError(t, err)
	d.vpcService = vpcSvc
	// MarkFailed tears down in the background; let it finish before the
	// temp dirs and the NATS connection go away.
	t.Cleanup(d.vmMgr.WaitForBackgroundWork)
	// Launches here never hand capacity back (no resource controller is
	// wired), so give the node room for every launch the test makes.
	d.resourceMgr.mu.Lock()
	d.resourceMgr.hostVCPU += 64
	d.resourceMgr.hostMemGB += 256
	d.resourceMgr.mu.Unlock()

	f := &sysInstFixture{d: d, js: js, itype: getTestInstanceType(t)}
	f.subnetID = f.newSubnet(t, testAccountID, "10.81")
	f.sysSubnetID = f.newSubnet(t, awsidentifiers.GlobalAccountID, "10.82")
	return f
}

func (f *sysInstFixture) newSubnet(t *testing.T, accountID, prefix string) string {
	t.Helper()
	vpcOut, err := f.d.vpcService.CreateVpc(t.Context(), &ec2.CreateVpcInput{
		CidrBlock: aws.String(prefix + ".0.0/16"),
	}, accountID)
	require.NoError(t, err)
	subnetOut, err := f.d.vpcService.CreateSubnet(t.Context(), &ec2.CreateSubnetInput{
		VpcId:     vpcOut.Vpc.VpcId,
		CidrBlock: aws.String(prefix + ".1.0/24"),
	}, accountID)
	require.NoError(t, err)
	return aws.StringValue(subnetOut.Subnet.SubnetId)
}

// newENI creates an available ENI in the customer subnet.
func (f *sysInstFixture) newENI(t *testing.T) *ec2.NetworkInterface {
	t.Helper()
	out, err := f.d.vpcService.CreateNetworkInterface(t.Context(), &ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(f.subnetID),
	}, testAccountID)
	require.NoError(t, err)
	return out.NetworkInterface
}

// input returns a direct-boot launch on a pre-created customer ENI with a
// valid single-NIC network config.
func (f *sysInstFixture) input(eni *ec2.NetworkInterface) *handlers_elbv2.SystemInstanceInput {
	return &handlers_elbv2.SystemInstanceInput{
		InstanceType: f.itype,
		SubnetID:     f.subnetID,
		AccountID:    testAccountID,
		ENIID:        aws.StringValue(eni.NetworkInterfaceId),
		ENIMac:       aws.StringValue(eni.MacAddress),
		ENIIP:        aws.StringValue(eni.PrivateIpAddress),
		NICs:         []handlers_elbv2.NICConfig{{MAC: aws.StringValue(eni.MacAddress), CIDR: "10.81.1.5/24", IsDefault: true}},
	}
}

func (f *sysInstFixture) eniRecord(t *testing.T, accountID, eniID string) ec2vpc.ENIRecord {
	t.Helper()
	rec, err := f.d.vpcService.GetENIRecord(accountID, eniID)
	require.NoError(t, err)
	return rec
}

// sysInstLaunchRuns swaps the volume mounter for one that marks the instance
// running, so vm.Manager.Run returns before it would start QEMU. It waits out
// earlier teardowns first, since they read the deps it replaces.
func sysInstLaunchRuns(d *Daemon) {
	d.vmMgr.WaitForBackgroundWork()
	d.vmMgr.SetDeps(vm.Deps{
		NodeID: d.node,
		VolumeMounter: &hookVolumeMounter{onMount: func(v *vm.VM) {
			d.vmMgr.UpdateState(v.ID, func(x *vm.VM) { x.Status = vm.StateRunning })
		}},
		VolumeStateUpdater: d.volumeService,
	})
}

// sysInstLaunchFails swaps the volume mounter for one that refuses every mount, so
// vm.Manager.Run fails without reaching QEMU.
func sysInstLaunchFails(d *Daemon) {
	d.vmMgr.WaitForBackgroundWork()
	d.vmMgr.SetDeps(vm.Deps{
		NodeID:             d.node,
		VolumeMounter:      &hookVolumeMounter{mountErr: errSysInstLaunchRefused},
		VolumeStateUpdater: d.volumeService,
	})
}

var errSysInstLaunchRefused = errors.New("launch refused")

// sysInstMgmtAllocator returns an allocator bound to the shared cluster-state KV
// on a subnet no other test uses.
func sysInstMgmtAllocator(t *testing.T, bridgeIP string) *MgmtIPAllocator {
	t.Helper()
	jsm := newTestMgmtJSM(t)
	a, err := NewMgmtIPAllocator(bridgeIP)
	require.NoError(t, err)
	a.BindKV(jsm, "node-1")
	cleanupMgmtIPAM(t, jsm, a)
	return a
}

// sysInstUnboundMgmtAllocator has no KV, so every Allocate fails.
func sysInstUnboundMgmtAllocator(t *testing.T) *MgmtIPAllocator {
	t.Helper()
	a, err := NewMgmtIPAllocator("10.94.8.1")
	require.NoError(t, err)
	return a
}

// sysInstReclaimingEIP adds the KV-backed backstop release that
// reclaimSystemInstanceEIP looks for.
type sysInstReclaimingEIP struct {
	fakeEIPService

	reclaimErr error
	reclaimed  []string
}

func (s *sysInstReclaimingEIP) ReleaseAddressByInstanceID(id string) error {
	s.reclaimed = append(s.reclaimed, id)
	return s.reclaimErr
}

func TestLaunchSystemInstance_EarlyErrors(t *testing.T) {
	t.Run("BootAMI dispatches to the AMI path", func(t *testing.T) {
		_, err := (&Daemon{}).LaunchSystemInstance(&handlers_elbv2.SystemInstanceInput{BootMode: systeminstance.BootAMI})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "instance service not initialized")
	})

	d := createTestDaemon(t, sharedNATSURL)
	itype := getTestInstanceType(t)

	t.Run("unknown instance type", func(t *testing.T) {
		_, err := d.LaunchSystemInstance(&handlers_elbv2.SystemInstanceInput{InstanceType: "zz9.none"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown instance type: zz9.none")
	})

	t.Run("capacity exhausted", func(t *testing.T) {
		d.resourceMgr.mu.Lock()
		saved := d.resourceMgr.allocatedVCPU
		d.resourceMgr.allocatedVCPU = 1 << 20
		d.resourceMgr.mu.Unlock()
		t.Cleanup(func() {
			d.resourceMgr.mu.Lock()
			d.resourceMgr.allocatedVCPU = saved
			d.resourceMgr.mu.Unlock()
		})

		_, err := d.LaunchSystemInstance(&handlers_elbv2.SystemInstanceInput{InstanceType: itype})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "insufficient capacity for "+itype)
	})

	t.Run("RunInstance failure returns the capacity", func(t *testing.T) {
		orig := d.instanceService
		t.Cleanup(func() { d.instanceService = orig })
		// An instance service that knows no types rejects the launch after the
		// daemon has already reserved capacity for it.
		d.instanceService = ec2instance.NewInstanceServiceImpl(d.config, map[string]*ec2.InstanceTypeInfo{},
			d.natsConn, objectstore.NewMemoryObjectStore(), d.vmMgr, d.resourceMgr, nil)

		d.resourceMgr.mu.Lock()
		before := d.resourceMgr.allocatedVCPU
		d.resourceMgr.mu.Unlock()

		_, err := d.LaunchSystemInstance(&handlers_elbv2.SystemInstanceInput{InstanceType: itype})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create instance")

		d.resourceMgr.mu.Lock()
		after := d.resourceMgr.allocatedVCPU
		d.resourceMgr.mu.Unlock()
		assert.Equal(t, before, after, "the reserved capacity must be released")
	})
}

func TestLaunchSystemInstance_PreCreatedENI(t *testing.T) {
	f := newSysInstFixture(t)
	d := f.d

	t.Run("made-up ENI still reaches launch", func(t *testing.T) {
		sysInstLaunchFails(d)
		in := f.input(f.newENI(t))
		in.ENIID = "eni-madeup"
		in.IamInstanceProfileArn = "arn:aws:iam::123456789012:instance-profile/lb"

		// The attach and owner stamp are best-effort, so only the launch fails.
		_, err := d.LaunchSystemInstance(in)
		require.ErrorIs(t, err, errSysInstLaunchRefused)
		assert.Contains(t, err.Error(), "launch instance")
	})

	t.Run("cross-account ENI and extras are attached and stamped", func(t *testing.T) {
		sysInstLaunchRuns(d)

		primary, extra := f.newENI(t), f.newENI(t)
		extraID := aws.StringValue(extra.NetworkInterfaceId)
		in := f.input(primary)
		in.ExtraENIs = []systeminstance.ExtraENIInput{{
			ENIID:               extraID,
			ENIMac:              aws.StringValue(extra.MacAddress),
			ENIIP:               aws.StringValue(extra.PrivateIpAddress),
			SubnetID:            f.subnetID,
			DeleteOnTermination: aws.Bool(false),
		}}
		in.HostfwdPorts = []int{6443}

		out, err := d.LaunchSystemInstance(in)
		require.NoError(t, err)
		assert.Equal(t, aws.StringValue(primary.PrivateIpAddress), out.PrivateIP)
		assert.Empty(t, out.PublicIP)

		rec := f.eniRecord(t, testAccountID, aws.StringValue(primary.NetworkInterfaceId))
		assert.Equal(t, out.InstanceID, rec.InstanceId)
		assert.Equal(t, awsidentifiers.GlobalAccountID, rec.InstanceOwnerId, "a system VM on a customer ENI must stamp its own account")

		extraRec := f.eniRecord(t, testAccountID, extraID)
		assert.Equal(t, out.InstanceID, extraRec.InstanceId)
		assert.Equal(t, int64(1), extraRec.DeviceIndex)
		require.NotNil(t, extraRec.DeleteOnTermination)
		assert.False(t, *extraRec.DeleteOnTermination)

		v, ok := d.vmMgr.Get(out.InstanceID)
		require.True(t, ok)
		assert.Equal(t, tags.ManagedByELBv2, v.ManagedBy)
		assert.Equal(t, awsidentifiers.GlobalAccountID, v.AccountID)
		assert.NotEmpty(t, aws.StringValue(v.Instance.VpcId), "the VPC is resolved from the pre-created ENI")
		require.Len(t, v.ExtraENIs, 1)
		assert.Equal(t, extraID, v.ExtraENIs[0].ENIID)
		assert.Equal(t, []int{6443}, v.HostfwdPorts)
		assert.True(t, v.DirectBoot)

		d.mu.Lock()
		_, subscribed := d.natsSubscriptions[out.InstanceID]
		d.mu.Unlock()
		assert.True(t, subscribed, "the instance must listen for terminate commands")
	})

	t.Run("failed extra attach fails the launch", func(t *testing.T) {
		in := f.input(f.newENI(t))
		in.ExtraENIs = []systeminstance.ExtraENIInput{{ENIID: "eni-missing"}}

		_, err := d.LaunchSystemInstance(in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "attach extra ENI eni-missing")
	})

	t.Run("same-account ENI skips the owner stamp", func(t *testing.T) {
		out, err := d.vpcService.CreateNetworkInterface(t.Context(), &ec2.CreateNetworkInterfaceInput{
			SubnetId: aws.String(f.sysSubnetID),
		}, awsidentifiers.GlobalAccountID)
		require.NoError(t, err)
		eni := out.NetworkInterface
		in := f.input(eni)
		in.AccountID = ""
		sysInstLaunchFails(d)

		_, err = d.LaunchSystemInstance(in)
		require.ErrorIs(t, err, errSysInstLaunchRefused)

		rec := f.eniRecord(t, awsidentifiers.GlobalAccountID, aws.StringValue(eni.NetworkInterfaceId))
		assert.Empty(t, rec.InstanceOwnerId, "same-account attachments leave the owner empty")
	})
}

func TestLaunchSystemInstance_AutoCreateENI(t *testing.T) {
	f := newSysInstFixture(t)
	d := f.d
	sysInstLaunchRuns(d)

	t.Run("creates and attaches an ENI in the system account", func(t *testing.T) {
		out, err := d.LaunchSystemInstance(&handlers_elbv2.SystemInstanceInput{
			InstanceType: f.itype,
			SubnetID:     f.sysSubnetID,
			NICs:         []handlers_elbv2.NICConfig{{MAC: "02:00:00:00:00:01", IsDefault: true}},
		})
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(out.PrivateIP, "10.82.1."), "private IP %q must come from the subnet", out.PrivateIP)

		v, ok := d.vmMgr.Get(out.InstanceID)
		require.True(t, ok)
		require.NotEmpty(t, v.ENIId)
		rec := f.eniRecord(t, awsidentifiers.GlobalAccountID, v.ENIId)
		assert.Equal(t, out.InstanceID, rec.InstanceId)
		assert.Equal(t, f.sysSubnetID, aws.StringValue(v.Instance.SubnetId))
	})

	t.Run("unknown subnet", func(t *testing.T) {
		_, err := d.LaunchSystemInstance(&handlers_elbv2.SystemInstanceInput{
			InstanceType: f.itype,
			SubnetID:     "subnet-madeup",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create ENI")
	})
}

func TestLaunchSystemInstance_EIPService(t *testing.T) {
	f := newSysInstFixture(t)
	d := f.d

	internetFacing := func(t *testing.T) *handlers_elbv2.SystemInstanceInput {
		in := f.input(f.newENI(t))
		in.Scheme = handlers_elbv2.SchemeInternetFacing
		return in
	}
	allocated := &ec2.AllocateAddressOutput{
		PublicIp:       aws.String("203.0.113.50"),
		PublicIpv4Pool: aws.String("wan"),
		AllocationId:   aws.String("eipalloc-1"),
	}

	t.Run("allocate failure", func(t *testing.T) {
		d.eipService = &fakeEIPService{allocateErr: errInjected}
		_, err := d.LaunchSystemInstance(internetFacing(t))
		require.ErrorIs(t, err, errInjected)
		assert.Contains(t, err.Error(), "allocate public IP for internet-facing ALB")
	})

	for _, releaseErr := range []error{nil, errors.New("release failed")} {
		t.Run(fmt.Sprintf("associate failure releases the address (release error %v)", releaseErr), func(t *testing.T) {
			eip := &fakeEIPService{allocateOut: allocated, associateErr: errInjected, releaseErr: releaseErr}
			d.eipService = eip
			_, err := d.LaunchSystemInstance(internetFacing(t))
			require.ErrorIs(t, err, errInjected)
			assert.Contains(t, err.Error(), "associate public IP")
			assert.Equal(t, []string{"eipalloc-1"}, eip.released)
		})
	}

	t.Run("success records the address on the ENI and the VM", func(t *testing.T) {
		sysInstLaunchRuns(d)
		d.eipService = &fakeEIPService{
			allocateOut:  allocated,
			associateOut: &ec2.AssociateAddressOutput{AssociationId: aws.String("eipassoc-1")},
		}
		in := internetFacing(t)

		out, err := d.LaunchSystemInstance(in)
		require.NoError(t, err)
		assert.Equal(t, "203.0.113.50", out.PublicIP)

		v, ok := d.vmMgr.Get(out.InstanceID)
		require.True(t, ok)
		assert.Equal(t, "203.0.113.50", v.PublicIP)
		assert.Equal(t, "wan", v.PublicIPPool)
		assert.Equal(t, "eipalloc-1", v.PublicIPAllocID)
		assert.Equal(t, "eipassoc-1", v.PublicIPAssocID)
		assert.Equal(t, "203.0.113.50", f.eniRecord(t, testAccountID, in.ENIID).PublicIpAddress)
	})

	t.Run("launch failure releases the associated address", func(t *testing.T) {
		sysInstLaunchFails(d)
		eip := &fakeEIPService{
			allocateOut:  allocated,
			associateOut: &ec2.AssociateAddressOutput{AssociationId: aws.String("eipassoc-2")},
		}
		d.eipService = eip
		_, err := d.LaunchSystemInstance(internetFacing(t))
		require.ErrorIs(t, err, errSysInstLaunchRefused)
		assert.Equal(t, []string{"eipassoc-2"}, eip.disassociated)
		assert.Equal(t, []string{"eipalloc-1"}, eip.released)
	})
}

func (f *sysInstFixture) externalIPAM(t *testing.T, pool external.ExternalPoolConfig) *ec2vpc.ExternalIPAM {
	t.Helper()
	ipam, err := ec2vpc.NewExternalIPAM(t.Context(), f.js, []external.ExternalPoolConfig{pool})
	require.NoError(t, err)
	return ipam
}

func TestLaunchSystemInstance_IPAM(t *testing.T) {
	f := newSysInstFixture(t)
	d := f.d
	sysInstLaunchRuns(d)

	t.Run("NAT committed", func(t *testing.T) {
		d.externalIPAM = f.externalIPAM(t, external.ExternalPoolConfig{
			Name: "wan-ok", RangeStart: "198.51.100.10", RangeEnd: "198.51.100.20", Gateway: "198.51.100.1", PrefixLen: 24,
		})
		sub, err := d.natsConn.Subscribe("vpc.add-nat", func(msg *nats.Msg) {
			_ = msg.Respond([]byte(`{"success":true}`))
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })

		in := f.input(f.newENI(t))
		in.Scheme = handlers_elbv2.SchemeInternetFacing
		out, err := d.LaunchSystemInstance(in)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(out.PublicIP, "198.51.100."), "public IP %q must come from the pool", out.PublicIP)

		v, ok := d.vmMgr.Get(out.InstanceID)
		require.True(t, ok)
		assert.Equal(t, "wan-ok", v.PublicIPPool)
		assert.Empty(t, v.PublicIPAllocID, "a direct IPAM address has no EIP allocation")
		assert.Equal(t, out.PublicIP, f.eniRecord(t, testAccountID, in.ENIID).PublicIpAddress)
	})

	t.Run("pool exhausted", func(t *testing.T) {
		// A one-address range holds only the reserved first address.
		d.externalIPAM = f.externalIPAM(t, external.ExternalPoolConfig{
			Name: "wan-full", RangeStart: "198.51.101.10", RangeEnd: "198.51.101.10", Gateway: "198.51.101.1", PrefixLen: 24,
		})

		in := f.input(f.newENI(t))
		in.Scheme = handlers_elbv2.SchemeInternetFacing
		_, err := d.LaunchSystemInstance(in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "allocate public IP for internet-facing ALB")
		assert.Empty(t, f.eniRecord(t, testAccountID, in.ENIID).PublicIpAddress)
	})
}

func TestLaunchSystemInstance_MgmtNIC(t *testing.T) {
	f := newSysInstFixture(t)
	d := f.d
	_, primed := captureNeighHooks(t)
	d.mgmtBridgeIP = "10.93.8.1"

	tapErr := errors.New("tap failed")
	cases := []struct {
		name      string
		scheme    string
		alloc     func(t *testing.T) *MgmtIPAllocator
		tapErr    error
		wantErr   string
		wantMgmt  bool
		wantAlloc int
	}{
		{name: "internal allocate error", scheme: handlers_elbv2.SchemeInternal, alloc: sysInstUnboundMgmtAllocator,
			wantErr: "allocate mgmt IP for internal-scheme ALB"},
		{name: "internet-facing allocate error continues", scheme: "", alloc: sysInstUnboundMgmtAllocator},
		{name: "internal tap error", scheme: handlers_elbv2.SchemeInternal, tapErr: tapErr,
			wantErr: "setup mgmt tap for internal-scheme ALB"},
		{name: "internet-facing tap error continues", scheme: "", tapErr: tapErr},
		{name: "success", scheme: handlers_elbv2.SchemeInternal, wantMgmt: true, wantAlloc: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sysInstLaunchRuns(d)
			var alloc *MgmtIPAllocator
			if tc.alloc != nil {
				alloc = tc.alloc(t)
			} else {
				alloc = sysInstMgmtAllocator(t, "10.93.8.1")
			}
			d.mgmtIPAllocator = alloc
			d.networkPlumber = &erroringPlumber{setupErr: tc.tapErr}
			*primed = nil

			in := f.input(f.newENI(t))
			in.Scheme = tc.scheme
			in.NICs = append(in.NICs, handlers_elbv2.NICConfig{})

			out, err := d.LaunchSystemInstance(in)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Equal(t, 0, alloc.AllocatedCount(), "a failed mgmt NIC must return its address")
				return
			}
			require.NoError(t, err)
			v, ok := d.vmMgr.Get(out.InstanceID)
			require.True(t, ok)
			assert.Equal(t, tc.wantAlloc, alloc.AllocatedCount())
			if !tc.wantMgmt {
				assert.Empty(t, v.MgmtIP)
				assert.Empty(t, v.MgmtMAC)
				return
			}
			assert.True(t, strings.HasPrefix(v.MgmtIP, "10.93.8."))
			assert.Equal(t, vm.GenerateMgmtMAC(out.InstanceID), v.MgmtMAC)
			assert.Equal(t, v.MgmtMAC, in.NICs[1].MAC, "the allocated MAC is injected into the guest NIC config")
			assert.Equal(t, v.MgmtIP+"/24", in.NICs[1].CIDR)
			require.Len(t, *primed, 1)
			assert.Equal(t, neighCall{dev: defaultMgmtBridge, ip: v.MgmtIP, mac: v.MgmtMAC}, (*primed)[0])
		})
	}
}

func TestLaunchSystemInstance_ConfigAndRun(t *testing.T) {
	f := newSysInstFixture(t)
	d := f.d

	t.Run("no default NIC fails the direct-boot config", func(t *testing.T) {
		in := f.input(f.newENI(t))
		in.NICs = nil
		_, err := d.LaunchSystemInstance(in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "build direct-boot config")
	})

	t.Run("dev networking derives a dev MAC", func(t *testing.T) {
		sysInstLaunchRuns(d)
		d.config.Daemon.DevNetworking = true
		t.Cleanup(func() { d.config.Daemon.DevNetworking = false })

		out, err := d.LaunchSystemInstance(f.input(f.newENI(t)))
		require.NoError(t, err)
		v, ok := d.vmMgr.Get(out.InstanceID)
		require.True(t, ok)
		assert.Equal(t, vm.GenerateDevMAC(out.InstanceID), v.DevMAC)
		assert.Equal(t, microvmMachineType(), v.Config.MachineType)
	})
}

// sysInstAMILoader serves one AMI whose snapshot the memory provider holds.
type sysInstAMILoader struct {
	ami       ebsmetadata.AMI
	sourceVol string
}

func (l *sysInstAMILoader) GetAMIConfig(_ context.Context, imageID string) (ebsmetadata.AMI, error) {
	if imageID != l.ami.ImageID {
		return ebsmetadata.AMI{}, fmt.Errorf("ami %s not found", imageID)
	}
	return l.ami, nil
}

func (l *sysInstAMILoader) GetAMISourceVolumeID(context.Context, string) (string, error) {
	return l.sourceVol, nil
}

func TestLaunchAMISystemInstance_Guards(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	base := systeminstance.SystemInstanceInput{
		BootMode: systeminstance.BootAMI, ImageID: "ami-x", AccountID: testAccountID, ENIID: "eni-x", InstanceType: getTestInstanceType(t),
	}
	cases := []struct {
		name    string
		mutate  func(*systeminstance.SystemInstanceInput)
		wantErr string
	}{
		{"missing image", func(in *systeminstance.SystemInstanceInput) { in.ImageID = "" }, "requires ImageID"},
		{"missing account", func(in *systeminstance.SystemInstanceInput) { in.AccountID = "" }, "requires AccountID"},
		{"missing ENI", func(in *systeminstance.SystemInstanceInput) { in.ENIID = "" }, "requires a pre-created ENI"},
		// The test daemon wires no AMI loader, so Prepare refuses the launch.
		{"prepare error", func(in *systeminstance.SystemInstanceInput) {
			in.UserData = "#cloud-config"
			in.IamInstanceProfileArn = "arn:aws:iam::123456789012:instance-profile/cp"
			in.ManagedBy = tags.ManagedByEKS
		}, "ServerInternal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			_, err := d.LaunchSystemInstance(&in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestLaunchAMISystemInstance(t *testing.T) {
	f := newSysInstFixture(t)
	d := f.d
	_, _ = captureNeighHooks(t)

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	_, err := provider.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-src",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	_, err = provider.CreateSnapshot(t.Context(), ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: "snap-src", VolumeID: "vol-src",
	})
	require.NoError(t, err)

	loader := &sysInstAMILoader{ami: ebsmetadata.AMI{ImageID: "ami-sys", State: "available", SnapshotID: "snap-src"}, sourceVol: "vol-src"}
	d.instanceService.SetRunInstancesDeps(loader, nil, &daemonENICreator{d: d}, nil)
	d.instanceService.SetEBSProvider(provider)
	sysInstLaunchFails(d)

	input := func(t *testing.T) *systeminstance.SystemInstanceInput {
		eni := f.newENI(t)
		return &systeminstance.SystemInstanceInput{
			BootMode:     systeminstance.BootAMI,
			InstanceType: f.itype,
			ImageID:      "ami-sys",
			AccountID:    testAccountID,
			ManagedBy:    tags.ManagedByEKS,
			ENIID:        aws.StringValue(eni.NetworkInterfaceId),
		}
	}

	t.Run("no management bridge", func(t *testing.T) {
		d.mgmtIPAllocator = nil
		_, err := d.LaunchSystemInstance(input(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "management bridge unavailable")
	})

	d.mgmtBridgeIP = "10.95.8.1"
	d.networkPlumber = &recordingPlumber{}
	alloc := sysInstMgmtAllocator(t, "10.95.8.1")
	d.mgmtIPAllocator = alloc

	t.Run("extra ENI attach failure", func(t *testing.T) {
		in := input(t)
		in.ExtraENIs = []systeminstance.ExtraENIInput{{ENIID: "eni-missing"}}
		_, err := d.LaunchSystemInstance(in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "attach extra ENI eni-missing")
	})

	t.Run("launch that never reaches running", func(t *testing.T) {
		_, err := d.LaunchSystemInstance(input(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not reach running state")
	})

	t.Run("success", func(t *testing.T) {
		sysInstLaunchRuns(d)
		in := input(t)
		extra := f.newENI(t)
		in.ExtraENIs = []systeminstance.ExtraENIInput{{ENIID: aws.StringValue(extra.NetworkInterfaceId), SubnetID: f.subnetID}}

		out, err := d.LaunchSystemInstance(in)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(out.MgmtIP, "10.95.8."))
		assert.True(t, strings.HasPrefix(out.PrivateIP, "10.81.1."), "private IP %q comes from the pre-created ENI", out.PrivateIP)

		v, ok := d.vmMgr.Get(out.InstanceID)
		require.True(t, ok)
		assert.Equal(t, tags.ManagedByEKS, v.ManagedBy)
		assert.Equal(t, testAccountID, v.AccountID)
		require.Len(t, v.ExtraENIs, 1)
		assert.Equal(t, out.InstanceID, f.eniRecord(t, testAccountID, v.ExtraENIs[0].ENIID).InstanceId)
	})
}

func TestAttachSystemMgmtNIC(t *testing.T) {
	_, primed := captureNeighHooks(t)

	t.Run("no allocator", func(t *testing.T) {
		err := (&Daemon{mgmtBridgeIP: "10.96.8.1"}).attachSystemMgmtNIC(&vm.VM{ID: "i-a"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "management bridge unavailable")
	})

	t.Run("allocate error", func(t *testing.T) {
		d := &Daemon{mgmtBridgeIP: "10.94.8.1", mgmtIPAllocator: sysInstUnboundMgmtAllocator(t)}
		err := d.attachSystemMgmtNIC(&vm.VM{ID: "i-b"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "allocate mgmt IP")
	})

	alloc := sysInstMgmtAllocator(t, "10.96.8.1")

	t.Run("tap error releases the address", func(t *testing.T) {
		d := &Daemon{mgmtBridgeIP: "10.96.8.1", mgmtIPAllocator: alloc, networkPlumber: &erroringPlumber{setupErr: errInjected}}
		inst := &vm.VM{ID: "i-c"}
		err := d.attachSystemMgmtNIC(inst)
		require.ErrorIs(t, err, errInjected)
		assert.Empty(t, inst.MgmtIP)
		assert.Empty(t, inst.MgmtMAC)
		assert.Equal(t, 0, alloc.AllocatedCount())
	})

	t.Run("success", func(t *testing.T) {
		d := &Daemon{mgmtBridgeIP: "10.96.8.1", mgmtIPAllocator: alloc, networkPlumber: &recordingPlumber{}}
		inst := &vm.VM{ID: "i-d"}
		require.NoError(t, d.attachSystemMgmtNIC(inst))
		assert.Equal(t, "10.96.8.10", inst.MgmtIP)
		assert.Equal(t, vm.GenerateMgmtMAC("i-d"), inst.MgmtMAC)
		require.Len(t, *primed, 1)
		assert.Equal(t, "10.96.8.10", (*primed)[0].ip)
	})
}

func TestTerminateSystemInstance(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	t.Cleanup(d.vmMgr.WaitForBackgroundWork)

	t.Run("local terminate releases the EIP", func(t *testing.T) {
		eip := &sysInstReclaimingEIP{fakeEIPService: fakeEIPService{disassociateErr: errInjected}}
		d.eipService = eip
		inst := &vm.VM{ID: "i-term-ok", Status: vm.StateRunning, AccountID: testAccountID,
			PublicIP: "203.0.113.7", PublicIPAllocID: "eipalloc-t", PublicIPAssocID: "eipassoc-t"}
		d.vmMgr.Insert(inst)

		require.NoError(t, d.TerminateSystemInstance("i-term-ok"))
		assert.Equal(t, []string{"eipassoc-t"}, eip.disassociated)
		assert.Equal(t, []string{"eipalloc-t"}, eip.released, "release still runs after a failed disassociate")
		// Once by the owning-node path and once by the backstop.
		assert.Equal(t, []string{"i-term-ok", "i-term-ok"}, eip.reclaimed)
		assert.Equal(t, vm.StateTerminated, d.vmMgr.Status(inst))
	})

	t.Run("local terminate failure", func(t *testing.T) {
		d.eipService = nil
		d.vmMgr.SetDeps(vm.Deps{NodeID: d.node, TransitionState: func(*vm.VM, vm.InstanceState) error { return errInjected }})
		d.vmMgr.Insert(&vm.VM{ID: "i-term-fail", Status: vm.StateRunning})

		err := d.TerminateSystemInstance("i-term-fail")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "terminate:")
	})

	t.Run("no owner and failing reclaim", func(t *testing.T) {
		eip := &sysInstReclaimingEIP{reclaimErr: errInjected}
		d.eipService = eip
		err := d.TerminateSystemInstance("i-term-nobody")
		require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound)
		assert.Equal(t, []string{"i-term-nobody"}, eip.reclaimed, "the backstop runs even when no node owns the VM")
	})

	t.Run("local lookup miss", func(t *testing.T) {
		err := d.terminateSystemInstanceLocal("i-term-gone")
		require.ErrorIs(t, err, systeminstance.ErrSystemInstanceNotFound)
	})
}

func TestReleaseSystemInstanceEIP_Errors(t *testing.T) {
	d := newDaemonWithVMs()
	eip := &fakeEIPService{disassociateErr: errInjected, releaseErr: errInjected}
	d.eipService = eip
	inst := &vm.VM{ID: "i-rel", PublicIP: "203.0.113.9", PublicIPPool: "wan", PublicIPAllocID: "eipalloc-r", PublicIPAssocID: "eipassoc-r"}
	d.vmMgr.Insert(inst)

	d.releaseSystemInstanceEIP(inst)

	assert.Equal(t, []string{"eipassoc-r"}, eip.disassociated)
	assert.Equal(t, []string{"eipalloc-r"}, eip.released)
	// Fields clear even on failure so teardown does not release the IP again.
	assert.Empty(t, inst.PublicIP)
	assert.Empty(t, inst.PublicIPPool)
	assert.Empty(t, inst.PublicIPAllocID)
	assert.Empty(t, inst.PublicIPAssocID)
}

func TestBuildDirectBootConfig_InstanceTypeShape(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	rm, err := NewResourceManager(nil, nil, nil)
	require.NoError(t, err)
	itype := getTestInstanceType(t)
	it := rm.instanceTypes[itype]
	d := &Daemon{resourceMgr: rm}

	cfg, err := d.buildDirectBootConfig("i-shape", &handlers_elbv2.SystemInstanceInput{
		InstanceType: itype,
		ENIID:        "eni-shape",
		NICs:         []handlers_elbv2.NICConfig{{MAC: "02:00:00:00:00:02", IsDefault: true}},
	})
	require.NoError(t, err)
	assert.Equal(t, int(instanceTypeVCPUs(it)), cfg.CPUCount)
	assert.Equal(t, int(instanceTypeMemoryMiB(it)), cfg.Memory)
	assert.Equal(t, aws.StringValue(it.ProcessorInfo.SupportedArchitectures[0]), cfg.Architecture)
	assert.Len(t, cfg.FwCfg, 3)
}

func TestBuildDirectBootConfig_FwCfgWriteError(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notADir, nil, 0o600))
	t.Setenv("XDG_RUNTIME_DIR", notADir)

	_, err := (&Daemon{resourceMgr: &ResourceManager{}}).buildDirectBootConfig("i-werr", &handlers_elbv2.SystemInstanceInput{
		NICs: []handlers_elbv2.NICConfig{{MAC: "02:00:00:00:00:03", IsDefault: true}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write fw_cfg blobs")
}

// A directory at a blob's path fails that write; the blobs written before it
// must be removed so no partial fw_cfg set is left behind.
func TestWriteFwCfgBlobs_LaterWriteErrorsCleanUp(t *testing.T) {
	input := &handlers_elbv2.SystemInstanceInput{
		NICs: []handlers_elbv2.NICConfig{{MAC: "02:00:00:00:00:04", IsDefault: true}},
	}
	for _, blob := range []string{"lbenv", "cacert"} {
		t.Run(blob, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_RUNTIME_DIR", dir)
			require.NoError(t, os.Mkdir(filepath.Join(dir, "fwcfg-i-blob-"+blob+".tmp"), 0o700))

			_, err := (&Daemon{}).writeFwCfgBlobs("i-blob", input)
			require.Error(t, err)

			_, statErr := os.Stat(filepath.Join(dir, "fwcfg-i-blob-netcfg.tmp"))
			assert.ErrorIs(t, statErr, os.ErrNotExist, "the netcfg blob must be removed")
			_, statErr = os.Stat(filepath.Join(dir, "fwcfg-i-blob-lbenv.tmp"))
			if blob == "cacert" {
				assert.ErrorIs(t, statErr, os.ErrNotExist, "the lb-agent-env blob must be removed")
			}
		})
	}
}
