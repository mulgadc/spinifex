//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/stretchr/testify/require"
)

// TestVPCD_SecurityGroupReachesOVN asserts the synchronous half of the
// handler-to-vpcd contract against a real responder. The SG topics use
// utils.RequestEvent, so unlike the topology events these need no polling: by
// the time the SDK call returns, vpcd has already committed to OVN.
//
// This is the case the tier previously faked outright with a canned
// {"success":true} — the API call succeeded whether or not any OVN work was
// possible, let alone correct.
func TestVPCD_SecurityGroupReachesOVN(t *testing.T) {
	vpcd, c := startVPCDStack(t)
	ctx := context.Background()

	vpcID := createScratchVPC(t, c, "10.44.0.0/16")
	sgOut, err := c.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		VpcId:       aws.String(vpcID),
		GroupName:   aws.String("integration-sg"),
		Description: aws.String("vpcd integration tier"),
	})
	require.NoError(t, err, "create-security-group in %s", vpcID)
	sgID := aws.StringValue(sgOut.GroupId)
	require.NotEmpty(t, sgID, "GroupId empty")

	pgName := topology.SecurityGroupPortGroup(sgID)
	pg, err := vpcd.OVN.GetPortGroup(ctx, pgName)
	require.NoError(t, err, "SG port group %s absent after create-security-group", pgName)
	baseACLs := len(pg.ACLs)

	_, err = c.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(22),
			ToPort:     aws.Int64(22),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.44.0.0/16")}},
		}},
	})
	require.NoError(t, err, "authorize-security-group-ingress on %s", sgID)

	// The rule count is the assertion, not the ACL match text: that the rule
	// crossed the wire at all is this tier's claim, while the match expression
	// it compiles to belongs to policy's own tests.
	pg, err = vpcd.OVN.GetPortGroup(ctx, pgName)
	require.NoError(t, err, "SG port group %s after authorize", pgName)
	require.Greater(t, len(pg.ACLs), baseACLs,
		"authorize-security-group-ingress added no ACL to port group %s", pgName)

	_, err = c.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{GroupId: aws.String(sgID)})
	require.NoError(t, err, "delete-security-group %s", sgID)

	_, err = vpcd.OVN.GetPortGroup(ctx, pgName)
	require.Error(t, err, "SG port group %s still present after delete-security-group", pgName)
}
