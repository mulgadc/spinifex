package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awsiam "github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	ec2volume "github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerrole"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testWorkerProfile = "arn:aws:iam::000000000000:instance-profile/sqsWorkerRole"

// sqsWorkerFixture is a daemon that can launch the worker: an encrypting
// volume service, the worker image, a mgmt bridge and a cluster CA.
func sqsWorkerFixture(t *testing.T, encrypted bool) *sysInstFixture {
	t.Helper()
	f := newSysInstFixture(t)
	f.d.config.AZ = "ap-southeast-2a"
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
	sqsStoreImage(t)
	require.NoError(t, os.MkdirAll(sqsWorkerImageDir(), 0o755))
	for _, n := range []string{"vmlinuz", "initramfs.cpio.gz"} {
		require.NoError(t, os.WriteFile(filepath.Join(sqsWorkerImageDir(), n), []byte("x"), 0o644))
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----\n"), 0o600))
	f.d.config.NATS.CACert = ca
	captureNeighHooks(t)
	f.d.mgmtBridgeIP = "10.97.8.1"
	f.d.mgmtIPAllocator = sysInstMgmtAllocator(t, "10.97.8.1")
	f.d.networkPlumber = &erroringPlumber{}
	sysInstLaunchRuns(f.d)
	return f
}

const testWorkerRegion = "ap-southeast-2"

func (f *sysInstFixture) workerInput(t *testing.T, eni *ec2.NetworkInterface, volumeID string) *elbv2domain.SystemInstanceInput {
	t.Helper()
	in := sqsWorkerInput(workerlaunch.Spec{
		InstanceType: f.itype, ProfileARN: testWorkerProfile, DataVolumeID: volumeID, Region: testWorkerRegion,
		Network: workerlaunch.Network{SubnetID: f.subnetID, ENIID: aws.StringValue(eni.NetworkInterfaceId),
			ENIMac: aws.StringValue(eni.MacAddress), ENIIP: aws.StringValue(eni.PrivateIpAddress), CIDR: "10.81.1.5/24", Gateway: "10.81.1.1"},
	})
	// The fixture's ENIs are in the customer test account.
	in.AccountID = testAccountID
	return in
}

func fwCfgValues(t *testing.T, inst *vm.VM) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, e := range inst.Config.FwCfg {
		b, err := os.ReadFile(e.File)
		require.NoError(t, err)
		got[e.Name] = string(b)
	}
	return got
}

// The worker launches through the real direct-boot path with its encrypted
// volume attached at boot, a mgmt NIC, its profile, and only public values in
// fw_cfg: data serial, Region, gateway URL on this node's bridge, CA. No
// first-store decision is passed: the guest asks the gateway for it.
func TestLaunchSQSWorkerInstance_BootsWithMgmtNICProfileAndPublicConfig(t *testing.T) {
	f := sqsWorkerFixture(t, true)
	vol := f.systemVolume(t)
	in := f.workerInput(t, f.newENI(t), vol)

	out, err := f.d.LaunchSQSWorkerInstance(t.Context(), in)
	require.NoError(t, err)
	inst, ok := f.d.vmMgr.Get(out.InstanceID)
	require.True(t, ok)

	assert.Equal(t, tags.ManagedBySQS, inst.ManagedBy)
	assert.NotEmpty(t, inst.MgmtIP, "the worker reaches the gateway only over its mgmt NIC")
	assert.Equal(t, testWorkerProfile, aws.StringValue(inst.Instance.IamInstanceProfile.Arn))
	assert.Equal(t, []vm.EBSRequest{{Name: vol, DeviceName: sqsStoreDataDevice, DeleteOnTermination: false}}, inst.EBSRequests.Requests)
	assert.Equal(t, filepath.Join(sqsWorkerImageDir(), "vmlinuz"), inst.Config.KernelImage)
	require.NotNil(t, inst.SQSWorkerGuest)
	assert.Nil(t, inst.SQSStoreGuest)
	assert.Equal(t, f.d.node, inst.SQSWorkerGuest.NodeID)

	got := fwCfgValues(t, inst)
	assert.Equal(t, vm.VolumeSerial(vol), got["opt/spinifex/sqs-worker/data-serial"])
	assert.Equal(t, testWorkerRegion, got["opt/spinifex/sqs-worker/region"])
	assert.Equal(t, "https://10.97.8.1:9999", got["opt/spinifex/sqs-worker/gateway-url"])
	assert.Contains(t, got["opt/spinifex/ca-cert"], "BEGIN CERTIFICATE")
	assert.Contains(t, got, "opt/spinifex/netcfg")
	assert.Len(t, got, 5, "no store decision or load balancer environment reaches the worker")
	for name, v := range got {
		_, err := nkeys.FromSeed([]byte(strings.TrimSpace(v)))
		assert.Error(t, err, "%s must hold no NKey seed", name)
	}
}

// Every refusal happens before capacity, a VM record or the ENI is touched.
func TestLaunchSQSWorkerInstance_RefusesBeforeAnySideEffect(t *testing.T) {
	f := sqsWorkerFixture(t, true)
	vol := f.systemVolume(t)
	seed, err := func() (string, error) {
		kp, err := nkeys.CreateUser()
		if err != nil {
			return "", err
		}
		s, err := kp.Seed()
		return string(s), err
	}()
	require.NoError(t, err)

	plain := sqsWorkerFixture(t, false)
	plainVol := plain.systemVolume(t)

	cases := []struct {
		name   string
		mutate func(*sysInstFixture, *elbv2domain.SystemInstanceInput)
	}{
		{"no profile", func(_ *sysInstFixture, in *elbv2domain.SystemInstanceInput) {
			in.IamInstanceProfileArn = ""
		}},
		{"caller CA", func(_ *sysInstFixture, in *elbv2domain.SystemInstanceInput) {
			in.CACert = "x"
		}},
		{"no mgmt NIC slot", func(_ *sysInstFixture, in *elbv2domain.SystemInstanceInput) {
			in.NICs = in.NICs[:1]
		}},
		{"seed as Region", func(_ *sysInstFixture, in *elbv2domain.SystemInstanceInput) {
			in.SQSWorker.Region = seed
		}},
		{"no Region", func(_ *sysInstFixture, in *elbv2domain.SystemInstanceInput) {
			in.SQSWorker.Region = ""
		}},
		{"store settings too", func(_ *sysInstFixture, in *elbv2domain.SystemInstanceInput) {
			in.SQSStore = &systeminstance.SQSStoreInput{}
		}},
		{"no mgmt bridge", func(f *sysInstFixture, _ *elbv2domain.SystemInstanceInput) {
			f.d.mgmtBridgeIP = ""
		}},
		{"no cluster CA", func(f *sysInstFixture, _ *elbv2domain.SystemInstanceInput) {
			f.d.config.NATS.CACert = ""
		}},
		{"unknown volume", func(_ *sysInstFixture, in *elbv2domain.SystemInstanceInput) {
			in.SQSWorker.DataVolumeID = "vol-0000000000000dead"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bridge, ca := f.d.mgmtBridgeIP, f.d.config.NATS.CACert
			t.Cleanup(func() { f.d.mgmtBridgeIP, f.d.config.NATS.CACert = bridge, ca })
			eni := f.newENI(t)
			in := f.workerInput(t, eni, vol)
			tc.mutate(f, in)
			vcpu, vms := f.capacity(), f.d.vmMgr.Count()
			_, err := f.d.LaunchSQSWorkerInstance(t.Context(), in)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), seed)
			f.sysInstNoSideEffects(t, eni, vcpu, vms)
		})
	}

	t.Run("unencrypted volume", func(t *testing.T) {
		eni := plain.newENI(t)
		in := plain.workerInput(t, eni, plainVol)
		vcpu, vms := plain.capacity(), plain.d.vmMgr.Count()
		_, err := plain.d.LaunchSQSWorkerInstance(t.Context(), in)
		require.ErrorContains(t, err, "not encrypted")
		plain.sysInstNoSideEffects(t, eni, vcpu, vms)
	})
}

// A failed mgmt NIC fails the worker launch and returns its address, where an
// internet-facing ALB would carry on without one.
func TestLaunchSQSWorkerInstance_RequiresTheMgmtNIC(t *testing.T) {
	f := sqsWorkerFixture(t, true)
	f.d.networkPlumber = &erroringPlumber{setupErr: errors.New("tap failed")}
	in := f.workerInput(t, f.newENI(t), f.systemVolume(t))
	_, err := f.d.LaunchSQSWorkerInstance(t.Context(), in)
	require.ErrorContains(t, err, "setup mgmt tap for SQS worker")
	assert.Equal(t, 0, f.d.mgmtIPAllocator.AllocatedCount(), "a failed mgmt NIC must return its address")
}

// A same-node relaunch rebuilds exactly the recorded config; another node is
// refused, so the worker is never moved.
func TestRefreshSQSWorkerState_SameNodeOnly(t *testing.T) {
	f := sqsWorkerFixture(t, true)
	in := f.workerInput(t, f.newENI(t), f.systemVolume(t))
	out, err := f.d.LaunchSQSWorkerInstance(t.Context(), in)
	require.NoError(t, err)
	inst, _ := f.d.vmMgr.Get(out.InstanceID)
	before := fwCfgValues(t, inst)
	for _, e := range inst.Config.FwCfg {
		require.NoError(t, os.Remove(e.File))
	}

	require.NoError(t, f.d.refreshSystemInstanceState(inst))
	assert.Equal(t, before, fwCfgValues(t, inst))

	node := f.d.node
	f.d.node = "node-elsewhere"
	t.Cleanup(func() { f.d.node = node })
	require.ErrorContains(t, f.d.refreshSystemInstanceState(inst), "only restarted there")
}

// The spec maps onto a launch the worker validator accepts, with the VPC ENI
// as the default NIC and an empty mgmt slot for the daemon to fill.
func TestSQSWorkerInput_MapsSpec(t *testing.T) {
	spec := workerlaunch.Spec{InstanceType: "sys.test", ProfileARN: testWorkerProfile, DataVolumeID: "vol-0123456789abcdef0",
		Region: testWorkerRegion, Network: workerlaunch.Network{SubnetID: "subnet-1", ENIID: "eni-1", ENIMac: "02:00:00:00:00:01",
			ENIIP: "10.240.0.10", CIDR: "10.240.0.10/22", Gateway: "10.240.0.1"}}
	in := sqsWorkerInput(spec)
	require.NoError(t, in.ValidateSQSWorker())
	assert.Equal(t, awsidentifiers.GlobalAccountID, in.AccountID)
	assert.Equal(t, systeminstance.KindSQSWorker, in.Kind)
	assert.Equal(t, testWorkerProfile, in.IamInstanceProfileArn)
	assert.Equal(t, []systeminstance.NICConfig{{MAC: "02:00:00:00:00:01", CIDR: "10.240.0.10/22", Gateway: "10.240.0.1", IsDefault: true}, {}}, in.NICs)
	assert.Equal(t, &systeminstance.SQSWorkerInput{DataVolumeID: spec.DataVolumeID, Region: testWorkerRegion}, in.SQSWorker)
}

type fakeRoleIAM struct {
	trust        string
	createdTrust string // trust a racing create left in place; "" uses the request's
	policy       string
	profile      *awsiam.InstanceProfile
	writes       []string
}

func (f *fakeRoleIAM) GetRole(_ string, in *awsiam.GetRoleInput) (*awsiam.GetRoleOutput, error) {
	if f.trust == "" {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &awsiam.GetRoleOutput{Role: &awsiam.Role{RoleName: in.RoleName, AssumeRolePolicyDocument: aws.String(f.trust)}}, nil
}

func (f *fakeRoleIAM) CreateRole(_ string, in *awsiam.CreateRoleInput) (*awsiam.CreateRoleOutput, error) {
	f.writes = append(f.writes, "CreateRole")
	if f.createdTrust != "" {
		f.trust = f.createdTrust
		return nil, errors.New(awserrors.ErrorIAMEntityAlreadyExists)
	}
	f.trust = aws.StringValue(in.AssumeRolePolicyDocument)
	return &awsiam.CreateRoleOutput{}, nil
}

func (f *fakeRoleIAM) PutRolePolicy(_ string, in *awsiam.PutRolePolicyInput) (*awsiam.PutRolePolicyOutput, error) {
	f.writes = append(f.writes, "PutRolePolicy")
	f.policy = aws.StringValue(in.PolicyDocument)
	return &awsiam.PutRolePolicyOutput{}, nil
}

func (f *fakeRoleIAM) GetInstanceProfile(string, *awsiam.GetInstanceProfileInput) (*awsiam.GetInstanceProfileOutput, error) {
	if f.profile == nil {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	p := *f.profile
	p.Roles = slices.Clone(f.profile.Roles)
	return &awsiam.GetInstanceProfileOutput{InstanceProfile: &p}, nil
}

func (f *fakeRoleIAM) CreateInstanceProfile(_ string, in *awsiam.CreateInstanceProfileInput) (*awsiam.CreateInstanceProfileOutput, error) {
	f.writes = append(f.writes, "CreateInstanceProfile")
	f.profile = &awsiam.InstanceProfile{Arn: aws.String(testWorkerProfile), InstanceProfileName: in.InstanceProfileName}
	return &awsiam.CreateInstanceProfileOutput{InstanceProfile: f.profile}, nil
}

func (f *fakeRoleIAM) AddRoleToInstanceProfile(_ string, in *awsiam.AddRoleToInstanceProfileInput) (*awsiam.AddRoleToInstanceProfileOutput, error) {
	f.writes = append(f.writes, "AddRoleToInstanceProfile")
	f.profile.Roles = append(f.profile.Roles, &awsiam.Role{RoleName: in.RoleName})
	return &awsiam.AddRoleToInstanceProfileOutput{}, nil
}

func sqsWorkerRolesWith(f *fakeRoleIAM) sqsWorkerRoles {
	return sqsWorkerRoles{provider: func() iamdomain.SystemInstanceRoleEnsurer { return f }}
}

var broadWorkerTrust = strings.Replace(iamdomain.EC2InstanceTrustPolicy, `{"Service":"ec2.amazonaws.com"}`,
	`{"Service":"ec2.amazonaws.com","AWS":"arn:aws:iam::123456789012:root"}`, 1)

// The role is created with EC2-only trust and granted exactly the gateway's
// internal actions, and the new profile carries exactly that role.
func TestSQSWorkerRoles_EC2OnlyTrustAndMinimalPolicy(t *testing.T) {
	ctx := context.Background()
	fake := &fakeRoleIAM{}
	arn, err := sqsWorkerRolesWith(fake).EnsureWorkerProfile(ctx)
	require.NoError(t, err)
	assert.Equal(t, testWorkerProfile, arn)
	assert.Equal(t, []string{"CreateRole", "PutRolePolicy", "CreateInstanceProfile", "AddRoleToInstanceProfile"}, fake.writes)

	var doc iamdomain.PolicyDocument
	require.NoError(t, json.Unmarshal([]byte(fake.policy), &doc))
	require.Len(t, doc.Statement, 1)
	got := slices.Sorted(slices.Values(doc.Statement[0].Action))
	assert.Equal(t, []string{"spinifex-sqs:ClaimFirstStore", "spinifex-sqs:CompleteWork", "spinifex-sqs:DescribeStore",
		"spinifex-sqs:PollWork", "spinifex-sqs:RecordWorkerPublicKey"}, got)

	fake.trust = url.QueryEscape(iamdomain.EC2InstanceTrustPolicy)
	fake.writes = nil
	_, err = sqsWorkerRolesWith(fake).EnsureWorkerProfile(ctx)
	require.NoError(t, err, "an URL-encoded EC2-only trust is the same trust")
	assert.Equal(t, []string{"PutRolePolicy"}, fake.writes, "a correct profile is reused unchanged")

	_, err = sqsWorkerRoles{}.EnsureWorkerProfile(ctx)
	require.Error(t, err)
}

// A pre-existing role with broader trust, found directly or left by a racing
// create, is refused before any policy, profile or attachment is written.
func TestSQSWorkerRoles_UnsafeRoleIsUntouched(t *testing.T) {
	for name, fake := range map[string]*fakeRoleIAM{
		"existing":     {trust: broadWorkerTrust},
		"racingCreate": {createdTrust: broadWorkerTrust},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := sqsWorkerRolesWith(fake).EnsureWorkerProfile(context.Background())
			require.ErrorContains(t, err, "trusts more than the EC2 service")
			assert.Empty(t, fake.policy, "policy granted to an unsafe role")
			assert.Nil(t, fake.profile, "profile created for an unsafe role")
			assert.NotContains(t, fake.writes, "PutRolePolicy")
		})
	}
}

// A profile that already carries another role is refused and never modified.
func TestSQSWorkerRoles_RefusesAMismatchedProfile(t *testing.T) {
	for name, roles := range map[string][]*awsiam.Role{
		"otherRole":  {{RoleName: aws.String("rdsInstanceRole")}},
		"extraRole":  {{RoleName: aws.String(workerrole.RoleName)}, {RoleName: aws.String("rdsInstanceRole")}},
		"nilRoleRef": {nil},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeRoleIAM{trust: iamdomain.EC2InstanceTrustPolicy,
				profile: &awsiam.InstanceProfile{Arn: aws.String(testWorkerProfile), Roles: roles}}
			_, err := sqsWorkerRolesWith(fake).EnsureWorkerProfile(context.Background())
			require.ErrorContains(t, err, "instance profile")
			assert.NotContains(t, fake.writes, "AddRoleToInstanceProfile")
			assert.Len(t, fake.profile.Roles, len(roles), "profile roles changed")
		})
	}
}

// Without a cluster storage key the volume adapter refuses before the
// preparer runs, so no unencrypted volume is allocated.
func TestSQSWorkerVolumes_RefusesBeforeAllocatingWithoutAStorageKey(t *testing.T) {
	v := sqsWorkerVolumes{prep: &storevolume.Preparer{}, availabilityZone: "az-1", sizeGiB: 1}
	_, err := v.Prepare(t.Context())
	require.ErrorIs(t, err, workerlaunch.ErrUnencrypted)
}

// Encryption is read from the volume's own record, never a caller flag.
func TestSQSWorkerVolumes_EncryptedReadsTheRecord(t *testing.T) {
	for _, enc := range []bool{true, false} {
		f := sqsWorkerFixture(t, enc)
		v := sqsWorkerVolumes{volumes: f.d.volumeService}
		got, err := v.Encrypted(t.Context(), f.systemVolume(t))
		require.NoError(t, err)
		assert.Equal(t, enc, got)
		_, err = v.Encrypted(t.Context(), "vol-0000000000000dead")
		require.Error(t, err)
	}
}
