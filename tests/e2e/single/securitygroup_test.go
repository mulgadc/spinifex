//go:build e2e

package single

import (
	"regexp"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// pingReceivedRE matches the busybox/iputils summary line for 1–3 echo replies
// (`3 packets received` or `1 received`). Mirrors the bash grep pattern.
var pingReceivedRE = regexp.MustCompile(`[1-3] (packets )?received`)

// pingDroppedRE matches the summary for a fully dropped probe — either
// `0 received` or `100% packet loss`. Mirrors the bash grep pattern.
var pingDroppedRE = regexp.MustCompile(`0 (packets )?received|100% packet loss`)

// allowAllEgressPermission returns the SDK shape for the default SG's stock
// `IpProtocol=-1, CidrIp=0.0.0.0/0` rule.
func allowAllEgressPermission() *ec2.IpPermission {
	return &ec2.IpPermission{
		IpProtocol: aws.String("-1"),
		IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
	}
}

// authorizeAllowAllEgress is the idempotent re-add used both by the happy
// path and t.Cleanup. Returns the raw SDK error so the caller can decide
// whether `InvalidPermission.Duplicate` is fatal in its context.
func authorizeAllowAllEgress(c *harness.AWSClient, sgID string) error {
	_, err := c.EC2.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{allowAllEgressPermission()},
	})
	return err
}
