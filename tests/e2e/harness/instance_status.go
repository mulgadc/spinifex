//go:build e2e

package harness

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// InstanceStatusSummary is what DescribeInstanceStatus says about one instance,
// flattened to the three labels a customer reads. Empty strings mean the field
// was absent, which is a different answer from any of the labels.
type InstanceStatusSummary struct {
	State          string
	InstanceStatus string
	SystemStatus   string
	AZ             string
}

func (s InstanceStatusSummary) String() string {
	return fmt.Sprintf("state=%s instance-status=%s system-status=%s az=%s",
		s.State, s.InstanceStatus, s.SystemStatus, s.AZ)
}

// InstanceStatus returns DescribeInstanceStatus for one instance.
//
// IncludeAllInstances is set because the default is running-only, and a
// host-failure test has to be able to ask about an instance whose state is in
// question — which is exactly the case the default hides. Absent from the answer
// is reported as a zero summary with ok=false rather than an error: a gateway
// that returns no frame is a finding, not a failed call.
func InstanceStatus(c *AWSClient, instanceID string) (summary InstanceStatusSummary, ok bool, err error) {
	out, err := c.EC2.DescribeInstanceStatus(&ec2.DescribeInstanceStatusInput{
		InstanceIds:         []*string{aws.String(instanceID)},
		IncludeAllInstances: aws.Bool(true),
	})
	if err != nil {
		return InstanceStatusSummary{}, false, fmt.Errorf("describe-instance-status %s: %w", instanceID, err)
	}

	for _, status := range out.InstanceStatuses {
		if status == nil || aws.StringValue(status.InstanceId) != instanceID {
			continue
		}
		summary.AZ = aws.StringValue(status.AvailabilityZone)
		if status.InstanceState != nil {
			summary.State = aws.StringValue(status.InstanceState.Name)
		}
		if status.InstanceStatus != nil {
			summary.InstanceStatus = aws.StringValue(status.InstanceStatus.Status)
		}
		if status.SystemStatus != nil {
			summary.SystemStatus = aws.StringValue(status.SystemStatus.Status)
		}
		return summary, true, nil
	}
	return InstanceStatusSummary{}, false, nil
}
