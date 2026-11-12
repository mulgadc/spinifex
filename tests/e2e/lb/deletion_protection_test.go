//go:build e2e

package lb

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runDeletionProtectionSuite proves a delete of a protected NLB is refused and
// it keeps serving, then the delete succeeds once protection is off.
func runDeletionProtectionSuite(t *testing.T, c *harness.AWSClient, f *sharedFixture) {
	t.Helper()
	label := "NLB deletion protection"

	tgArn := createTargetGroup(t, c, f, "lb-e2e-nlb-prot-tg", "TCP", tcpPort, "")
	t.Cleanup(func() { deleteTargetGroup(t, c, tgArn) })
	registerTargets(t, c, tgArn, f.AppInstanceIDs)
	t.Cleanup(func() { deregisterTargets(t, c, tgArn, f.AppInstanceIDs) })

	lb, listener := createActiveLB(t, c, f, "lb-e2e-nlb-prot", "network", "internal", "TCP", tcpPort, tgArn, label, nil)
	setDeletionProtection(t, c, lb.ARN, true)
	// Runs before createActiveLB's delete cleanup, so a failure never leaks a protected LB.
	t.Cleanup(func() {
		if _, err := c.ELBv2.ModifyLoadBalancerAttributes(deletionProtectionInput(lb.ARN, false)); err != nil {
			t.Logf("cleanup: unprotect %s: %v", lb.ARN, err)
		}
	})

	harness.WaitForTargetsHealthy(t, c, tgArn, 2, label, 2*time.Minute)
	priv := privateIP(lbENI(t, c, "net", lb))
	require.NotEmpty(t, priv, label+" needs private IP")

	_, err := c.ELBv2.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(lb.ARN)})
	var aerr awserr.Error
	require.Truef(t, errors.As(err, &aerr), "protected delete must fail, got %v", err)
	assert.Equal(t, "OperationNotPermitted", aerr.Code())
	assert.Equal(t, "Load balancer '"+lb.ARN+"' cannot be deleted because deletion protection is enabled", aerr.Message())

	listeners, err := c.ELBv2.DescribeListeners(&elbv2.DescribeListenersInput{LoadBalancerArn: aws.String(lb.ARN)})
	require.NoError(t, err)
	require.Len(t, listeners.Listeners, 1, "the listener must survive a refused delete")
	assert.Equal(t, listener, aws.StringValue(listeners.Listeners[0].ListenerArn))
	assert.Equal(t, priv, privateIP(lbENI(t, c, "net", lb)), "the ENI and its address must survive a refused delete")
	runInternalTrafficViaClient(t, c, f, kindNLB, priv)

	setDeletionProtection(t, c, lb.ARN, false)
	_, err = c.ELBv2.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(lb.ARN)})
	require.NoError(t, err, "delete must succeed once protection is off")
	waitForLBGone(t, c, lb.ARN, 60*time.Second)
	_, err = c.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{LoadBalancerArns: []*string{aws.String(lb.ARN)}})
	require.Truef(t, errors.As(err, &aerr) && aerr.Code() == "LoadBalancerNotFound", "LB must be gone, got %v", err)
}

func setDeletionProtection(t *testing.T, c *harness.AWSClient, arn string, on bool) {
	t.Helper()
	_, err := c.ELBv2.ModifyLoadBalancerAttributes(deletionProtectionInput(arn, on))
	require.NoErrorf(t, err, "set deletion protection %t on %s", on, arn)
}

func deletionProtectionInput(arn string, on bool) *elbv2.ModifyLoadBalancerAttributesInput {
	value := "false"
	if on {
		value = "true"
	}
	return &elbv2.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: aws.String(arn),
		Attributes:      []*elbv2.LoadBalancerAttribute{{Key: aws.String("deletion_protection.enabled"), Value: aws.String(value)}},
	}
}
