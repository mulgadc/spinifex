package handlers_ec2_vpc

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestSG(t *testing.T, svc *VPCServiceImpl, vpcID, name string) string {
	t.Helper()
	out, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String(name),
		Description: aws.String("test sg"),
		VpcId:       aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)
	return *out.GroupId
}

// --- CreateSecurityGroup ---

func TestCreateSecurityGroup_Success(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("web-sg"),
		Description: aws.String("Web security group"),
		VpcId:       aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)
	assert.NotEmpty(t, *out.GroupId)
}

func TestCreateSecurityGroup_MissingGroupName(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		VpcId: aws.String("vpc-test"),
	}, testAccountID)
	assert.Error(t, err)
}

func TestCreateSecurityGroup_MissingVpcId(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName: aws.String("web-sg"),
	}, testAccountID)
	assert.Error(t, err)
}

func TestCreateSecurityGroup_InvalidVpc(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName: aws.String("web-sg"),
		VpcId:     aws.String("vpc-nonexistent"),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidVpcID.NotFound")
}

func TestCreateSecurityGroup_DuplicateName(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName: aws.String("dup-sg"),
		VpcId:     aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName: aws.String("dup-sg"),
		VpcId:     aws.String(vpcID),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidGroup.Duplicate")
}

// --- DeleteSecurityGroup ---

func TestDeleteSecurityGroup_Success(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "delete-me")

	_, err := svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}, testAccountID)
	require.NoError(t, err)

	// Verify it's gone
	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{}, testAccountID)
	require.NoError(t, err)
	for _, sg := range desc.SecurityGroups {
		assert.NotEqual(t, sgID, *sg.GroupId)
	}
}

func TestDeleteSecurityGroup_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
		GroupId: aws.String("sg-nonexistent"),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidGroup.NotFound")
}

func TestDeleteSecurityGroup_MissingGroupId(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{}, testAccountID)
	assert.Error(t, err)
}

// --- DescribeSecurityGroups ---

func TestDescribeSecurityGroups_All(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSG(t, svc, vpcID, "sg-a")
	createTestSG(t, svc, vpcID, "sg-b")

	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{}, testAccountID)
	require.NoError(t, err)
	// 2 user-created + 1 auto-created default SG (CreateVpc provisions one).
	assert.Len(t, desc.SecurityGroups, 3)
}

func TestDescribeSecurityGroups_ByGroupId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "target-sg")
	createTestSG(t, svc, vpcID, "other-sg")

	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{aws.String(sgID)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.SecurityGroups, 1)
	assert.Equal(t, sgID, *desc.SecurityGroups[0].GroupId)
}

func TestDescribeSecurityGroups_ByGroupName(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSG(t, svc, vpcID, "find-me")
	createTestSG(t, svc, vpcID, "skip-me")

	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-name"), Values: []*string{aws.String("find-me")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.SecurityGroups, 1)
	assert.Equal(t, "find-me", *desc.SecurityGroups[0].GroupName)
}

func TestDescribeSecurityGroups_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{aws.String("sg-nonexistent")},
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidGroup.NotFound")
}

// --- AuthorizeSecurityGroupIngress ---

func TestAuthorizeSecurityGroupIngress_Success(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "ingress-sg")

	proto := "tcp"
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{
			{
				IpProtocol: &proto,
				FromPort:   aws.Int64(80),
				ToPort:     aws.Int64(80),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
			},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify rule was added
	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{aws.String(sgID)},
	}, testAccountID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(desc.SecurityGroups[0].IpPermissions), 1)
}

func TestAuthorizeSecurityGroupIngress_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String("sg-nonexistent"),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidGroup.NotFound")
}

func TestAuthorizeSecurityGroupIngress_MissingGroupId(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{}, testAccountID)
	assert.Error(t, err)
}

// --- AuthorizeSecurityGroupEgress ---

func TestAuthorizeSecurityGroupEgress_Success(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "egress-sg")

	proto := "tcp"
	_, err := svc.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{
			{
				IpProtocol: &proto,
				FromPort:   aws.Int64(443),
				ToPort:     aws.Int64(443),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
}

func TestAuthorizeSecurityGroupEgress_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String("sg-nonexistent"),
	}, testAccountID)
	assert.Error(t, err)
}

// --- RevokeSecurityGroupIngress ---

func TestRevokeSecurityGroupIngress_Success(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "revoke-ingress-sg")

	proto := "tcp"
	perm := &ec2.IpPermission{
		IpProtocol: &proto,
		FromPort:   aws.Int64(22),
		ToPort:     aws.Int64(22),
		IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
	}

	// Add rule
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{perm},
	}, testAccountID)
	require.NoError(t, err)

	// Revoke rule
	_, err = svc.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{perm},
	}, testAccountID)
	require.NoError(t, err)
}

func TestRevokeSecurityGroupIngress_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String("sg-nonexistent"),
	}, testAccountID)
	assert.Error(t, err)
}

// TestRevokeSecurityGroupIngress_RuleNotFound: revoking a rule that doesn't
// exist in the SG must return InvalidPermission.NotFound, matching AWS.
// Terraform / SDK consumers branch on this code to distinguish "already
// revoked, idempotent" from "operator typo".
func TestRevokeSecurityGroupIngress_RuleNotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "rule-notfound-ingress")

	proto := "tcp"
	_, err := svc.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: &proto,
			FromPort:   aws.Int64(9999),
			ToPort:     aws.Int64(9999),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("1.2.3.4/32")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidPermission.NotFound")
}

// --- RevokeSecurityGroupEgress ---

func TestRevokeSecurityGroupEgress_Success(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "revoke-egress-sg")

	// Revoke default egress rule (0.0.0.0/0 all)
	allProto := "-1"
	_, err := svc.RevokeSecurityGroupEgress(&ec2.RevokeSecurityGroupEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{
			{
				IpProtocol: &allProto,
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
			},
		},
	}, testAccountID)
	require.NoError(t, err)
}

func TestRevokeSecurityGroupEgress_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.RevokeSecurityGroupEgress(&ec2.RevokeSecurityGroupEgressInput{
		GroupId: aws.String("sg-nonexistent"),
	}, testAccountID)
	assert.Error(t, err)
}

// TestRevokeSecurityGroupEgress_RuleNotFound: egress counterpart — revoking a
// non-existent egress rule returns InvalidPermission.NotFound.
func TestRevokeSecurityGroupEgress_RuleNotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "rule-notfound-egress")

	proto := "tcp"
	_, err := svc.RevokeSecurityGroupEgress(&ec2.RevokeSecurityGroupEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: &proto,
			FromPort:   aws.Int64(9999),
			ToPort:     aws.Int64(9999),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("1.2.3.4/32")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidPermission.NotFound")
}

// --- Helper function tests ---

func TestIpPermissionsToSGRules_TCP(t *testing.T) {
	proto := "tcp"
	perms := []*ec2.IpPermission{
		{
			IpProtocol: &proto,
			FromPort:   aws.Int64(80),
			ToPort:     aws.Int64(80),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
		},
	}

	rules, err := ipPermissionsToSGRules(perms)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "tcp", rules[0].IpProtocol)
	assert.Equal(t, int64(80), rules[0].FromPort)
	assert.Equal(t, int64(80), rules[0].ToPort)
	assert.Equal(t, "10.0.0.0/8", rules[0].CidrIp)
}

func TestIpPermissionsToSGRules_AllTraffic(t *testing.T) {
	allProto := "-1"
	perms := []*ec2.IpPermission{
		{
			IpProtocol: &allProto,
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		},
	}

	rules, err := ipPermissionsToSGRules(perms)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "-1", rules[0].IpProtocol)
}

func TestIpPermissionsToSGRules_SourceSG(t *testing.T) {
	proto := "-1"
	srcSG := "sg-0123456789abcdef0"
	perms := []*ec2.IpPermission{
		{
			IpProtocol:       &proto,
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String(srcSG)}},
		},
	}

	rules, err := ipPermissionsToSGRules(perms)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, srcSG, rules[0].SourceSG)
}

func TestSGRulesToIpPermissions_Conversion(t *testing.T) {
	rules := []SGRule{
		{IpProtocol: "tcp", FromPort: 443, ToPort: 443, CidrIp: "0.0.0.0/0"},
		{IpProtocol: "-1", SourceSG: "sg-other"},
	}

	perms := sgRulesToIpPermissions(rules)
	require.Len(t, perms, 2)

	// Order is non-deterministic (map-based), find each by protocol
	var tcpPerm, allPerm *ec2.IpPermission
	for _, p := range perms {
		switch *p.IpProtocol {
		case "tcp":
			tcpPerm = p
		case "-1":
			allPerm = p
		}
	}

	require.NotNil(t, tcpPerm, "should have TCP permission")
	assert.Equal(t, int64(443), *tcpPerm.FromPort)
	assert.Len(t, tcpPerm.IpRanges, 1)

	require.NotNil(t, allPerm, "should have all-traffic permission")
	assert.Len(t, allPerm.UserIdGroupPairs, 1)
	assert.Equal(t, "sg-other", *allPerm.UserIdGroupPairs[0].GroupId)
}

func TestSGRuleKey(t *testing.T) {
	rule := SGRule{IpProtocol: "tcp", FromPort: 80, ToPort: 80, CidrIp: "10.0.0.0/8"}
	key := sgRuleKey(rule)
	assert.NotEmpty(t, key)
	assert.Contains(t, key, "tcp")
	assert.Contains(t, key, "80")

	// Same rule should produce same key
	assert.Equal(t, key, sgRuleKey(rule))

	// Different rule should produce different key
	rule2 := SGRule{IpProtocol: "udp", FromPort: 53, ToPort: 53, CidrIp: "10.0.0.0/8"}
	assert.NotEqual(t, key, sgRuleKey(rule2))
}

func TestSGRecordToEC2_Basic(t *testing.T) {
	svc := setupTestVPCService(t)

	record := &SecurityGroupRecord{
		GroupId:     "sg-test",
		GroupName:   "test-group",
		Description: "A test group",
		VpcId:       "vpc-test",
		IngressRules: []SGRule{
			{IpProtocol: "tcp", FromPort: 80, ToPort: 80, CidrIp: "0.0.0.0/0"},
		},
		EgressRules: []SGRule{
			{IpProtocol: "-1", CidrIp: "0.0.0.0/0"},
		},
		Tags: map[string]string{"Name": "test"},
	}

	sg := svc.sgRecordToEC2(record, testAccountID)
	assert.Equal(t, "sg-test", *sg.GroupId)
	assert.Equal(t, "test-group", *sg.GroupName)
	assert.Equal(t, "A test group", *sg.Description)
	assert.Equal(t, "vpc-test", *sg.VpcId)
	assert.Len(t, sg.IpPermissions, 1)
	assert.Len(t, sg.IpPermissionsEgress, 1)
	assert.Len(t, sg.Tags, 1)
}

func TestRemoveSGRules_MatchingRule(t *testing.T) {
	existing := []SGRule{
		{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/8"},
		{IpProtocol: "tcp", FromPort: 80, ToPort: 80, CidrIp: "0.0.0.0/0"},
	}
	toRemove := []SGRule{
		{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/8"},
	}

	result := removeSGRules(existing, toRemove)
	assert.Len(t, result, 1)
	assert.Equal(t, int64(80), result[0].FromPort)
}

func TestRemoveSGRules_NoMatch(t *testing.T) {
	existing := []SGRule{
		{IpProtocol: "tcp", FromPort: 80, ToPort: 80, CidrIp: "0.0.0.0/0"},
	}
	toRemove := []SGRule{
		{IpProtocol: "udp", FromPort: 53, ToPort: 53, CidrIp: "10.0.0.0/8"},
	}

	result := removeSGRules(existing, toRemove)
	assert.Len(t, result, 1)
}

// --- SG-rule input validation (OVN ACL match-expression injection) ---

func TestValidateSGRule(t *testing.T) {
	cases := []struct {
		name    string
		rule    SGRule
		wantErr bool
	}{
		// Valid
		{"canonical /32", SGRule{CidrIp: "1.2.3.4/32"}, false},
		{"canonical /8", SGRule{CidrIp: "10.0.0.0/8"}, false},
		{"any", SGRule{CidrIp: "0.0.0.0/0"}, false},
		{"SG ref accepted (validated downstream by validateSGRuleReferences)", SGRule{SourceSG: "sg-0123456789abcdef0"}, false},

		// Both empty would render as "no source filter" in the ACL builder — must be rejected.
		{"both empty", SGRule{}, true},

		// CidrIp invalid / injection-shaped
		{"host bits set", SGRule{CidrIp: "10.0.0.5/8"}, true},
		{"missing prefix", SGRule{CidrIp: "10.0.0.0"}, true},
		{"bad mask", SGRule{CidrIp: "10.0.0.0/33"}, true},
		{"injection ||", SGRule{CidrIp: "1.2.3.4/32 || ip4.src == 0.0.0.0/0"}, true},
		{"injection &&", SGRule{CidrIp: "10.0.0.0/8 && ip4.src == 0.0.0.0/0"}, true},
		{"injection ;drop", SGRule{CidrIp: "0.0.0.0/0; drop"}, true},
		{"injection jndi", SGRule{CidrIp: "${jndi:ldap://x}"}, true},
		{"injection newline", SGRule{CidrIp: "10.0.0.0/8\n outport == @other"}, true},
		{"injection nbsp (multibyte)", SGRule{CidrIp: "10.0.0.0/8 "}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSGRule(c.rule)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// Each handler smoke-test confirms the wire-up to ipPermissionsToSGRules; the
// full payload table lives in TestValidateSGRule above.

func TestAuthorizeSecurityGroupIngress_RejectsInvalidRule(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "inj-sg")

	proto := "tcp"
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: &proto,
			FromPort:   aws.Int64(80),
			ToPort:     aws.Int64(80),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("1.2.3.4/32 || ip4.src == 0.0.0.0/0")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestAuthorizeSecurityGroupEgress_RejectsInvalidRule(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "inj-sg-egress")

	proto := "-1"
	_, err := svc.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol:       &proto,
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String("sg-abc || outport == @other")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestRevokeSecurityGroupIngress_RejectsInvalidRule(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "inj-sg-revoke")

	proto := "tcp"
	_, err := svc.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: &proto,
			FromPort:   aws.Int64(80),
			ToPort:     aws.Int64(80),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0; drop")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestAuthorizeSecurityGroupIngress_AcceptsValidSourceSG(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "valid-src-sg")
	srcSG := createTestSG(t, svc, vpcID, "valid-target-sg")

	proto := "-1"
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol:       &proto,
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String(srcSG)}},
		}},
	}, testAccountID)
	require.NoError(t, err)
}

// --- DescribeSecurityGroups filter tests ---

func TestDescribeSecurityGroups_FilterByGroupId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "target-sg")
	createTestSG(t, svc, vpcID, "other-sg")

	out, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-id"), Values: []*string{aws.String(sgID)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.SecurityGroups, 1)
	assert.Equal(t, sgID, *out.SecurityGroups[0].GroupId)
}

func TestDescribeSecurityGroups_FilterByDescription(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("web-sg"),
		Description: aws.String("Web server security group"),
		VpcId:       aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("db-sg"),
		Description: aws.String("Database security group"),
		VpcId:       aws.String(vpcID),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("description"), Values: []*string{aws.String("Web server security group")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.SecurityGroups, 1)
	assert.Equal(t, "web-sg", *out.SecurityGroups[0].GroupName)
}

func TestDescribeSecurityGroups_FilterByVpcId(t *testing.T) {
	svc := setupTestVPCService(t)
	vpc1 := createTestVPC(t, svc, "10.0.0.0/16")
	vpc2 := createTestVPC(t, svc, "172.16.0.0/16")
	createTestSG(t, svc, vpc1, "sg-in-vpc1")
	createTestSG(t, svc, vpc2, "sg-in-vpc2")

	out, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpc1)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	// 1 user-created + 1 auto-created default SG in vpc1.
	assert.Len(t, out.SecurityGroups, 2)
	for _, sg := range out.SecurityGroups {
		assert.Equal(t, vpc1, *sg.VpcId)
	}
}

func TestDescribeSecurityGroups_FilterByIpPermissionCidr(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "cidr-sg")

	proto := "tcp"
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{
			{
				IpProtocol: &proto,
				FromPort:   aws.Int64(80),
				ToPort:     aws.Int64(80),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
			},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Create another SG without the ingress rule
	createTestSG(t, svc, vpcID, "no-cidr-sg")

	out, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("ip-permission.cidr"), Values: []*string{aws.String("10.0.0.0/8")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.SecurityGroups, 1)
	assert.Equal(t, sgID, *out.SecurityGroups[0].GroupId)
}

func TestDescribeSecurityGroups_FilterMultipleValues_OR(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSG(t, svc, vpcID, "sg-alpha")
	createTestSG(t, svc, vpcID, "sg-beta")
	createTestSG(t, svc, vpcID, "sg-gamma")

	out, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-name"), Values: []*string{aws.String("sg-alpha"), aws.String("sg-gamma")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.SecurityGroups, 2)
}

func TestDescribeSecurityGroups_FilterMultipleFilters_AND(t *testing.T) {
	svc := setupTestVPCService(t)
	vpc1 := createTestVPC(t, svc, "10.0.0.0/16")
	vpc2 := createTestVPC(t, svc, "172.16.0.0/16")
	createTestSG(t, svc, vpc1, "same-name")
	createTestSG(t, svc, vpc2, "same-name")

	out, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-name"), Values: []*string{aws.String("same-name")}},
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpc1)}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.SecurityGroups, 1)
	assert.Equal(t, vpc1, *out.SecurityGroups[0].VpcId)
}

func TestDescribeSecurityGroups_FilterUnknownName_Error(t *testing.T) {
	svc := setupTestVPCService(t)

	_, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}},
		},
	}, testAccountID)
	assert.Error(t, err)
}

func TestDescribeSecurityGroups_FilterWildcard(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSG(t, svc, vpcID, "prod-web")
	createTestSG(t, svc, vpcID, "prod-api")
	createTestSG(t, svc, vpcID, "staging-web")

	out, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-name"), Values: []*string{aws.String("prod-*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.SecurityGroups, 2)
}

func TestDescribeSecurityGroups_FilterByTag(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	out, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("tagged-sg"),
		Description: aws.String("tagged"),
		VpcId:       aws.String(vpcID),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("security-group"),
				Tags:         []*ec2.Tag{{Key: aws.String("Env"), Value: aws.String("prod")}},
			},
		},
	}, testAccountID)
	require.NoError(t, err)

	createTestSG(t, svc, vpcID, "untagged-sg")

	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("tag:Env"), Values: []*string{aws.String("prod")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.SecurityGroups, 1)
	assert.Equal(t, *out.GroupId, *desc.SecurityGroups[0].GroupId)
}

func TestDescribeSecurityGroups_FilterNoResults(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSG(t, svc, vpcID, "my-sg")

	out, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-name"), Values: []*string{aws.String("nonexistent")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.SecurityGroups)
}

// --- Phase 3: default SG lifecycle, dependency checks, quotas ---

// findDefaultSGInVPC returns the default-SG ID a CreateVpc call provisions.
// Walks the SG KV bucket and filters by IsDefault=true so a regression in
// the discriminator field (rather than the GroupName="default" convention)
// is caught — production code (FindDefaultSGForVPC, DeleteVpc cascade,
// DeleteSecurityGroup default guard) keys off IsDefault, not GroupName.
func findDefaultSGInVPC(t *testing.T, svc *VPCServiceImpl, vpcID string) string {
	t.Helper()
	keys, err := svc.sgKV.Keys()
	require.NoError(t, err)
	var matches []string
	for _, k := range keys {
		if k == utils.VersionKey {
			continue
		}
		entry, err := svc.sgKV.Get(k)
		require.NoError(t, err)
		var rec SecurityGroupRecord
		require.NoError(t, json.Unmarshal(entry.Value(), &rec))
		if rec.IsDefault && rec.VpcId == vpcID {
			matches = append(matches, rec.GroupId)
		}
	}
	require.Len(t, matches, 1, "expected exactly one IsDefault=true SG in vpc %s", vpcID)
	return matches[0]
}

func TestCreateVpc_ProvisionsDefaultSG(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	sgID := findDefaultSGInVPC(t, svc, vpcID)
	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{aws.String(sgID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.SecurityGroups, 1)
	sg := desc.SecurityGroups[0]
	assert.Equal(t, "default", *sg.GroupName)
	assert.Equal(t, "default VPC security group", *sg.Description)
	require.Len(t, sg.IpPermissions, 1)
	require.Len(t, sg.IpPermissions[0].UserIdGroupPairs, 1)
	assert.Equal(t, sgID, *sg.IpPermissions[0].UserIdGroupPairs[0].GroupId)
	require.Len(t, sg.IpPermissionsEgress, 1)
	require.Len(t, sg.IpPermissionsEgress[0].IpRanges, 1)
	assert.Equal(t, "0.0.0.0/0", *sg.IpPermissionsEgress[0].IpRanges[0].CidrIp)
}

func TestCreateSecurityGroup_RejectsReservedDefaultName(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("default"),
		Description: aws.String("user-supplied"),
		VpcId:       aws.String(vpcID),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidGroup.Reserved")
}

func TestDeleteSecurityGroup_RejectsDefault(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := findDefaultSGInVPC(t, svc, vpcID)

	_, err := svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CannotDelete")
}

func TestDeleteSecurityGroup_RejectsAttachedToENI(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	sgID := createTestSG(t, svc, vpcID, "in-use-sg")

	_, err := svc.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
		SubnetId: aws.String(subnetID),
		Groups:   []*string{aws.String(sgID)},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DependencyViolation")
}

func TestDeleteSecurityGroup_RejectsReferencedFromOtherSG(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	srcSG := createTestSG(t, svc, vpcID, "src-sg")
	dstSG := createTestSG(t, svc, vpcID, "dst-sg")

	allProto := "-1"
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(dstSG),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol:       &allProto,
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String(srcSG)}},
		}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(srcSG),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DependencyViolation")
}

func TestDeleteVpc_BlocksOnNonDefaultSG(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	createTestSG(t, svc, vpcID, "user-sg")

	_, err := svc.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DependencyViolation")
}

func TestDeleteVpc_CascadesDefaultSG(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	defaultSG := findDefaultSGInVPC(t, svc, vpcID)

	_, err := svc.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.NoError(t, err)

	// Default SG must be gone too.
	_, err = svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{aws.String(defaultSG)},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidGroup.NotFound")
}

func TestAuthorizeSecurityGroupIngress_RuleLimit(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "rule-limit-sg")

	proto := "tcp"
	for port := range maxRulesPerSGSide {
		_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []*ec2.IpPermission{{
				IpProtocol: &proto,
				FromPort:   aws.Int64(int64(port)),
				ToPort:     aws.Int64(int64(port)),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
			}},
		}, testAccountID)
		require.NoError(t, err, "rule %d should fit under the cap", port)
	}

	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: &proto,
			FromPort:   aws.Int64(int64(maxRulesPerSGSide)),
			ToPort:     aws.Int64(int64(maxRulesPerSGSide)),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RulesPerSecurityGroupLimitExceeded")
}

func TestEnsureDefaultVPC_CreatesDefaultSG(t *testing.T) {
	svc := setupTestVPCService(t)
	info, err := svc.EnsureDefaultVPC(testAccountID)
	require.NoError(t, err)
	require.NotNil(t, info)

	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(info.VpcId)}},
			{Name: aws.String("group-name"), Values: []*string{aws.String("default")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.SecurityGroups, 1)
}

// --- Phase 5.3: SourceSG existence + same-VPC validation on Authorize ---

func TestAuthorizeSecurityGroupIngress_SourceSG_SameVPC(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	ownerSG := createTestSG(t, svc, vpcID, "owner-sg")
	srcSG := createTestSG(t, svc, vpcID, "source-sg")

	allProto := "-1"
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(ownerSG),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol:       &allProto,
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String(srcSG)}},
		}},
	}, testAccountID)
	require.NoError(t, err)
}

func TestAuthorizeSecurityGroupIngress_SourceSG_NotFound(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	ownerSG := createTestSG(t, svc, vpcID, "owner-sg-nf")

	allProto := "-1"
	// Well-formed sg-id that does not exist in KV — must fail with InvalidGroup.NotFound.
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(ownerSG),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol:       &allProto,
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String("sg-0123456789abcdef0")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidGroup.NotFound")
}

func TestAuthorizeSecurityGroupIngress_SourceSG_CrossVPC(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcA := createTestVPC(t, svc, "10.0.0.0/16")
	vpcB := createTestVPC(t, svc, "10.1.0.0/16")
	ownerSG := createTestSG(t, svc, vpcA, "owner-sg-cross")
	srcSG := createTestSG(t, svc, vpcB, "source-sg-cross")

	allProto := "-1"
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(ownerSG),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol:       &allProto,
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String(srcSG)}},
		}},
	}, testAccountID)
	require.Error(t, err)
	// AWS uses InvalidGroup.NotFound for both "missing" and "different VPC".
	assert.Contains(t, err.Error(), "InvalidGroup.NotFound")
}

func TestAuthorizeSecurityGroupEgress_SourceSG_CrossVPC(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcA := createTestVPC(t, svc, "10.0.0.0/16")
	vpcB := createTestVPC(t, svc, "10.1.0.0/16")
	ownerSG := createTestSG(t, svc, vpcA, "owner-sg-cross-egr")
	dstSG := createTestSG(t, svc, vpcB, "dst-sg-cross-egr")

	allProto := "-1"
	_, err := svc.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(ownerSG),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol:       &allProto,
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String(dstSG)}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidGroup.NotFound")
}

// --- Egress dependency check (mirror of ingress coverage) ---

func TestDeleteSecurityGroup_RejectsReferencedFromOtherSGEgress(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	srcSG := createTestSG(t, svc, vpcID, "egress-src-sg")
	dstSG := createTestSG(t, svc, vpcID, "egress-dst-sg")

	allProto := "-1"
	_, err := svc.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(srcSG),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol:       &allProto,
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String(dstSG)}},
		}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(dstSG),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DependencyViolation")
}

// --- Egress rule-count limit (mirror of ingress) ---

func TestAuthorizeSecurityGroupEgress_RuleLimit(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "egress-rule-limit-sg")

	proto := "tcp"
	// Default egress allow-all already counts as 1 rule, so the cap is hit
	// after maxRulesPerSGSide-1 additions.
	for port := range maxRulesPerSGSide - 1 {
		_, err := svc.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []*ec2.IpPermission{{
				IpProtocol: &proto,
				FromPort:   aws.Int64(int64(port)),
				ToPort:     aws.Int64(int64(port)),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
			}},
		}, testAccountID)
		require.NoError(t, err, "rule %d should fit under the cap", port)
	}

	_, err := svc.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: &proto,
			FromPort:   aws.Int64(int64(maxRulesPerSGSide)),
			ToPort:     aws.Int64(int64(maxRulesPerSGSide)),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RulesPerSecurityGroupLimitExceeded")
}

// --- Phase 7: vpcd error propagation across the SG surface ---

// CreateSecurityGroup writes to KV before requesting vpcd, so the record
// persists on vpcd error — the orphan-PG reconciler is the safety net. This
// asserts both the error is surfaced AND the KV-first contract is held.
func TestCreateSecurityGroup_VpcdError_RecordPersists(t *testing.T) {
	svc, _ := setupTestVPCServiceWithFailingVpcd(t, "forced-create-sg-error")

	// Bootstrap a VPC with a working stub by swapping in a success responder
	// for vpc.create-sg only during VPC creation. We do that by short-circuit:
	// CreateVpc here will fail because it also publishes vpc.create-sg.
	// Instead seed a VPC record directly so we can isolate the SG path.
	seedAvailableVPC(t, svc, "vpc-fail000000000")

	_, err := svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
		GroupName:   aws.String("user-sg"),
		Description: aws.String("d"),
		VpcId:       aws.String("vpc-fail000000000"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced-create-sg-error")

	// KV write happens before the request, so the record IS in KV — vpcd's
	// orphan-PG reconciler is the safety net. Verify the user can DescribeSGs
	// and see exactly the one failed-to-provision SG so they can retry/delete.
	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: []*string{aws.String("vpc-fail000000000")}}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.SecurityGroups, 1, "KV record should persist after vpcd error so reconciler can converge")
}

func TestDeleteSecurityGroup_VpcdError_Propagated(t *testing.T) {
	// Build the SG using a temporary success stub, then swap to failing for
	// the delete call.
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "to-delete")

	// Replace the success stub with a failing one for the delete-sg path.
	failingDeleteResponder(t, nc, "forced-delete-sg-error")

	_, err := svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced-delete-sg-error")

	// KV record is removed even on vpcd error — reconciler's orphan-PG scan
	// converges OVN. Asserts the documented "KV-first, no rollback" contract.
	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{aws.String(sgID)},
	}, testAccountID)
	require.Error(t, err, "SG record should be gone post-DeleteSG even when vpcd errors")
	assert.Contains(t, err.Error(), "InvalidGroup.NotFound")
	_ = desc
}

func TestAuthorizeSecurityGroupIngress_VpcdError_Propagated(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "to-authorize")

	failingUpdateResponder(t, nc, "forced-update-sg-error")

	proto := "tcp"
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: &proto,
			FromPort:   aws.Int64(80),
			ToPort:     aws.Int64(80),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced-update-sg-error")

	// Rule must remain in KV so the reconciler can converge OVN.
	desc, err := svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{aws.String(sgID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.SecurityGroups, 1)
	require.Len(t, desc.SecurityGroups[0].IpPermissions, 1, "ingress rule must persist after vpcd error")
}

func TestRevokeSecurityGroupIngress_VpcdError_Propagated(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "to-revoke")

	proto := "tcp"
	rule := &ec2.IpPermission{
		IpProtocol: &proto,
		FromPort:   aws.Int64(443),
		ToPort:     aws.Int64(443),
		IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
	}
	_, err := svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{rule},
	}, testAccountID)
	require.NoError(t, err)

	failingUpdateResponder(t, nc, "forced-revoke-error")

	_, err = svc.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{rule},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced-revoke-error")
}

// TestAuthorizeSecurityGroupEgress_VpcdError_Propagated and the matching
// Revoke variant lock Phase 7's sync contract on the egress path. Without
// these, an OVN port-group failure during egress rule provisioning would be
// silently swallowed — the KV record would gain the rule but OVN would never
// enforce it.
func TestAuthorizeSecurityGroupEgress_VpcdError_Propagated(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "egress-vpcd-fail")

	failingUpdateResponder(t, nc, "forced-egress-auth-error")

	proto := "tcp"
	_, err := svc.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: &proto,
			FromPort:   aws.Int64(443),
			ToPort:     aws.Int64(443),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced-egress-auth-error")
}

func TestRevokeSecurityGroupEgress_VpcdError_Propagated(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "egress-revoke-fail")

	allProto := "-1"
	rule := &ec2.IpPermission{
		IpProtocol: &allProto,
		IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
	}

	failingUpdateResponder(t, nc, "forced-egress-revoke-error")

	_, err := svc.RevokeSecurityGroupEgress(&ec2.RevokeSecurityGroupEgressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{rule},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced-egress-revoke-error")
}

// TestFindDefaultSGForVPC_NoMatch: an account with SGs in vpcA must return ""
// when asked for the default SG of vpcB. The "" return — not an error —
// signals "no default exists; create one" to RunInstances' fallback path.
func TestFindDefaultSGForVPC_NoMatch(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcA := createTestVPC(t, svc, "10.0.0.0/16")
	_ = createTestSG(t, svc, vpcA, "non-default-a")

	got, err := svc.FindDefaultSGForVPC(testAccountID, "vpc-other")
	require.NoError(t, err)
	assert.Equal(t, "", got, "no default SG → empty string, not error")
}

// TestFindDefaultSGForVPC_SkipsMalformedRecord: a corrupt SG entry under the
// account prefix must be skipped, not crash the scan. Without this guard a
// single bad write to the SG bucket would brick the default-SG lookup
// cluster-wide.
func TestFindDefaultSGForVPC_SkipsMalformedRecord(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	_, err := svc.sgKV.Put(testAccountID+".sg-corrupt0000000", []byte("{not json"))
	require.NoError(t, err)

	// CreateVpc above already provisioned the default SG; FindDefaultSGForVPC
	// must locate it despite the corrupt sibling entry.
	got, err := svc.FindDefaultSGForVPC(testAccountID, vpcID)
	require.NoError(t, err)
	assert.Regexp(t, `^sg-`, got, "default SG lookup must skip malformed entry and find the real default")
}

// --- Fix #1: DeleteVpc cascade failure must leave VPC record intact ---

func TestDeleteVpc_VpcdError_VPCNotDeleted(t *testing.T) {
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")

	// Cascade fires vpc.delete-sg through the internal helper.
	failingDeleteResponder(t, nc, "forced-cascade-error")

	_, err := svc.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced-cascade-error")

	// VPC record must still exist so the user can retry.
	desc, err := svc.DescribeVpcs(&ec2.DescribeVpcsInput{
		VpcIds: []*string{aws.String(vpcID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1, "VPC must persist when cascade-delete of default SG fails")
}

// --- Fix #3: checkSGDependencies fails closed on KV read error ---

func TestDeleteSecurityGroup_FailsClosedOnCorruptENI(t *testing.T) {
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "to-delete-corrupt")

	// Inject a malformed ENI record into the bucket; checkSGDependencies must
	// reject the delete rather than silently treating it as "no dependent ENI".
	_, err := svc.eniKV.Put(testAccountID+".eni-corrupt", []byte("{not json"))
	require.NoError(t, err)

	_, err = svc.DeleteSecurityGroup(&ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ServerInternal")
}

// --- Helpers ---

// seedAvailableVPC writes a VPC record directly to KV in "available" state,
// bypassing CreateVpc (which itself depends on a working vpcd stub). Used to
// isolate non-CreateVpc paths in failing-vpcd tests.
func seedAvailableVPC(t *testing.T, svc *VPCServiceImpl, vpcID string) {
	t.Helper()
	rec := VPCRecord{VpcId: vpcID, CidrBlock: "10.0.0.0/16", State: "available"}
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = svc.vpcKV.Put(utils.AccountKey(testAccountID, vpcID), data)
	require.NoError(t, err)
}

// failingDeleteResponder layers a failing handler for vpc.delete-sg on top of
// the default success stubs (NATS picks the responder that subscribes to the
// queue group; here neither uses a queue, so we drain the success sub first).
func failingDeleteResponder(t *testing.T, nc *nats.Conn, msg string) {
	t.Helper()
	testutil.OverrideVpcdStubResponse(nc, "vpc.delete-sg", []byte(`{"success":false,"error":"`+msg+`"}`))
	t.Cleanup(func() {
		testutil.OverrideVpcdStubResponse(nc, "vpc.delete-sg", []byte(`{"success":true}`))
	})
}

func failingUpdateResponder(t *testing.T, nc *nats.Conn, msg string) {
	t.Helper()
	testutil.OverrideVpcdStubResponse(nc, "vpc.update-sg", []byte(`{"success":false,"error":"`+msg+`"}`))
	t.Cleanup(func() {
		testutil.OverrideVpcdStubResponse(nc, "vpc.update-sg", []byte(`{"success":true}`))
	})
}
