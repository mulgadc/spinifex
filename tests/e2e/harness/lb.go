//go:build e2e

package harness

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
)

// ErrLBTerminalFailed marks a load balancer that entered a terminal failure
// state during provisioning — e.g. the host had no free sys.micro slot for
// the LB VM. Callers can errors.Is on it to retry after capacity reclaim.
var ErrLBTerminalFailed = errors.New("LB entered terminal failure state")

// WaitForLBActive polls describe-load-balancers until state=active. Bails
// immediately if the LB enters a terminal failure state — no point waiting
// the full timeout when provisioning has already given up. Progress emits
// via Step so long polls don't go silent in CI between subtest boundaries.
func WaitForLBActive(t *testing.T, c *AWSClient, lbArn, label string, timeout time.Duration) {
	t.Helper()
	if err := WaitForLBActiveErr(t, c, lbArn, label, timeout); err != nil {
		t.Fatal(err.Error())
	}
}

// WaitForLBActiveErr is WaitForLBActive returning an error instead of failing
// the test, so capacity-race-aware callers can tear down and retry. A terminal
// failure state wraps ErrLBTerminalFailed.
func WaitForLBActiveErr(t *testing.T, c *AWSClient, lbArn, label string, timeout time.Duration) error {
	t.Helper()
	Step(t, "%s: waiting for state=active (timeout %s)", label, timeout)
	var lastState, lastReason string
	deadline := time.Now().Add(timeout)
	nextLog := time.Now().Add(15 * time.Second)
	for {
		out, err := c.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
			LoadBalancerArns: []*string{aws.String(lbArn)},
		})
		if err == nil && len(out.LoadBalancers) > 0 {
			lastState = aws.StringValue(out.LoadBalancers[0].State.Code)
			lastReason = aws.StringValue(out.LoadBalancers[0].State.Reason)
			if lastState == "active" {
				Step(t, "%s active", label)
				return nil
			}
			if lastState == "failed" || lastState == "provisioning_failed" {
				return fmt.Errorf("%s entered terminal state %s: %s: %w", label, lastState, lastReason, ErrLBTerminalFailed)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become active within %s (last state=%s reason=%s)", label, timeout, lastState, lastReason)
		}
		if time.Now().After(nextLog) {
			Step(t, "%s: state=%s, still waiting...", label, lastState)
			nextLog = time.Now().Add(30 * time.Second)
		}
		time.Sleep(3 * time.Second)
	}
}

// WaitForTargetsHealthy polls describe-target-health until expected targets
// report state=healthy or timeout. Returns the final health output for logging.
func WaitForTargetsHealthy(t *testing.T, c *AWSClient, tgArn string, expected int, label string, timeout time.Duration) {
	t.Helper()
	Step(t, "%s: waiting for %d targets healthy (timeout %s)", label, expected, timeout)
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
	Step(t, "%s: %d targets healthy", label, lastHealthy)
}

// WaitForENICleanup polls describe-network-interfaces until no ENIs match the
// description filter, confirming the LB VM tore down cleanly.
func WaitForENICleanup(t *testing.T, c *AWSClient, descFilter, label string, timeout time.Duration) {
	t.Helper()
	Step(t, "%s: waiting for ENIs to drain (timeout %s)", label, timeout)
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
	Step(t, "%s ENIs cleaned up", label)
}

// WaitForInstanceRunning polls describe-instances until state=running.
// Logs the StateReason on failure to aid debugging stuck launches.
func WaitForInstanceRunning(t *testing.T, c *AWSClient, instanceID string, timeout time.Duration) {
	t.Helper()
	Step(t, "%s: waiting for state=running (timeout %s)", instanceID, timeout)
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
	Step(t, "%s running", instanceID)
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
