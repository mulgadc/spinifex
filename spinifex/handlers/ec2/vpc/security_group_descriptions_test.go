package handlers_ec2_vpc

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sgRuleByID returns the stored rule with the given ID as the API reports it.
func sgRuleByID(t *testing.T, svc *VPCServiceImpl, accountID, ruleID string) *ec2.SecurityGroupRule {
	t.Helper()
	out, err := svc.DescribeSecurityGroupRules(context.Background(), &ec2.DescribeSecurityGroupRulesInput{
		SecurityGroupRuleIds: []*string{aws.String(ruleID)},
	}, accountID)
	require.NoError(t, err)
	require.Len(t, out.SecurityGroupRules, 1)
	return out.SecurityGroupRules[0]
}

// defaultEgressRuleID returns the sgr- ID of the all-traffic egress rule every
// new SG carries.
func defaultEgressRuleID(t *testing.T, svc *VPCServiceImpl, sgID string) string {
	t.Helper()
	out, err := svc.DescribeSecurityGroupRules(context.Background(), &ec2.DescribeSecurityGroupRulesInput{
		Filters: []*ec2.Filter{{Name: aws.String("group-id"), Values: []*string{aws.String(sgID)}}},
	}, testAccountID)
	require.NoError(t, err)
	for _, r := range out.SecurityGroupRules {
		if aws.BoolValue(r.IsEgress) {
			return aws.StringValue(r.SecurityGroupRuleId)
		}
	}
	t.Fatalf("no egress rule on %s", sgID)
	return ""
}

func updateIngressDescription(svc *VPCServiceImpl, sgID, ruleID, desc string) error {
	_, err := svc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{
		GroupId: aws.String(sgID),
		SecurityGroupRuleDescriptions: []*ec2.SecurityGroupRuleDescription{
			{SecurityGroupRuleId: aws.String(ruleID), Description: aws.String(desc)},
		},
	}, testAccountID)
	return err
}

func TestUpdateSecurityGroupRuleDescriptionsIngress_ByRuleID(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-ingress-byid")
	ruleID := authorizeIngressTCP(t, svc, sgID, 443, "10.0.0.0/24")

	require.NoError(t, updateIngressDescription(svc, sgID, ruleID, "managed by lbc"))

	assert.Equal(t, "managed by lbc", aws.StringValue(sgRuleByID(t, svc, testAccountID, ruleID).Description))
}

func TestUpdateSecurityGroupRuleDescriptionsEgress_ByRuleID(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-egress-byid")
	ruleID := defaultEgressRuleID(t, svc, sgID)

	_, err := svc.UpdateSecurityGroupRuleDescriptionsEgress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId: aws.String(sgID),
		SecurityGroupRuleDescriptions: []*ec2.SecurityGroupRuleDescription{
			{SecurityGroupRuleId: aws.String(ruleID), Description: aws.String("all egress")},
		},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, "all egress", aws.StringValue(sgRuleByID(t, svc, testAccountID, ruleID).Description))
}

func TestUpdateSecurityGroupRuleDescriptionsIngress_ByIpPermissions(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-ingress-byperm")
	ruleID := authorizeIngressTCP(t, svc, sgID, 22, "10.0.0.0/24")

	_, err := svc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(22),
			ToPort:     aws.Int64(22),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24"), Description: aws.String("ssh from office")}},
		}},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, "ssh from office", aws.StringValue(sgRuleByID(t, svc, testAccountID, ruleID).Description))
}

func TestUpdateSecurityGroupRuleDescriptionsEgress_ByIpPermissions(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-egress-byperm")
	ruleID := defaultEgressRuleID(t, svc, sgID)

	_, err := svc.UpdateSecurityGroupRuleDescriptionsEgress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("-1"),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0"), Description: aws.String("default egress")}},
		}},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, "default egress", aws.StringValue(sgRuleByID(t, svc, testAccountID, ruleID).Description))
}

// TestUpdateSecurityGroupRuleDescriptions_PreservesIdentity is the AWS Load
// Balancer Controller's path: it re-tags a node-SG rule and then revokes it by
// the original content match, which only works if the rule's identity and ID
// survive the description write.
func TestUpdateSecurityGroupRuleDescriptions_PreservesIdentity(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-identity")
	ruleID := authorizeIngressTCP(t, svc, sgID, 8080, "10.0.0.0/24")
	before := sgRuleByID(t, svc, testAccountID, ruleID)

	require.NoError(t, updateIngressDescription(svc, sgID, ruleID, "elbv2.k8s.aws/targetGroupBinding=shared"))

	after := sgRuleByID(t, svc, testAccountID, ruleID)
	assert.Equal(t, ruleID, aws.StringValue(after.SecurityGroupRuleId), "the rule ID must not be reassigned")
	assert.Equal(t, aws.StringValue(before.IpProtocol), aws.StringValue(after.IpProtocol))
	assert.Equal(t, aws.Int64Value(before.FromPort), aws.Int64Value(after.FromPort))
	assert.Equal(t, aws.Int64Value(before.ToPort), aws.Int64Value(after.ToPort))
	assert.Equal(t, aws.StringValue(before.CidrIpv4), aws.StringValue(after.CidrIpv4))

	// Still revocable by exactly the content match used before the re-tag.
	_, err := svc.RevokeSecurityGroupIngress(context.Background(), &ec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(8080),
			ToPort:     aws.Int64(8080),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24")}},
		}},
	}, testAccountID)
	require.NoError(t, err, "a re-tagged rule stays revocable by its original content match")
}

// TestUpdateSecurityGroupRuleDescriptions_ClearVersusUnset pins that an empty
// description clears the stored value while an absent one leaves it alone, so
// a caller can remove a tag without a revoke/re-authorize cycle.
func TestUpdateSecurityGroupRuleDescriptions_ClearVersusUnset(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-clear")
	ruleID := authorizeIngressTCP(t, svc, sgID, 9090, "10.0.0.0/24")
	require.NoError(t, updateIngressDescription(svc, sgID, ruleID, "tagged"))

	_, err := svc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{
		GroupId: aws.String(sgID),
		SecurityGroupRuleDescriptions: []*ec2.SecurityGroupRuleDescription{
			{SecurityGroupRuleId: aws.String(ruleID)},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "tagged", aws.StringValue(sgRuleByID(t, svc, testAccountID, ruleID).Description),
		"omitting the description must leave the stored one untouched")

	require.NoError(t, updateIngressDescription(svc, sgID, ruleID, ""))
	assert.Nil(t, sgRuleByID(t, svc, testAccountID, ruleID).Description,
		"an empty description must clear the stored one")
}

func TestUpdateSecurityGroupRuleDescriptionsIngress_CannotReachEgressRule(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-side-ingress")
	egressID := defaultEgressRuleID(t, svc, sgID)

	err := updateIngressDescription(svc, sgID, egressID, "wrong side")
	require.ErrorContains(t, err, awserrors.ErrorInvalidSecurityGroupRuleIdNotFound)
	assert.Nil(t, sgRuleByID(t, svc, testAccountID, egressID).Description)
}

func TestUpdateSecurityGroupRuleDescriptionsEgress_CannotReachIngressRule(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-side-egress")
	ingressID := authorizeIngressTCP(t, svc, sgID, 443, "10.0.0.0/24")

	_, err := svc.UpdateSecurityGroupRuleDescriptionsEgress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId: aws.String(sgID),
		SecurityGroupRuleDescriptions: []*ec2.SecurityGroupRuleDescription{
			{SecurityGroupRuleId: aws.String(ingressID), Description: aws.String("wrong side")},
		},
	}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidSecurityGroupRuleIdNotFound)
	assert.Nil(t, sgRuleByID(t, svc, testAccountID, ingressID).Description)
}

func TestUpdateSecurityGroupRuleDescriptionsIngress_MalformedRuleID(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-malformed")

	err := updateIngressDescription(svc, sgID, "not-a-rule-id", "x")
	require.ErrorContains(t, err, awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
}

func TestUpdateSecurityGroupRuleDescriptionsIngress_UnknownRuleID(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-unknown")

	err := updateIngressDescription(svc, sgID, "sgr-00000000000000000", "x")
	require.ErrorContains(t, err, awserrors.ErrorInvalidSecurityGroupRuleIdNotFound)
}

// TestUpdateSecurityGroupRuleDescriptions_OtherAccountIsNotFound pins that a
// valid rule ID from another account is not-found, never a successful no-op.
func TestUpdateSecurityGroupRuleDescriptions_OtherAccountIsNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-other-account")
	ruleID := authorizeIngressTCP(t, svc, sgID, 443, "10.0.0.0/24")

	_, err := svc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{
		GroupId: aws.String(sgID),
		SecurityGroupRuleDescriptions: []*ec2.SecurityGroupRuleDescription{
			{SecurityGroupRuleId: aws.String(ruleID), Description: aws.String("stolen")},
		},
	}, "210987654321")
	require.ErrorContains(t, err, awserrors.ErrorInvalidGroupNotFound)
	assert.Nil(t, sgRuleByID(t, svc, testAccountID, ruleID).Description)
}

// TestUpdateSecurityGroupRuleDescriptions_OtherGroupIsNotFound pins the same
// for a rule ID that exists but belongs to a different group.
func TestUpdateSecurityGroupRuleDescriptions_OtherGroupIsNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgA := createTestSG(t, svc, vpcID, "desc-group-a")
	sgB := createTestSG(t, svc, vpcID, "desc-group-b")
	ruleID := authorizeIngressTCP(t, svc, sgA, 443, "10.0.0.0/24")

	err := updateIngressDescription(svc, sgB, ruleID, "wrong group")
	require.ErrorContains(t, err, awserrors.ErrorInvalidSecurityGroupRuleIdNotFound)
	assert.Nil(t, sgRuleByID(t, svc, testAccountID, ruleID).Description)
}

func TestUpdateSecurityGroupRuleDescriptionsIngress_UnmatchedPermission(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-unmatched-perm")
	authorizeIngressTCP(t, svc, sgID, 22, "10.0.0.0/24")

	_, err := svc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(23),
			ToPort:     aws.Int64(23),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24"), Description: aws.String("telnet")}},
		}},
	}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPermissionNotFound)
}

func TestUpdateSecurityGroupRuleDescriptions_RejectsEmptyRequest(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-empty-request")

	_, err := svc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{
		GroupId: aws.String(sgID),
	}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorMissingParameter)

	_, err = svc.UpdateSecurityGroupRuleDescriptionsIngress(context.Background(),
		&ec2.UpdateSecurityGroupRuleDescriptionsIngressInput{}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorMissingParameter)

	_, err = svc.UpdateSecurityGroupRuleDescriptionsEgress(context.Background(), nil, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorMissingParameter)
}

func TestUpdateSecurityGroupRuleDescriptions_GroupNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)

	err := updateIngressDescription(svc, "sg-00000000000000000", "sgr-00000000000000000", "x")
	require.ErrorContains(t, err, awserrors.ErrorInvalidGroupNotFound)
}

func TestUpdateSecurityGroupRuleDescriptions_VpcdFailureFailsCall(t *testing.T) {
	t.Parallel()
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-vpcd-fail")
	ruleID := authorizeIngressTCP(t, svc, sgID, 443, "10.0.0.0/24")

	failingUpdateResponder(t, nc, "ovn unavailable")

	err := updateIngressDescription(svc, sgID, ruleID, "x")
	require.Error(t, err, "a vpc.update-sg failure must fail the call")
	assert.ErrorContains(t, err, "ovn unavailable")
}

// TestUpdateSecurityGroupRuleDescriptions_ConcurrentAuthorize races a
// description write against an authorize on the same group. The compare-and-swap
// makes one of them fail loudly; neither may silently drop the other's write.
func TestUpdateSecurityGroupRuleDescriptions_ConcurrentAuthorize(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "desc-concurrent")
	ruleID := authorizeIngressTCP(t, svc, sgID, 443, "10.0.0.0/24")

	var wg sync.WaitGroup
	var descErr, authErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		descErr = updateIngressDescription(svc, sgID, ruleID, "concurrent tag")
	}()
	go func() {
		defer wg.Done()
		_, authErr = svc.AuthorizeSecurityGroupIngress(context.Background(), &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []*ec2.IpPermission{{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(8443),
				ToPort:     aws.Int64(8443),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24")}},
			}},
		}, testAccountID)
	}()
	wg.Wait()

	out, err := svc.DescribeSecurityGroupRules(context.Background(), &ec2.DescribeSecurityGroupRulesInput{
		Filters: []*ec2.Filter{{Name: aws.String("group-id"), Values: []*string{aws.String(sgID)}}},
	}, testAccountID)
	require.NoError(t, err)

	var tagged, authorized bool
	for _, r := range out.SecurityGroupRules {
		if aws.StringValue(r.SecurityGroupRuleId) == ruleID && aws.StringValue(r.Description) == "concurrent tag" {
			tagged = true
		}
		if aws.Int64Value(r.FromPort) == 8443 {
			authorized = true
		}
	}
	assert.Equal(t, descErr == nil, tagged, "the description write succeeded iff its effect is stored")
	assert.Equal(t, authErr == nil, authorized, "the authorize succeeded iff its effect is stored")
}
