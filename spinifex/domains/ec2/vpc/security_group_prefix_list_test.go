//test:in-package — builds on the package's unexported SG fixtures
//(setupTestVPCServiceWithNC, createTestSG, modifyRules) and the configured
//Region, which no exported API returns.

package vpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	networkv1 "github.com/mulgadc/spinifex/contracts/network/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLevel is the feature-level admission: whether level 2 is active, and
// what each admitted write held.
type fakeLevel struct {
	mu       sync.Mutex
	active   bool
	err      error
	inFlight int
	admits   int
	onDone   func()
}

func (f *fakeLevel) admit(context.Context) (func(), bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight++
	f.admits++
	var once sync.Once
	return func() {
		once.Do(func() {
			if f.onDone != nil {
				f.onDone()
			}
			f.mu.Lock()
			f.inFlight--
			f.mu.Unlock()
		})
	}, f.active, f.err
}

func (f *fakeLevel) held() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight
}

func activatePrefixLists(svc *VPCServiceImpl) *fakeLevel {
	f := &fakeLevel{active: true}
	svc.SetPrefixListAdmission(f.admit)
	return f
}

// s3Permission is a tcp permission naming the test Region's S3 prefix list.
func s3Permission(svc *VPCServiceImpl, port int64, description string) *ec2.IpPermission {
	pl := &ec2.PrefixListId{PrefixListId: aws.String(prefixlist.ID(svc.region()))}
	if description != "" {
		pl.Description = aws.String(description)
	}
	return &ec2.IpPermission{
		IpProtocol:    aws.String("tcp"),
		FromPort:      aws.Int64(port),
		ToPort:        aws.Int64(port),
		PrefixListIds: []*ec2.PrefixListId{pl},
	}
}

func describeRules(t *testing.T, svc *VPCServiceImpl, sgID string) []*ec2.SecurityGroupRule {
	t.Helper()
	out, err := svc.DescribeSecurityGroupRules(context.Background(), &ec2.DescribeSecurityGroupRulesInput{
		Filters: []*ec2.Filter{{Name: aws.String("group-id"), Values: []*string{aws.String(sgID)}}},
	}, testAccountID)
	require.NoError(t, err)
	return out.SecurityGroupRules
}

// prefixListRules returns the rules naming a prefix list, by direction.
func prefixListRules(t *testing.T, svc *VPCServiceImpl, sgID string, egress bool) []*ec2.SecurityGroupRule {
	t.Helper()
	var out []*ec2.SecurityGroupRule
	for _, r := range describeRules(t, svc, sgID) {
		if r.PrefixListId != nil && aws.BoolValue(r.IsEgress) == egress {
			out = append(out, r)
		}
	}
	return out
}

func TestPrefixListRule_AuthorizeRendersAsAWSDoes(t *testing.T) {
	t.Parallel()
	svc, nc := setupTestVPCServiceWithNC(t)
	activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-render")
	pid := prefixlist.ID(svc.region())

	events := make(chan *nats.Msg, 4)
	sub, err := nc.ChanSubscribe(networkv1.SecurityGroupUpdateSubject, events)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	out, err := svc.AuthorizeSecurityGroupEgress(context.Background(), &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "s3")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SecurityGroupRules, 1)
	got := out.SecurityGroupRules[0]
	assert.Equal(t, pid, aws.StringValue(got.PrefixListId))
	assert.Equal(t, "s3", aws.StringValue(got.Description))
	assert.True(t, aws.BoolValue(got.IsEgress))
	assert.Nil(t, got.CidrIpv4)
	assert.Nil(t, got.ReferencedGroupInfo)

	select {
	case msg := <-events:
		var evt networkv1.SecurityGroupEvent
		require.NoError(t, json.Unmarshal(msg.Data, &evt))
		var named []string
		for _, r := range evt.EgressRules {
			if r.PrefixListId != "" {
				named = append(named, r.PrefixListId)
			}
		}
		assert.Equal(t, []string{pid}, named, "vpcd must be told which list the rule names")
	case <-time.After(5 * time.Second):
		t.Fatal("no vpc.update-sg event published")
	}

	rules := prefixListRules(t, svc, sgID, true)
	require.Len(t, rules, 1)
	assert.Equal(t, aws.StringValue(got.SecurityGroupRuleId), aws.StringValue(rules[0].SecurityGroupRuleId))
	assert.Equal(t, "s3", aws.StringValue(rules[0].Description))

	groups, err := svc.DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{GroupIds: []*string{aws.String(sgID)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, groups.SecurityGroups, 1)
	var plPerms []*ec2.IpPermission
	for _, p := range groups.SecurityGroups[0].IpPermissionsEgress {
		if len(p.PrefixListIds) > 0 {
			plPerms = append(plPerms, p)
		}
	}
	require.Len(t, plPerms, 1)
	assert.Equal(t, []*ec2.PrefixListId{{PrefixListId: aws.String(pid), Description: aws.String("s3")}}, plPerms[0].PrefixListIds)
	assert.Empty(t, plPerms[0].IpRanges, "the prefix list's permission names no CIDR")
	assert.Equal(t, int64(443), aws.Int64Value(plPerms[0].FromPort))
}

func TestPrefixListRule_IngressAndRevokeBothDirections(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-both")
	ctx := context.Background()
	perm := []*ec2.IpPermission{s3Permission(svc, 443, "s3")}

	_, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{GroupId: aws.String(sgID), IpPermissions: perm}, testAccountID)
	require.NoError(t, err)
	_, err = svc.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(sgID), IpPermissions: perm}, testAccountID)
	require.NoError(t, err, "AWS accepts the prefix list as an ingress peer too")
	require.Len(t, prefixListRules(t, svc, sgID, false), 1)

	// Revoke matches by content, not description.
	revoke := []*ec2.IpPermission{s3Permission(svc, 443, "")}
	_, err = svc.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{GroupId: aws.String(sgID), IpPermissions: revoke}, testAccountID)
	require.NoError(t, err)
	_, err = svc.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(sgID), IpPermissions: revoke}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, prefixListRules(t, svc, sgID, true))
	assert.Empty(t, prefixListRules(t, svc, sgID, false))

	_, err = svc.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{GroupId: aws.String(sgID), IpPermissions: revoke}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPermissionNotFound)
}

func TestPrefixListRule_DuplicateIsRefusedAsAWSDescribesIt(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-dup")
	ctx := context.Background()
	in := &ec2.AuthorizeSecurityGroupEgressInput{GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "s3")}}

	_, err := svc.AuthorizeSecurityGroupEgress(ctx, in, testAccountID)
	require.NoError(t, err)
	_, err = svc.AuthorizeSecurityGroupEgress(ctx, in, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPermissionDuplicate)
	assert.Contains(t, err.Error(), `peer: `+prefixlist.ID(svc.region())+`, TCP, from port: 443, to port: 443, ALLOW`)
}

// Only the S3 list exists here; every other ID is unknown, whatever its shape.
func TestPrefixListRule_UnknownListIsNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-unknown")
	ctx := context.Background()

	for _, id := range []string{"pl-0123456789abcdef0", prefixlist.ID("eu-west-1"), "pl-xyz"} {
		_, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
			GroupId: aws.String(sgID),
			IpPermissions: []*ec2.IpPermission{{
				IpProtocol: aws.String("tcp"), FromPort: aws.Int64(80), ToPort: aws.Int64(80),
				PrefixListIds: []*ec2.PrefixListId{{PrefixListId: aws.String(id)}},
			}},
		}, testAccountID)
		require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound, id)
		assert.Contains(t, err.Error(), "The prefix list ID '"+id+"' does not exist")
	}
	assert.Empty(t, prefixListRules(t, svc, sgID, true), "a refused authorize stores nothing")
}

// A permission naming a CIDR and the prefix list creates both rules; the list
// was once dropped without a word.
func TestPrefixListRule_BesideACIDRCreatesBoth(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-and-cidr")

	perm := s3Permission(svc, 8443, "")
	perm.IpRanges = []*ec2.IpRange{{CidrIp: aws.String("10.9.0.0/16")}}
	out, err := svc.AuthorizeSecurityGroupEgress(context.Background(), &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{perm},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.SecurityGroupRules, 2)
	assert.Equal(t, "10.9.0.0/16", aws.StringValue(out.SecurityGroupRules[0].CidrIpv4))
	assert.Equal(t, prefixlist.ID(svc.region()), aws.StringValue(out.SecurityGroupRules[1].PrefixListId))
	require.Len(t, prefixListRules(t, svc, sgID, true), 1)
}

func TestPrefixListRule_Modify(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-modify")
	pid := prefixlist.ID(svc.region())
	ctx := context.Background()

	out, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "s3")},
	}, testAccountID)
	require.NoError(t, err)
	plRule := aws.StringValue(out.SecurityGroupRules[0].SecurityGroupRuleId)
	cidrRule := authorizeIngressTCP(t, svc, sgID, 22, "10.0.0.0/24")

	update := func(id string, req *ec2.SecurityGroupRuleRequest) error {
		return modifyRules(svc, sgID, &ec2.SecurityGroupRuleUpdate{SecurityGroupRuleId: aws.String(id), SecurityGroupRule: req})
	}
	require.NoError(t, update(plRule, &ec2.SecurityGroupRuleRequest{
		IpProtocol: aws.String("tcp"), FromPort: aws.Int64(8443), ToPort: aws.Int64(8443), PrefixListId: aws.String(pid),
	}))
	rules := prefixListRules(t, svc, sgID, true)
	require.Len(t, rules, 1)
	assert.Equal(t, plRule, aws.StringValue(rules[0].SecurityGroupRuleId))
	assert.Equal(t, int64(8443), aws.Int64Value(rules[0].FromPort))

	err = update(plRule, &ec2.SecurityGroupRuleRequest{
		IpProtocol: aws.String("tcp"), FromPort: aws.Int64(443), ToPort: aws.Int64(443), PrefixListId: aws.String("pl-0123456789abcdef0"),
	})
	require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound)

	err = update(cidrRule, &ec2.SecurityGroupRuleRequest{
		IpProtocol: aws.String("tcp"), FromPort: aws.Int64(22), ToPort: aws.Int64(22), PrefixListId: aws.String(pid),
	})
	require.ErrorContains(t, err, awserrors.ErrorInvalidParameterValue)
	assert.Contains(t, err.Error(), "You may not specify PrefixListId for an existing CidrIpv4 rule")
}

func TestPrefixListRule_DescriptionByPermission(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-describe")
	ctx := context.Background()

	_, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "before")},
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.UpdateSecurityGroupRuleDescriptionsEgress(ctx, &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "after")},
	}, testAccountID)
	require.NoError(t, err)
	rules := prefixListRules(t, svc, sgID, true)
	require.Len(t, rules, 1)
	assert.Equal(t, "after", aws.StringValue(rules[0].Description))

	_, err = svc.UpdateSecurityGroupRuleDescriptionsEgress(ctx, &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 80, "none")},
	}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPermissionNotFound)
}

// As AWS does, a revoke naming a prefix-list rule by its old description after
// a rename is not found and leaves the rule; the current description revokes.
func TestPrefixListRule_RevokeWithAStaleDescriptionIsNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-stale")
	ctx := context.Background()

	_, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "s3")},
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.UpdateSecurityGroupRuleDescriptionsEgress(ctx, &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "s3 renamed")},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "s3")},
	}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPermissionNotFound)
	require.Len(t, storedEgressPrefixListRules(t, svc, sgID), 1, "a stale revoke leaves the rule")

	_, err = svc.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "s3 renamed")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, storedEgressPrefixListRules(t, svc, sgID))
}

// Below level 2 the list does not exist yet: a write adding it answers as an
// unknown list and stores nothing, with or without an admission installed,
// while a write naming no list is not gated.
func TestPrefixListRule_BelowLevelIsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, install := range map[string]bool{"inactive": true, "no admission": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := setupTestVPCService(t)
			level := &fakeLevel{}
			if install {
				svc.SetPrefixListAdmission(level.admit)
			}
			vpcID := createTestVPC(t, svc, "10.0.0.0/16")
			sgID := createTestSG(t, svc, vpcID, "pl-below")
			pid := prefixlist.ID(svc.region())

			_, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
				GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "s3")},
			}, testAccountID)
			require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound)
			assert.Contains(t, err.Error(), "The prefix list ID '"+pid+"' does not exist")
			_, err = svc.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
				GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "s3")},
			}, testAccountID)
			require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound)
			assert.Empty(t, prefixListRules(t, svc, sgID, true))
			assert.Empty(t, prefixListRules(t, svc, sgID, false))

			authorizeIngressTCP(t, svc, sgID, 22, "10.0.0.0/24")
			assert.Zero(t, level.held(), "every admission is released")
		})
	}
}

// An admitted write holds its admission until its record is written, so a
// rollback's in-flight wait covers it; an unreadable level refuses the write.
func TestPrefixListRule_AdmissionHeldUntilWritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := setupTestVPCService(t)
	level := activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-held")
	storedAtRelease := -1
	level.onDone = func() { storedAtRelease = len(storedEgressPrefixListRules(t, svc, sgID)) }

	_, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "s3")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, 1, storedAtRelease, "the admission is released only after the rule is stored")
	assert.Zero(t, level.held())

	level.onDone = nil
	level.err = errors.New("level record unreadable")
	_, err = svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 8443, "s3")},
	}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorServerInternal)
	assert.Len(t, storedEgressPrefixListRules(t, svc, sgID), 1)
	assert.Zero(t, level.held())
}

func storedEgressPrefixListRules(t *testing.T, svc *VPCServiceImpl, sgID string) []SGRule {
	t.Helper()
	entry, err := svc.sgKV.Get(context.Background(), kvstore.AccountKey(testAccountID, sgID))
	require.NoError(t, err)
	var rec SecurityGroupRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	var out []SGRule
	for _, r := range rec.EgressRules {
		if r.PrefixListId != "" {
			out = append(out, r)
		}
	}
	return out
}

// While a rollback below level 2 is pending, a customer can still remove the
// rules it waits on, by permission or by rule ID, and update their
// descriptions; only adding a peer is refused.
func TestPrefixListRule_RemovableDuringRollback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := setupTestVPCService(t)
	level := activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-rollback")
	perm := []*ec2.IpPermission{s3Permission(svc, 443, "s3")}
	_, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{GroupId: aws.String(sgID), IpPermissions: perm}, testAccountID)
	require.NoError(t, err)
	in, err := svc.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{GroupId: aws.String(sgID), IpPermissions: perm}, testAccountID)
	require.NoError(t, err)

	level.mu.Lock()
	level.active = false
	level.mu.Unlock()
	admits := level.admits

	_, err = svc.UpdateSecurityGroupRuleDescriptionsEgress(ctx, &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "renamed")},
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 8443, "")},
	}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound)

	_, err = svc.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "")},
	}, testAccountID)
	require.NoError(t, err, "revoke by permission")
	_, err = svc.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{
		GroupId: aws.String(sgID), SecurityGroupRuleIds: []*string{in.SecurityGroupRules[0].SecurityGroupRuleId},
	}, testAccountID)
	require.NoError(t, err, "revoke by rule ID")
	assert.Empty(t, prefixListRules(t, svc, sgID, true))
	assert.Empty(t, prefixListRules(t, svc, sgID, false))
	assert.Equal(t, admits+1, level.admits, "only the authorize asked for admission")
}

// A revoke naming an unknown list, in either direction, and a permission with
// an empty list ID beside a valid CIDR are refused whole with AWS's code, and
// change nothing.
func TestPrefixListRule_PeersValidatedBeforeAnyMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := setupTestVPCService(t)
	activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-validate")
	_, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "")},
	}, testAccountID)
	require.NoError(t, err)
	before := describeRules(t, svc, sgID)

	unknown := "pl-0123456789abcdef0"
	withUnknown := func() []*ec2.IpPermission {
		p := s3Permission(svc, 443, "")
		p.PrefixListIds = append(p.PrefixListIds, &ec2.PrefixListId{PrefixListId: aws.String(unknown)})
		return []*ec2.IpPermission{p}
	}
	_, err = svc.RevokeSecurityGroupEgress(ctx, &ec2.RevokeSecurityGroupEgressInput{GroupId: aws.String(sgID), IpPermissions: withUnknown()}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound)
	assert.Contains(t, err.Error(), "The prefix list ID '"+unknown+"' does not exist")
	_, err = svc.RevokeSecurityGroupIngress(ctx, &ec2.RevokeSecurityGroupIngressInput{GroupId: aws.String(sgID), IpPermissions: withUnknown()}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound)
	_, err = svc.UpdateSecurityGroupRuleDescriptionsEgress(ctx, &ec2.UpdateSecurityGroupRuleDescriptionsEgressInput{
		GroupId: aws.String(sgID), IpPermissions: withUnknown(),
	}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound)

	empty := &ec2.IpPermission{
		IpProtocol: aws.String("tcp"), FromPort: aws.Int64(9443), ToPort: aws.Int64(9443),
		IpRanges:      []*ec2.IpRange{{CidrIp: aws.String("10.8.0.0/16")}},
		PrefixListIds: []*ec2.PrefixListId{{PrefixListId: aws.String("")}},
	}
	_, err = svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{empty}}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound)
	assert.Contains(t, err.Error(), "The prefix list ID '' does not exist")

	assert.ElementsMatch(t, before, describeRules(t, svc, sgID), "a refused request changes nothing")
}

// A modify naming an unknown list, or the S3 list below the level, is not
// found before its rule ID is matched or its peer type compared.
func TestPrefixListRule_ModifyValidatesPeersBeforeAnyMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := setupTestVPCService(t)
	level := activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-modify-validate")
	out, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int64(443), ToPort: aws.Int64(443),
			IpRanges: []*ec2.IpRange{{CidrIp: aws.String("10.8.0.0/16")}},
		}},
	}, testAccountID)
	require.NoError(t, err)
	cidrRuleID := out.SecurityGroupRules[0].SecurityGroupRuleId
	before := describeRules(t, svc, sgID)

	unknown := "pl-0123456789abcdef0"
	for name, ruleID := range map[string]*string{
		"an existing CIDR rule": cidrRuleID,
		"an unknown rule ID":    aws.String("sgr-0123456789abcdef0"),
	} {
		_, err := svc.ModifySecurityGroupRules(ctx, &ec2.ModifySecurityGroupRulesInput{
			GroupId: aws.String(sgID),
			SecurityGroupRules: []*ec2.SecurityGroupRuleUpdate{{
				SecurityGroupRuleId: ruleID,
				SecurityGroupRule: &ec2.SecurityGroupRuleRequest{
					IpProtocol: aws.String("tcp"), FromPort: aws.Int64(443), ToPort: aws.Int64(443),
					PrefixListId: aws.String(unknown),
				},
			}},
		}, testAccountID)
		require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound, name)
		assert.Contains(t, err.Error(), "The prefix list ID '"+unknown+"' does not exist", name)
	}

	level.active = false
	s3 := prefixlist.ID(svc.region())
	_, err = svc.ModifySecurityGroupRules(ctx, &ec2.ModifySecurityGroupRulesInput{
		GroupId: aws.String(sgID),
		SecurityGroupRules: []*ec2.SecurityGroupRuleUpdate{{
			SecurityGroupRuleId: cidrRuleID,
			SecurityGroupRule: &ec2.SecurityGroupRuleRequest{
				IpProtocol: aws.String("tcp"), FromPort: aws.Int64(443), ToPort: aws.Int64(443), PrefixListId: aws.String(s3),
			},
		}},
	}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorInvalidPrefixListIDNotFound, "below the level the S3 list does not exist either")
	assert.Contains(t, err.Error(), "The prefix list ID '"+s3+"' does not exist")
	assert.ElementsMatch(t, before, describeRules(t, svc, sgID), "a refused modify changes nothing")
}

// A modify that sets a prefix-list peer is admitted like an authorize: below
// level 2 it is not found and changes nothing, and while active the admission
// is released only after the new body is stored.
func TestPrefixListRule_ModifyIsAdmittedUntilWritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := setupTestVPCService(t)
	level := activatePrefixLists(svc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	sgID := createTestSG(t, svc, vpcID, "pl-modify-held")
	out, err := svc.AuthorizeSecurityGroupEgress(ctx, &ec2.AuthorizeSecurityGroupEgressInput{
		GroupId: aws.String(sgID), IpPermissions: []*ec2.IpPermission{s3Permission(svc, 443, "")},
	}, testAccountID)
	require.NoError(t, err)
	modify := func(port int64) error {
		_, err := svc.ModifySecurityGroupRules(ctx, &ec2.ModifySecurityGroupRulesInput{
			GroupId: aws.String(sgID),
			SecurityGroupRules: []*ec2.SecurityGroupRuleUpdate{{
				SecurityGroupRuleId: out.SecurityGroupRules[0].SecurityGroupRuleId,
				SecurityGroupRule: &ec2.SecurityGroupRuleRequest{
					IpProtocol: aws.String("tcp"), FromPort: aws.Int64(port), ToPort: aws.Int64(port),
					PrefixListId: aws.String(prefixlist.ID(svc.region())),
				},
			}},
		}, testAccountID)
		return err
	}

	level.active = false
	require.ErrorContains(t, modify(8443), awserrors.ErrorInvalidPrefixListIDNotFound)
	stored := storedEgressPrefixListRules(t, svc, sgID)
	require.Len(t, stored, 1)
	assert.Equal(t, int64(443), stored[0].FromPort, "a refused modify changes nothing")
	assert.Zero(t, level.held())

	level.active = true
	storedAtRelease := int64(-1)
	level.onDone = func() { storedAtRelease = storedEgressPrefixListRules(t, svc, sgID)[0].FromPort }
	require.NoError(t, modify(8443))
	assert.Equal(t, int64(8443), storedAtRelease, "the admission is released only after the new body is stored")
	assert.Zero(t, level.held())
}
