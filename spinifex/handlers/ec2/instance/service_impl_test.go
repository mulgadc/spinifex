package handlers_ec2_instance

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"text/template"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mgrWith returns a vm.Manager pre-populated with vms. Test fixture for
// services that previously took a *vm.Instances; post-vm.Manager refactor we
// build a Manager and Replace its set rather than inlining a map.
func mgrWith(vms map[string]*vm.VM) *vm.Manager {
	m := vm.NewManager()
	if len(vms) > 0 {
		m.Replace(vms)
	}
	return m
}

func TestNewInstanceServiceImpl(t *testing.T) {
	cfg := &config.Config{}
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}
	store := objectstore.NewMemoryObjectStore()
	mgr := vm.NewManager()

	svc := NewInstanceServiceImpl(cfg, instanceTypes, nil, store, mgr, nil, nil)

	require.NotNil(t, svc)
	assert.Equal(t, cfg, svc.config)
	assert.Equal(t, instanceTypes, svc.instanceTypes)
	assert.Nil(t, svc.natsConn)
	assert.Equal(t, mgr, svc.vmMgr)
	assert.Equal(t, store, svc.objectStore)
}

func TestGenerateHostname(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		want       string
	}{
		{
			name:       "Normal instance ID",
			instanceID: "i-0123456789abcdef0",
			want:       "spinifex-vm-01234567",
		},
		{
			name:       "Too short (2 chars)",
			instanceID: "ab",
			want:       "spinifex-vm-unknown",
		},
		{
			name:       "Empty string",
			instanceID: "",
			want:       "spinifex-vm-unknown",
		},
		{
			name:       "Exactly 10 chars",
			instanceID: "i-abcdef01",
			want:       "spinifex-vm-abcdef01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generateHostname(tt.instanceID)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRunInstance_Success(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
		"t3.small": {InstanceType: aws.String("t3.small")},
	}

	svc := &InstanceServiceImpl{
		instanceTypes: instanceTypes,
	}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0abcdef1234567890"),
		InstanceType: aws.String("t3.micro"),
		KeyName:      aws.String("my-key"),
	}

	instance, ec2Instance, err := svc.RunInstance(input)

	require.NoError(t, err)
	require.NotNil(t, instance)
	require.NotNil(t, ec2Instance)

	// Verify VM struct
	assert.Contains(t, instance.ID, "i-")
	assert.Equal(t, vm.StateProvisioning, instance.Status)
	assert.Equal(t, "t3.micro", instance.InstanceType)
	assert.Equal(t, input, instance.RunInstancesInput)
	assert.Equal(t, ec2Instance, instance.Instance)

	// Verify EC2 metadata
	assert.Equal(t, instance.ID, *ec2Instance.InstanceId)
	assert.Equal(t, "t3.micro", *ec2Instance.InstanceType)
	assert.Equal(t, "ami-0abcdef1234567890", *ec2Instance.ImageId)
	assert.Equal(t, "my-key", *ec2Instance.KeyName)
	assert.Equal(t, int64(0), *ec2Instance.State.Code)
	assert.Equal(t, "pending", *ec2Instance.State.Name)
	assert.NotNil(t, ec2Instance.LaunchTime)
}

func TestRunInstance_NoKeyName(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}

	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
	}

	instance, ec2Instance, err := svc.RunInstance(input)

	require.NoError(t, err)
	require.NotNil(t, instance)
	assert.Nil(t, ec2Instance.KeyName)
}

func TestRunInstance_InvalidInstanceType(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}

	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("nonexistent.type"),
	}

	instance, ec2Instance, err := svc.RunInstance(input)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceType, err.Error())
	assert.Nil(t, instance)
	assert.Nil(t, ec2Instance)
}

func TestRunInstance_UniqueIDs(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}

	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
	}

	instance1, _, err1 := svc.RunInstance(input)
	instance2, _, err2 := svc.RunInstance(input)

	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.NotEqual(t, instance1.ID, instance2.ID, "Each instance should have a unique ID")
}

func TestCloudInitTemplateRendering(t *testing.T) {
	tests := []struct {
		name        string
		data        CloudInitData
		contains    []string
		notContains []string
	}{
		{
			name: "Basic SSH key and hostname",
			data: CloudInitData{
				Username: "ec2-user",
				SSHKey:   "ssh-rsa AAAAB3... user@host",
				Hostname: "spinifex-vm-01234567",
			},
			contains: []string{
				"ec2-user",
				"ssh-rsa AAAAB3... user@host",
				"spinifex-vm-01234567",
				"#cloud-config",
			},
		},
		{
			name: "With cloud-config userdata",
			data: CloudInitData{
				Username:            "ec2-user",
				SSHKey:              "ssh-ed25519 AAAA...",
				Hostname:            "spinifex-vm-abcdef01",
				UserDataCloudConfig: "packages:\n  - nginx",
			},
			contains: []string{
				"packages:",
				"nginx",
			},
		},
		{
			name: "With script userdata",
			data: CloudInitData{
				Username:       "ec2-user",
				SSHKey:         "ssh-rsa AAAA...",
				Hostname:       "spinifex-vm-test",
				UserDataScript: "    #!/bin/bash\n    echo hello",
			},
			contains: []string{
				"write_files:",
				"/tmp/cloud-init-startup.sh",
				"runcmd:",
				"echo hello",
			},
		},
		{
			name: "With CA certificate PEM",
			data: CloudInitData{
				Username: "ec2-user",
				SSHKey:   "ssh-rsa AAAA...",
				Hostname: "spinifex-vm-ca-test",
				CACertPEM: "      -----BEGIN CERTIFICATE-----\n" +
					"      MIIFazCCA1OgAwIBAgIUAbcdefg1234567890ABCDEFG=\n" +
					"      -----END CERTIFICATE-----\n",
			},
			contains: []string{
				"ca_certs:",
				"trusted:",
				"-----BEGIN CERTIFICATE-----",
				"-----END CERTIFICATE-----",
				"MIIFazCCA1OgAwIBAgIUAbcdefg1234567890ABCDEFG=",
			},
		},
		{
			name: "Without CA certificate PEM",
			data: CloudInitData{
				Username: "ec2-user",
				SSHKey:   "ssh-rsa AAAA...",
				Hostname: "spinifex-vm-no-ca",
			},
			contains: []string{
				"#cloud-config",
				"spinifex-vm-no-ca",
			},
			notContains: []string{
				"ca_certs:",
				"trusted:",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := template.Must(template.New("cloud-init").Parse(cloudInitUserDataTemplate))
			var buf bytes.Buffer
			err := tmpl.Execute(&buf, tt.data)
			require.NoError(t, err)

			rendered := buf.String()
			for _, s := range tt.contains {
				assert.Contains(t, rendered, s)
			}
			for _, s := range tt.notContains {
				assert.NotContains(t, rendered, s)
			}
		})
	}
}

func TestCloudInitMetaTemplateRendering(t *testing.T) {
	data := CloudInitMetaData{
		InstanceID: "i-0123456789abcdef0",
		Hostname:   "spinifex-vm-01234567",
	}

	tmpl := template.Must(template.New("meta-data").Parse(cloudInitMetaTemplate))
	var buf bytes.Buffer
	err := tmpl.Execute(&buf, data)
	require.NoError(t, err)

	rendered := buf.String()
	assert.Contains(t, rendered, "i-0123456789abcdef0")
	assert.Contains(t, rendered, "spinifex-vm-01234567")
	assert.Contains(t, rendered, "instance-id:")
	assert.Contains(t, rendered, "local-hostname:")
}

// TestCloudInitVolumeNamePerInstance verifies that AMI-based launches produce
// unique root volume IDs, which in turn produce unique cloud-init volume names.
// This prevents the bug where a cached cloud-init ISO (keyed by AMI) would
// serve stale SSH keys or hostnames to subsequent instances.
func TestRunInstance_NoImageId(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}

	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
	}

	instance, ec2Instance, err := svc.RunInstance(input)
	require.NoError(t, err)
	require.NotNil(t, instance)
	assert.Nil(t, ec2Instance.ImageId)
	assert.Nil(t, ec2Instance.KeyName)
}

func TestParseVolumeParams_Defaults(t *testing.T) {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-0abcdef1234567890"),
	}

	p := parseVolumeParams(input)

	assert.Equal(t, 4*1024*1024*1024, p.size, "default size should be 4GB")
	assert.Equal(t, "/dev/vda", p.deviceName, "default device should be /dev/vda")
	assert.True(t, p.deleteOnTermination, "deleteOnTermination should default to true")
	assert.Empty(t, p.volumeType)
	assert.Zero(t, p.iops)
	// AMI-based: imageId should be a generated vol-*, snapshotId should be the AMI
	assert.True(t, strings.HasPrefix(p.imageId, "vol-"), "AMI launch should generate vol- ID")
	assert.Equal(t, "ami-0abcdef1234567890", p.snapshotId)
}

func TestParseVolumeParams_CustomBlockDeviceMapping(t *testing.T) {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-test123"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize:          aws.Int64(20),
					VolumeType:          aws.String("gp3"),
					Iops:                aws.Int64(3000),
					DeleteOnTermination: aws.Bool(false),
				},
			},
		},
	}

	p := parseVolumeParams(input)

	assert.Equal(t, 20*1024*1024*1024, p.size, "size should be 20 GiB in bytes")
	assert.Equal(t, "/dev/sda1", p.deviceName)
	assert.Equal(t, "gp3", p.volumeType)
	assert.Equal(t, 3000, p.iops)
	assert.False(t, p.deleteOnTermination)
}

func TestParseVolumeParams_BlockDeviceMappingNoEbs(t *testing.T) {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-test"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/xvda"),
			},
		},
	}

	p := parseVolumeParams(input)

	assert.Equal(t, "/dev/xvda", p.deviceName)
	assert.Equal(t, 4*1024*1024*1024, p.size, "size should stay at default without Ebs")
	assert.True(t, p.deleteOnTermination)
}

func TestParseVolumeParams_NonAMIImageId(t *testing.T) {
	rawImageId := "vol-existing-volume-id"
	input := &ec2.RunInstancesInput{
		ImageId: aws.String(rawImageId),
	}

	p := parseVolumeParams(input)

	assert.Equal(t, rawImageId, p.imageId, "non-AMI imageId should be used directly")
	assert.Empty(t, p.snapshotId, "non-AMI launch should have no snapshotId")
}

func TestParseVolumeParams_PartialEbs(t *testing.T) {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-partial"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize: aws.Int64(8),
				},
			},
		},
	}

	p := parseVolumeParams(input)

	assert.Equal(t, 8*1024*1024*1024, p.size)
	assert.Equal(t, "/dev/vda", p.deviceName, "device should stay at default")
	assert.Empty(t, p.volumeType, "volumeType should stay empty")
	assert.Zero(t, p.iops, "iops should stay zero")
	assert.True(t, p.deleteOnTermination, "deleteOnTermination should stay at default")
}

func TestMockInstanceService(t *testing.T) {
	svc := NewMockInstanceService()
	require.NotNil(t, svc)

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-0123456789abcdef0"),
		InstanceType: aws.String("t3.micro"),
		KeyName:      aws.String("my-key"),
		SubnetId:     aws.String("subnet-abc123"),
	}

	reservation, err := svc.RunInstances(input, "123456789012")
	require.NoError(t, err)
	require.NotNil(t, reservation)
	assert.Equal(t, "123456789012", *reservation.OwnerId)
	require.Len(t, reservation.Instances, 1)

	inst := reservation.Instances[0]
	assert.Equal(t, "i-0123456789abcdef0", *inst.InstanceId)
	assert.Equal(t, "running", *inst.State.Name)
	assert.Equal(t, "ami-0123456789abcdef0", *inst.ImageId)
	assert.Equal(t, "t3.micro", *inst.InstanceType)
	assert.Equal(t, "my-key", *inst.KeyName)
	assert.Equal(t, "subnet-abc123", *inst.SubnetId)
}

func TestCloudInitNetworkConfigWildcard(t *testing.T) {
	// No MACs → wildcard config (non-VPC or VPC without DEV_NETWORKING)
	cfg := generateNetworkConfig("", "", "", "", nil)
	assert.Contains(t, cfg, "version: 2")
	assert.Contains(t, cfg, "dhcp4: true")
	assert.Contains(t, cfg, "dhcp-identifier: mac")
	assert.Contains(t, cfg, `name: "e*"`, "wildcard should match both eth* and en* interfaces")
	assert.NotContains(t, cfg, "use-routes")
}

func TestCloudInitNetworkConfigDualNIC(t *testing.T) {
	eniMAC := "02:00:00:61:ef:c2"
	devMAC := "02:de:00:60:83:0d"

	cfg := generateNetworkConfig(eniMAC, devMAC, "", "", nil)

	// Both MACs present in config
	assert.Contains(t, cfg, eniMAC)
	assert.Contains(t, cfg, devMAC)

	// VPC NIC gets normal DHCP (with default route)
	assert.Contains(t, cfg, "vpc0:")
	assert.Contains(t, cfg, "dev0:")

	// Dev NIC has route/DNS suppressed
	assert.Contains(t, cfg, "use-routes: false")
	assert.Contains(t, cfg, "use-dns: false")

	// No wildcard match — per-interface only
	assert.NotContains(t, cfg, `name: "e*"`)
}

func TestCloudInitNetworkConfigPartialMAC(t *testing.T) {
	// Only ENI MAC (VPC without dev) → per-interface config with VPC NIC only
	cfg := generateNetworkConfig("02:00:00:61:ef:c2", "", "", "", nil)
	assert.Contains(t, cfg, "vpc0:")
	assert.NotContains(t, cfg, "dev0:")

	// Only dev MAC (shouldn't happen, but defensive) → wildcard
	cfg = generateNetworkConfig("", "02:de:00:60:83:0d", "", "", nil)
	assert.Contains(t, cfg, `name: "e*"`)
	assert.NotContains(t, cfg, "use-routes")
}

func TestCloudInitNetworkConfigMultiVPCNICs(t *testing.T) {
	// Multi-subnet ALB VM: primary ENI + two extras, each on a different AZ.
	extras := []string{
		"02:00:00:bb:bb:bb",
		"02:00:00:cc:cc:cc",
	}
	cfg := generateNetworkConfig("02:00:00:aa:aa:aa", "", "", "", extras)

	assert.Contains(t, cfg, "vpc0:")
	assert.Contains(t, cfg, "vpc1:")
	assert.Contains(t, cfg, "vpc2:")
	assert.Contains(t, cfg, `macaddress: "02:00:00:aa:aa:aa"`)
	assert.Contains(t, cfg, `macaddress: "02:00:00:bb:bb:bb"`)
	assert.Contains(t, cfg, `macaddress: "02:00:00:cc:cc:cc"`)
	// Each extra NIC gets its own DHCP block with dhcp-identifier: mac.
	assert.Equal(t, 3, strings.Count(cfg, "dhcp4: true"))
	assert.Equal(t, 3, strings.Count(cfg, "dhcp-identifier: mac"))
	// No dev0 / mgmt0 unless explicitly configured.
	assert.NotContains(t, cfg, "dev0:")
	assert.NotContains(t, cfg, "mgmt0:")
}

func TestCloudInitNetworkConfigEmptyExtraMACSkipped(t *testing.T) {
	// Empty strings inside the extras slice are ignored rather than producing
	// a malformed ethernets block.
	cfg := generateNetworkConfig("02:00:00:aa:aa:aa", "", "", "", []string{""})
	assert.Contains(t, cfg, "vpc0:")
	assert.NotContains(t, cfg, "vpc1:")
}

func TestRunInstance_WithTags(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}
	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("instance"),
				Tags: []*ec2.Tag{
					{Key: aws.String("Name"), Value: aws.String("test-vm")},
					{Key: aws.String("env"), Value: aws.String("dev")},
				},
			},
		},
	}

	instance, _, err := svc.RunInstance(input)
	require.NoError(t, err)
	// Tags are stored in RunInstancesInput which is preserved on the VM
	assert.Equal(t, input, instance.RunInstancesInput)
	assert.Len(t, input.TagSpecifications, 1)
	assert.Len(t, input.TagSpecifications[0].Tags, 2)
}

func TestRunInstance_WithPlacement(t *testing.T) {
	instanceTypes := map[string]*ec2.InstanceTypeInfo{
		"t3.micro": {InstanceType: aws.String("t3.micro")},
	}
	svc := &InstanceServiceImpl{instanceTypes: instanceTypes}

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-012345"),
		InstanceType: aws.String("t3.micro"),
		Placement: &ec2.Placement{
			GroupName: aws.String("my-pg"),
		},
	}

	instance, _, err := svc.RunInstance(input)
	require.NoError(t, err)
	assert.Equal(t, "my-pg", *instance.RunInstancesInput.Placement.GroupName)
}

func TestParseVolumeParams_MultipleBlockDeviceMappings(t *testing.T) {
	// parseVolumeParams only uses the first block device mapping
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-multi"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize: aws.Int64(30),
					VolumeType: aws.String("gp3"),
				},
			},
			{
				DeviceName: aws.String("/dev/sdb"),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize: aws.Int64(100),
					VolumeType: aws.String("io1"),
					Iops:       aws.Int64(5000),
				},
			},
		},
	}

	p := parseVolumeParams(input)
	// First mapping wins for root volume
	assert.Equal(t, 30*1024*1024*1024, p.size)
	assert.Equal(t, "/dev/sda1", p.deviceName)
	assert.Equal(t, "gp3", p.volumeType)
}

func TestParseVolumeParams_Io1WithIops(t *testing.T) {
	input := &ec2.RunInstancesInput{
		ImageId: aws.String("ami-io1"),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize: aws.Int64(100),
					VolumeType: aws.String("io1"),
					Iops:       aws.Int64(5000),
				},
			},
		},
	}

	p := parseVolumeParams(input)
	assert.Equal(t, 100*1024*1024*1024, p.size)
	assert.Equal(t, "io1", p.volumeType)
	assert.Equal(t, 5000, p.iops)
}

func TestCloudInitVolumeNamePerInstance(t *testing.T) {
	amiID := "ami-0abcdef1234567890"

	seen := make(map[string]bool)
	for range 100 {
		// Simulate GenerateVolumes logic for AMI-based launches (line 194-195)
		var rootVolumeId string
		if strings.HasPrefix(amiID, "ami-") {
			rootVolumeId = utils.GenerateResourceID("vol")
		}

		cloudInitName := fmt.Sprintf("%s-cloudinit", rootVolumeId)

		assert.True(t, strings.HasPrefix(cloudInitName, "vol-"),
			"cloud-init volume should be keyed by root volume ID, not AMI ID")
		assert.True(t, strings.HasSuffix(cloudInitName, "-cloudinit"))
		assert.False(t, strings.Contains(cloudInitName, "ami-"),
			"cloud-init volume name must not contain the AMI ID")
		assert.False(t, seen[cloudInitName],
			"each instance must get a unique cloud-init volume name")
		seen[cloudInitName] = true
	}
}

// --- 1b-pre Describe* coverage (siv-22 parts) ------------------------------

type fakeResourceCapacityProvider struct {
	types      []*ec2.InstanceTypeInfo
	gotShowCap bool
	calls      int
}

func (f *fakeResourceCapacityProvider) GetAvailableInstanceTypeInfos(showCapacity bool) []*ec2.InstanceTypeInfo {
	f.calls++
	f.gotShowCap = showCapacity
	return f.types
}

type fakeStoppedStore struct {
	stopped     []*vm.VM
	terminated  []*vm.VM
	loadByID    map[string]*vm.VM
	listErr     error
	listTermErr error
	loadErr     error
}

func (f *fakeStoppedStore) LoadStoppedInstance(id string) (*vm.VM, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if v, ok := f.loadByID[id]; ok {
		return v, nil
	}
	return nil, nil
}
func (f *fakeStoppedStore) ListStoppedInstances() ([]*vm.VM, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.stopped, nil
}
func (f *fakeStoppedStore) ListTerminatedInstances() ([]*vm.VM, error) {
	if f.listTermErr != nil {
		return nil, f.listTermErr
	}
	return f.terminated, nil
}

func TestDescribeInstanceTypes_NilResourceMgr(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDescribeInstanceTypes_ReturnsProviderTypes(t *testing.T) {
	prov := &fakeResourceCapacityProvider{
		types: []*ec2.InstanceTypeInfo{
			{InstanceType: aws.String("t3.micro")},
			{InstanceType: aws.String("t3.small")},
		},
	}
	svc := &InstanceServiceImpl{resourceMgr: prov}

	out, err := svc.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{}, "")
	require.NoError(t, err)
	require.Len(t, out.InstanceTypes, 2)
	assert.False(t, prov.gotShowCap)
}

func TestDescribeInstanceTypes_CapacityFilterPropagates(t *testing.T) {
	prov := &fakeResourceCapacityProvider{}
	svc := &InstanceServiceImpl{resourceMgr: prov}

	input := &ec2.DescribeInstanceTypesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("capacity"), Values: []*string{aws.String("true")}},
		},
	}
	_, err := svc.DescribeInstanceTypes(input, "")
	require.NoError(t, err)
	assert.True(t, prov.gotShowCap, "capacity=true filter must reach the provider")
}

func TestDescribeInstances_Empty(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	out, err := svc.DescribeInstances(&ec2.DescribeInstancesInput{}, utils.GlobalAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Reservations)
}

func TestDescribeInstances_OneVisibleInstance(t *testing.T) {
	id := "i-aaa111"
	resID := "r-1"
	owner := "111122223333"
	v := &vm.VM{
		ID:           id,
		InstanceType: "t3.micro",
		Status:       vm.StateRunning,
		AccountID:    owner,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String(resID),
			OwnerId:       aws.String(owner),
		},
		Instance: &ec2.Instance{InstanceId: aws.String(id), InstanceType: aws.String("t3.micro")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}

	out, err := svc.DescribeInstances(&ec2.DescribeInstancesInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	assert.Equal(t, resID, *out.Reservations[0].ReservationId)
	require.Len(t, out.Reservations[0].Instances, 1)
	assert.Equal(t, id, *out.Reservations[0].Instances[0].InstanceId)
}

func TestDescribeInstances_AccountFilteringHidesOtherTenant(t *testing.T) {
	v := &vm.VM{
		ID:        "i-other",
		AccountID: "999988887777",
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-other"),
			OwnerId:       aws.String("999988887777"),
		},
		Instance: &ec2.Instance{InstanceId: aws.String("i-other")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{v.ID: v})}

	out, err := svc.DescribeInstances(&ec2.DescribeInstancesInput{}, "111122223333")
	require.NoError(t, err)
	assert.Empty(t, out.Reservations)
}

func TestDescribeInstances_MalformedID(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	input := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String("not-an-id")},
	}
	_, err := svc.DescribeInstances(input, utils.GlobalAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}

func TestDescribeInstances_FilterByInstanceID(t *testing.T) {
	keep := &vm.VM{
		ID: "i-keep",
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-keep"),
		},
		Instance: &ec2.Instance{InstanceId: aws.String("i-keep")},
	}
	drop := &vm.VM{
		ID: "i-drop",
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-drop"),
		},
		Instance: &ec2.Instance{InstanceId: aws.String("i-drop")},
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{
		keep.ID: keep,
		drop.ID: drop,
	})}

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-keep")}}
	out, err := svc.DescribeInstances(input, utils.GlobalAccountID)
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	assert.Equal(t, "i-keep", *out.Reservations[0].Instances[0].InstanceId)
}

func TestDescribeInstanceAttribute_MissingInstanceID(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	_, err := svc.DescribeInstanceAttribute(&ec2.DescribeInstanceAttributeInput{
		Attribute: aws.String(ec2.InstanceAttributeNameInstanceType),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDescribeInstanceAttribute_MissingAttribute(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	_, err := svc.DescribeInstanceAttribute(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-aaa"),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDescribeInstanceAttribute_RunningInstance(t *testing.T) {
	id := "i-attr1"
	owner := utils.GlobalAccountID
	v := &vm.VM{
		ID:           id,
		InstanceType: "t3.large",
		AccountID:    owner,
		UserData:     "raw-user-data",
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}

	tests := []struct {
		name       string
		attribute  string
		assertions func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput)
	}{
		{
			name:      "instanceType",
			attribute: ec2.InstanceAttributeNameInstanceType,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.InstanceType)
				assert.Equal(t, "t3.large", *out.InstanceType.Value)
			},
		},
		{
			name:      "userData",
			attribute: ec2.InstanceAttributeNameUserData,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.UserData)
				assert.Equal(t, "raw-user-data", *out.UserData.Value)
			},
		},
		{
			name:      "disableApiTermination",
			attribute: ec2.InstanceAttributeNameDisableApiTermination,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.DisableApiTermination)
				assert.False(t, *out.DisableApiTermination.Value)
			},
		},
		{
			name:      "ebsOptimized",
			attribute: ec2.InstanceAttributeNameEbsOptimized,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.EbsOptimized)
				assert.False(t, *out.EbsOptimized.Value)
			},
		},
		{
			name:      "enaSupport",
			attribute: ec2.InstanceAttributeNameEnaSupport,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.EnaSupport)
				assert.True(t, *out.EnaSupport.Value)
			},
		},
		{
			name:      "sourceDestCheck",
			attribute: ec2.InstanceAttributeNameSourceDestCheck,
			assertions: func(t *testing.T, out *ec2.DescribeInstanceAttributeOutput) {
				require.NotNil(t, out.SourceDestCheck)
				assert.True(t, *out.SourceDestCheck.Value)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.DescribeInstanceAttribute(&ec2.DescribeInstanceAttributeInput{
				InstanceId: aws.String(id),
				Attribute:  aws.String(tc.attribute),
			}, owner)
			require.NoError(t, err)
			require.NotNil(t, out)
			assert.Equal(t, id, *out.InstanceId)
			tc.assertions(t, out)
		})
	}
}

func TestDescribeInstanceAttribute_NotRunning_NoStore(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{})}
	_, err := svc.DescribeInstanceAttribute(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-missing"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDescribeInstanceAttribute_FoundInStoppedStore(t *testing.T) {
	id := "i-stopped1"
	owner := utils.GlobalAccountID
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{
		id: {ID: id, InstanceType: "t3.medium", AccountID: owner},
	}}
	svc := &InstanceServiceImpl{
		vmMgr:        mgrWith(map[string]*vm.VM{}),
		stoppedStore: store,
	}

	out, err := svc.DescribeInstanceAttribute(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(id),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}, owner)
	require.NoError(t, err)
	assert.Equal(t, "t3.medium", *out.InstanceType.Value)
}

func TestDescribeInstanceAttribute_NotFound(t *testing.T) {
	svc := &InstanceServiceImpl{
		vmMgr:        mgrWith(map[string]*vm.VM{}),
		stoppedStore: &fakeStoppedStore{loadByID: map[string]*vm.VM{}},
	}
	_, err := svc.DescribeInstanceAttribute(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-ghost"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}, utils.GlobalAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestDescribeInstanceAttribute_HiddenForOtherAccount(t *testing.T) {
	id := "i-other-acct"
	v := &vm.VM{ID: id, InstanceType: "t3.micro", AccountID: "999988887777"}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{id: v})}

	_, err := svc.DescribeInstanceAttribute(&ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(id),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}, "111122223333")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestDescribeStoppedInstances_NilStore(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.DescribeStoppedInstances(&ec2.DescribeInstancesInput{}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDescribeStoppedInstances_HappyPath(t *testing.T) {
	owner := utils.GlobalAccountID
	store := &fakeStoppedStore{
		stopped: []*vm.VM{
			{
				ID:        "i-stop1",
				AccountID: owner,
				Reservation: &ec2.Reservation{
					ReservationId: aws.String("r-stop1"),
					OwnerId:       aws.String(owner),
				},
				Instance: &ec2.Instance{InstanceId: aws.String("i-stop1")},
			},
		},
	}
	svc := &InstanceServiceImpl{stoppedStore: store}

	out, err := svc.DescribeStoppedInstances(&ec2.DescribeInstancesInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	assert.Equal(t, "i-stop1", *out.Reservations[0].Instances[0].InstanceId)
}

func TestDescribeTerminatedInstances_NilStore(t *testing.T) {
	svc := &InstanceServiceImpl{}
	_, err := svc.DescribeTerminatedInstances(&ec2.DescribeInstancesInput{}, "")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestDescribeTerminatedInstances_HappyPath(t *testing.T) {
	owner := utils.GlobalAccountID
	store := &fakeStoppedStore{
		terminated: []*vm.VM{
			{
				ID:        "i-term1",
				AccountID: owner,
				Reservation: &ec2.Reservation{
					ReservationId: aws.String("r-term1"),
					OwnerId:       aws.String(owner),
				},
				Instance: &ec2.Instance{InstanceId: aws.String("i-term1")},
			},
		},
	}
	svc := &InstanceServiceImpl{stoppedStore: store}

	out, err := svc.DescribeTerminatedInstances(&ec2.DescribeInstancesInput{}, owner)
	require.NoError(t, err)
	require.Len(t, out.Reservations, 1)
	assert.Equal(t, "i-term1", *out.Reservations[0].Instances[0].InstanceId)
}

func TestIsInstanceVisible(t *testing.T) {
	tests := []struct {
		name   string
		caller string
		owner  string
		want   bool
	}{
		{"empty owner, global caller", utils.GlobalAccountID, "", true},
		{"empty owner, non-global caller", "111122223333", "", false},
		{"matching account", "111122223333", "111122223333", true},
		{"different accounts", "111122223333", "999988887777", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsInstanceVisible(tc.caller, tc.owner))
		})
	}
}
