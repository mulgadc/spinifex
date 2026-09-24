package handlers_ec2_vpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// sgIDRegex must stay in lockstep with utils.GenerateResourceID("sg").
var sgIDRegex = regexp.MustCompile(`^sg-[0-9a-f]{17}$`)

// SGRuleIDRegex must stay in lockstep with utils.GenerateResourceID("sgr").
// Exported so the EC2 gateway can validate SecurityGroupRuleIds without
// re-implementing the format check.
var SGRuleIDRegex = regexp.MustCompile(`^sgr-[0-9a-f]{17}$`)

// allProtocols is the IpProtocol value naming every protocol, which is also
// the port value AWS reports for such a rule.
const allProtocols = "-1"

// canonicalIPProtocols are the only values the OVN ACL builder can express.
// Anything else must be rejected: the builder emits no L4 predicate for an
// unknown protocol, which widens the rule to every IP protocol.
var canonicalIPProtocols = []string{"tcp", "udp", "icmp", "-1"}

// numericIPProtocols maps the IANA numbers AWS accepts onto the canonical
// names. 58 (ICMPv6) is absent deliberately: the ACL builder is IPv4-only.
var numericIPProtocols = map[string]string{"1": "icmp", "6": "tcp", "17": "udp"}

// normalizeIPProtocol canonicalises an AWS IpProtocol value the way AWS does,
// returning "tcp" for both "tcp" and "6". An empty value means all protocols.
func normalizeIPProtocol(proto string) (string, error) {
	proto = strings.ToLower(strings.TrimSpace(proto))
	if proto == "" {
		return "-1", nil
	}
	if name, ok := numericIPProtocols[proto]; ok {
		return name, nil
	}
	if slices.Contains(canonicalIPProtocols, proto) {
		return proto, nil
	}
	return "", fmt.Errorf("invalid IpProtocol %q: supported values are tcp, udp, icmp, -1 (or 6, 17, 1)", proto)
}

// validateSGRule rejects CidrIp values that are non-canonical or IPv6, CidrIpv6
// values that are non-canonical or IPv4, IpProtocol values the ACL builder
// cannot express, and SourceSG values not matching the sg-ID format. At least
// one source must be specified. The two CIDR fields stay strictly separated so
// that address family remains part of a rule's identity.
func validateSGRule(r SGRule) error {
	if r.CidrIp == "" && r.CidrIpv6 == "" && r.SourceSG == "" {
		return errors.New("rule must specify CidrIp, CidrIpv6 or SourceSG")
	}
	if !slices.Contains(canonicalIPProtocols, r.IpProtocol) {
		return fmt.Errorf("invalid IpProtocol %q: must be one of %v", r.IpProtocol, canonicalIPProtocols)
	}
	if r.CidrIp != "" {
		_, ipnet, err := net.ParseCIDR(r.CidrIp)
		if err != nil {
			return fmt.Errorf("invalid CidrIp %q: %w", r.CidrIp, err)
		}
		if ipnet.IP.To4() == nil {
			return fmt.Errorf("invalid CidrIp %q: IPv6 belongs in CidrIpv6", r.CidrIp)
		}
		if ipnet.String() != r.CidrIp {
			return fmt.Errorf("invalid CidrIp %q: not canonical (expected %q)", r.CidrIp, ipnet.String())
		}
	}
	if r.CidrIpv6 != "" {
		_, ipnet, err := net.ParseCIDR(r.CidrIpv6)
		if err != nil {
			return fmt.Errorf("invalid CidrIpv6 %q: %w", r.CidrIpv6, err)
		}
		if ipnet.IP.To4() != nil {
			return fmt.Errorf("invalid CidrIpv6 %q: IPv4 belongs in CidrIp", r.CidrIpv6)
		}
		if ipnet.String() != r.CidrIpv6 {
			return fmt.Errorf("invalid CidrIpv6 %q: not canonical (expected %q)", r.CidrIpv6, ipnet.String())
		}
	}
	if r.SourceSG != "" && !sgIDRegex.MatchString(r.SourceSG) {
		return fmt.Errorf("invalid SourceSG %q: must match sg-<17 hex chars>", r.SourceSG)
	}
	return nil
}

const (
	KVBucketSecurityGroups        = "spinifex-vpc-security-groups"
	KVBucketSecurityGroupsVersion = 2

	// AWS defaults: 2,500 SGs per VPC, 60 inbound + 60 outbound rules per SG.
	maxSGsPerVPC      = 2500
	maxRulesPerSGSide = 60

	// defaultSecurityGroupName is the reserved name AWS uses for the per-VPC
	// default SG. Public CreateSecurityGroup rejects it; the internal helper
	// invoked from CreateVpc/EnsureDefaultVPC bypasses the guard.
	defaultSecurityGroupName        = "default"
	defaultSecurityGroupDescription = "default VPC security group"
)

// SGRule represents a single ingress or egress rule in a security group.
// RuleId is assigned on Authorize and excluded from sgRuleKey so
// duplicate-content rules are rejected regardless of ID.
type SGRule struct {
	RuleId     string `json:"rule_id"`     // sgr-<17 hex>; assigned on Authorize, backfilled by migration
	IpProtocol string `json:"ip_protocol"` // "tcp", "udp", "icmp", "-1" (all)
	FromPort   int64  `json:"from_port"`
	ToPort     int64  `json:"to_port"`
	CidrIp     string `json:"cidr_ip,omitempty"`
	// CidrIpv6 is stored and projected but never reaches the ACL builder: no
	// interface here has an IPv6 address, so the rule matches nothing, which is
	// what an IPv6 rule does on AWS in a VPC with no IPv6 CIDR.
	CidrIpv6 string `json:"cidr_ipv6,omitempty"`
	SourceSG string `json:"source_sg,omitempty"` // Another SG ID for intra-SG rules
	// Description is rule metadata, not identity: it is excluded from sgRuleKey
	// so revoke/duplicate matching ignores it. The AWS Load Balancer Controller
	// tags the node-SG rules it manages here and revokes them by this value.
	Description string `json:"description,omitempty"`
	// Tags are excluded from sgRuleKey for the same reason as Description: two
	// rules with identical permissions are the same rule whatever they are
	// labelled.
	Tags map[string]string `json:"tags,omitempty"`
}

// SecurityGroupRecord represents a stored security group.
type SecurityGroupRecord struct {
	GroupId      string            `json:"group_id"`
	GroupName    string            `json:"group_name"`
	Description  string            `json:"description"`
	VpcId        string            `json:"vpc_id"`
	IngressRules []SGRule          `json:"ingress_rules"`
	EgressRules  []SGRule          `json:"egress_rules"`
	Tags         map[string]string `json:"tags"`
	IsDefault    bool              `json:"is_default,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
}

// SGEvent is published on vpc.create-sg / vpc.delete-sg / vpc.update-sg for vpcd consumption.
type SGEvent struct {
	GroupId      string   `json:"group_id"`
	VpcId        string   `json:"vpc_id"`
	IngressRules []SGRule `json:"ingress_rules,omitempty"`
	EgressRules  []SGRule `json:"egress_rules,omitempty"`
}

// CreateSecurityGroup creates a new security group in a VPC.
func (s *VPCServiceImpl) CreateSecurityGroup(ctx context.Context, input *ec2.CreateSecurityGroupInput, accountID string) (*ec2.CreateSecurityGroupOutput, error) {
	if input.GroupName == nil || *input.GroupName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	vpcId := *input.VpcId
	groupName := *input.GroupName

	// "default" is reserved for the per-VPC default SG that CreateVpc
	// provisions internally. Matches AWS behavior.
	if groupName == defaultSecurityGroupName {
		return nil, errors.New(awserrors.ErrorInvalidGroupReserved)
	}

	if err := s.requireVPCExists(ctx, accountID, vpcId); err != nil {
		return nil, err
	}

	// Check for duplicate group name in the same VPC and enforce the per-VPC
	// SG quota in the same bucket walk.
	prefix := accountID + "."
	sgKeys, err := s.sgKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	sgsInVPC := 0
	for _, k := range sgKeys {
		if k == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.sgKV.Get(ctx, k)
		if err != nil {
			// Fail closed — a transient read error must not let a duplicate
			// SG name slip past, nor undercount the per-VPC quota.
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			slog.WarnContext(ctx, "CreateSecurityGroup: SG read failed", "key", k, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		var existing SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &existing); err != nil {
			slog.WarnContext(ctx, "CreateSecurityGroup: SG unmarshal failed", "key", k, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if existing.VpcId != vpcId {
			continue
		}
		sgsInVPC++
		if existing.GroupName == groupName {
			return nil, errors.New(awserrors.ErrorInvalidGroupDuplicate)
		}
	}
	if sgsInVPC >= maxSGsPerVPC {
		return nil, errors.New(awserrors.ErrorResourceLimitExceeded)
	}

	groupId := utils.GenerateResourceID("sg")

	description := ""
	if input.Description != nil {
		description = *input.Description
	}

	// Default egress rule: allow all outbound traffic
	defaultEgress := []SGRule{
		{RuleId: utils.GenerateResourceID("sgr"), IpProtocol: "-1", FromPort: 0, ToPort: 0, CidrIp: "0.0.0.0/0"},
	}

	record := SecurityGroupRecord{
		GroupId:      groupId,
		GroupName:    groupName,
		Description:  description,
		VpcId:        vpcId,
		IngressRules: []SGRule{},
		EgressRules:  defaultEgress,
		Tags:         utils.ExtractTags(input.TagSpecifications, "security-group"),
		CreatedAt:    time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal security group record: %w", err)
	}
	if _, err := s.sgKV.Put(ctx, utils.AccountKey(accountID, groupId), data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "CreateSecurityGroup completed", "groupId", groupId, "groupName", groupName, "vpcId", vpcId, "accountID", accountID)

	s.projectRecordTags(ctx, accountID, groupId, record.Tags)

	if err := s.requestSGEvent("vpc.create-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        vpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.ErrorContext(ctx, "CreateSecurityGroup: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.CreateSecurityGroupOutput{
		GroupId: aws.String(groupId),
	}, nil
}

// DeleteSecurityGroup deletes a security group.
func (s *VPCServiceImpl) DeleteSecurityGroup(ctx context.Context, input *ec2.DeleteSecurityGroupInput, accountID string) (*ec2.DeleteSecurityGroupOutput, error) {
	if input.GroupId == nil || *input.GroupId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(ctx, key)
	if err != nil {
		// AWS-faithful: an absent security group is InvalidGroup.NotFound, not
		// success. Destroy orchestration tolerates it via awserrors.IsNotFound;
		// a transient read error stays a server error.
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if record.IsDefault {
		return nil, errors.New(awserrors.ErrorCannotDelete)
	}

	if err := s.checkSGDependencies(ctx, accountID, groupId); err != nil {
		return nil, err
	}

	if err := s.sgKV.Delete(ctx, key); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	s.clearRecordTags(ctx, accountID, groupId)

	slog.InfoContext(ctx, "DeleteSecurityGroup completed", "groupId", groupId, "accountID", accountID)

	if err := s.requestSGEvent("vpc.delete-sg", SGEvent{
		GroupId: groupId,
		VpcId:   record.VpcId,
	}); err != nil {
		slog.ErrorContext(ctx, "DeleteSecurityGroup: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.DeleteSecurityGroupOutput{}, nil
}

// validateSGRuleReferences returns InvalidGroup.NotFound if any SourceSG in
// rules is missing or belongs to a different VPC (cross-VPC refs not
// supported; AWS uses the same error for both cases).
func (s *VPCServiceImpl) validateSGRuleReferences(ctx context.Context, accountID, ownerVpcId string, rules []SGRule) error {
	for _, r := range rules {
		if r.SourceSG == "" {
			continue
		}
		entry, err := s.sgKV.Get(ctx, utils.AccountKey(accountID, r.SourceSG))
		if err != nil {
			return errors.New(awserrors.ErrorInvalidGroupNotFound)
		}
		var rec SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		if rec.VpcId != ownerVpcId {
			return errors.New(awserrors.ErrorInvalidGroupNotFound)
		}
	}
	return nil
}

// checkSGDependencies returns DependencyViolation if the given SG is still
// attached to any ENI in the account or referenced as SourceSG by any other
// SG's rules in the account.
func (s *VPCServiceImpl) checkSGDependencies(ctx context.Context, accountID, groupId string) error {
	prefix := accountID + "."

	eniKeys, err := s.eniKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return errors.New(awserrors.ErrorServerInternal)
	}
	for _, k := range eniKeys {
		if k == utils.VersionKey || !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.eniKV.Get(ctx, k)
		if err != nil {
			// Fail closed — a transient read error must not let us delete an
			// SG that's actually still attached.
			slog.WarnContext(ctx, "checkSGDependencies: ENI read failed", "key", k, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		var eni ENIRecord
		if err := json.Unmarshal(entry.Value(), &eni); err != nil {
			slog.WarnContext(ctx, "checkSGDependencies: ENI unmarshal failed", "key", k, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		if slices.Contains(eni.SecurityGroupIds, groupId) {
			// Name the blocker in the refusal and in the logs: recovering it
			// afterwards otherwise means cross-referencing the whole ENI table.
			slog.WarnContext(ctx, "checkSGDependencies: SG still attached to ENI",
				"groupId", groupId, "eniId", eni.NetworkInterfaceId, "accountID", accountID)
			return awserrors.Errorf(awserrors.ErrorDependencyViolation,
				"the security group has a dependent network interface %s that must be detached first", eni.NetworkInterfaceId)
		}
	}

	sgKeys, err := s.sgKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return errors.New(awserrors.ErrorServerInternal)
	}
	for _, k := range sgKeys {
		if k == utils.VersionKey || !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.sgKV.Get(ctx, k)
		if err != nil {
			slog.WarnContext(ctx, "checkSGDependencies: SG read failed", "key", k, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		var other SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &other); err != nil {
			slog.WarnContext(ctx, "checkSGDependencies: SG unmarshal failed", "key", k, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		if other.GroupId == groupId {
			continue
		}
		for _, r := range other.IngressRules {
			if r.SourceSG == groupId {
				return sgReferencedByError(ctx, accountID, groupId, other.GroupId, "ingress")
			}
		}
		for _, r := range other.EgressRules {
			if r.SourceSG == groupId {
				return sgReferencedByError(ctx, accountID, groupId, other.GroupId, "egress")
			}
		}
	}
	return nil
}

// sgReferencedByError logs and returns the DependencyViolation naming the SG
// whose rule still references groupId, so the blocking id survives even when
// the message does not reach the client.
func sgReferencedByError(ctx context.Context, accountID, groupId, referencingGroupId, direction string) error {
	slog.WarnContext(ctx, "checkSGDependencies: SG referenced by another SG's rule",
		"groupId", groupId, "referencingGroupId", referencingGroupId,
		"direction", direction, "accountID", accountID)
	return awserrors.Errorf(awserrors.ErrorDependencyViolation,
		"the security group is referenced by a %s rule on security group %s that must be revoked first",
		direction, referencingGroupId)
}

// describeSecurityGroupsValidFilters defines the set of filter names accepted by DescribeSecurityGroups.
var describeSecurityGroupsValidFilters = map[string]bool{
	"group-id":           true,
	"group-name":         true,
	"vpc-id":             true,
	"description":        true,
	"ip-permission.cidr": true,
}

// DescribeSecurityGroups lists security groups with optional filters.
func (s *VPCServiceImpl) DescribeSecurityGroups(ctx context.Context, input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error) {
	groups := []*ec2.SecurityGroup{}

	groupIDs := make(map[string]bool)
	for _, id := range input.GroupIds {
		if id != nil {
			groupIDs[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeSecurityGroupsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeSecurityGroups: invalid filter", "err", err)
		return nil, err
	}

	prefix := accountID + "."
	keys, err := s.sgKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.sgKV.Get(ctx, key)
		if err != nil {
			slog.WarnContext(ctx, "Failed to get security group record", "key", key, "error", err)
			continue
		}

		var record SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.WarnContext(ctx, "Failed to unmarshal security group record", "key", key, "error", err)
			continue
		}

		if len(groupIDs) > 0 && !groupIDs[record.GroupId] {
			continue
		}

		if len(parsedFilters) > 0 && !sgMatchesFilters(&record, parsedFilters) {
			continue
		}

		groups = append(groups, s.sgRecordToEC2(&record, accountID))
	}

	// If specific group IDs were requested but not found, return error
	if len(groupIDs) > 0 {
		found := make(map[string]bool)
		for _, sg := range groups {
			if sg.GroupId != nil {
				found[*sg.GroupId] = true
			}
		}
		for id := range groupIDs {
			if !found[id] {
				return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
			}
		}
	}

	slog.InfoContext(ctx, "DescribeSecurityGroups completed", "count", len(groups), "accountID", accountID)

	return &ec2.DescribeSecurityGroupsOutput{
		SecurityGroups: groups,
	}, nil
}

// sgMatchesFilters checks whether a SecurityGroupRecord satisfies all parsed filters.
func sgMatchesFilters(record *SecurityGroupRecord, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		switch name {
		case "group-id":
			if !filterutil.MatchesAny(values, record.GroupId) {
				return false
			}
		case "group-name":
			if !filterutil.MatchesAny(values, record.GroupName) {
				return false
			}
		case "vpc-id":
			if !filterutil.MatchesAny(values, record.VpcId) {
				return false
			}
		case "description":
			if !filterutil.MatchesAny(values, record.Description) {
				return false
			}
		case "ip-permission.cidr":
			if !sgIngressCIDRMatchesAny(record.IngressRules, values) {
				return false
			}
		default:
			return false
		}
	}

	return filterutil.MatchesTags(filters, record.Tags)
}

// sgIngressCIDRMatchesAny checks if any ingress rule's CIDR matches any of the filter values.
func sgIngressCIDRMatchesAny(rules []SGRule, values []string) bool {
	for _, rule := range rules {
		if rule.CidrIp != "" && filterutil.MatchesAny(values, rule.CidrIp) {
			return true
		}
	}
	return false
}

// getSecurityGroupsForVpcValidFilters defines the set of filter names accepted
// by GetSecurityGroupsForVpc. The model names the VPC primary-vpc-id here, and
// does not accept the vpc-id or ip-permission.cidr DescribeSecurityGroups takes.
var getSecurityGroupsForVpcValidFilters = map[string]bool{
	"group-id":       true,
	"group-name":     true,
	"description":    true,
	"owner-id":       true,
	"primary-vpc-id": true,
}

// defaultSGForVpcPerPage caps a request that names no MaxResults, matching the
// model's ceiling.
const defaultSGForVpcPerPage = 1000

// GetSecurityGroupsForVpc lists the security groups an interface in the named
// VPC can be associated with. It is the same account-prefixed scan
// DescribeSecurityGroups runs, projected to the fields the model returns: the
// rule set is deliberately absent, since a caller wanting rules has
// DescribeSecurityGroupRules.
func (s *VPCServiceImpl) GetSecurityGroupsForVpc(ctx context.Context, input *ec2.GetSecurityGroupsForVpcInput, accountID string) (*ec2.GetSecurityGroupsForVpcOutput, error) {
	if input == nil || input.VpcId == nil || *input.VpcId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	vpcID := *input.VpcId

	// An unknown VPC is not-found rather than an empty list: an empty list is
	// indistinguishable from a VPC that genuinely holds no groups, and would
	// send the caller on to create an interface in a VPC that does not exist.
	if err := s.requireVPCExists(ctx, accountID, vpcID); err != nil {
		return nil, err
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, getSecurityGroupsForVpcValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "GetSecurityGroupsForVpc: invalid filter", "err", err)
		return nil, err
	}

	prefix := accountID + "."
	keys, err := s.sgKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	groups := []*ec2.SecurityGroupForVpc{}
	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.sgKV.Get(ctx, key)
		if err != nil {
			slog.ErrorContext(ctx, "GetSecurityGroupsForVpc: SG read failed", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		var record SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.ErrorContext(ctx, "GetSecurityGroupsForVpc: SG unmarshal failed", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		if record.VpcId != vpcID {
			continue
		}
		if len(parsedFilters) > 0 && !sgForVpcMatchesFilters(&record, accountID, parsedFilters) {
			continue
		}

		groups = append(groups, &ec2.SecurityGroupForVpc{
			GroupId:      aws.String(record.GroupId),
			GroupName:    aws.String(record.GroupName),
			Description:  aws.String(record.Description),
			OwnerId:      aws.String(accountID),
			PrimaryVpcId: aws.String(record.VpcId),
			Tags:         utils.MapToEC2Tags(record.Tags),
		})
	}

	// The KV key order is not guaranteed stable across calls, so a token
	// addressing an offset into an unsorted set could skip or repeat a group.
	slices.SortFunc(groups, func(a, b *ec2.SecurityGroupForVpc) int {
		return strings.Compare(aws.StringValue(a.GroupId), aws.StringValue(b.GroupId))
	})

	page, nextToken, err := pageSecurityGroupsForVpc(groups, input.NextToken, input.MaxResults)
	if err != nil {
		slog.WarnContext(ctx, "GetSecurityGroupsForVpc: invalid NextToken", "next_token", aws.StringValue(input.NextToken))
		return nil, err
	}

	slog.InfoContext(ctx, "GetSecurityGroupsForVpc completed",
		"count", len(page), "total", len(groups), "vpcId", vpcID, "accountID", accountID)

	return &ec2.GetSecurityGroupsForVpcOutput{
		SecurityGroupForVpcs: page,
		NextToken:            nextToken,
	}, nil
}

// sgForVpcMatchesFilters applies the GetSecurityGroupsForVpc filter set. The
// owner is the account the scan is prefixed by, so owner-id can only match the
// caller's own account.
func sgForVpcMatchesFilters(record *SecurityGroupRecord, accountID string, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		switch name {
		case "group-id":
			if !filterutil.MatchesAny(values, record.GroupId) {
				return false
			}
		case "group-name":
			if !filterutil.MatchesAny(values, record.GroupName) {
				return false
			}
		case "description":
			if !filterutil.MatchesAny(values, record.Description) {
				return false
			}
		case "owner-id":
			if !filterutil.MatchesAny(values, accountID) {
				return false
			}
		case "primary-vpc-id":
			if !filterutil.MatchesAny(values, record.VpcId) {
				return false
			}
		default:
			return false
		}
	}

	return filterutil.MatchesTags(filters, record.Tags)
}

// pageSecurityGroupsForVpc slices one page out of a sorted group set, using an
// opaque integer offset as the token. The last page carries a nil token.
func pageSecurityGroupsForVpc(groups []*ec2.SecurityGroupForVpc, token *string, maxResults *int64) ([]*ec2.SecurityGroupForVpc, *string, error) {
	start := 0
	if aws.StringValue(token) != "" {
		n, err := strconv.Atoi(*token)
		if err != nil || n < 0 {
			return nil, nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		start = n
	}
	if start > len(groups) {
		start = len(groups)
	}

	size := defaultSGForVpcPerPage
	if maxResults != nil && *maxResults > 0 {
		size = int(*maxResults)
	}

	end := start + size
	if end >= len(groups) {
		return groups[start:], nil, nil
	}
	return groups[start:end], aws.String(strconv.Itoa(end)), nil
}

// describeSecurityGroupRulesValidFilters defines the set of filter names accepted by DescribeSecurityGroupRules.
var describeSecurityGroupRulesValidFilters = map[string]bool{
	"group-id":               true,
	"security-group-rule-id": true,
	"tag-key":                true,
}

// DescribeSecurityGroupRules returns a flat list of SecurityGroupRule objects
// for the caller's account, optionally narrowed by SecurityGroupRuleIds or
// filters. MaxResults and NextToken are accepted but ignored.
func (s *VPCServiceImpl) DescribeSecurityGroupRules(ctx context.Context, input *ec2.DescribeSecurityGroupRulesInput, accountID string) (*ec2.DescribeSecurityGroupRulesOutput, error) {
	requested := make(map[string]bool)
	if input != nil {
		for _, id := range input.SecurityGroupRuleIds {
			if id == nil || *id == "" {
				return nil, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
			}
			requested[*id] = true
		}
	}

	var filters []*ec2.Filter
	if input != nil {
		filters = input.Filters
	}
	parsedFilters, err := filterutil.ParseFilters(filters, describeSecurityGroupRulesValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeSecurityGroupRules: invalid filter", "err", err)
		return nil, err
	}

	prefix := accountID + "."
	keys, err := s.sgKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	rules := []*ec2.SecurityGroupRule{}
	emitted := make(map[string]bool)

	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.sgKV.Get(ctx, key)
		if err != nil {
			slog.ErrorContext(ctx, "DescribeSecurityGroupRules: SG read failed", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		var record SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.ErrorContext(ctx, "DescribeSecurityGroupRules: SG unmarshal failed", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		for _, r := range record.IngressRules {
			if !sgRuleMatchesFilters(&record, r, parsedFilters) {
				continue
			}
			if len(requested) > 0 && !requested[r.RuleId] {
				continue
			}
			rules = append(rules, sgRuleToSecurityGroupRule(&record, r, false, accountID))
			emitted[r.RuleId] = true
		}
		for _, r := range record.EgressRules {
			if !sgRuleMatchesFilters(&record, r, parsedFilters) {
				continue
			}
			if len(requested) > 0 && !requested[r.RuleId] {
				continue
			}
			rules = append(rules, sgRuleToSecurityGroupRule(&record, r, true, accountID))
			emitted[r.RuleId] = true
		}
	}

	if len(requested) > 0 {
		for id := range requested {
			if !emitted[id] {
				return nil, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdNotFound)
			}
		}
	}

	slog.InfoContext(ctx, "DescribeSecurityGroupRules completed", "count", len(rules), "accountID", accountID)

	return &ec2.DescribeSecurityGroupRulesOutput{
		SecurityGroupRules: rules,
	}, nil
}

// sgRuleMatchesFilters applies the DescribeSecurityGroupRules filter set to a
// single rule.
func sgRuleMatchesFilters(record *SecurityGroupRecord, rule SGRule, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}
		switch name {
		case "group-id":
			if !filterutil.MatchesAny(values, record.GroupId) {
				return false
			}
		case "security-group-rule-id":
			if !filterutil.MatchesAny(values, rule.RuleId) {
				return false
			}
		case "tag-key":
			if !sgRuleMatchesTagKey(rule.Tags, values) {
				return false
			}
		default:
			// Unreachable: ParseFilters rejects names not in the valid set.
			// Logs an error if a future map entry lacks a matching case.
			slog.Error("sgRuleMatchesFilters: filter accepted by ParseFilters but no case", "filter", name)
			return false
		}
	}
	return filterutil.MatchesTags(filters, rule.Tags)
}

// updateSGRuleTags applies mut to the tags of the rule named by ruleID, which
// lives inside its parent group's record rather than a record of its own. The
// parent is found by scanning the account's groups: a rule id carries no
// pointer back to the group holding it.
func (s *VPCServiceImpl) updateSGRuleTags(ctx context.Context, accountID, ruleID string, mut func(map[string]string)) error {
	prefix := accountID + "."
	keys, err := s.sgKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		slog.ErrorContext(ctx, "updateSGRuleTags: key listing failed", "ruleId", ruleID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.sgKV.Get(ctx, key)
		if err != nil {
			slog.ErrorContext(ctx, "updateSGRuleTags: SG read failed", "key", key, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		var record SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.ErrorContext(ctx, "updateSGRuleTags: SG unmarshal failed", "key", key, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}

		rule := findSGRuleByID(record.IngressRules, ruleID)
		if rule == nil {
			rule = findSGRuleByID(record.EgressRules, ruleID)
		}
		if rule == nil {
			continue
		}
		if rule.Tags == nil {
			rule.Tags = map[string]string{}
		}
		mut(rule.Tags)

		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("failed to marshal security group record: %w", err)
		}
		// Compare-and-swap on the revision read above: a concurrent authorize on
		// the same group would otherwise lose its rule to this write.
		if _, err := s.sgKV.Update(ctx, key, data, entry.Revision()); err != nil {
			slog.ErrorContext(ctx, "updateSGRuleTags: SG write failed", "key", key, "ruleId", ruleID, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		return nil
	}
	return nil
}

// findSGRuleByID returns a pointer into rules so a caller can mutate the stored
// rule in place.
func findSGRuleByID(rules []SGRule, ruleID string) *SGRule {
	for i := range rules {
		if rules[i].RuleId == ruleID {
			return &rules[i]
		}
	}
	return nil
}

// sgRuleMatchesTagKey reports whether the rule carries any of the named tag
// keys, whatever their values.
func sgRuleMatchesTagKey(tags map[string]string, keys []string) bool {
	for key := range tags {
		if filterutil.MatchesAny(keys, key) {
			return true
		}
	}
	return false
}

// applySGRuleTags stamps the security-group-rule TagSpecification onto every
// rule an authorize call creates. AWS tags the rules the call creates and no
// others, so this runs on the new rules alone.
func applySGRuleTags(rules []SGRule, specs []*ec2.TagSpecification) []SGRule {
	tags := utils.ExtractTags(specs, ec2.ResourceTypeSecurityGroupRule)
	if len(tags) == 0 {
		return rules
	}
	for i := range rules {
		rules[i].Tags = maps.Clone(tags)
	}
	return rules
}

// sgRuleToSecurityGroupRule flattens a stored SGRule into the AWS API shape.
// accountID supplies GroupOwnerId and ReferencedGroupInfo.UserId; VpcId is
// derived from the parent record (same-VPC references are enforced on write).
func sgRuleToSecurityGroupRule(record *SecurityGroupRecord, rule SGRule, isEgress bool, accountID string) *ec2.SecurityGroupRule {
	// An all-protocol rule has no ports to report. AWS answers -1 for both;
	// reporting the stored zeroes offers a caller two ports it never sent.
	fromPort, toPort := rule.FromPort, rule.ToPort
	if rule.IpProtocol == allProtocols {
		fromPort, toPort = -1, -1
	}
	out := &ec2.SecurityGroupRule{
		SecurityGroupRuleId: aws.String(rule.RuleId),
		GroupId:             aws.String(record.GroupId),
		GroupOwnerId:        aws.String(accountID),
		IsEgress:            aws.Bool(isEgress),
		IpProtocol:          aws.String(rule.IpProtocol),
		FromPort:            aws.Int64(fromPort),
		ToPort:              aws.Int64(toPort),
		Tags:                utils.MapToEC2Tags(rule.Tags),
	}
	if rule.Description != "" {
		out.Description = aws.String(rule.Description)
	}
	if rule.CidrIp != "" {
		out.CidrIpv4 = aws.String(rule.CidrIp)
	}
	if rule.CidrIpv6 != "" {
		out.CidrIpv6 = aws.String(rule.CidrIpv6)
	}
	if rule.SourceSG != "" {
		out.ReferencedGroupInfo = &ec2.ReferencedSecurityGroup{
			GroupId: aws.String(rule.SourceSG),
			UserId:  aws.String(accountID),
			VpcId:   aws.String(record.VpcId),
		}
	}
	return out
}

// AuthorizeSecurityGroupIngress adds ingress rules to a security group.
func (s *VPCServiceImpl) AuthorizeSecurityGroupIngress(ctx context.Context, input *ec2.AuthorizeSecurityGroupIngressInput, accountID string) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	if input.GroupId == nil || *input.GroupId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(ctx, key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.requireVPCExists(ctx, accountID, record.VpcId); err != nil {
		return nil, err
	}

	newRules, err := ipPermissionsToSGRules(input.IpPermissions, sgParseAuthorize)
	if err != nil {
		slog.WarnContext(ctx, "AuthorizeSecurityGroupIngress: invalid rule", "groupId", groupId, "err", err)
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s", err)
	}
	newRules = applySGRuleTags(newRules, input.TagSpecifications)
	if err := s.validateSGRuleReferences(ctx, accountID, record.VpcId, newRules); err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(record.IngressRules))
	for _, r := range record.IngressRules {
		existing[sgRuleKey(r)] = struct{}{}
	}
	for _, nr := range newRules {
		if _, ok := existing[sgRuleKey(nr)]; ok {
			return nil, errors.New(awserrors.ErrorInvalidPermissionDuplicate)
		}
	}
	if len(record.IngressRules)+len(newRules) > maxRulesPerSGSide {
		return nil, errors.New(awserrors.ErrorRulesPerSecurityGroupLimitExceeded)
	}
	record.IngressRules = append(record.IngressRules, newRules...)

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal security group record: %w", err)
	}
	if _, err := s.sgKV.Update(ctx, key, data, entry.Revision()); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, nr := range newRules {
		s.projectRecordTags(ctx, accountID, nr.RuleId, nr.Tags)
	}

	slog.InfoContext(ctx, "AuthorizeSecurityGroupIngress completed", "groupId", groupId, "newRules", len(newRules), "accountID", accountID)

	if err := s.requestSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.ErrorContext(ctx, "AuthorizeSecurityGroupIngress: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	ruleSet := make([]*ec2.SecurityGroupRule, 0, len(newRules))
	for _, nr := range newRules {
		ruleSet = append(ruleSet, sgRuleToSecurityGroupRule(&record, nr, false, accountID))
	}
	return &ec2.AuthorizeSecurityGroupIngressOutput{
		Return:             aws.Bool(true),
		SecurityGroupRules: ruleSet,
	}, nil
}

// AuthorizeSecurityGroupEgress adds egress rules to a security group.
func (s *VPCServiceImpl) AuthorizeSecurityGroupEgress(ctx context.Context, input *ec2.AuthorizeSecurityGroupEgressInput, accountID string) (*ec2.AuthorizeSecurityGroupEgressOutput, error) {
	if input.GroupId == nil || *input.GroupId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(ctx, key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.requireVPCExists(ctx, accountID, record.VpcId); err != nil {
		return nil, err
	}

	newRules, err := ipPermissionsToSGRules(input.IpPermissions, sgParseAuthorize)
	if err != nil {
		slog.WarnContext(ctx, "AuthorizeSecurityGroupEgress: invalid rule", "groupId", groupId, "err", err)
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s", err)
	}
	newRules = applySGRuleTags(newRules, input.TagSpecifications)
	if err := s.validateSGRuleReferences(ctx, accountID, record.VpcId, newRules); err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(record.EgressRules))
	for _, r := range record.EgressRules {
		existing[sgRuleKey(r)] = struct{}{}
	}
	for _, nr := range newRules {
		if _, ok := existing[sgRuleKey(nr)]; ok {
			return nil, errors.New(awserrors.ErrorInvalidPermissionDuplicate)
		}
	}
	if len(record.EgressRules)+len(newRules) > maxRulesPerSGSide {
		return nil, errors.New(awserrors.ErrorRulesPerSecurityGroupLimitExceeded)
	}
	record.EgressRules = append(record.EgressRules, newRules...)

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal security group record: %w", err)
	}
	if _, err := s.sgKV.Update(ctx, key, data, entry.Revision()); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, nr := range newRules {
		s.projectRecordTags(ctx, accountID, nr.RuleId, nr.Tags)
	}

	slog.InfoContext(ctx, "AuthorizeSecurityGroupEgress completed", "groupId", groupId, "newRules", len(newRules), "accountID", accountID)

	if err := s.requestSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.ErrorContext(ctx, "AuthorizeSecurityGroupEgress: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	ruleSet := make([]*ec2.SecurityGroupRule, 0, len(newRules))
	for _, nr := range newRules {
		ruleSet = append(ruleSet, sgRuleToSecurityGroupRule(&record, nr, true, accountID))
	}
	return &ec2.AuthorizeSecurityGroupEgressOutput{
		Return:             aws.Bool(true),
		SecurityGroupRules: ruleSet,
	}, nil
}

// RevokeSecurityGroupIngress removes ingress rules from a security group.
func (s *VPCServiceImpl) RevokeSecurityGroupIngress(ctx context.Context, input *ec2.RevokeSecurityGroupIngressInput, accountID string) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	if input.GroupId == nil || *input.GroupId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(ctx, key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	revokeRules, err := ipPermissionsToSGRules(input.IpPermissions, sgParseRevoke)
	if err != nil {
		slog.WarnContext(ctx, "RevokeSecurityGroupIngress: invalid rule", "groupId", groupId, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	idRules, err := resolveRuleIDsToRemove(record.IngressRules, input.SecurityGroupRuleIds)
	if err != nil {
		return nil, err
	}
	revokeRules = append(revokeRules, idRules...)
	if len(revokeRules) == 0 {
		return &ec2.RevokeSecurityGroupIngressOutput{Return: aws.Bool(true)}, nil
	}
	if err := assertRulesPresent(record.IngressRules, revokeRules); err != nil {
		return nil, err
	}
	revokedIDs := removedSGRuleIDs(record.IngressRules, revokeRules)
	record.IngressRules = removeSGRules(record.IngressRules, revokeRules)

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal security group record: %w", err)
	}
	if _, err := s.sgKV.Update(ctx, key, data, entry.Revision()); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, id := range revokedIDs {
		s.clearRecordTags(ctx, accountID, id)
	}

	slog.InfoContext(ctx, "RevokeSecurityGroupIngress completed", "groupId", groupId, "revokedRules", len(revokeRules), "accountID", accountID)

	if err := s.requestSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.ErrorContext(ctx, "RevokeSecurityGroupIngress: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.RevokeSecurityGroupIngressOutput{
		Return: aws.Bool(true),
	}, nil
}

// RevokeSecurityGroupEgress removes egress rules from a security group.
func (s *VPCServiceImpl) RevokeSecurityGroupEgress(ctx context.Context, input *ec2.RevokeSecurityGroupEgressInput, accountID string) (*ec2.RevokeSecurityGroupEgressOutput, error) {
	if input.GroupId == nil || *input.GroupId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(ctx, key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	revokeRules, err := ipPermissionsToSGRules(input.IpPermissions, sgParseRevoke)
	if err != nil {
		slog.WarnContext(ctx, "RevokeSecurityGroupEgress: invalid rule", "groupId", groupId, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	idRules, err := resolveRuleIDsToRemove(record.EgressRules, input.SecurityGroupRuleIds)
	if err != nil {
		return nil, err
	}
	revokeRules = append(revokeRules, idRules...)
	if len(revokeRules) == 0 {
		return &ec2.RevokeSecurityGroupEgressOutput{Return: aws.Bool(true)}, nil
	}
	if err := assertRulesPresent(record.EgressRules, revokeRules); err != nil {
		return nil, err
	}
	revokedIDs := removedSGRuleIDs(record.EgressRules, revokeRules)
	record.EgressRules = removeSGRules(record.EgressRules, revokeRules)

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal security group record: %w", err)
	}
	if _, err := s.sgKV.Update(ctx, key, data, entry.Revision()); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, id := range revokedIDs {
		s.clearRecordTags(ctx, accountID, id)
	}

	slog.InfoContext(ctx, "RevokeSecurityGroupEgress completed", "groupId", groupId, "revokedRules", len(revokeRules), "accountID", accountID)

	if err := s.requestSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.ErrorContext(ctx, "RevokeSecurityGroupEgress: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.RevokeSecurityGroupEgressOutput{
		Return: aws.Bool(true),
	}, nil
}

// UpdateSecurityGroupRuleDescriptionsIngress sets the description on existing
// ingress rules, resolved by SecurityGroupRuleId or by matching IpPermissions.
func (s *VPCServiceImpl) UpdateSecurityGroupRuleDescriptionsIngress(ctx context.Context, input *ec2.UpdateSecurityGroupRuleDescriptionsIngressInput, accountID string) (*ec2.UpdateSecurityGroupRuleDescriptionsIngressOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if err := s.updateSGRuleDescriptions(ctx, accountID, sgDescriptionRequest{
		op:           "UpdateSecurityGroupRuleDescriptionsIngress",
		groupId:      aws.StringValue(input.GroupId),
		descriptions: input.SecurityGroupRuleDescriptions,
		permissions:  input.IpPermissions,
		egress:       false,
	}); err != nil {
		return nil, err
	}
	return &ec2.UpdateSecurityGroupRuleDescriptionsIngressOutput{Return: aws.Bool(true)}, nil
}

// UpdateSecurityGroupRuleDescriptionsEgress sets the description on existing
// egress rules, resolved by SecurityGroupRuleId or by matching IpPermissions.
func (s *VPCServiceImpl) UpdateSecurityGroupRuleDescriptionsEgress(ctx context.Context, input *ec2.UpdateSecurityGroupRuleDescriptionsEgressInput, accountID string) (*ec2.UpdateSecurityGroupRuleDescriptionsEgressOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if err := s.updateSGRuleDescriptions(ctx, accountID, sgDescriptionRequest{
		op:           "UpdateSecurityGroupRuleDescriptionsEgress",
		groupId:      aws.StringValue(input.GroupId),
		descriptions: input.SecurityGroupRuleDescriptions,
		permissions:  input.IpPermissions,
		egress:       true,
	}); err != nil {
		return nil, err
	}
	return &ec2.UpdateSecurityGroupRuleDescriptionsEgressOutput{Return: aws.Bool(true)}, nil
}

// sgDescriptionRequest carries the two Update*RuleDescriptions* variants into
// one implementation; egress selects which side of the record is walked.
type sgDescriptionRequest struct {
	op           string
	groupId      string
	descriptions []*ec2.SecurityGroupRuleDescription
	permissions  []*ec2.IpPermission
	egress       bool
}

// updateSGRuleDescriptions writes Description onto already-stored rules.
// Description sits outside sgRuleKey, so no rule's identity, RuleId or
// duplicate status changes and the projected ACL set is unaffected.
func (s *VPCServiceImpl) updateSGRuleDescriptions(ctx context.Context, accountID string, req sgDescriptionRequest) error {
	if req.groupId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(req.descriptions) == 0 && len(req.permissions) == 0 {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	// AWS takes one resolution mode per request. Honouring both would let the
	// two lists name the same rule and resolve last-wins, invisibly.
	if len(req.descriptions) > 0 && len(req.permissions) > 0 {
		return errors.New(awserrors.ErrorInvalidParameterCombination)
	}

	key := utils.AccountKey(accountID, req.groupId)
	entry, err := s.sgKV.Get(ctx, key)
	if err != nil {
		return errors.New(awserrors.ErrorInvalidGroupNotFound)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	rules := record.IngressRules
	if req.egress {
		rules = record.EgressRules
	}

	updated, err := applySGRuleDescriptions(rules, req.descriptions, req.permissions)
	if err != nil {
		slog.WarnContext(ctx, req.op+": rule resolution failed", "groupId", req.groupId, "err", err)
		return err
	}

	if req.egress {
		record.EgressRules = updated
	} else {
		record.IngressRules = updated
	}

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal security group record: %w", err)
	}
	if _, err := s.sgKV.Update(ctx, key, data, entry.Revision()); err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, req.op+" completed", "groupId", req.groupId, "accountID", accountID)

	// The ACL set this rebuilds is byte-identical, because toPolicyRules drops
	// Description. Publishing anyway keeps every mutating path on one shape.
	if err := s.requestSGEvent("vpc.update-sg", SGEvent{
		GroupId:      req.groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.ErrorContext(ctx, req.op+": vpcd request failed", "groupId", req.groupId, "err", err)
		return err
	}
	return nil
}

// applySGRuleDescriptions returns rules with the requested descriptions set,
// resolving targets by SecurityGroupRuleId then by IpPermissions content. An
// unresolvable target is an error, so a caller never sees a successful no-op.
func applySGRuleDescriptions(rules []SGRule, descriptions []*ec2.SecurityGroupRuleDescription, permissions []*ec2.IpPermission) ([]SGRule, error) {
	byID := make(map[string]int, len(rules))
	byKey := make(map[string]int, len(rules))
	for i, r := range rules {
		if r.RuleId != "" {
			byID[r.RuleId] = i
		}
		byKey[sgRuleKey(r)] = i
	}

	out := slices.Clone(rules)

	for _, d := range descriptions {
		if d == nil || aws.StringValue(d.SecurityGroupRuleId) == "" {
			return nil, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
		}
		id := *d.SecurityGroupRuleId
		if !SGRuleIDRegex.MatchString(id) {
			return nil, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
		}
		// A rule on the other side, in another group or in another account is
		// absent from this record and so is not-found, never a silent no-op.
		i, ok := byID[id]
		if !ok {
			return nil, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdNotFound)
		}
		if d.Description != nil {
			out[i].Description = *d.Description
		}
	}

	targets, err := ipPermissionsToDescriptionTargets(permissions)
	if err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "invalid IpPermissions: %w", err)
	}
	for _, t := range targets {
		i, ok := byKey[t.key]
		if !ok {
			return nil, errors.New(awserrors.ErrorInvalidPermissionNotFound)
		}
		if t.description != nil {
			out[i].Description = *t.description
		}
	}

	return out, nil
}

// sgDescriptionTarget is one rule matched by content plus the description the
// caller asked for. A nil description leaves the stored one untouched, so
// clearing a description to "" stays distinguishable from not supplying one.
type sgDescriptionTarget struct {
	key         string
	description *string
}

// ipPermissionsToDescriptionTargets resolves IpPermissions to rule-identity
// keys, applying the same protocol normalisation and validation as the
// authorize path. A permission that resolves to no target is an error: the
// authorize path rejects the same input, so no matching rule can exist.
func ipPermissionsToDescriptionTargets(perms []*ec2.IpPermission) ([]sgDescriptionTarget, error) {
	var targets []sgDescriptionTarget
	for _, perm := range perms {
		if perm == nil {
			continue
		}
		raw := ""
		if perm.IpProtocol != nil {
			raw = *perm.IpProtocol
		}
		proto, err := normalizeIPProtocol(raw)
		if err != nil {
			return nil, err
		}

		var fromPort, toPort int64
		if perm.FromPort != nil {
			fromPort = *perm.FromPort
		}
		if perm.ToPort != nil {
			toPort = *perm.ToPort
		}

		before := len(targets)

		for _, ipRange := range perm.IpRanges {
			if ipRange == nil || ipRange.CidrIp == nil {
				continue
			}
			r := SGRule{IpProtocol: proto, FromPort: fromPort, ToPort: toPort, CidrIp: *ipRange.CidrIp}
			if err := validateSGRule(r); err != nil {
				return nil, err
			}
			targets = append(targets, sgDescriptionTarget{key: sgRuleKey(r), description: ipRange.Description})
		}

		for _, pair := range perm.UserIdGroupPairs {
			if pair == nil || pair.GroupId == nil {
				continue
			}
			r := SGRule{IpProtocol: proto, FromPort: fromPort, ToPort: toPort, SourceSG: *pair.GroupId}
			if err := validateSGRule(r); err != nil {
				return nil, err
			}
			targets = append(targets, sgDescriptionTarget{key: sgRuleKey(r), description: pair.Description})
		}

		for _, r6 := range perm.Ipv6Ranges {
			if r6 == nil || r6.CidrIpv6 == nil {
				continue
			}
			r := SGRule{IpProtocol: proto, FromPort: fromPort, ToPort: toPort, CidrIpv6: *r6.CidrIpv6}
			if err := validateSGRule(r); err != nil {
				return nil, err
			}
			targets = append(targets, sgDescriptionTarget{key: sgRuleKey(r), description: r6.Description})
		}

		if len(targets) == before {
			return nil, errors.New("IpPermission must specify at least one IpRange, Ipv6Range or UserIdGroupPair")
		}
	}
	return targets, nil
}

// ModifySecurityGroupRules replaces the body of stored rules in place, keeping
// each rule's SecurityGroupRuleId and tags. Every update is validated before
// anything is written, so one bad update leaves the whole group untouched.
func (s *VPCServiceImpl) ModifySecurityGroupRules(ctx context.Context, input *ec2.ModifySecurityGroupRulesInput, accountID string) (*ec2.ModifySecurityGroupRulesOutput, error) {
	if input == nil || aws.StringValue(input.GroupId) == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.SecurityGroupRules) == 0 {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must contain the parameter securityGroupRules")
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(ctx, key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.requireVPCExists(ctx, accountID, record.VpcId); err != nil {
		return nil, err
	}

	modified, err := applySGRuleModifications(&record, input.SecurityGroupRules)
	if err != nil {
		slog.WarnContext(ctx, "ModifySecurityGroupRules: invalid update", "groupId", groupId, "err", err)
		return nil, err
	}
	if err := s.validateSGRuleReferences(ctx, accountID, record.VpcId, modified); err != nil {
		return nil, err
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal security group record: %w", err)
	}
	if _, err := s.sgKV.Update(ctx, key, data, entry.Revision()); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "ModifySecurityGroupRules completed", "groupId", groupId, "modifiedRules", len(modified), "accountID", accountID)

	if err := s.requestSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.ErrorContext(ctx, "ModifySecurityGroupRules: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.ModifySecurityGroupRulesOutput{Return: aws.Bool(true)}, nil
}

// sgRuleModification is one resolved update: the side and index of the stored
// rule it replaces, and the rule that replaces it.
type sgRuleModification struct {
	egress bool
	index  int
	rule   SGRule
}

// applySGRuleModifications resolves and validates every update, then writes
// the new bodies into record. It returns the modified rules, and on error
// leaves record untouched.
func applySGRuleModifications(record *SecurityGroupRecord, updates []*ec2.SecurityGroupRuleUpdate) ([]SGRule, error) {
	seen := make(map[string]bool, len(updates))
	mods := make([]sgRuleModification, 0, len(updates))
	for _, u := range updates {
		if u == nil || u.SecurityGroupRuleId == nil {
			return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must contain the parameter securityGroupRuleId")
		}
		id := *u.SecurityGroupRuleId
		if SGRuleIDIsMalformed(id) {
			return nil, sgRuleIDLookupError(id)
		}
		if seen[id] {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"Duplicated security group rule ID '%s'. The same security group rule ID must not appear multiple times.", id)
		}
		seen[id] = true

		mod := sgRuleModification{index: slices.IndexFunc(record.IngressRules, func(r SGRule) bool { return r.RuleId == id })}
		stored := record.IngressRules
		if mod.index < 0 {
			mod.egress = true
			mod.index = slices.IndexFunc(record.EgressRules, func(r SGRule) bool { return r.RuleId == id })
			stored = record.EgressRules
		}
		if mod.index < 0 {
			return nil, sgRuleIDLookupError(id)
		}
		current := stored[mod.index]

		rule, err := sgRuleRequestToSGRule(u.SecurityGroupRule)
		if err != nil {
			return nil, err
		}
		if want, have := sgRuleSourceField(rule), sgRuleSourceField(current); want != have {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"Invalid rule type for security group rule '%s'. You may not specify %s for an existing %s rule.", id, want, have)
		}
		rule.RuleId = current.RuleId
		rule.Tags = current.Tags
		mod.rule = rule
		mods = append(mods, mod)
	}

	for _, egress := range []bool{false, true} {
		if err := checkSGRuleModificationDuplicates(record, mods, egress); err != nil {
			return nil, err
		}
	}

	ingress, egressRules := slices.Clone(record.IngressRules), slices.Clone(record.EgressRules)
	modified := make([]SGRule, 0, len(mods))
	for _, m := range mods {
		if m.egress {
			egressRules[m.index] = m.rule
		} else {
			ingress[m.index] = m.rule
		}
		modified = append(modified, m.rule)
	}
	record.IngressRules, record.EgressRules = ingress, egressRules
	return modified, nil
}

// checkSGRuleModificationDuplicates rejects new contents that collide with each
// other or with a rule on the same side the request leaves alone. Excluding
// the rules being modified lets a rule keep its own content and two rules swap.
func checkSGRuleModificationDuplicates(record *SecurityGroupRecord, mods []sgRuleModification, egress bool) error {
	rules := record.IngressRules
	if egress {
		rules = record.EgressRules
	}

	replaced := make(map[int]bool)
	newKeys := make(map[string]bool)
	for _, m := range mods {
		if m.egress != egress {
			continue
		}
		replaced[m.index] = true
		k := sgRuleKey(m.rule)
		if newKeys[k] {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "The same permission must not appear multiple times")
		}
		newKeys[k] = true
	}

	for i, r := range rules {
		if !replaced[i] && newKeys[sgRuleKey(r)] {
			return errors.New(awserrors.ErrorInvalidPermissionDuplicate)
		}
	}
	return nil
}

// sgRuleRequestToSGRule parses a SecurityGroupRuleRequest into a full rule.
// It is stricter than the authorize path: the protocol is required and tcp/udp
// need both ports, as AWS requires for a modify.
func sgRuleRequestToSGRule(req *ec2.SecurityGroupRuleRequest) (SGRule, error) {
	if req == nil {
		return SGRule{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "No value specified for securityGroupRule.")
	}

	if strings.TrimSpace(aws.StringValue(req.IpProtocol)) == "" {
		return SGRule{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Invalid value 'null' for protocol. VPC security group rules must specify protocols explicitly.")
	}
	proto, err := normalizeIPProtocol(*req.IpProtocol)
	if err != nil {
		return SGRule{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s", err)
	}

	r := SGRule{IpProtocol: proto, Description: aws.StringValue(req.Description)}
	switch proto {
	case "tcp", "udp":
		if req.FromPort == nil || req.ToPort == nil {
			return SGRule{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"Invalid value for portRange. Must specify both from and to ports with TCP/UDP.")
		}
		r.FromPort, r.ToPort = *req.FromPort, *req.ToPort
	case allProtocols:
		// Stored as 0/0, the form authorize stores for the no-ports case, so the
		// rule's sgRuleKey matches Terraform's and the default egress rule's.
		noPorts := req.FromPort == nil && req.ToPort == nil
		allPorts := req.FromPort != nil && req.ToPort != nil && *req.FromPort == -1 && *req.ToPort == -1
		if !noPorts && !allPorts {
			return SGRule{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"You may not specify all protocols and specific ports. Please specify each protocol and port range combination individually, or all protocols and no port range.")
		}
	default:
		r.FromPort, r.ToPort = aws.Int64Value(req.FromPort), aws.Int64Value(req.ToPort)
	}

	sources := 0
	for _, src := range []*string{req.CidrIpv4, req.CidrIpv6, req.ReferencedGroupId, req.PrefixListId} {
		if aws.StringValue(src) != "" {
			sources++
		}
	}
	switch {
	case sources == 0:
		return SGRule{}, awserrors.Errorf(awserrors.ErrorMissingParameter,
			"The request must contain exactly one of: cidrIp, cidrIpv6, prefixListId, or referencedGroupId")
	case sources > 1:
		return SGRule{}, awserrors.Errorf(awserrors.ErrorInvalidParameterCombination,
			"Only one of cidrIp, cidrIpv6, prefixListId, or referencedGroupId can be specified")
	case aws.StringValue(req.PrefixListId) != "":
		return SGRule{}, awserrors.Errorf(awserrors.ErrorInvalidPrefixListIDNotFound,
			"The prefix list ID '%s' does not exist", *req.PrefixListId)
	}

	r.CidrIp = aws.StringValue(req.CidrIpv4)
	r.CidrIpv6 = aws.StringValue(req.CidrIpv6)
	r.SourceSG = aws.StringValue(req.ReferencedGroupId)
	if r.SourceSG != "" && !sgIDRegex.MatchString(r.SourceSG) {
		return SGRule{}, awserrors.Errorf(awserrors.ErrorInvalidGroupIdMalformed, "Invalid id: %q", r.SourceSG)
	}
	if err := validateSGRule(r); err != nil {
		return SGRule{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s", err)
	}
	return r, nil
}

// sgRuleSourceField names a rule's source type by its API field, since a
// modify may not move a rule from one source type to another.
func sgRuleSourceField(r SGRule) string {
	switch {
	case r.CidrIpv6 != "":
		return "CidrIpv6"
	case r.SourceSG != "":
		return "ReferencedGroupId"
	default:
		return "CidrIpv4"
	}
}

// sgRuleIDMaxSuffix is the longest suffix AWS accepts after "sgr-" before it
// calls a rule ID malformed rather than unknown.
const sgRuleIDMaxSuffix = 17

// SGRuleIDIsMalformed reports whether AWS rejects id as malformed: it lacks the
// lowercase "sgr-" prefix or has too long a suffix. Looser than SGRuleIDRegex,
// because AWS answers NotFound for a short or non-hex suffix such as sgr-xyz.
func SGRuleIDIsMalformed(id string) bool {
	suffix, ok := strings.CutPrefix(id, "sgr-")
	return !ok || len(suffix) > sgRuleIDMaxSuffix
}

// sgRuleIDLookupError is the error AWS returns for a rule ID that resolves to
// no stored rule: Malformed or NotFound by SGRuleIDIsMalformed.
func sgRuleIDLookupError(id string) error {
	if SGRuleIDIsMalformed(id) {
		return awserrors.Errorf(awserrors.ErrorInvalidSecurityGroupRuleIdMalformed, "Invalid id: %q", id)
	}
	return awserrors.Errorf(awserrors.ErrorInvalidSecurityGroupRuleIdNotFound,
		"The security group rule ID '%s' does not exist", id)
}

// sgRecordToEC2 converts a SecurityGroupRecord to an EC2 SecurityGroup.
func (s *VPCServiceImpl) sgRecordToEC2(record *SecurityGroupRecord, accountID string) *ec2.SecurityGroup {
	sg := &ec2.SecurityGroup{
		GroupId:             aws.String(record.GroupId),
		GroupName:           aws.String(record.GroupName),
		Description:         aws.String(record.Description),
		VpcId:               aws.String(record.VpcId),
		OwnerId:             aws.String(accountID),
		IpPermissions:       sgRulesToIpPermissions(record.IngressRules),
		IpPermissionsEgress: sgRulesToIpPermissions(record.EgressRules),
	}

	sg.Tags = utils.MapToEC2Tags(record.Tags)

	return sg
}

// sgParseMode selects whether ipPermissionsToSGRules mints a RuleId for each
// parsed rule. Authorize does; Revoke matches on rule identity instead.
type sgParseMode int

const (
	sgParseAuthorize sgParseMode = iota
	sgParseRevoke
)

// ipPermissionsToSGRules converts AWS IpPermission slice to SGRule slice,
// normalising IpProtocol to its canonical name and validating every
// tenant-supplied CidrIp/CidrIpv6/SourceSG.
func ipPermissionsToSGRules(perms []*ec2.IpPermission, mode sgParseMode) ([]SGRule, error) {
	var rules []SGRule
	for _, perm := range perms {
		if perm == nil {
			continue
		}

		raw := ""
		if perm.IpProtocol != nil {
			raw = *perm.IpProtocol
		}
		proto, err := normalizeIPProtocol(raw)
		if err != nil {
			return nil, err
		}

		var fromPort, toPort int64
		if perm.FromPort != nil {
			fromPort = *perm.FromPort
		}
		if perm.ToPort != nil {
			toPort = *perm.ToPort
		}

		appended := false
		for _, ipRange := range perm.IpRanges {
			if ipRange.CidrIp == nil {
				continue
			}
			r := SGRule{IpProtocol: proto, FromPort: fromPort, ToPort: toPort, CidrIp: *ipRange.CidrIp}
			if ipRange.Description != nil {
				r.Description = *ipRange.Description
			}
			if err := validateSGRule(r); err != nil {
				return nil, err
			}
			if mode == sgParseAuthorize {
				r.RuleId = utils.GenerateResourceID("sgr")
			}
			rules = append(rules, r)
			appended = true
		}

		for _, pair := range perm.UserIdGroupPairs {
			if pair.GroupId == nil {
				continue
			}
			r := SGRule{IpProtocol: proto, FromPort: fromPort, ToPort: toPort, SourceSG: *pair.GroupId}
			if pair.Description != nil {
				r.Description = *pair.Description
			}
			if err := validateSGRule(r); err != nil {
				return nil, err
			}
			if mode == sgParseAuthorize {
				r.RuleId = utils.GenerateResourceID("sgr")
			}
			rules = append(rules, r)
			appended = true
		}

		for _, r6 := range perm.Ipv6Ranges {
			if r6 == nil || r6.CidrIpv6 == nil {
				continue
			}
			r := SGRule{IpProtocol: proto, FromPort: fromPort, ToPort: toPort, CidrIpv6: *r6.CidrIpv6}
			if r6.Description != nil {
				r.Description = *r6.Description
			}
			if err := validateSGRule(r); err != nil {
				return nil, err
			}
			if mode == sgParseAuthorize {
				r.RuleId = utils.GenerateResourceID("sgr")
			}
			rules = append(rules, r)
			appended = true
		}

		if !appended {
			return nil, errors.New("IpPermission must specify at least one IpRange, Ipv6Range or UserIdGroupPair")
		}
	}
	return rules, nil
}

// sgRulesToIpPermissions converts SGRule slice to AWS IpPermission slice.
func sgRulesToIpPermissions(rules []SGRule) []*ec2.IpPermission {
	// Group rules by protocol+port range
	type permKey struct {
		IpProtocol string
		FromPort   int64
		ToPort     int64
	}

	grouped := make(map[permKey]*ec2.IpPermission)
	for _, rule := range rules {
		key := permKey{IpProtocol: rule.IpProtocol, FromPort: rule.FromPort, ToPort: rule.ToPort}
		perm, exists := grouped[key]
		if !exists {
			perm = &ec2.IpPermission{
				IpProtocol: aws.String(rule.IpProtocol),
				FromPort:   aws.Int64(rule.FromPort),
				ToPort:     aws.Int64(rule.ToPort),
			}
			grouped[key] = perm
		}

		if rule.CidrIp != "" {
			ipRange := &ec2.IpRange{CidrIp: aws.String(rule.CidrIp)}
			if rule.Description != "" {
				ipRange.Description = aws.String(rule.Description)
			}
			perm.IpRanges = append(perm.IpRanges, ipRange)
		}
		if rule.CidrIpv6 != "" {
			ipRange := &ec2.Ipv6Range{CidrIpv6: aws.String(rule.CidrIpv6)}
			if rule.Description != "" {
				ipRange.Description = aws.String(rule.Description)
			}
			perm.Ipv6Ranges = append(perm.Ipv6Ranges, ipRange)
		}
		if rule.SourceSG != "" {
			pair := &ec2.UserIdGroupPair{GroupId: aws.String(rule.SourceSG)}
			if rule.Description != "" {
				pair.Description = aws.String(rule.Description)
			}
			perm.UserIdGroupPairs = append(perm.UserIdGroupPairs, pair)
		}
	}

	result := make([]*ec2.IpPermission, 0, len(grouped))
	for _, perm := range grouped {
		result = append(result, perm)
	}
	return result
}

// assertRulesPresent returns InvalidPermission.NotFound if any rule in
// toRevoke is absent from existing, matching AWS's non-idempotent revoke
// behavior.
func assertRulesPresent(existing, toRevoke []SGRule) error {
	if len(toRevoke) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(existing))
	for _, r := range existing {
		have[sgRuleKey(r)] = struct{}{}
	}
	for _, r := range toRevoke {
		if _, ok := have[sgRuleKey(r)]; !ok {
			return errors.New(awserrors.ErrorInvalidPermissionNotFound)
		}
	}
	return nil
}

// removedSGRuleIDs names the stored rules a revoke will drop. A revoke request
// carries permissions rather than IDs, so the IDs whose tag entries have to go
// with them can only be read off the stored rules before they are removed.
func removedSGRuleIDs(existing, toRemove []SGRule) []string {
	removeSet := make(map[string]bool, len(toRemove))
	for _, r := range toRemove {
		removeSet[sgRuleKey(r)] = true
	}

	var ids []string
	for _, r := range existing {
		if removeSet[sgRuleKey(r)] {
			ids = append(ids, r.RuleId)
		}
	}
	return ids
}

// removeSGRules removes matching rules from the existing set.
func removeSGRules(existing, toRemove []SGRule) []SGRule {
	removeSet := make(map[string]bool)
	for _, r := range toRemove {
		removeSet[sgRuleKey(r)] = true
	}

	var result []SGRule
	for _, r := range existing {
		if !removeSet[sgRuleKey(r)] {
			result = append(result, r)
		}
	}
	return result
}

// sgRuleKey returns a string key for deduplication/matching of SG rules.
func sgRuleKey(r SGRule) string {
	return fmt.Sprintf("%s:%d:%d:%s:%s:%s", r.IpProtocol, r.FromPort, r.ToPort, r.CidrIp, r.CidrIpv6, r.SourceSG)
}

// resolveRuleIDsToRemove maps SecurityGroupRuleIds to the matching rules in the
// SG's existing set. Modern Terraform (aws_vpc_security_group_ingress_rule)
// revokes per-rule resources by SecurityGroupRuleId with empty IpPermissions;
// without this the revoke silently no-ops and the leftover rule pins the SG
// undeletable (DependencyViolation) when it references a peer SG.
func resolveRuleIDsToRemove(existing []SGRule, ruleIDs []*string) ([]SGRule, error) {
	if len(ruleIDs) == 0 {
		return nil, nil
	}
	byID := make(map[string]SGRule, len(existing))
	for _, r := range existing {
		if r.RuleId != "" {
			byID[r.RuleId] = r
		}
	}
	out := make([]SGRule, 0, len(ruleIDs))
	for _, idp := range ruleIDs {
		if idp == nil || *idp == "" {
			continue
		}
		if !SGRuleIDRegex.MatchString(*idp) {
			return nil, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdMalformed)
		}
		r, ok := byID[*idp]
		if !ok {
			return nil, errors.New(awserrors.ErrorInvalidSecurityGroupRuleIdNotFound)
		}
		out = append(out, r)
	}
	return out, nil
}

// vpcdSGEventTimeout bounds the synchronous vpcd round-trip for SG events.
const vpcdSGEventTimeout = 5 * time.Second

// requestSGEvent sends a security group lifecycle event to vpcd via
// request-reply and surfaces vpcd-side failures to the API caller.
func (s *VPCServiceImpl) requestSGEvent(topic string, evt SGEvent) error {
	return utils.RequestEvent(s.natsConn, topic, evt, vpcdSGEventTimeout)
}

// createDefaultSecurityGroupInternal provisions the per-VPC default SG with
// AWS-equivalent rules, bypassing the public-API reserved-name guard. Used by
// CreateVpc.
func (s *VPCServiceImpl) createDefaultSecurityGroupInternal(ctx context.Context, accountID, vpcId string) (string, error) {
	record, _, err := s.storeDefaultSecurityGroup(ctx, accountID, vpcId, utils.GenerateResourceID("sg"))
	if err != nil {
		return "", err
	}
	if err := s.announceDefaultSecurityGroup(record); err != nil {
		return "", err
	}
	return record.GroupId, nil
}

// storeDefaultSecurityGroup creates the default SG record under groupId. It
// never overwrites: created is false when the record already exists.
func (s *VPCServiceImpl) storeDefaultSecurityGroup(ctx context.Context, accountID, vpcId, groupId string) (record *SecurityGroupRecord, created bool, err error) {
	record = &SecurityGroupRecord{
		GroupId:     groupId,
		GroupName:   defaultSecurityGroupName,
		Description: defaultSecurityGroupDescription,
		VpcId:       vpcId,
		IngressRules: []SGRule{
			{RuleId: utils.GenerateResourceID("sgr"), IpProtocol: "-1", FromPort: 0, ToPort: 0, SourceSG: groupId},
		},
		EgressRules: []SGRule{
			{RuleId: utils.GenerateResourceID("sgr"), IpProtocol: "-1", FromPort: 0, ToPort: 0, CidrIp: "0.0.0.0/0"},
		},
		Tags:      map[string]string{},
		IsDefault: true,
		CreatedAt: time.Now(),
	}

	data, err := json.Marshal(record)
	if err != nil {
		return nil, false, fmt.Errorf("marshal default security group: %w", err)
	}
	if _, err := s.sgKV.Create(ctx, utils.AccountKey(accountID, groupId), data); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return record, false, nil
		}
		return nil, false, fmt.Errorf("store default security group: %w", err)
	}

	slog.InfoContext(ctx, "Created default security group", "groupId", groupId, "vpcId", vpcId, "accountID", accountID)
	return record, true, nil
}

// announceDefaultSecurityGroup asks vpcd to build the port group for a newly
// stored default SG.
func (s *VPCServiceImpl) announceDefaultSecurityGroup(record *SecurityGroupRecord) error {
	if err := s.requestSGEvent("vpc.create-sg", SGEvent{
		GroupId:      record.GroupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		return fmt.Errorf("vpcd vpc.create-sg: %w", err)
	}
	return nil
}

// deleteSecurityGroupInternal removes an SG record without the public-API
// CannotDelete guard for default SGs. Used by DeleteVpc to cascade-delete the
// per-VPC default SG. Surfaces vpcd tear-down failures to the caller.
func (s *VPCServiceImpl) deleteSecurityGroupInternal(ctx context.Context, accountID, groupId string) error {
	key := utils.AccountKey(accountID, groupId)
	entry, err := s.sgKV.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("read default security group: %w", err)
	}
	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return fmt.Errorf("unmarshal default security group: %w", err)
	}
	if err := s.sgKV.Delete(ctx, key); err != nil {
		return fmt.Errorf("delete default security group: %w", err)
	}
	// The cascade from DeleteVpc reaches the default SG only through here, so
	// without this the one group the caller never deletes by hand is the one
	// that always leaks its tags.
	s.clearRecordTags(ctx, accountID, groupId)

	return s.requestSGEvent("vpc.delete-sg", SGEvent{
		GroupId: groupId,
		VpcId:   record.VpcId,
	})
}

// FindDefaultSGForVPC scans the account's SG bucket for the SG with
// IsDefault=true and the given VPC. Returns "" if none found (only happens
// when CreateVpc failed mid-flow and left the VPC without a default SG).
func (s *VPCServiceImpl) FindDefaultSGForVPC(accountID, vpcId string) (string, error) {
	return s.findDefaultSGForVPC(context.Background(), accountID, vpcId)
}

func (s *VPCServiceImpl) findDefaultSGForVPC(ctx context.Context, accountID, vpcId string) (string, error) {
	prefix := accountID + "."
	keys, err := s.sgKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return "", nil
		}
		return "", err
	}
	for _, k := range keys {
		if k == utils.VersionKey || !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.sgKV.Get(ctx, k)
		if err != nil {
			continue
		}
		var rec SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			continue
		}
		if rec.IsDefault && rec.VpcId == vpcId {
			return rec.GroupId, nil
		}
	}
	return "", nil
}
