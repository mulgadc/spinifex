package gateway

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// render drives a single EC2 action through ec2Handler exactly as the real
// dispatch table does (see ec2Actions in ec2.go), with a stub inner handler
// standing in for the NATS round trip. This exercises the real marshal path
// (currently marshalEC2Response) without needing a live NATS connection.
func render[In any](t *testing.T, action string, out any) string {
	t.Helper()
	h := ec2Handler(func(_ context.Context, _ *In, _ *GatewayConfig, _ string) (any, error) {
		return out, nil
	})
	input, err := h.parse(map[string]string{})
	require.NoError(t, err)
	xmlOutput, err := h.dispatch(action, input, &GatewayConfig{}, "acct-123", nil)
	require.NoError(t, err)
	return string(xmlOutput)
}

// TestEC2Describe_EmptyResult_EmitsContainer asserts on the raw wire XML for
// an empty Describe result: real AWS renders the empty list container (e.g.
// <keySet></keySet>); a struct-level assertion would pass even with the
// container omitted entirely, since a nil slice unmarshals from either body.
func TestEC2Describe_EmptyResult_EmitsContainer(t *testing.T) {
	cases := []struct {
		name      string
		action    string
		container string
		render    func(t *testing.T) string
	}{
		{"DescribeKeyPairs", "DescribeKeyPairs", "keySet", func(t *testing.T) string {
			return render[ec2.DescribeKeyPairsInput](t, "DescribeKeyPairs", ec2.DescribeKeyPairsOutput{})
		}},
		{"DescribeVolumes", "DescribeVolumes", "volumeSet", func(t *testing.T) string {
			return render[ec2.DescribeVolumesInput](t, "DescribeVolumes", ec2.DescribeVolumesOutput{})
		}},
		{"DescribeSubnets", "DescribeSubnets", "subnetSet", func(t *testing.T) string {
			return render[ec2.DescribeSubnetsInput](t, "DescribeSubnets", ec2.DescribeSubnetsOutput{})
		}},
		{"DescribeTags", "DescribeTags", "tagSet", func(t *testing.T) string {
			return render[ec2.DescribeTagsInput](t, "DescribeTags", ec2.DescribeTagsOutput{})
		}},
		{"DescribeInstances", "DescribeInstances", "reservationSet", func(t *testing.T) string {
			return render[ec2.DescribeInstancesInput](t, "DescribeInstances", ec2.DescribeInstancesOutput{})
		}},
		{"DescribeSnapshots", "DescribeSnapshots", "snapshotSet", func(t *testing.T) string {
			return render[ec2.DescribeSnapshotsInput](t, "DescribeSnapshots", ec2.DescribeSnapshotsOutput{})
		}},
		{"DescribeImages", "DescribeImages", "imagesSet", func(t *testing.T) string {
			return render[ec2.DescribeImagesInput](t, "DescribeImages", ec2.DescribeImagesOutput{})
		}},
		{"DescribeInstanceTypeOfferings", "DescribeInstanceTypeOfferings", "instanceTypeOfferingSet", func(t *testing.T) string {
			return render[ec2.DescribeInstanceTypeOfferingsInput](t, "DescribeInstanceTypeOfferings", ec2.DescribeInstanceTypeOfferingsOutput{})
		}},
		{"GetSecurityGroupsForVpc", "GetSecurityGroupsForVpc", "securityGroupForVpcSet", func(t *testing.T) string {
			return render[ec2.GetSecurityGroupsForVpcInput](t, "GetSecurityGroupsForVpc", ec2.GetSecurityGroupsForVpcOutput{})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.render(t)

			// The bug: the container element is omitted entirely rather than
			// rendered empty, e.g. "<DescribeKeyPairsResponse></DescribeKeyPairsResponse>".
			openEmpty := "<" + tc.container + "></" + tc.container + ">"
			openSelfClose := "<" + tc.container + "/>"
			assert.Truef(t, strings.Contains(body, openEmpty) || strings.Contains(body, openSelfClose),
				"expected empty %s container in wire XML, got: %s", tc.container, body)

			// Response element must not be empty-bodied.
			assert.NotContainsf(t, body, "<"+tc.action+"Response></"+tc.action+"Response>",
				"response body was empty, container omitted entirely: %s", body)
		})
	}
}

// TestEC2Describe_EmitsRequestId asserts every EC2 response carries a
// <requestId>, which real AWS always includes and which botocore/CLI tooling
// relies on for diagnostics (e.g. `aws ... --debug`).
func TestEC2Describe_EmitsRequestId(t *testing.T) {
	requestIDPattern := regexp.MustCompile(`<requestId>[^<]+</requestId>`)

	body := render[ec2.DescribeKeyPairsInput](t, "DescribeKeyPairs", ec2.DescribeKeyPairsOutput{})
	assert.Regexp(t, requestIDPattern, body)

	nonEmptyBody := render[ec2.DescribeKeyPairsInput](t, "DescribeKeyPairs", ec2.DescribeKeyPairsOutput{
		KeyPairs: []*ec2.KeyPairInfo{{KeyName: aws.String("spinifex-key")}},
	})
	assert.Regexp(t, requestIDPattern, nonEmptyBody)
}

// TestEC2Describe_NonEmptyResult_Unchanged is the regression guard: a
// populated Describe result must still render its real data untouched by the
// empty-container/requestId fix.
func TestEC2Describe_NonEmptyResult_Unchanged(t *testing.T) {
	body := render[ec2.DescribeKeyPairsInput](t, "DescribeKeyPairs", ec2.DescribeKeyPairsOutput{
		KeyPairs: []*ec2.KeyPairInfo{
			{KeyName: aws.String("spinifex-key"), KeyPairId: aws.String("key-abc123")},
		},
	})

	assert.Contains(t, body, "<keyName>spinifex-key</keyName>")
	assert.Contains(t, body, "<keyPairId>key-abc123</keyPairId>")
	assert.Contains(t, body, "<keySet>")
	assert.NotContains(t, body, "<keySet></keySet>", "non-empty KeyPairs must not render as an empty container")
}

// AWS omits some empty lists rather than rendering them: an untagged
// CreateKeyPair's tagSet, and an image's tagSet and productCodes. Other empty
// lists in the same responses are still rendered.
func TestEC2_ListsAWSOmitsWhenEmpty(t *testing.T) {
	key := render[ec2.CreateKeyPairInput](t, "CreateKeyPair", ec2.CreateKeyPairOutput{KeyName: aws.String("k")})
	assert.NotContains(t, key, "tagSet")

	images := render[ec2.DescribeImagesInput](t, "DescribeImages", ec2.DescribeImagesOutput{
		Images: []*ec2.Image{{ImageId: aws.String("ami-1")}},
	})
	assert.NotContains(t, images, "tagSet")
	assert.NotContains(t, images, "productCodes")
	assert.Contains(t, images, "<blockDeviceMapping></blockDeviceMapping>")

	tagged := render[ec2.DescribeImagesInput](t, "DescribeImages", ec2.DescribeImagesOutput{
		Images: []*ec2.Image{{ImageId: aws.String("ami-1"), Tags: []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}}}},
	})
	assert.Contains(t, tagged, "<tagSet><item><key>k</key><value>v</value></item></tagSet>")
}

// The network lists AWS omits when empty: an untagged security group's and
// rule's tagSet, Revoke*'s unknownIpPermissionSet, an interface's prefix
// lists, and a described VPC's IPv6 association set.
func TestEC2_NetworkListsAWSOmitsWhenEmpty(t *testing.T) {
	sg := render[ec2.CreateSecurityGroupInput](t, "CreateSecurityGroup", ec2.CreateSecurityGroupOutput{GroupId: aws.String("sg-1")})
	assert.NotContains(t, sg, "tagSet")

	groups := render[ec2.DescribeSecurityGroupsInput](t, "DescribeSecurityGroups", ec2.DescribeSecurityGroupsOutput{
		SecurityGroups: []*ec2.SecurityGroup{{GroupId: aws.String("sg-1")}},
	})
	assert.NotContains(t, groups, "tagSet")
	assert.Contains(t, groups, "<ipPermissions></ipPermissions>")

	auth := render[ec2.AuthorizeSecurityGroupIngressInput](t, "AuthorizeSecurityGroupIngress", ec2.AuthorizeSecurityGroupIngressOutput{
		SecurityGroupRules: []*ec2.SecurityGroupRule{{SecurityGroupRuleId: aws.String("sgr-1")}},
	})
	assert.NotContains(t, auth, "tagSet")

	rules := render[ec2.DescribeSecurityGroupRulesInput](t, "DescribeSecurityGroupRules", ec2.DescribeSecurityGroupRulesOutput{
		SecurityGroupRules: []*ec2.SecurityGroupRule{{SecurityGroupRuleId: aws.String("sgr-1"), Tags: []*ec2.Tag{}}},
	})
	assert.Contains(t, rules, "<tagSet></tagSet>")

	revoke := render[ec2.RevokeSecurityGroupIngressInput](t, "RevokeSecurityGroupIngress", ec2.RevokeSecurityGroupIngressOutput{Return: aws.Bool(true)})
	assert.NotContains(t, revoke, "unknownIpPermissionSet")
	revokeEgress := render[ec2.RevokeSecurityGroupEgressInput](t, "RevokeSecurityGroupEgress", ec2.RevokeSecurityGroupEgressOutput{Return: aws.Bool(true)})
	assert.NotContains(t, revokeEgress, "unknownIpPermissionSet")

	enis := render[ec2.DescribeNetworkInterfacesInput](t, "DescribeNetworkInterfaces", ec2.DescribeNetworkInterfacesOutput{
		NetworkInterfaces: []*ec2.NetworkInterface{{NetworkInterfaceId: aws.String("eni-1")}},
	})
	assert.NotContains(t, enis, "ipv4PrefixSet")
	assert.NotContains(t, enis, "ipv6PrefixSet")
	assert.Contains(t, enis, "<groupSet></groupSet>")

	vpcs := render[ec2.DescribeVpcsInput](t, "DescribeVpcs", ec2.DescribeVpcsOutput{Vpcs: []*ec2.Vpc{{VpcId: aws.String("vpc-1")}}})
	assert.NotContains(t, vpcs, "ipv6CidrBlockAssociationSet")

	created := render[ec2.CreateVpcInput](t, "CreateVpc", ec2.CreateVpcOutput{Vpc: &ec2.Vpc{
		VpcId:                       aws.String("vpc-1"),
		Ipv6CidrBlockAssociationSet: []*ec2.VpcIpv6CidrBlockAssociation{},
	}})
	assert.Contains(t, created, "<ipv6CidrBlockAssociationSet></ipv6CidrBlockAssociationSet>")
}
