//go:build e2e

package quota

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// quotaExceeded is the code every enforced dimension refuses with. Asserting on
// it rather than on "an error" is what separates a quota denial from a create
// that failed for an unrelated reason and would otherwise read as a pass.
const quotaExceeded = "ResourceLimitExceeded"

// setLimit overrides one dimension on an account for the duration of the test
// and clears every override afterwards, so each test starts from the baseline
// regardless of the order they run in.
func setLimit(t *testing.T, accountID, dimension string, value int) {
	t.Helper()
	harness.SpxAdminQuotaSet(t, accountID, "--"+dimension, strconv.Itoa(value))
	t.Cleanup(func() { harness.SpxAdminQuotaClear(t, accountID) })
}

// headroom resolves what an account already holds and caps the dimension one
// above it. Every limit test is then about the boundary rather than about the
// state the suite happened to inherit.
func headroom(t *testing.T, accountID, dimension string, used int) {
	t.Helper()
	harness.Detail(t, "dimension", dimension, "used", used, "limit", used+1)
	setLimit(t, accountID, dimension, used+1)
}

func itoa(v int) string { return strconv.Itoa(v) }

// requireEIPSupported fails rather than skips when a node answers from the
// disabled-EIP stub. A cell whose nodes disagree about whether EIPs exist
// cannot count addresses, and the cap under test is what exposes it.
func requireEIPSupported(t *testing.T, err error) {
	t.Helper()
	require.False(t, harness.ErrorCodeIs(err, "UnsupportedOperation"),
		"a node answered from the disabled-EIP stub: this cell does not serve external IPAM from every node")
}

func countVPCs(t *testing.T, c *harness.AWSClient) int {
	t.Helper()
	out, err := c.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{})
	require.NoError(t, err, "describe-vpcs")
	return len(out.Vpcs)
}

func countSubnets(t *testing.T, c *harness.AWSClient) int {
	t.Helper()
	out, err := c.EC2.DescribeSubnets(&ec2.DescribeSubnetsInput{})
	require.NoError(t, err, "describe-subnets")
	return len(out.Subnets)
}

func countAddresses(t *testing.T, c *harness.AWSClient) int {
	t.Helper()
	out, err := c.EC2.DescribeAddresses(&ec2.DescribeAddressesInput{})
	require.NoError(t, err, "describe-addresses")
	return len(out.Addresses)
}

// volumeUsage returns the account's volume count and the GiB they sum to, the
// two independently capped facts about EBS.
func volumeUsage(t *testing.T, c *harness.AWSClient) (count, gib int) {
	t.Helper()
	out, err := c.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{})
	require.NoError(t, err, "describe-volumes")
	for _, v := range out.Volumes {
		gib += int(aws.Int64Value(v.Size))
	}
	return len(out.Volumes), gib
}

// createVPC creates a VPC and registers its removal. The caller gets the ID so
// it can delete early, which is how the release assertions are written.
func createVPC(t *testing.T, c *harness.AWSClient, cidr string) string {
	t.Helper()
	// The create path is the subject under test, so it cannot route through a
	// fixture that would hide the call being counted. e2e:allow-create
	out, err := c.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(cidr)})
	require.NoError(t, err, "create-vpc %s", cidr)
	id := aws.StringValue(out.Vpc.VpcId)
	require.NotEmpty(t, id, "create-vpc returned no VpcId")
	t.Cleanup(func() { deleteVPC(c, id) })
	return id
}

func deleteVPC(c *harness.AWSClient, vpcID string) {
	_, _ = c.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
}

// createVolume creates one EBS volume and registers its removal.
func createVolume(t *testing.T, c *harness.AWSClient, az string, sizeGiB int64) string {
	t.Helper()
	// e2e:allow-create — the create path is the subject under test.
	out, err := c.EC2.CreateVolume(&ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		Size:             aws.Int64(sizeGiB),
	})
	require.NoError(t, err, "create-volume %d GiB", sizeGiB)
	id := aws.StringValue(out.VolumeId)
	require.NotEmpty(t, id, "create-volume returned no VolumeId")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteVolume(&ec2.DeleteVolumeInput{VolumeId: aws.String(id)})
	})
	return id
}

// tenantSubnet returns a subnet the tenant can launch into, preferring one of
// the default VPC's so no test pays for a topology it does not assert on.
func tenantSubnet(t *testing.T, c *harness.AWSClient) string {
	t.Helper()
	out, err := c.EC2.DescribeSubnets(&ec2.DescribeSubnetsInput{})
	require.NoError(t, err, "describe-subnets")
	require.NotEmpty(t, out.Subnets, "account has no subnet to launch into")
	return aws.StringValue(out.Subnets[0].SubnetId)
}

// tenantKeyPair creates a key pair for the account and registers its removal.
func tenantKeyPair(t *testing.T, c *harness.AWSClient) string {
	t.Helper()
	name := fmt.Sprintf("e2e-quota-%d", time.Now().UnixNano())
	// The fixture key pair belongs to the super-admin account; a launch by this
	// tenant needs one of its own. e2e:allow-create
	_, err := c.EC2.CreateKeyPair(&ec2.CreateKeyPairInput{KeyName: aws.String(name)})
	require.NoError(t, err, "create-key-pair")
	t.Cleanup(func() {
		_, _ = c.EC2.DeleteKeyPair(&ec2.DeleteKeyPairInput{KeyName: aws.String(name)})
	})
	return name
}

// launchSpec is everything a tenant launch needs that is fixed for the run.
type launchSpec struct {
	amiID   string
	keyName string
	subnet  string
}

// runInstances issues one launch of count instances. It returns the error
// unwrapped so the caller can assert on the code rather than on a wrapper.
func (s launchSpec) runInstances(c *harness.AWSClient, instanceType string, count int64) (*ec2.Reservation, error) {
	// The launch is the checked operation; a fixture would memoize it and the
	// second, refused launch would never be issued. e2e:allow-create
	return c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(s.amiID),
		InstanceType: aws.String(instanceType),
		KeyName:      aws.String(s.keyName),
		SubnetId:     aws.String(s.subnet),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(count),
	})
}

// launchOne runs a single instance and registers its termination.
func (s launchSpec) launchOne(t *testing.T, c *harness.AWSClient, instanceType string) (string, error) {
	t.Helper()
	out, err := s.runInstances(c, instanceType, 1)
	if err != nil {
		return "", err
	}
	require.NotEmpty(t, out.Instances, "RunInstances returned no instance")
	id := aws.StringValue(out.Instances[0].InstanceId)
	t.Cleanup(func() { terminateInstance(c, id) })
	return id, nil
}

// registerTerminate queues every instance in a reservation for termination and
// returns their IDs.
func registerTerminate(t *testing.T, c *harness.AWSClient, reservation *ec2.Reservation) []string {
	t.Helper()
	ids := make([]string, 0, len(reservation.Instances))
	for _, inst := range reservation.Instances {
		id := aws.StringValue(inst.InstanceId)
		require.NotEmpty(t, id, "RunInstances returned an instance with no ID")
		ids = append(ids, id)
		t.Cleanup(func() { terminateInstance(c, id) })
	}
	return ids
}

func terminateInstance(c *harness.AWSClient, instanceID string) {
	_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	})
}

// waitTerminated blocks until the instance reaches terminated. The vCPU counter
// is released by the gateway's reconcile sweep, which reads instance state, so
// polling the state first keeps the release assertion honest about what it is
// waiting for.
func waitTerminated(t *testing.T, c *harness.AWSClient, instanceID string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		if err != nil {
			return err
		}
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				if state := aws.StringValue(inst.State.Name); state != "terminated" {
					return fmt.Errorf("instance %s state=%s", instanceID, state)
				}
			}
		}
		return nil
	}, 5*time.Minute, 5*time.Second)
}

func countInstances(t *testing.T, c *harness.AWSClient) int {
	t.Helper()
	out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{})
	require.NoError(t, err, "describe-instances")
	live := 0
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if aws.StringValue(inst.State.Name) != "terminated" {
				live++
			}
		}
	}
	return live
}

// instanceTypeVCPUs returns the vCPUs one instance of instanceType reserves.
func instanceTypeVCPUs(t *testing.T, c *harness.AWSClient, instanceType string) int {
	t.Helper()
	out, err := c.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{
		InstanceTypes: []*string{aws.String(instanceType)},
	})
	require.NoError(t, err, "describe-instance-types %s", instanceType)
	require.NotEmpty(t, out.InstanceTypes, "instance type %s not offered", instanceType)
	require.NotNil(t, out.InstanceTypes[0].VCpuInfo, "instance type %s reports no VCpuInfo", instanceType)
	return int(aws.Int64Value(out.InstanceTypes[0].VCpuInfo.DefaultVCpus))
}

// widestInstanceType returns the offered type with the most vCPUs, which is how
// a tenant would try to consume its whole allowance in one launch.
func widestInstanceType(t *testing.T, c *harness.AWSClient) (string, int) {
	t.Helper()
	out, err := c.EC2.DescribeInstanceTypes(&ec2.DescribeInstanceTypesInput{})
	require.NoError(t, err, "describe-instance-types")

	var name string
	var vcpus int
	for _, it := range out.InstanceTypes {
		if it.VCpuInfo == nil {
			continue
		}
		if v := int(aws.Int64Value(it.VCpuInfo.DefaultVCpus)); v > vcpus {
			name, vcpus = aws.StringValue(it.InstanceType), v
		}
	}
	require.NotEmpty(t, name, "no instance type reports a vCPU count")
	return name, vcpus
}
