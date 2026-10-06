//test:in-package — builds on the package's unexported SG fixtures
//(setupTestVPCService, createTestSG, storedSGRecord) and plants rules in the
//stored record that no exported API can create.

package handlers_ec2_vpc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func authorizeIngress(svc *VPCServiceImpl, sgID string, perm *ec2.IpPermission) error {
	_, err := svc.AuthorizeSecurityGroupIngress(context.Background(), &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{perm},
	}, testAccountID)
	return err
}

// plantIngressRules appends rules straight to the stored record, standing in
// for rules written before the current validation existed.
func plantIngressRules(t *testing.T, svc *VPCServiceImpl, sgID string, rules ...SGRule) {
	t.Helper()
	var rec SecurityGroupRecord
	require.NoError(t, json.Unmarshal(storedSGRecord(t, svc, sgID), &rec))
	rec.IngressRules = append(rec.IngressRules, rules...)
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = svc.sgKV.Put(t.Context(), utils.AccountKey(testAccountID, sgID), data)
	require.NoError(t, err)
}

// An omitted protocol must not be read as all protocols, which would open
// every port to the CIDR.
func TestAuthorizeSecurityGroup_RequiresProtocol(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "no-proto-sg")
	before := storedSGRecord(t, svc, sgID)
	const msg = "Invalid value 'null' for protocol. VPC security group rules must specify protocols explicitly."

	for _, proto := range []*string{nil, aws.String(""), aws.String("  ")} {
		perm := &ec2.IpPermission{
			IpProtocol: proto, FromPort: aws.Int64(80), ToPort: aws.Int64(80),
			IpRanges: []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24")}},
		}
		requireAWSError(t, authorizeIngress(svc, sgID, perm), awserrors.ErrorInvalidParameterValue, msg)

		_, err := svc.AuthorizeSecurityGroupEgress(context.Background(), &ec2.AuthorizeSecurityGroupEgressInput{
			GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{perm},
		}, testAccountID)
		requireAWSError(t, err, awserrors.ErrorInvalidParameterValue, msg)
	}
	assert.Equal(t, before, storedSGRecord(t, svc, sgID), "a rejected authorize must store nothing")
}

// A revoke without a protocol must not match the all-traffic rule.
func TestRevokeSecurityGroupEgress_RequiresProtocol(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "revoke-no-proto-sg")
	before := storedSGRecord(t, svc, sgID)

	_, err := svc.RevokeSecurityGroupEgress(context.Background(), &ec2.RevokeSecurityGroupEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpRanges: []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
		}},
	}, testAccountID)
	requireAWSCode(t, err, awserrors.ErrorInvalidParameterValue)
	assert.Equal(t, before, storedSGRecord(t, svc, sgID), "the default egress rule must survive")
}

func TestAuthorizeSecurityGroupIngress_TCPUDPRequireBothPorts(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "no-ports-sg")
	before := storedSGRecord(t, svc, sgID)

	for _, proto := range []string{"tcp", "6", "udp", "17"} {
		for _, ports := range [][2]*int64{{nil, nil}, {aws.Int64(80), nil}, {nil, aws.Int64(80)}} {
			err := authorizeIngress(svc, sgID, &ec2.IpPermission{
				IpProtocol: aws.String(proto), FromPort: ports[0], ToPort: ports[1],
				IpRanges: []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24")}},
			})
			requireAWSError(t, err, awserrors.ErrorInvalidParameterValue,
				"Invalid value for portRange. Must specify both from and to ports with TCP/UDP.")
		}
	}
	assert.Equal(t, before, storedSGRecord(t, svc, sgID), "a rejected authorize must store nothing")
}

// An all-protocol rule is one rule whatever ports it was sent with, so a
// second authorize differing only in ports is a duplicate.
func TestAuthorizeSecurityGroupIngress_AllProtocolPortsShareIdentity(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "all-proto-identity-sg")

	out, err := svc.AuthorizeSecurityGroupIngress(context.Background(), &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("-1"), FromPort: aws.Int64(-1), ToPort: aws.Int64(-1),
			IpRanges: []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24")}},
		}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SecurityGroupRules, 1)
	stored := storedSGRule(t, svc, sgID, aws.StringValue(out.SecurityGroupRules[0].SecurityGroupRuleId))
	assert.Equal(t, [2]int64{0, 0}, [2]int64{stored.FromPort, stored.ToPort}, "stored in the default egress rule's form")

	err = authorizeIngress(svc, sgID, &ec2.IpPermission{
		IpProtocol: aws.String("-1"),
		IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24")}},
	})
	requireAWSCode(t, err, awserrors.ErrorInvalidPermissionDuplicate)
}

// A rule stored with -1/-1 before authorize canonicalised the ports is the
// same rule as its 0/0 form for duplicate checks, revoke and reporting.
func TestAllProtocolRuleStoredWithMinusOnePorts(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "legacy-all-proto-sg")
	plantIngressRules(t, svc, sgID,
		SGRule{RuleId: "sgr-0000000000000000a", IpProtocol: "-1", FromPort: -1, ToPort: -1, CidrIp: "10.0.0.0/24"},
		SGRule{RuleId: "sgr-0000000000000000b", IpProtocol: "-1", FromPort: 0, ToPort: 0, CidrIp: "10.0.1.0/24"},
	)

	err := authorizeIngress(svc, sgID, &ec2.IpPermission{
		IpProtocol: aws.String("-1"),
		IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24")}},
	})
	requireAWSCode(t, err, awserrors.ErrorInvalidPermissionDuplicate)

	desc, err := svc.DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{aws.String(sgID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.SecurityGroups, 1)
	require.Len(t, desc.SecurityGroups[0].IpPermissions, 1, "both rules are one all-protocol permission")
	assert.Len(t, desc.SecurityGroups[0].IpPermissions[0].IpRanges, 2)

	_, err = svc.RevokeSecurityGroupIngress(context.Background(), &ec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("-1"),
			IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24")}},
		}},
	}, testAccountID)
	require.NoError(t, err)
	var rec SecurityGroupRecord
	require.NoError(t, json.Unmarshal(storedSGRecord(t, svc, sgID), &rec))
	require.Len(t, rec.IngressRules, 1)
	assert.Equal(t, "sgr-0000000000000000b", rec.IngressRules[0].RuleId)
}

// AWS calls a rule ID malformed only without the lowercase sgr- prefix or with
// more than 17 characters after it; anything else that matches no rule is
// not found. Every operation taking a rule ID must agree.
func TestSecurityGroupRuleIDClassification(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "rule-id-class-sg")

	ops := map[string]func(id string) error{
		"DescribeSecurityGroupRules": func(id string) error {
			_, err := svc.DescribeSecurityGroupRules(context.Background(), &ec2.DescribeSecurityGroupRulesInput{
				SecurityGroupRuleIds: []*string{aws.String(id)},
			}, testAccountID)
			return err
		},
		"RevokeSecurityGroupIngress": func(id string) error {
			_, err := svc.RevokeSecurityGroupIngress(context.Background(), &ec2.RevokeSecurityGroupIngressInput{
				GroupId: aws.String(sgID), SecurityGroupRuleIds: []*string{aws.String(id)},
			}, testAccountID)
			return err
		},
		"RevokeSecurityGroupEgress": func(id string) error {
			_, err := svc.RevokeSecurityGroupEgress(context.Background(), &ec2.RevokeSecurityGroupEgressInput{
				GroupId: aws.String(sgID), SecurityGroupRuleIds: []*string{aws.String(id)},
			}, testAccountID)
			return err
		},
		"UpdateSecurityGroupRuleDescriptionsIngress": func(id string) error {
			return updateIngressDescription(svc, sgID, id, "x")
		},
		"UpdateSecurityGroupRuleDescriptionsEgress": func(id string) error {
			_, err := svc.UpdateSecurityGroupRuleDescriptionsEgress(context.Background(), &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
				GroupId: aws.String(sgID),
				SecurityGroupRuleDescriptions: []*ec2.SecurityGroupRuleDescription{
					{SecurityGroupRuleId: aws.String(id), Description: aws.String("x")},
				},
			}, testAccountID)
			return err
		},
	}
	ids := map[string]string{
		"sgr-xyz":                awserrors.ErrorInvalidSecurityGroupRuleIdNotFound,
		"sgr-0123-4567":          awserrors.ErrorInvalidSecurityGroupRuleIdNotFound,
		"sgr-":                   awserrors.ErrorInvalidSecurityGroupRuleIdNotFound,
		"sgr-00000000000000000":  awserrors.ErrorInvalidSecurityGroupRuleIdNotFound,
		"SGR-00000000000000000":  awserrors.ErrorInvalidSecurityGroupRuleIdMalformed,
		"sgr-000000000000000000": awserrors.ErrorInvalidSecurityGroupRuleIdMalformed,
		"not-a-rule-id":          awserrors.ErrorInvalidSecurityGroupRuleIdMalformed,
	}
	for name, op := range ops {
		for id, code := range ids {
			t.Run(name+"/"+id, func(t *testing.T) {
				t.Parallel()
				requireAWSCode(t, op(id), code)
			})
		}
	}
}
