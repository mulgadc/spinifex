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

func (f *sysInstFixture) workerInput(t *testing.T, eni *ec2.NetworkInterface, volumeID, status, key string) *elbv2domain.SystemInstanceInput {
	t.Helper()
	in := sqsWorkerInput(workerlaunch.Spec{
		InstanceType: f.itype, ProfileARN: testWorkerProfile, DataVolumeID: volumeID, StoreStatus: status, StoreNKey: key,
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
// fw_cfg: data serial, store decision, gateway URL on this node's bridge, CA.
func TestLaunchSQSWorkerInstance_BootsWithMgmtNICProfileAndPublicConfig(t *testing.T) {
	f := sqsWorkerFixture(t, true)
	vol := f.systemVolume(t)
	key := testClientNKey(t)
	in := f.workerInput(t, f.newENI(t), vol, systeminstance.SQSWorkerStoreExisting, key)

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
	assert.Equal(t, "existing", got["opt/spinifex/sqs-worker/store-status"])
	assert.Equal(t, key, got["opt/spinifex/sqs-worker/store-nkey"])
	assert.Equal(t, "https://10.97.8.1:9999", got["opt/spinifex/sqs-worker/gateway-url"])
	assert.Contains(t, got["opt/spinifex/ca-cert"], "BEGIN CERTIFICATE")
	assert.Contains(t, got, "opt/spinifex/netcfg")
	assert.Len(t, got, 6, "no load balancer environment reaches the worker")
	for name, v := range got {
		_, err := nkeys.FromSeed([]byte(strings.TrimSpace(v)))
		assert.Error(t, err, "%s must hold no NKey seed", name)
	}
}

// Every refusal happens before capacity, a VM record or the ENI is touched.
func TestLaunchSQSWorkerInstance_RefusesBeforeAnySideEffect(t *testing.T) {
	f := sqsWorkerFixture(t, true)
	vol := f.systemVolume(t)
	key := testClientNKey(t)
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
		{"seed as store key", func(_ *sysInstFixture, in *elbv2domain.SystemInstanceInput) {
			in.SQSWorker.StoreNKey = seed
		}},
		{"unknown status", func(_ *sysInstFixture, in *elbv2domain.SystemInstanceInput) {
			in.SQSWorker.StoreStatus = ""
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
			in := f.workerInput(t, eni, vol, systeminstance.SQSWorkerStoreExisting, key)
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
		in := plain.workerInput(t, eni, plainVol, systeminstance.SQSWorkerStoreExisting, key)
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
	in := f.workerInput(t, f.newENI(t), f.systemVolume(t), systeminstance.SQSWorkerStoreExisting, testClientNKey(t))
	_, err := f.d.LaunchSQSWorkerInstance(t.Context(), in)
	require.ErrorContains(t, err, "setup mgmt tap for SQS worker")
	assert.Equal(t, 0, f.d.mgmtIPAllocator.AllocatedCount(), "a failed mgmt NIC must return its address")
}

// A same-node relaunch rebuilds exactly the recorded config; another node is
// refused, so the worker is never moved.
func TestRefreshSQSWorkerState_SameNodeOnly(t *testing.T) {
	f := sqsWorkerFixture(t, true)
	in := f.workerInput(t, f.newENI(t), f.systemVolume(t), systeminstance.SQSWorkerStoreExisting, testClientNKey(t))
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
		StoreStatus: "new", Network: workerlaunch.Network{SubnetID: "subnet-1", ENIID: "eni-1", ENIMac: "02:00:00:00:00:01",
			ENIIP: "10.240.0.10", CIDR: "10.240.0.10/22", Gateway: "10.240.0.1"}}
	in := sqsWorkerInput(spec)
	require.NoError(t, in.ValidateSQSWorker())
	assert.Equal(t, awsidentifiers.GlobalAccountID, in.AccountID)
	assert.Equal(t, systeminstance.KindSQSWorker, in.Kind)
	assert.Equal(t, testWorkerProfile, in.IamInstanceProfileArn)
	assert.Equal(t, []systeminstance.NICConfig{{MAC: "02:00:00:00:00:01", CIDR: "10.240.0.10/22", Gateway: "10.240.0.1", IsDefault: true}, {}}, in.NICs)
	assert.Equal(t, &systeminstance.SQSWorkerInput{DataVolumeID: spec.DataVolumeID, StoreStatus: "new"}, in.SQSWorker)
}

type fakeRoleIAM struct {
	trust  string
	policy string
}

func (f *fakeRoleIAM) GetRole(_ string, in *awsiam.GetRoleInput) (*awsiam.GetRoleOutput, error) {
	if f.trust == "" {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &awsiam.GetRoleOutput{Role: &awsiam.Role{RoleName: in.RoleName, AssumeRolePolicyDocument: aws.String(f.trust)}}, nil
}

func (f *fakeRoleIAM) CreateRole(_ string, in *awsiam.CreateRoleInput) (*awsiam.CreateRoleOutput, error) {
	f.trust = aws.StringValue(in.AssumeRolePolicyDocument)
	return &awsiam.CreateRoleOutput{}, nil
}

func (f *fakeRoleIAM) PutRolePolicy(_ string, in *awsiam.PutRolePolicyInput) (*awsiam.PutRolePolicyOutput, error) {
	f.policy = aws.StringValue(in.PolicyDocument)
	return &awsiam.PutRolePolicyOutput{}, nil
}

func (f *fakeRoleIAM) GetInstanceProfile(_ string, in *awsiam.GetInstanceProfileInput) (*awsiam.GetInstanceProfileOutput, error) {
	return &awsiam.GetInstanceProfileOutput{InstanceProfile: &awsiam.InstanceProfile{
		Arn: aws.String(testWorkerProfile), InstanceProfileName: in.InstanceProfileName,
		Roles: []*awsiam.Role{{RoleName: aws.String(workerrole.RoleName)}},
	}}, nil
}

func (f *fakeRoleIAM) CreateInstanceProfile(string, *awsiam.CreateInstanceProfileInput) (*awsiam.CreateInstanceProfileOutput, error) {
	return &awsiam.CreateInstanceProfileOutput{}, nil
}

func (f *fakeRoleIAM) AddRoleToInstanceProfile(string, *awsiam.AddRoleToInstanceProfileInput) (*awsiam.AddRoleToInstanceProfileOutput, error) {
	return &awsiam.AddRoleToInstanceProfileOutput{}, nil
}

// The role is created with EC2-only trust and granted exactly the gateway's
// internal actions; a pre-existing role that trusts anything more is refused.
func TestSQSWorkerRoles_EC2OnlyTrustAndMinimalPolicy(t *testing.T) {
	ctx := context.Background()
	fake := &fakeRoleIAM{}
	roles := sqsWorkerRoles{provider: func() iamdomain.SystemInstanceRoleEnsurer { return fake }}
	arn, err := roles.EnsureWorkerProfile(ctx)
	require.NoError(t, err)
	assert.Equal(t, testWorkerProfile, arn)

	var doc iamdomain.PolicyDocument
	require.NoError(t, json.Unmarshal([]byte(fake.policy), &doc))
	require.Len(t, doc.Statement, 1)
	got := slices.Sorted(slices.Values(doc.Statement[0].Action))
	assert.Equal(t, []string{"spinifex-sqs:ClaimFirstStore", "spinifex-sqs:RecordWorkerPublicKey"}, got)

	fake.trust = url.QueryEscape(iamdomain.EC2InstanceTrustPolicy)
	_, err = roles.EnsureWorkerProfile(ctx)
	require.NoError(t, err, "an URL-encoded EC2-only trust is the same trust")

	broad := strings.Replace(iamdomain.EC2InstanceTrustPolicy, `{"Service":"ec2.amazonaws.com"}`,
		`{"Service":"ec2.amazonaws.com","AWS":"arn:aws:iam::123456789012:root"}`, 1)
	fake.trust = broad
	_, err = roles.EnsureWorkerProfile(ctx)
	require.ErrorContains(t, err, "trusts more than the EC2 service")

	_, err = sqsWorkerRoles{}.EnsureWorkerProfile(ctx)
	require.Error(t, err)
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
