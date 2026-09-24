//test:in-package — builds on the package's unexported SG fixtures
//(setupTestVPCService, createTestSG, authorizeIngressTCP, failingUpdateResponder)
//and asserts on the stored record, which no exported API returns verbatim.

package handlers_ec2_vpc

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireAWSCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	got, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "error %q carries no AWS code", err)
	assert.Equal(t, code, got, "error: %v", err)
}

func modifyRules(svc *VPCServiceImpl, sgID string, updates ...*ec2.SecurityGroupRuleUpdate) error {
	_, err := svc.ModifySecurityGroupRules(context.Background(), &ec2.ModifySecurityGroupRulesInput{
		GroupId:            aws.String(sgID),
		SecurityGroupRules: updates,
	}, testAccountID)
	return err
}

func ruleUpdate(ruleID string, req *ec2.SecurityGroupRuleRequest) *ec2.SecurityGroupRuleUpdate {
	return &ec2.SecurityGroupRuleUpdate{SecurityGroupRuleId: aws.String(ruleID), SecurityGroupRule: req}
}

func tcpCIDRRequest(port int64, cidr string) *ec2.SecurityGroupRuleRequest {
	return &ec2.SecurityGroupRuleRequest{
		IpProtocol: aws.String("tcp"),
		FromPort:   aws.Int64(port),
		ToPort:     aws.Int64(port),
		CidrIpv4:   aws.String(cidr),
	}
}

func tcpPeerRequest(port int64, peer string) *ec2.SecurityGroupRuleRequest {
	return &ec2.SecurityGroupRuleRequest{
		IpProtocol:        aws.String("tcp"),
		FromPort:          aws.Int64(port),
		ToPort:            aws.Int64(port),
		ReferencedGroupId: aws.String(peer),
	}
}

// storedSGRecord returns the raw stored record, for asserting a rejected
// modify wrote nothing.
func storedSGRecord(t *testing.T, svc *VPCServiceImpl, sgID string) []byte {
	t.Helper()
	entry, err := svc.sgKV.Get(t.Context(), utils.AccountKey(testAccountID, sgID))
	require.NoError(t, err)
	return entry.Value()
}

func storedSGRule(t *testing.T, svc *VPCServiceImpl, sgID, ruleID string) SGRule {
	t.Helper()
	var rec SecurityGroupRecord
	require.NoError(t, json.Unmarshal(storedSGRecord(t, svc, sgID), &rec))
	for _, r := range append(rec.IngressRules, rec.EgressRules...) {
		if r.RuleId == ruleID {
			return r
		}
	}
	t.Fatalf("rule %s not stored on %s", ruleID, sgID)
	return SGRule{}
}

func authorizeIngressPeer(t *testing.T, svc *VPCServiceImpl, sgID string, port int64, peer string) string {
	t.Helper()
	out, err := svc.AuthorizeSecurityGroupIngress(context.Background(), &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol:       aws.String("tcp"),
			FromPort:         aws.Int64(port),
			ToPort:           aws.Int64(port),
			UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String(peer)}},
		}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SecurityGroupRules, 1)
	return aws.StringValue(out.SecurityGroupRules[0].SecurityGroupRuleId)
}

func authorizeIngressIPv6(t *testing.T, svc *VPCServiceImpl, sgID string, port int64, cidr string) string {
	t.Helper()
	out, err := svc.AuthorizeSecurityGroupIngress(context.Background(), &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"),
			FromPort:   aws.Int64(port),
			ToPort:     aws.Int64(port),
			Ipv6Ranges: []*ec2.Ipv6Range{{CidrIpv6: aws.String(cidr)}},
		}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SecurityGroupRules, 1)
	return aws.StringValue(out.SecurityGroupRules[0].SecurityGroupRuleId)
}

func TestModifySecurityGroupRules_ChangesContentKeepsID(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-content")
	ruleID := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")

	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, tcpCIDRRequest(8080, "10.0.0.0/24"))))
	got := sgRuleByID(t, svc, testAccountID, ruleID)
	assert.Equal(t, int64(8080), aws.Int64Value(got.FromPort))
	assert.Equal(t, int64(8080), aws.Int64Value(got.ToPort))

	udp := tcpCIDRRequest(8080, "10.0.0.0/24")
	udp.IpProtocol = aws.String("udp")
	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, udp)))
	assert.Equal(t, "udp", aws.StringValue(sgRuleByID(t, svc, testAccountID, ruleID).IpProtocol))

	udp.CidrIpv4 = aws.String("192.168.0.0/16")
	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, udp)))
	got = sgRuleByID(t, svc, testAccountID, ruleID)
	assert.Equal(t, "192.168.0.0/16", aws.StringValue(got.CidrIpv4))
	assert.Equal(t, ruleID, aws.StringValue(got.SecurityGroupRuleId))

	peerA := createTestSG(t, svc, vpcID, "mod-peer-a")
	peerB := createTestSG(t, svc, vpcID, "mod-peer-b")
	peerRule := authorizeIngressPeer(t, svc, sgID, 5432, peerA)
	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(peerRule, tcpPeerRequest(5432, peerB))))
	got = sgRuleByID(t, svc, testAccountID, peerRule)
	require.NotNil(t, got.ReferencedGroupInfo)
	assert.Equal(t, peerB, aws.StringValue(got.ReferencedGroupInfo.GroupId))
	assert.Equal(t, peerRule, aws.StringValue(got.SecurityGroupRuleId))
}

func TestModifySecurityGroupRules_EgressByID(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-egress")
	ruleID := defaultEgressRuleID(t, svc, sgID)

	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, tcpCIDRRequest(443, "0.0.0.0/0"))))

	got := sgRuleByID(t, svc, testAccountID, ruleID)
	assert.True(t, aws.BoolValue(got.IsEgress))
	assert.Equal(t, "tcp", aws.StringValue(got.IpProtocol))
	assert.Equal(t, int64(443), aws.Int64Value(got.FromPort))
}

// TestModifySecurityGroupRules_RevocableByNewContent pins that sgRuleKey is
// recomputed: the rule answers to its new content and no longer to its old.
func TestModifySecurityGroupRules_RevocableByNewContent(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-revoke")
	ruleID := authorizeIngressTCP(t, svc, sgID, 8080, "192.0.2.0/24")

	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, tcpCIDRRequest(8081, "198.51.100.0/24"))))

	revoke := func(port int64, cidr string) error {
		_, err := svc.RevokeSecurityGroupIngress(context.Background(), &ec2.RevokeSecurityGroupIngressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []*ec2.IpPermission{{
				IpProtocol: aws.String("tcp"),
				FromPort:   aws.Int64(port),
				ToPort:     aws.Int64(port),
				IpRanges:   []*ec2.IpRange{{CidrIp: aws.String(cidr)}},
			}},
		}, testAccountID)
		return err
	}
	requireAWSCode(t, revoke(8080, "192.0.2.0/24"), awserrors.ErrorInvalidPermissionNotFound)
	require.NoError(t, revoke(8081, "198.51.100.0/24"))
}

func TestModifySecurityGroupRules_PreservesTags(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	writer := &fakeCentralTagStore{}
	svc.SetCentralTagStore(writer)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-tags")

	out, err := svc.AuthorizeSecurityGroupIngress(context.Background(), &ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int64(80), ToPort: aws.Int64(80),
			IpRanges: []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/24")}},
		}},
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String(ec2.ResourceTypeSecurityGroupRule),
			Tags:         []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("web")}},
		}},
	}, testAccountID)
	require.NoError(t, err)
	ruleID := aws.StringValue(out.SecurityGroupRules[0].SecurityGroupRuleId)

	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, tcpCIDRRequest(81, "10.0.0.0/24"))))

	want := map[string]string{"Name": "web"}
	assert.Equal(t, want, filterutil.EC2TagsToMap(sgRuleByID(t, svc, testAccountID, ruleID).Tags))
	assert.Equal(t, want, writer.calls[ruleID], "the central tag store must still hold the rule's tags")
	assert.NotContains(t, writer.deleted, ruleID, "a modify must not clear the rule's central tags")
}

// TestModifySecurityGroupRules_DescriptionIsReplaced pins AWS's whole-rule
// replace: an omitted description clears the stored one, unlike the
// description operations, where omission leaves it untouched.
func TestModifySecurityGroupRules_DescriptionIsReplaced(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-desc")
	ruleID := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")
	require.NoError(t, updateIngressDescription(svc, sgID, ruleID, "old"))

	req := tcpCIDRRequest(80, "10.0.0.0/24")
	req.Description = aws.String("new")
	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, req)))
	assert.Equal(t, "new", aws.StringValue(sgRuleByID(t, svc, testAccountID, ruleID).Description))

	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, tcpCIDRRequest(80, "10.0.0.0/24"))))
	assert.Nil(t, sgRuleByID(t, svc, testAccountID, ruleID).Description, "an omitted description must clear the stored one")
}

func TestModifySecurityGroupRules_RejectsSourceTypeChange(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-source-type")
	peer := createTestSG(t, svc, vpcID, "mod-source-peer")
	v4Rule := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")
	v6Rule := authorizeIngressIPv6(t, svc, sgID, 80, "2001:db8::/32")
	peerRule := authorizeIngressPeer(t, svc, sgID, 80, peer)
	v6Request := &ec2.SecurityGroupRuleRequest{
		IpProtocol: aws.String("tcp"), FromPort: aws.Int64(81), ToPort: aws.Int64(81),
		CidrIpv6: aws.String("2001:db8::/48"),
	}

	for name, u := range map[string]*ec2.SecurityGroupRuleUpdate{
		"ipv4 to ipv6": ruleUpdate(v4Rule, v6Request),
		"ipv4 to peer": ruleUpdate(v4Rule, tcpPeerRequest(81, peer)),
		"ipv6 to ipv4": ruleUpdate(v6Rule, tcpCIDRRequest(81, "10.1.0.0/24")),
		"ipv6 to peer": ruleUpdate(v6Rule, tcpPeerRequest(81, peer)),
		"peer to ipv4": ruleUpdate(peerRule, tcpCIDRRequest(81, "10.1.0.0/24")),
		"peer to ipv6": ruleUpdate(peerRule, v6Request),
	} {
		before := storedSGRecord(t, svc, sgID)
		err := modifyRules(svc, sgID, u)
		requireAWSCode(t, err, awserrors.ErrorInvalidParameterValue)
		assert.ErrorContains(t, err, "Invalid rule type", name)
		assert.Equal(t, before, storedSGRecord(t, svc, sgID), name)
	}
}

func TestModifySecurityGroupRules_SameSideCollisionIsDuplicate(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-dup")
	ruleA := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")
	authorizeIngressTCP(t, svc, sgID, 443, "10.0.0.0/24")
	before := storedSGRecord(t, svc, sgID)

	requireAWSCode(t, modifyRules(svc, sgID, ruleUpdate(ruleA, tcpCIDRRequest(443, "10.0.0.0/24"))),
		awserrors.ErrorInvalidPermissionDuplicate)
	assert.Equal(t, before, storedSGRecord(t, svc, sgID), "a rejected modify must leave the record byte-identical")
}

// An all-protocol rule sent with -1/-1 is the same rule as one with no ports,
// so it collides with the default egress rule.
func TestModifySecurityGroupRules_AllProtocolMinusOnePortsCollides(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-dup-all")
	_, err := svc.AuthorizeSecurityGroupEgress(context.Background(), &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int64(443), ToPort: aws.Int64(443),
			IpRanges: []*ec2.IpRange{{CidrIp: aws.String("10.0.0.0/8")}},
		}},
	}, testAccountID)
	require.NoError(t, err)
	var ruleID string
	for _, r := range describeGroupRules(t, svc, sgID) {
		if aws.BoolValue(r.IsEgress) && aws.StringValue(r.IpProtocol) == "tcp" {
			ruleID = aws.StringValue(r.SecurityGroupRuleId)
		}
	}
	require.NotEmpty(t, ruleID)

	requireAWSCode(t, modifyRules(svc, sgID, ruleUpdate(ruleID, &ec2.SecurityGroupRuleRequest{
		IpProtocol: aws.String("-1"), FromPort: aws.Int64(-1), ToPort: aws.Int64(-1),
		CidrIpv4: aws.String("0.0.0.0/0"),
	})), awserrors.ErrorInvalidPermissionDuplicate)
}

func describeGroupRules(t *testing.T, svc *VPCServiceImpl, sgID string) []*ec2.SecurityGroupRule {
	t.Helper()
	out, err := svc.DescribeSecurityGroupRules(context.Background(), &ec2.DescribeSecurityGroupRulesInput{
		Filters: []*ec2.Filter{{Name: aws.String("group-id"), Values: []*string{aws.String(sgID)}}},
	}, testAccountID)
	require.NoError(t, err)
	return out.SecurityGroupRules
}

func TestModifySecurityGroupRules_OtherSideCollisionSucceeds(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-other-side")
	ruleID := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")

	// Same content as the default egress rule, which lives on the other side.
	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, &ec2.SecurityGroupRuleRequest{
		IpProtocol: aws.String("-1"),
		CidrIpv4:   aws.String("0.0.0.0/0"),
	})))
	assert.Equal(t, "-1", aws.StringValue(sgRuleByID(t, svc, testAccountID, ruleID).IpProtocol))
}

func TestModifySecurityGroupRules_OwnContentSucceeds(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-own")
	ruleID := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")

	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, tcpCIDRRequest(80, "10.0.0.0/24"))))
}

func TestModifySecurityGroupRules_SwapSucceeds(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-swap")
	ruleA := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")
	ruleB := authorizeIngressTCP(t, svc, sgID, 443, "10.0.0.0/24")

	require.NoError(t, modifyRules(svc, sgID,
		ruleUpdate(ruleA, tcpCIDRRequest(443, "10.0.0.0/24")),
		ruleUpdate(ruleB, tcpCIDRRequest(80, "10.0.0.0/24")),
	))
	assert.Equal(t, int64(443), aws.Int64Value(sgRuleByID(t, svc, testAccountID, ruleA).FromPort))
	assert.Equal(t, int64(80), aws.Int64Value(sgRuleByID(t, svc, testAccountID, ruleB).FromPort))
}

func TestModifySecurityGroupRules_TwoUpdatesSameContent(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-same-content")
	ruleA := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")
	ruleB := authorizeIngressTCP(t, svc, sgID, 443, "10.0.0.0/24")

	err := modifyRules(svc, sgID,
		ruleUpdate(ruleA, tcpCIDRRequest(8080, "10.0.0.0/24")),
		ruleUpdate(ruleB, tcpCIDRRequest(8080, "10.0.0.0/24")),
	)
	requireAWSCode(t, err, awserrors.ErrorInvalidParameterValue)
	assert.ErrorContains(t, err, "The same permission must not appear multiple times")
}

func TestModifySecurityGroupRules_OneBadUpdateAppliesNothing(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-atomic")
	ruleA := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")
	ruleB := authorizeIngressTCP(t, svc, sgID, 443, "10.0.0.0/24")
	before := storedSGRecord(t, svc, sgID)

	requireAWSCode(t, modifyRules(svc, sgID,
		ruleUpdate(ruleA, tcpCIDRRequest(8080, "10.0.0.0/24")),
		ruleUpdate(ruleB, tcpCIDRRequest(8443, "10.0.0.1/24")),
	), awserrors.ErrorInvalidParameterValue)
	assert.Equal(t, before, storedSGRecord(t, svc, sgID), "no update in a failed request may be applied")
}

func TestModifySecurityGroupRules_PeerValidation(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	otherVPC := createTestVPC(t, svc, "10.1.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-peer")
	peer := createTestSG(t, svc, vpcID, "mod-peer-same")
	foreign := createTestSG(t, svc, otherVPC, "mod-peer-foreign")
	ruleID := authorizeIngressPeer(t, svc, sgID, 22, peer)

	for peerID, code := range map[string]string{
		foreign:                              awserrors.ErrorInvalidGroupNotFound,
		"sg-00000000000000000":               awserrors.ErrorInvalidGroupNotFound,
		"sg-xyz":                             awserrors.ErrorInvalidGroupIdMalformed,
		testAccountID + "/" + peer:           awserrors.ErrorInvalidGroupIdMalformed,
		testAccountID + "/sg-xyz":            awserrors.ErrorInvalidGroupIdMalformed,
		"sg-0123456789abcdef0123456789abcde": awserrors.ErrorInvalidGroupIdMalformed,
	} {
		err := modifyRules(svc, sgID, ruleUpdate(ruleID, tcpPeerRequest(22, peerID)))
		got, _ := awserrors.ResolveErrorCode(err)
		assert.Equal(t, code, got, "peer %q: %v", peerID, err)
	}

	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, tcpPeerRequest(22, sgID))), "a self-reference is allowed")
	assert.Equal(t, sgID, aws.StringValue(sgRuleByID(t, svc, testAccountID, ruleID).ReferencedGroupInfo.GroupId))
}

func TestSGRuleIDLookupError(t *testing.T) {
	t.Parallel()
	for id, code := range map[string]string{
		"sgr-xyz":                awserrors.ErrorInvalidSecurityGroupRuleIdNotFound,
		"sgr-0123-4567":          awserrors.ErrorInvalidSecurityGroupRuleIdNotFound,
		"sgr-":                   awserrors.ErrorInvalidSecurityGroupRuleIdNotFound,
		"sgr-0123456789abcdef0":  awserrors.ErrorInvalidSecurityGroupRuleIdNotFound,
		"foo":                    awserrors.ErrorInvalidSecurityGroupRuleIdMalformed,
		"":                       awserrors.ErrorInvalidSecurityGroupRuleIdMalformed,
		"SGR-0123456789abcdef0":  awserrors.ErrorInvalidSecurityGroupRuleIdMalformed,
		"sgr-0123456789abcdef01": awserrors.ErrorInvalidSecurityGroupRuleIdMalformed,
	} {
		requireAWSCode(t, sgRuleIDLookupError(id), code)
	}
}

func TestModifySecurityGroupRules_RuleIDResolution(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-ids")
	otherSG := createTestSG(t, svc, vpcID, "mod-ids-other")
	ruleID := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")
	otherRule := authorizeIngressTCP(t, svc, otherSG, 80, "10.0.0.0/24")

	requireAWSCode(t, modifyRules(svc, sgID, ruleUpdate("sgr-00000000000000000", tcpCIDRRequest(81, "10.0.0.0/24"))),
		awserrors.ErrorInvalidSecurityGroupRuleIdNotFound)
	requireAWSCode(t, modifyRules(svc, sgID, ruleUpdate("sgr-xyz", tcpCIDRRequest(81, "10.0.0.0/24"))),
		awserrors.ErrorInvalidSecurityGroupRuleIdNotFound)
	requireAWSCode(t, modifyRules(svc, sgID, ruleUpdate(otherRule, tcpCIDRRequest(81, "10.0.0.0/24"))),
		awserrors.ErrorInvalidSecurityGroupRuleIdNotFound)
	requireAWSCode(t, modifyRules(svc, sgID, ruleUpdate("SGR-0123456789abcdef0", tcpCIDRRequest(81, "10.0.0.0/24"))),
		awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)

	err := modifyRules(svc, sgID,
		ruleUpdate(ruleID, tcpCIDRRequest(81, "10.0.0.0/24")),
		ruleUpdate(ruleID, tcpCIDRRequest(82, "10.0.0.0/24")),
	)
	requireAWSCode(t, err, awserrors.ErrorInvalidParameterValue)
	assert.ErrorContains(t, err, "Duplicated security group rule ID")
}

func TestModifySecurityGroupRules_RejectsEmptyRequest(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-empty")

	requireAWSCode(t, modifyRules(svc, sgID), awserrors.ErrorMissingParameter)
	requireAWSCode(t, modifyRules(svc, ""), awserrors.ErrorMissingParameter)
	_, err := svc.ModifySecurityGroupRules(context.Background(), nil, testAccountID)
	requireAWSCode(t, err, awserrors.ErrorMissingParameter)
}

func TestModifySecurityGroupRules_GroupNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)

	requireAWSCode(t, modifyRules(svc, "sg-00000000000000000", ruleUpdate("sgr-00000000000000000", tcpCIDRRequest(80, "10.0.0.0/24"))),
		awserrors.ErrorInvalidGroupNotFound)
}

func TestSGRuleRequestToSGRule_Rejections(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		req  *ec2.SecurityGroupRuleRequest
		code string
	}{
		"no body":          {nil, awserrors.ErrorInvalidParameterValue},
		"omitted protocol": {&ec2.SecurityGroupRuleRequest{CidrIpv4: aws.String("10.0.0.0/24")}, awserrors.ErrorInvalidParameterValue},
		"unknown protocol": {&ec2.SecurityGroupRuleRequest{IpProtocol: aws.String("gre"), CidrIpv4: aws.String("10.0.0.0/24")}, awserrors.ErrorInvalidParameterValue},
		"tcp without ports": {&ec2.SecurityGroupRuleRequest{
			IpProtocol: aws.String("tcp"), CidrIpv4: aws.String("10.0.0.0/24"),
		}, awserrors.ErrorInvalidParameterValue},
		"udp with one port": {&ec2.SecurityGroupRuleRequest{
			IpProtocol: aws.String("udp"), FromPort: aws.Int64(53), CidrIpv4: aws.String("10.0.0.0/24"),
		}, awserrors.ErrorInvalidParameterValue},
		"all protocols with 0/0": {&ec2.SecurityGroupRuleRequest{
			IpProtocol: aws.String("-1"), FromPort: aws.Int64(0), ToPort: aws.Int64(0), CidrIpv4: aws.String("10.0.0.0/24"),
		}, awserrors.ErrorInvalidParameterValue},
		"all protocols with 22/22": {&ec2.SecurityGroupRuleRequest{
			IpProtocol: aws.String("-1"), FromPort: aws.Int64(22), ToPort: aws.Int64(22), CidrIpv4: aws.String("10.0.0.0/24"),
		}, awserrors.ErrorInvalidParameterValue},
		"no source": {&ec2.SecurityGroupRuleRequest{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int64(22), ToPort: aws.Int64(22),
		}, awserrors.ErrorMissingParameter},
		"two sources": {&ec2.SecurityGroupRuleRequest{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int64(22), ToPort: aws.Int64(22),
			CidrIpv4: aws.String("10.0.0.0/24"), ReferencedGroupId: aws.String("sg-0123456789abcdef0"),
		}, awserrors.ErrorInvalidParameterCombination},
		"prefix list": {&ec2.SecurityGroupRuleRequest{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int64(22), ToPort: aws.Int64(22),
			PrefixListId: aws.String("pl-0123456789abcdef0"),
		}, awserrors.ErrorInvalidPrefixListIDNotFound},
		"non-canonical cidr": {tcpCIDRRequest(22, "10.0.0.1/24"), awserrors.ErrorInvalidParameterValue},
		"unparseable cidr":   {tcpCIDRRequest(22, "10.0.0.0/33"), awserrors.ErrorInvalidParameterValue},
		"ipv6 in CidrIpv4":   {tcpCIDRRequest(22, "2001:db8::/32"), awserrors.ErrorInvalidParameterValue},
	} {
		_, err := sgRuleRequestToSGRule(tc.req)
		require.Error(t, err, name)
		got, _ := awserrors.ResolveErrorCode(err)
		assert.Equal(t, tc.code, got, "%s: %v", name, err)
	}
}

func TestSGRuleRequestToSGRule_Normalisation(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		req  *ec2.SecurityGroupRuleRequest
		want SGRule
	}{
		"all protocols without ports": {
			&ec2.SecurityGroupRuleRequest{IpProtocol: aws.String("-1"), CidrIpv4: aws.String("10.0.0.0/24")},
			SGRule{IpProtocol: "-1", CidrIp: "10.0.0.0/24"},
		},
		"all protocols with -1/-1": {
			&ec2.SecurityGroupRuleRequest{IpProtocol: aws.String("-1"), FromPort: aws.Int64(-1), ToPort: aws.Int64(-1), CidrIpv4: aws.String("10.0.0.0/24")},
			SGRule{IpProtocol: "-1", CidrIp: "10.0.0.0/24"},
		},
		"numeric tcp": {
			&ec2.SecurityGroupRuleRequest{IpProtocol: aws.String("6"), FromPort: aws.Int64(22), ToPort: aws.Int64(22), CidrIpv4: aws.String("10.0.0.0/24")},
			SGRule{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/24"},
		},
		"description carried": {
			&ec2.SecurityGroupRuleRequest{IpProtocol: aws.String("icmp"), FromPort: aws.Int64(8), ToPort: aws.Int64(0), CidrIpv6: aws.String("2001:db8::/32"), Description: aws.String("ping")},
			SGRule{IpProtocol: "icmp", FromPort: 8, CidrIpv6: "2001:db8::/32", Description: "ping"},
		},
	} {
		got, err := sgRuleRequestToSGRule(tc.req)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, got, name)
	}
}

// A modified -1 rule is stored as 0/0 and reported as -1/-1, like authorize's
// no-ports form.
func TestModifySecurityGroupRules_AllProtocolStoresZeroPorts(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-all-proto")
	ruleID := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")

	require.NoError(t, modifyRules(svc, sgID, ruleUpdate(ruleID, &ec2.SecurityGroupRuleRequest{
		IpProtocol: aws.String("-1"), FromPort: aws.Int64(-1), ToPort: aws.Int64(-1),
		CidrIpv4: aws.String("10.0.0.0/24"),
	})))

	stored := storedSGRule(t, svc, sgID, ruleID)
	assert.Equal(t, int64(0), stored.FromPort)
	assert.Equal(t, int64(0), stored.ToPort)
	got := sgRuleByID(t, svc, testAccountID, ruleID)
	assert.Equal(t, int64(-1), aws.Int64Value(got.FromPort))
	assert.Equal(t, int64(-1), aws.Int64Value(got.ToPort))
}

func TestModifySecurityGroupRules_VpcdFailureFailsCall(t *testing.T) {
	t.Parallel()
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-vpcd-fail")
	ruleID := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")

	failingUpdateResponder(t, nc, "ovn unavailable")

	err := modifyRules(svc, sgID, ruleUpdate(ruleID, tcpCIDRRequest(81, "10.0.0.0/24")))
	require.Error(t, err, "a vpc.update-sg failure must fail the call")
	assert.ErrorContains(t, err, "ovn unavailable")
}

// TestModifySecurityGroupRules_ConcurrentAuthorize races a modify against an
// authorize on the same group. The compare-and-swap makes one of them fail
// loudly; neither may silently drop the other's write.
func TestModifySecurityGroupRules_ConcurrentAuthorize(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "mod-concurrent")
	ruleID := authorizeIngressTCP(t, svc, sgID, 80, "10.0.0.0/24")

	var wg sync.WaitGroup
	var modErr, authErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		modErr = modifyRules(svc, sgID, ruleUpdate(ruleID, tcpCIDRRequest(8080, "10.0.0.0/24")))
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

	var modified, authorized bool
	for _, r := range describeGroupRules(t, svc, sgID) {
		if aws.StringValue(r.SecurityGroupRuleId) == ruleID && aws.Int64Value(r.FromPort) == 8080 {
			modified = true
		}
		if aws.Int64Value(r.FromPort) == 8443 {
			authorized = true
		}
	}
	assert.Equal(t, modErr == nil, modified, "the modify succeeded iff its effect is stored")
	assert.Equal(t, authErr == nil, authorized, "the authorize succeeded iff its effect is stored")
}
