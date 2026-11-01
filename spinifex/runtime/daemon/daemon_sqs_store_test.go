package daemon

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storelaunch"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storerole"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sqsStoreVolumes gives the fixture a volume service in a named AZ, since the
// test daemon's config has none and CreateVolume requires one, and an
// instance type larger than the store's heap limit.
func (f *sysInstFixture) sqsStoreVolumes(t *testing.T) {
	t.Helper()
	f.sqsStoreVolumesEncrypted(t, true)
}

// sqsStoreVolumesEncrypted is sqsStoreVolumes with a cluster storage key only
// when encrypted is set, so new volumes are recorded encrypted or not.
func (f *sysInstFixture) sqsStoreVolumesEncrypted(t *testing.T, encrypted bool) {
	t.Helper()
	f.d.config.AZ = "ap-southeast-2a"
	f.itype = sqsStoreTestInstanceType(t, f.d)
	f.d.config.Viperblock.EncryptionKeyFile = ""
	if encrypted {
		raw := make([]byte, 32)
		_, err := rand.Read(raw)
		require.NoError(t, err)
		key := filepath.Join(t.TempDir(), "master.key")
		require.NoError(t, os.WriteFile(key, raw, 0o600))
		f.d.config.Viperblock.EncryptionKeyFile = key
	}
	f.d.volumeService = ec2volume.NewVolumeServiceImplWithStore(f.d.config, objectstore.NewMemoryObjectStore(), f.d.natsConn)
	f.d.volumeService.SetEBSProvider(ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{}))
	sqsGuestGatewayHost(t, f)
}

// sqsGuestGatewayHost gives the node the mgmt bridge and cluster CA an SQS
// guest reaches the gateway with.
func sqsGuestGatewayHost(t *testing.T, f *sysInstFixture) {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, []byte(testSQSGuestCA), 0o600))
	f.d.config.NATS.CACert = ca
	captureNeighHooks(t)
	f.d.mgmtBridgeIP = "10.97.8.1"
	f.d.mgmtIPAllocator = sysInstMgmtAllocator(t, "10.97.8.1")
	if f.d.networkPlumber == nil {
		f.d.networkPlumber = &erroringPlumber{}
	}
}

const testSQSGuestCA = "-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----\n"

// sqsStoreTestInstanceType is the smallest of the daemon's types that fits
// the store, so a fixture never lands on an undersized random pick.
func sqsStoreTestInstanceType(t *testing.T, d *Daemon) string {
	t.Helper()
	best, mem := "", int64(0)
	for name, it := range d.resourceMgr.instanceTypes {
		m := instanceTypeMemoryMiB(it)
		if m > sqsStoreMinMemoryMiB && (best == "" || m < mem || (m == mem && name < best)) {
			best, mem = name, m
		}
	}
	require.NotEmpty(t, best, "no instance type fits the store")
	return best
}

// sqsStoreImage points microvmDir at a temp tree holding the shared firmware
// and the store's kernel and initramfs, so no installed image is needed.
func sqsStoreImage(t *testing.T) {
	t.Helper()
	orig := microvmDir
	microvmDir = t.TempDir()
	t.Cleanup(func() { microvmDir = orig })
	require.NoError(t, os.MkdirAll(sqsStoreImageDir(), 0o755))
	for _, p := range []string{
		filepath.Join(microvmDir, vm.MicroVMFirmware),
		filepath.Join(microvmDir, "vmlinuz"),
		filepath.Join(microvmDir, "initramfs.cpio.gz"),
		filepath.Join(sqsStoreImageDir(), "vmlinuz"),
		filepath.Join(sqsStoreImageDir(), "initramfs.cpio.gz"),
	} {
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	}
}

func testClientNKey(t *testing.T) string {
	t.Helper()
	kp, err := nkeys.CreateUser()
	require.NoError(t, err)
	pub, err := kp.PublicKey()
	require.NoError(t, err)
	return pub
}

func (f *sysInstFixture) systemVolume(t *testing.T) string {
	t.Helper()
	out, err := f.d.volumeService.CreateVolume(t.Context(), &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(f.d.config.AZ),
		Size:             aws.Int64(1),
		VolumeType:       aws.String("gp3"),
	}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	return aws.StringValue(out.VolumeId)
}

func (f *sysInstFixture) volumeState(t *testing.T, volumeID string) string {
	t.Helper()
	out, err := f.d.volumeService.DescribeVolumes(t.Context(), &ec2.DescribeVolumesInput{
		VolumeIds: []*string{aws.String(volumeID)},
	}, awsidentifiers.GlobalAccountID)
	require.NoError(t, err)
	require.Len(t, out.Volumes, 1)
	return aws.StringValue(out.Volumes[0].State)
}

const testStoreProfile = "arn:aws:iam::000000000000:instance-profile/sqsStoreRole"

func (f *sysInstFixture) sqsInput(t *testing.T, eni *ec2.NetworkInterface, volumeID string) *elbv2domain.SystemInstanceInput {
	in := f.input(eni)
	in.Kind = systeminstance.KindSQSStore
	in.IamInstanceProfileArn = testStoreProfile
	in.NICs = append(in.NICs, elbv2domain.NICConfig{})
	in.SQSStore = &systeminstance.SQSStoreInput{DataVolumeID: volumeID, ClientNKey: testClientNKey(t), Region: testWorkerRegion}
	return in
}

// sysInstNoSideEffects asserts a refused launch reserved no capacity, recorded
// no VM and left its ENI unattached.
func (f *sysInstFixture) sysInstNoSideEffects(t *testing.T, eni *ec2.NetworkInterface, vcpuBefore, vmsBefore int) {
	t.Helper()
	f.d.resourceMgr.mu.Lock()
	vcpu := f.d.resourceMgr.allocatedVCPU
	f.d.resourceMgr.mu.Unlock()
	assert.Equal(t, vcpuBefore, vcpu, "no capacity may be reserved")
	assert.Equal(t, vmsBefore, f.d.vmMgr.Count(), "no VM may be recorded")
	rec := f.eniRecord(t, testAccountID, aws.StringValue(eni.NetworkInterfaceId))
	assert.Empty(t, rec.InstanceId, "the ENI must stay unattached")
}

func (f *sysInstFixture) capacity() int {
	f.d.resourceMgr.mu.Lock()
	defer f.d.resourceMgr.mu.Unlock()
	return f.d.resourceMgr.allocatedVCPU
}

func TestLaunchSQSStoreInstance_RefusesBeforeAnySideEffect(t *testing.T) {
	f := newSysInstFixture(t)
	f.sqsStoreVolumes(t)
	sqsStoreImage(t)
	vol := f.systemVolume(t)
	inUse := f.systemVolume(t)
	require.NoError(t, f.d.volumeService.UpdateVolumeState(awsidentifiers.GlobalAccountID, inUse, "in-use", "i-other", "/dev/sdf"))
	seed, err := nkeys.CreateUser()
	require.NoError(t, err)
	seedBytes, err := seed.Seed()
	require.NoError(t, err)
	customerVol, err := f.d.volumeService.CreateVolume(t.Context(), &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(f.d.config.AZ), Size: aws.Int64(1), VolumeType: aws.String("gp3"),
	}, testAccountID)
	require.NoError(t, err)
	key := f.d.config.Viperblock.EncryptionKeyFile
	f.d.config.Viperblock.EncryptionKeyFile = ""
	plainVol := f.systemVolume(t)
	f.d.config.Viperblock.EncryptionKeyFile = key
	small := ""
	for name, it := range f.d.resourceMgr.instanceTypes {
		if m := instanceTypeMemoryMiB(it); m > 0 && m <= sqsStoreMinMemoryMiB {
			small = name
			break
		}
	}
	require.NotEmpty(t, small, "the test needs an instance type at or below the heap limit")

	cases := []struct {
		name    string
		mutate  func(*elbv2domain.SystemInstanceInput)
		wantErr string
	}{
		{"load balancer kind", func(in *elbv2domain.SystemInstanceInput) { in.Kind = "" }, "is not"},
		{"AMI boot", func(in *elbv2domain.SystemInstanceInput) { in.BootMode = systeminstance.BootAMI }, "boots direct only"},
		{"no store settings", func(in *elbv2domain.SystemInstanceInput) { in.SQSStore = nil }, "no data volume"},
		{"malformed volume ID", func(in *elbv2domain.SystemInstanceInput) { in.SQSStore.DataVolumeID = "vol-../x" }, "malformed"},
		{"seed instead of public key", func(in *elbv2domain.SystemInstanceInput) { in.SQSStore.ClientNKey = string(seedBytes) }, "not a NATS user public key"},
		{"public key with a bad checksum", func(in *elbv2domain.SystemInstanceInput) {
			k := []byte(in.SQSStore.ClientNKey)
			k[len(k)-1] ^= 1
			in.SQSStore.ClientNKey = string(k)
		}, "not a NATS user public key"},
		{"load balancer settings", func(in *elbv2domain.SystemInstanceInput) { in.LBAgentEnv = "X=1" }, "load balancer settings"},
		{"unknown instance type", func(in *elbv2domain.SystemInstanceInput) { in.InstanceType = "sys.nope" }, "unknown instance type"},
		{"no default NIC", func(in *elbv2domain.SystemInstanceInput) { in.NICs[0].IsDefault = false }, "want a default VPC NIC"},
		{"no mgmt NIC", func(in *elbv2domain.SystemInstanceInput) { in.NICs = in.NICs[:1] }, "unallocated mgmt NIC"},
		{"no instance profile", func(in *elbv2domain.SystemInstanceInput) { in.IamInstanceProfileArn = "" }, "no instance profile"},
		{"no Region", func(in *elbv2domain.SystemInstanceInput) { in.SQSStore.Region = "" }, "Region is malformed"},
		{"unusable listen address", func(in *elbv2domain.SystemInstanceInput) { in.NICs[0].CIDR = "127.0.0.1/8" }, "not a usable IPv4"},
		{"volume not in the system account", func(in *elbv2domain.SystemInstanceInput) {
			in.SQSStore.DataVolumeID = aws.StringValue(customerVol.VolumeId)
		}, "describe data volume"},
		{"missing volume", func(in *elbv2domain.SystemInstanceInput) { in.SQSStore.DataVolumeID = "vol-0123456789abcdef0" }, "describe data volume"},
		{"volume already in use", func(in *elbv2domain.SystemInstanceInput) { in.SQSStore.DataVolumeID = inUse }, "not available"},
		{"unencrypted volume", func(in *elbv2domain.SystemInstanceInput) { in.SQSStore.DataVolumeID = plainVol }, "not encrypted"},
		{"undersized instance type", func(in *elbv2domain.SystemInstanceInput) { in.InstanceType = small }, "512 MiB heap limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eni := f.newENI(t)
			in := f.sqsInput(t, eni, vol)
			tc.mutate(in)
			vcpu, vms := f.capacity(), f.d.vmMgr.Count()

			_, err := f.d.LaunchSQSStoreInstance(in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), string(seedBytes), "a seed must never be echoed")
			f.sysInstNoSideEffects(t, eni, vcpu, vms)
		})
	}

	t.Run("missing store image", func(t *testing.T) {
		require.NoError(t, os.Remove(filepath.Join(sqsStoreImageDir(), "initramfs.cpio.gz")))
		t.Cleanup(func() {
			require.NoError(t, os.WriteFile(filepath.Join(sqsStoreImageDir(), "initramfs.cpio.gz"), []byte("x"), 0o644))
		})
		eni := f.newENI(t)
		vcpu, vms := f.capacity(), f.d.vmMgr.Count()
		_, err := f.d.LaunchSQSStoreInstance(f.sqsInput(t, eni, vol))
		require.ErrorContains(t, err, "sqs store image is not installed")
		f.sysInstNoSideEffects(t, eni, vcpu, vms)
	})

	assert.Equal(t, ec2.VolumeStateAvailable, f.volumeState(t, vol), "refusals must leave the volume untouched")
}

func TestLaunchSQSStoreInstance_AttachesTheDataVolumeAtBoot(t *testing.T) {
	f := newSysInstFixture(t)
	f.sqsStoreVolumes(t)
	sqsStoreImage(t)
	vol := f.systemVolume(t)

	var launched *vm.VM
	f.d.vmMgr.WaitForBackgroundWork()
	f.d.vmMgr.SetDeps(vm.Deps{
		NodeID: f.d.node,
		VolumeMounter: &hookVolumeMounter{onMount: func(v *vm.VM) {
			launched = v
			f.d.vmMgr.UpdateState(v.ID, func(x *vm.VM) { x.Status = vm.StateRunning })
		}},
		VolumeStateUpdater: f.d.volumeService,
	})

	eni := f.newENI(t)
	in := f.sqsInput(t, eni, vol)
	out, err := f.d.LaunchSQSStoreInstance(in)
	require.NoError(t, err)
	require.NotNil(t, launched)

	assert.Equal(t, tags.ManagedBySQS, launched.ManagedBy)
	assert.Equal(t, []vm.EBSRequest{{Name: vol, DeviceName: sqsStoreDataDevice, DeleteOnTermination: false}},
		launched.EBSRequests.Requests, "exactly the one data volume, kept on terminate")
	assert.Equal(t, filepath.Join(sqsStoreImageDir(), "vmlinuz"), launched.Config.KernelImage)
	assert.Equal(t, filepath.Join(sqsStoreImageDir(), "initramfs.cpio.gz"), launched.Config.Initrd)
	assert.NotContains(t, launched.Config.KernelCmdline, "mitigations=off", "the store holds tenant data")

	got := map[string]string{}
	for _, e := range launched.Config.FwCfg {
		b, err := os.ReadFile(e.File)
		require.NoError(t, err)
		got[e.Name] = string(b)
	}
	assert.Equal(t, vm.VolumeSerial(vol), got["opt/spinifex/sqs-store/data-serial"])
	assert.Equal(t, "10.81.1.5", got["opt/spinifex/sqs-store/listen-addr"])
	assert.Equal(t, in.SQSStore.ClientNKey, got["opt/spinifex/sqs-store/client-nkey"])
	assert.Equal(t, testWorkerRegion, got["opt/spinifex/sqs-store/region"])
	assert.Equal(t, "https://10.97.8.1:9999", got["opt/spinifex/sqs-store/gateway-url"], "the gateway on this node's mgmt bridge")
	assert.Equal(t, testSQSGuestCA, got["opt/spinifex/ca-cert"], "the daemon's own cluster CA")
	assert.Contains(t, got, "opt/spinifex/netcfg")
	assert.Len(t, got, 7, "no load balancer environment reaches the store")
	assert.NotEmpty(t, launched.MgmtIP, "the store reaches the gateway only over its mgmt NIC")

	assert.Equal(t, out.InstanceID, launched.ID)
}

func TestLaunchSQSStoreInstance_FailedMountKeepsTheVolume(t *testing.T) {
	f := newSysInstFixture(t)
	f.sqsStoreVolumes(t)
	sqsStoreImage(t)
	vol := f.systemVolume(t)
	sysInstLaunchFails(f.d)

	_, err := f.d.LaunchSQSStoreInstance(f.sqsInput(t, f.newENI(t), vol))
	require.ErrorIs(t, err, errSysInstLaunchRefused)
	f.d.vmMgr.WaitForBackgroundWork()

	assert.Equal(t, ec2.VolumeStateAvailable, f.volumeState(t, vol), "a failed launch must not delete or hold the volume")
}

func TestLaunchSystemInstance_LoadBalancerUnchangedAndRefusesOtherKinds(t *testing.T) {
	f := newSysInstFixture(t)
	f.sqsStoreVolumes(t)
	sqsStoreImage(t)

	var launched *vm.VM
	f.d.vmMgr.WaitForBackgroundWork()
	f.d.vmMgr.SetDeps(vm.Deps{
		NodeID: f.d.node,
		VolumeMounter: &hookVolumeMounter{onMount: func(v *vm.VM) {
			launched = v
			f.d.vmMgr.UpdateState(v.ID, func(x *vm.VM) { x.Status = vm.StateRunning })
		}},
		VolumeStateUpdater: f.d.volumeService,
	})
	_, err := f.d.LaunchSystemInstance(f.input(f.newENI(t)))
	require.NoError(t, err)
	require.NotNil(t, launched)
	assert.Equal(t, tags.ManagedByELBv2, launched.ManagedBy)
	assert.Empty(t, launched.EBSRequests.Requests)
	assert.Equal(t, filepath.Join(microvmDir, "vmlinuz"), launched.Config.KernelImage)
	assert.Contains(t, launched.Config.KernelCmdline, " mitigations=off ")
	names := make([]string, 0, len(launched.Config.FwCfg))
	for _, e := range launched.Config.FwCfg {
		names = append(names, e.Name)
	}
	assert.Equal(t, []string{"opt/spinifex/netcfg", "opt/spinifex/lb-agent-env", "opt/spinifex/ca-cert"}, names)

	// The legacy path would boot the load balancer image, so it refuses any
	// other kind before reserving anything.
	vol := f.systemVolume(t)
	eni := f.newENI(t)
	vcpu, vms := f.capacity(), f.d.vmMgr.Count()
	_, err = f.d.LaunchSystemInstance(f.sqsInput(t, eni, vol))
	require.ErrorIs(t, err, systeminstance.ErrInvalidSQSStoreInput)
	f.sysInstNoSideEffects(t, eni, vcpu, vms)

	onlyVolume := f.input(f.newENI(t))
	onlyVolume.SQSStore = &systeminstance.SQSStoreInput{DataVolumeID: vol}
	_, err = f.d.LaunchSystemInstance(onlyVolume)
	require.ErrorIs(t, err, systeminstance.ErrInvalidSQSStoreInput)
}

// TestLaunchSQSStoreInstanceOnNode_OlderNodeHasNoResponder stands in for a
// daemon that predates the store subject: it serves only the legacy subject.
// The store launch must fail without reaching it, and the legacy sender must
// refuse to carry a store kind there.
func TestLaunchSQSStoreInstanceOnNode_OlderNodeHasNoResponder(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d.natsConn = nc

	itype := getTestInstanceType(t)
	var received atomic.Int32
	sub, err := nc.Subscribe("system.LaunchInstance."+itype+".node-old", func(msg *nats.Msg) {
		received.Add(1)
		out, _ := json.Marshal(systemInstanceLaunchEnvelope{Output: &elbv2domain.SystemInstanceOutput{InstanceID: "i-lb"}})
		_ = msg.Respond(out)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	in := remoteSQSStoreInput(itype, testClientNKey(t))
	_, err = d.LaunchSQSStoreInstanceOnNode("node-old", in)
	require.ErrorIs(t, err, nats.ErrNoResponders)
	assert.Contains(t, err.Error(), "does not serve SQS store launches")

	_, err = d.LaunchSystemInstanceOnNode("node-old", in)
	require.ErrorIs(t, err, systeminstance.ErrInvalidSQSStoreInput)

	assert.Zero(t, received.Load(), "no store launch may reach the older node's load balancer path")
}

func TestLaunchSQSStoreInstanceOnNode_ReachesANodeServingTheSubject(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	d.natsConn = nc

	itype := getTestInstanceType(t)
	got := make(chan elbv2domain.SystemInstanceInput, 1)
	sub, err := nc.Subscribe(systeminstance.SQSStoreLaunchSubject(itype, "node-new"), func(msg *nats.Msg) {
		var in elbv2domain.SystemInstanceInput
		_ = json.Unmarshal(msg.Data, &in)
		got <- in
		out, _ := json.Marshal(systemInstanceLaunchEnvelope{Output: &elbv2domain.SystemInstanceOutput{InstanceID: "i-sqs"}})
		_ = msg.Respond(out)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	key := testClientNKey(t)
	out, err := d.LaunchSQSStoreInstanceOnNode("node-new", remoteSQSStoreInput(itype, key))
	require.NoError(t, err)
	assert.Equal(t, "i-sqs", out.InstanceID)
	select {
	case in := <-got:
		assert.Equal(t, systeminstance.KindSQSStore, in.Kind)
		assert.Equal(t, key, in.SQSStore.ClientNKey)
	case <-time.After(5 * time.Second):
		t.Fatal("store launch never reached the node")
	}
}

func TestResourceManager_SubscribesSQSStoreSubjectOnlyWhenServed(t *testing.T) {
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	handler := func(*nats.Msg) string { return outcomeSuccess }

	countStore := func(rm *ResourceManager) int {
		n := 0
		for topic := range rm.instanceSubs {
			if len(topic) > len(systeminstance.SQSStoreLaunchSubjectRoot) && topic[:len(systeminstance.SQSStoreLaunchSubjectRoot)] == systeminstance.SQSStoreLaunchSubjectRoot {
				n++
			}
		}
		return n
	}

	without, err := NewResourceManager(nil, nil, nil)
	require.NoError(t, err)
	without.initSubscriptions(nc, handler, handler, "node-x")
	t.Cleanup(func() {
		for _, s := range without.instanceSubs {
			_ = s.Unsubscribe()
		}
	})
	assert.Zero(t, countStore(without))

	with, err := NewResourceManager(nil, nil, nil)
	require.NoError(t, err)
	with.sqsStoreHandler = handler
	with.initSubscriptions(nc, handler, handler, "node-y")
	t.Cleanup(func() {
		for _, s := range with.instanceSubs {
			_ = s.Unsubscribe()
		}
	})
	for _, itype := range []string{"sys.micro", "sys.medium"} {
		_, ok := with.instanceSubs[systeminstance.SQSStoreLaunchSubject(itype, "node-y")]
		assert.True(t, ok, "a serving node subscribes its node-targeted store subject for %s", itype)
		_, ok = with.instanceSubs[systeminstance.SQSStoreLaunchSubject(itype, "")]
		assert.True(t, ok, "and its queue subject for %s", itype)
	}
}

// remoteSQSStoreInput is a well-formed store launch for another node.
func remoteSQSStoreInput(itype, key string) *elbv2domain.SystemInstanceInput {
	return &elbv2domain.SystemInstanceInput{
		InstanceType:          itype,
		Kind:                  systeminstance.KindSQSStore,
		IamInstanceProfileArn: testStoreProfile,
		NICs:                  []elbv2domain.NICConfig{{MAC: "02:00:00:00:00:01", CIDR: "10.81.1.5/24", IsDefault: true}, {}},
		SQSStore: &systeminstance.SQSStoreInput{DataVolumeID: "vol-0123456789abcdef0", ClientNKey: key,
			Region: testWorkerRegion},
	}
}

// The store role is created with EC2-only trust and granted only the store's
// own two actions, none of the worker's, and its profile carries only it.
func TestSQSStoreRoles_EC2OnlyTrustAndOnlyTheStoreActions(t *testing.T) {
	fake := &fakeRoleIAM{}
	roles := sqsStoreRoles{provider: func() iamdomain.SystemInstanceRoleEnsurer { return fake }}
	_, err := roles.EnsureStoreProfile(t.Context())
	require.NoError(t, err)
	assert.True(t, ec2OnlyTrust(fake.trust))
	require.Len(t, fake.profile.Roles, 1)
	assert.Equal(t, storerole.RoleName, aws.StringValue(fake.profile.Roles[0].RoleName))

	var doc iamdomain.PolicyDocument
	require.NoError(t, json.Unmarshal([]byte(fake.policy), &doc))
	require.Len(t, doc.Statement, 1)
	assert.Equal(t, []string{"spinifex-sqs:DescribeStoreServerKey", "spinifex-sqs:RecordStoreServerKey"},
		slices.Sorted(slices.Values(doc.Statement[0].Action)))

	broad := &fakeRoleIAM{trust: broadWorkerTrust}
	_, err = sqsStoreRoles{provider: func() iamdomain.SystemInstanceRoleEnsurer { return broad }}.EnsureStoreProfile(t.Context())
	require.ErrorContains(t, err, "sqs store: role sqsStoreRole trusts more than the EC2 service")
	assert.Empty(t, broad.policy, "policy granted to an unsafe role")
}

// The launcher's spec maps onto a store launch the daemon accepts: its profile,
// Region, VPC NIC and an unallocated mgmt NIC.
func TestSQSStoreInput_MapsSpec(t *testing.T) {
	spec := storelaunch.Spec{InstanceType: "sys.test", Region: testWorkerRegion, ProfileARN: testStoreProfile,
		DataVolumeID: "vol-0123456789abcdef0", ClientNKey: testClientNKey(t),
		Network: workerlaunch.Network{SubnetID: "subnet-1", ENIID: "eni-1", ENIMac: "02:00:00:00:00:01",
			ENIIP: "10.240.0.10", CIDR: "10.240.0.10/22", Gateway: "10.240.0.1"}}
	in := sqsStoreInput(spec)
	require.NoError(t, in.ValidateSQSStore())
	assert.Equal(t, testStoreProfile, in.IamInstanceProfileArn)
	assert.Equal(t, testWorkerRegion, in.SQSStore.Region)
	assert.Equal(t, systeminstance.NICConfig{}, in.NICs[1])
}
