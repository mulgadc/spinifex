//go:build e2e

package single

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// Package-local bootstrappers wrapping harness.Discover*/Ensure* so each Test*
// can self-bootstrap its prerequisites. Results are memoized by harness.Fixture.

// needAZ returns the discovered default availability zone.
func needAZ(t *testing.T, fix *Fixture) string {
	t.Helper()
	return harness.DiscoverDefaultAZ(t, fix.Harness)
}

// needInstanceTypeArch returns the nano instance type and its architecture.
func needInstanceTypeArch(t *testing.T, fix *Fixture) (instanceType, arch string) {
	t.Helper()
	return harness.DiscoverNanoInstanceType(t, fix.Harness)
}

// needAMI returns the discovered architecture-appropriate Ubuntu AMI ID.
func needAMI(t *testing.T, fix *Fixture) string {
	t.Helper()
	_, arch := needInstanceTypeArch(t, fix)
	return harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
}

// needKeyPair returns the memoized primary EC2 key pair name + PEM path.
func needKeyPair(t *testing.T, fix *Fixture) (name, pemPath string) {
	t.Helper()
	return harness.EnsureKeyPair(t, fix.Harness, fix.TmpDir)
}

// customAMIName is the well-known AMI name used by CreateImage tests.
const customAMIName = "e2e-custom-ami"

// needCustomAMI returns the memoized CreateImage-built custom AMI ID,
// building it (via harness.EnsureAMI) on first call. Bootstraps the source
// instance first.
func needCustomAMI(t *testing.T, fix *Fixture) string {
	t.Helper()
	inst, _ := needInstance(t, fix)
	return ensureCustomAMI(t, fix, aws.StringValue(inst.InstanceId), customAMIName, "E2E test custom image")
}

// ensureCustomAMI is the underlying create path used by needCustomAMI and
// the phase5e direct call. Routes through harness.EnsureAMI so the AMI is
// torn down at fixture cleanup.
func ensureCustomAMI(t *testing.T, fix *Fixture, sourceInstanceID, name, desc string) string {
	t.Helper()
	return harness.EnsureAMI(t, fix.Harness, harness.AMISource{
		CreateFrom: &harness.AMICreateSpec{
			SourceInstanceID: sourceInstanceID,
			Name:             name,
			Description:      desc,
			NoReboot:         true,
		},
	})
}

// needInstance bootstraps the suite's primary running instance and returns
// the *ec2.Instance plus its root volume ID. Idempotent via harness.EnsureInstance.
func needInstance(t *testing.T, fix *Fixture) (inst *ec2.Instance, rootVolumeID string) {
	t.Helper()
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, _ := needKeyPair(t, fix)
	ami := needAMI(t, fix)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, vpc.SGID, "default SG ID required")
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	instanceID := harness.EnsureInstance(t, fix.Harness, harness.InstanceSpec{
		AMIID:        ami,
		InstanceType: instType,
		KeyName:      keyName,
		SubnetID:     vpc.SubnetID,
		SGID:         vpc.SGID,
	})
	require.NotEmpty(t, instanceID, "EnsureInstance returned empty InstanceId")

	descOut, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
	require.NoError(t, err, "describe-instances %s", instanceID)
	require.NotEmpty(t, descOut.Reservations, "no reservations for %s", instanceID)
	require.NotEmpty(t, descOut.Reservations[0].Instances, "no instances for %s", instanceID)
	inst = descOut.Reservations[0].Instances[0]

	// Match BDM by RootDeviceName; fall back to first BDM if empty.
	rootDev := aws.StringValue(inst.RootDeviceName)
	for _, bdm := range inst.BlockDeviceMappings {
		if rootDev != "" && aws.StringValue(bdm.DeviceName) != rootDev {
			continue
		}
		if bdm.Ebs != nil {
			rootVolumeID = aws.StringValue(bdm.Ebs.VolumeId)
			break
		}
	}
	if rootVolumeID == "" && len(inst.BlockDeviceMappings) > 0 && inst.BlockDeviceMappings[0].Ebs != nil {
		rootVolumeID = aws.StringValue(inst.BlockDeviceMappings[0].Ebs.VolumeId)
	}
	require.NotEmpty(t, rootVolumeID, "could not resolve root volume from BlockDeviceMappings")
	return inst, rootVolumeID
}
