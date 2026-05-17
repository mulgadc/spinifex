//go:build e2e

package harness

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
)

// WaitForLBActive polls describe-load-balancers until state=active or timeout.
// Mirrors the wait_for_lb_active bash helper.
func WaitForLBActive(t *testing.T, c *AWSClient, lbArn, label string, timeout time.Duration) {
	t.Helper()
	var lastState string
	EventuallyErr(t, func() error {
		out, err := c.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
			LoadBalancerArns: []*string{aws.String(lbArn)},
		})
		if err != nil {
			return fmt.Errorf("describe %s: %w", label, err)
		}
		if len(out.LoadBalancers) == 0 {
			return fmt.Errorf("%s not found", label)
		}
		lastState = aws.StringValue(out.LoadBalancers[0].State.Code)
		if lastState == "active" {
			return nil
		}
		return fmt.Errorf("%s state=%s, want active", label, lastState)
	}, timeout, 3*time.Second)
	t.Logf("%s active", label)
}

// WaitForTargetsHealthy polls describe-target-health until expected targets
// report state=healthy or timeout. Returns the final health output for logging.
func WaitForTargetsHealthy(t *testing.T, c *AWSClient, tgArn string, expected int, label string, timeout time.Duration) {
	t.Helper()
	var lastHealthy int
	EventuallyErr(t, func() error {
		out, err := c.ELBv2.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
			TargetGroupArn: aws.String(tgArn),
		})
		if err != nil {
			return fmt.Errorf("describe target health %s: %w", label, err)
		}
		lastHealthy = 0
		for _, th := range out.TargetHealthDescriptions {
			if aws.StringValue(th.TargetHealth.State) == "healthy" {
				lastHealthy++
			}
		}
		if lastHealthy >= expected {
			return nil
		}
		return fmt.Errorf("%s: %d/%d healthy", label, lastHealthy, expected)
	}, timeout, 5*time.Second)
	t.Logf("%s: %d targets healthy", label, lastHealthy)
}

// WaitForENICleanup polls describe-network-interfaces until no ENIs match the
// description filter, confirming the LB VM tore down cleanly.
func WaitForENICleanup(t *testing.T, c *AWSClient, descFilter, label string, timeout time.Duration) {
	t.Helper()
	EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
			Filters: []*ec2.Filter{{
				Name:   aws.String("description"),
				Values: []*string{aws.String(descFilter)},
			}},
		})
		if err != nil {
			return fmt.Errorf("describe ENIs for %s: %w", label, err)
		}
		if len(out.NetworkInterfaces) == 0 {
			return nil
		}
		return fmt.Errorf("%s: %d ENIs still present", label, len(out.NetworkInterfaces))
	}, timeout, 3*time.Second)
	t.Logf("%s ENIs cleaned up", label)
}

// WaitForInstanceRunning polls describe-instances until state=running.
// Logs the StateReason on failure to aid debugging stuck launches.
func WaitForInstanceRunning(t *testing.T, c *AWSClient, instanceID string, timeout time.Duration) {
	t.Helper()
	var lastState string
	EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		if err != nil {
			return fmt.Errorf("describe %s: %w", instanceID, err)
		}
		if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
			return fmt.Errorf("%s not found", instanceID)
		}
		inst := out.Reservations[0].Instances[0]
		lastState = aws.StringValue(inst.State.Name)
		if lastState == "running" {
			return nil
		}
		if lastState == "terminated" {
			reason := aws.StringValue(inst.StateReason.Message)
			return fmt.Errorf("%s terminated: %s", instanceID, reason)
		}
		return fmt.Errorf("%s state=%s", instanceID, lastState)
	}, timeout, 2*time.Second)
	t.Logf("%s running", instanceID)
}

// WaitForInstanceTerminated polls describe-instances until state=terminated.
// Tolerates InvalidInstanceID.NotFound (terminated and reaped).
func WaitForInstanceTerminated(t *testing.T, c *AWSClient, instanceIDs []string, timeout time.Duration) {
	t.Helper()
	if len(instanceIDs) == 0 {
		return
	}
	ids := make([]*string, len(instanceIDs))
	for i, id := range instanceIDs {
		ids[i] = aws.String(id)
	}
	EventuallyErr(t, func() error {
		out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{InstanceIds: ids})
		if err != nil {
			return nil // most likely InvalidInstanceID.NotFound — accept as terminated
		}
		remaining := 0
		for _, r := range out.Reservations {
			for _, inst := range r.Instances {
				if aws.StringValue(inst.State.Name) != "terminated" {
					remaining++
				}
			}
		}
		if remaining == 0 {
			return nil
		}
		return fmt.Errorf("%d instances still terminating", remaining)
	}, timeout, 2*time.Second)
}

// InstanceENI returns the primary ENI for an instance — used to discover the
// public IP assigned to a LB client VM.
func InstanceENI(t *testing.T, c *AWSClient, instanceID string) *ec2.NetworkInterface {
	t.Helper()
	out, err := c.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("attachment.instance-id"),
			Values: []*string{aws.String(instanceID)},
		}},
	})
	if err != nil {
		t.Fatalf("describe ENI for %s: %v", instanceID, err)
	}
	if len(out.NetworkInterfaces) == 0 {
		t.Fatalf("%s has no ENI", instanceID)
	}
	return out.NetworkInterfaces[0]
}
