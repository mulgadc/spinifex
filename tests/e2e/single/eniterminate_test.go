//go:build e2e

package single

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// eniKeepAge outlasts every automatic timeout an attached interface passes
// through: the ENI orphan reaper's 15-minute minimum age plus a sweep.
const eniKeepAge = 17 * time.Minute

// runAttachedENISurvivesTerminate proves an interface attached with
// AttachNetworkInterface defaults to DeleteOnTermination=false, can be toggled
// with ModifyNetworkInterfaceAttribute, and outlives TerminateInstances.
func runAttachedENISurvivesTerminate(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — attached ENI survives TerminateInstances")
	harness.SkipIfNoOVN(t)

	c := fix.AWS
	instType, _ := harness.DiscoverNanoInstanceType(t, fix.Harness)
	keyName, _ := harness.EnsureKeyPair(t, fix.Harness)
	amiID := needAMI(t, fix)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, vpc.SubnetID, "default subnet ID required")
	require.NotEmpty(t, vpc.SGID, "default SG ID required")

	harness.Step(t, "create-network-interface subnet=%s", vpc.SubnetID)
	cni, err := c.EC2.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(vpc.SubnetID),
		Groups:      []*string{aws.String(vpc.SGID)},
		Description: aws.String("e2e-eni-keep-on-terminate"),
	})
	require.NoError(t, err, "create-network-interface")
	createdAt := time.Now()
	eniID := aws.StringValue(cni.NetworkInterface.NetworkInterfaceId)
	require.NotEmpty(t, eniID, "ENI NetworkInterfaceId empty")
	harness.Detail(t, "eni", eniID)
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		_ = waitForENIStatusSoft(c, eniID, "available", 2*time.Minute)
		_, _ = c.EC2.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(eniID)})
	})

	harness.Step(t, "run-instances ami=%s type=%s (test-owned guest)", amiID, instType)
	runOut, err := c.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:          aws.String(amiID),
		InstanceType:     aws.String(instType),
		KeyName:          aws.String(keyName),
		SubnetId:         aws.String(vpc.SubnetID),
		SecurityGroupIds: []*string{aws.String(vpc.SGID)},
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	})
	require.NoError(t, err, "run-instances")
	require.NotEmpty(t, runOut.Instances, "run-instances returned no Instances")
	instanceID := aws.StringValue(runOut.Instances[0].InstanceId)
	harness.Detail(t, "instance", instanceID)
	t.Cleanup(func() {
		_, _ = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: []*string{aws.String(instanceID)}})
	})
	harness.WaitForInstanceState(t, c, instanceID, "running")

	harness.Step(t, "attach-network-interface %s -> %s device-index=1", eniID, instanceID)
	attachOut, err := c.EC2.AttachNetworkInterface(&ec2.AttachNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
		InstanceId:         aws.String(instanceID),
		DeviceIndex:        aws.Int64(1),
	})
	require.NoError(t, err, "attach-network-interface")
	attachmentID := aws.StringValue(attachOut.AttachmentId)
	require.NotEmpty(t, attachmentID, "AttachmentId empty")
	waitForENIAttached(t, c, eniID, instanceID)
	requireENIDeleteOnTermination(t, c, eniID, false)

	for _, dot := range []bool{true, false} {
		harness.Step(t, "modify-network-interface-attribute %s attachment.delete-on-termination=%t", eniID, dot)
		_, err := c.EC2.ModifyNetworkInterfaceAttribute(&ec2.ModifyNetworkInterfaceAttributeInput{
			NetworkInterfaceId: aws.String(eniID),
			Attachment: &ec2.NetworkInterfaceAttachmentChanges{
				AttachmentId:        aws.String(attachmentID),
				DeleteOnTermination: aws.Bool(dot),
			},
		})
		require.NoError(t, err, "modify-network-interface-attribute")
		requireENIDeleteOnTermination(t, c, eniID, dot)
	}

	if wait := time.Until(createdAt.Add(eniKeepAge)); wait > 0 {
		harness.Step(t, "hold the attachment %s so it outlasts every automatic timeout", wait.Round(time.Second))
		time.Sleep(wait)
	}
	waitForENIAttached(t, c, eniID, instanceID)

	harness.Step(t, "terminate-instances %s", instanceID)
	_, err = c.EC2.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: []*string{aws.String(instanceID)}})
	require.NoError(t, err, "terminate-instances")
	harness.WaitForInstanceTerminated(t, c, []string{instanceID}, 5*time.Minute)

	harness.Step(t, "assert ENI %s survives as available", eniID)
	waitForENIStatus(t, c, eniID, "available")
	eni, err := describeENI(c, eniID)
	require.NoError(t, err, "the attached interface must outlive its instance")
	require.Nil(t, eni.Attachment, "terminated instance's attachment must be gone")

	harness.Step(t, "delete-network-interface %s", eniID)
	_, err = c.EC2.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(eniID)})
	require.NoError(t, err, "delete-network-interface")
	deleted = true
}

func requireENIDeleteOnTermination(t *testing.T, c *harness.AWSClient, eniID string, want bool) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		eni, err := describeENI(c, eniID)
		if err != nil {
			return err
		}
		if eni.Attachment == nil {
			return fmt.Errorf("ENI %s has no attachment", eniID)
		}
		if got := aws.BoolValue(eni.Attachment.DeleteOnTermination); got != want {
			return fmt.Errorf("ENI %s DeleteOnTermination=%t want %t", eniID, got, want)
		}
		return nil
	}, 30*time.Second, 2*time.Second)
}
