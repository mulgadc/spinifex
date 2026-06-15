package handlers_ec2_vpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// sgIDRegex must stay in lockstep with utils.GenerateResourceID("sg").
var sgIDRegex = regexp.MustCompile(`^sg-[0-9a-f]{17}$`)

// SGRuleIDRegex must stay in lockstep with utils.GenerateResourceID("sgr").
// Exported so the EC2 gateway can validate SecurityGroupRuleIds without
// re-implementing the format check.
var SGRuleIDRegex = regexp.MustCompile(`^sgr-[0-9a-f]{17}$`)

// validateSGRule rejects CidrIp values that are non-canonical or IPv6 (OVN ACL
// builder is IPv4-only), and SourceSG values not matching the sg-ID format.
// At least one source must be specified.
func validateSGRule(r SGRule) error {
	if r.CidrIp == "" && r.SourceSG == "" {
		return errors.New("rule must specify CidrIp or SourceSG")
	}
	if r.CidrIp != "" {
		_, ipnet, err := net.ParseCIDR(r.CidrIp)
		if err != nil {
			return fmt.Errorf("invalid CidrIp %q: %w", r.CidrIp, err)
		}
		if ipnet.IP.To4() == nil {
			return fmt.Errorf("invalid CidrIp %q: IPv6 not supported", r.CidrIp)
		}
		if ipnet.String() != r.CidrIp {
			return fmt.Errorf("invalid CidrIp %q: not canonical (expected %q)", r.CidrIp, ipnet.String())
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
	SourceSG   string `json:"source_sg,omitempty"` // Another SG ID for intra-SG rules
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
func (s *VPCServiceImpl) CreateSecurityGroup(input *ec2.CreateSecurityGroupInput, accountID string) (*ec2.CreateSecurityGroupOutput, error) {
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

	if err := s.requireVPCExists(accountID, vpcId); err != nil {
		return nil, err
	}

	// Check for duplicate group name in the same VPC and enforce the per-VPC
	// SG quota in the same bucket walk.
	prefix := accountID + "."
	sgKeys, err := s.sgKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
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
		entry, err := s.sgKV.Get(k)
		if err != nil {
			// Fail closed — a transient read error must not let a duplicate
			// SG name slip past, nor undercount the per-VPC quota.
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			slog.Warn("CreateSecurityGroup: SG read failed", "key", k, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		var existing SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &existing); err != nil {
			slog.Warn("CreateSecurityGroup: SG unmarshal failed", "key", k, "err", err)
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
	if _, err := s.sgKV.Put(utils.AccountKey(accountID, groupId), data); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateSecurityGroup completed", "groupId", groupId, "groupName", groupName, "vpcId", vpcId, "accountID", accountID)

	if err := s.requestSGEvent("vpc.create-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        vpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.Error("CreateSecurityGroup: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.CreateSecurityGroupOutput{
		GroupId: aws.String(groupId),
	}, nil
}

// DeleteSecurityGroup deletes a security group.
func (s *VPCServiceImpl) DeleteSecurityGroup(input *ec2.DeleteSecurityGroupInput, accountID string) (*ec2.DeleteSecurityGroupOutput, error) {
	if input.GroupId == nil || *input.GroupId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(key)
	if err != nil {
		// AWS-faithful: an absent security group is InvalidGroup.NotFound, not
		// success. Destroy orchestration tolerates it via awserrors.IsNotFound;
		// a transient read error stays a server error.
		if errors.Is(err, nats.ErrKeyNotFound) {
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

	if err := s.checkSGDependencies(accountID, groupId); err != nil {
		return nil, err
	}

	if err := s.sgKV.Delete(key); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("DeleteSecurityGroup completed", "groupId", groupId, "accountID", accountID)

	if err := s.requestSGEvent("vpc.delete-sg", SGEvent{
		GroupId: groupId,
		VpcId:   record.VpcId,
	}); err != nil {
		slog.Error("DeleteSecurityGroup: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.DeleteSecurityGroupOutput{}, nil
}

// validateSGRuleReferences returns InvalidGroup.NotFound if any SourceSG in
// rules is missing or belongs to a different VPC (cross-VPC refs not
// supported; AWS uses the same error for both cases).
func (s *VPCServiceImpl) validateSGRuleReferences(accountID, ownerVpcId string, rules []SGRule) error {
	for _, r := range rules {
		if r.SourceSG == "" {
			continue
		}
		entry, err := s.sgKV.Get(utils.AccountKey(accountID, r.SourceSG))
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
func (s *VPCServiceImpl) checkSGDependencies(accountID, groupId string) error {
	prefix := accountID + "."

	eniKeys, err := s.eniKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return errors.New(awserrors.ErrorServerInternal)
	}
	for _, k := range eniKeys {
		if k == utils.VersionKey || !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.eniKV.Get(k)
		if err != nil {
			// Fail closed — a transient read error must not let us delete an
			// SG that's actually still attached.
			slog.Warn("checkSGDependencies: ENI read failed", "key", k, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		var eni ENIRecord
		if err := json.Unmarshal(entry.Value(), &eni); err != nil {
			slog.Warn("checkSGDependencies: ENI unmarshal failed", "key", k, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		if slices.Contains(eni.SecurityGroupIds, groupId) {
			return errors.New(awserrors.ErrorDependencyViolation)
		}
	}

	sgKeys, err := s.sgKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return errors.New(awserrors.ErrorServerInternal)
	}
	for _, k := range sgKeys {
		if k == utils.VersionKey || !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.sgKV.Get(k)
		if err != nil {
			slog.Warn("checkSGDependencies: SG read failed", "key", k, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		var other SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &other); err != nil {
			slog.Warn("checkSGDependencies: SG unmarshal failed", "key", k, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		if other.GroupId == groupId {
			continue
		}
		for _, r := range other.IngressRules {
			if r.SourceSG == groupId {
				return errors.New(awserrors.ErrorDependencyViolation)
			}
		}
		for _, r := range other.EgressRules {
			if r.SourceSG == groupId {
				return errors.New(awserrors.ErrorDependencyViolation)
			}
		}
	}
	return nil
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
func (s *VPCServiceImpl) DescribeSecurityGroups(input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error) {
	groups := []*ec2.SecurityGroup{}

	groupIDs := make(map[string]bool)
	for _, id := range input.GroupIds {
		if id != nil {
			groupIDs[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeSecurityGroupsValidFilters)
	if err != nil {
		slog.Warn("DescribeSecurityGroups: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	prefix := accountID + "."
	keys, err := s.sgKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := s.sgKV.Get(key)
		if err != nil {
			slog.Warn("Failed to get security group record", "key", key, "error", err)
			continue
		}

		var record SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Warn("Failed to unmarshal security group record", "key", key, "error", err)
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

	slog.Info("DescribeSecurityGroups completed", "count", len(groups), "accountID", accountID)

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

// describeSecurityGroupRulesValidFilters defines the set of filter names accepted by DescribeSecurityGroupRules.
var describeSecurityGroupRulesValidFilters = map[string]bool{
	"group-id":               true,
	"security-group-rule-id": true,
	"tag-key":                true,
}

// DescribeSecurityGroupRules returns a flat list of SecurityGroupRule objects
// for the caller's account, optionally narrowed by SecurityGroupRuleIds or
// filters. MaxResults and NextToken are accepted but ignored.
func (s *VPCServiceImpl) DescribeSecurityGroupRules(input *ec2.DescribeSecurityGroupRulesInput, accountID string) (*ec2.DescribeSecurityGroupRulesOutput, error) {
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
		slog.Warn("DescribeSecurityGroupRules: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	prefix := accountID + "."
	keys, err := s.sgKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	rules := []*ec2.SecurityGroupRule{}
	emitted := make(map[string]bool)

	for _, key := range keys {
		if key == utils.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.sgKV.Get(key)
		if err != nil {
			slog.Error("DescribeSecurityGroupRules: SG read failed", "key", key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		var record SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.Error("DescribeSecurityGroupRules: SG unmarshal failed", "key", key, "err", err)
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

	slog.Info("DescribeSecurityGroupRules completed", "count", len(rules), "accountID", accountID)

	return &ec2.DescribeSecurityGroupRulesOutput{
		SecurityGroupRules: rules,
	}, nil
}

// sgRuleMatchesFilters applies the DescribeSecurityGroupRules filter set to a
// single rule. Rule-level tags are not yet persisted, so tag:* and tag-key
// filters always return false.
func sgRuleMatchesFilters(record *SecurityGroupRecord, rule SGRule, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			// rule.Tags is not yet populated; any tag:* filter excludes every rule.
			return false
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
			// rule.Tags is not yet populated; tag-key excludes every rule.
			return false
		default:
			// Unreachable: ParseFilters rejects names not in the valid set.
			// Logs an error if a future map entry lacks a matching case.
			slog.Error("sgRuleMatchesFilters: filter accepted by ParseFilters but no case", "filter", name)
			return false
		}
	}
	return true
}

// sgRuleToSecurityGroupRule flattens a stored SGRule into the AWS API shape.
// accountID supplies GroupOwnerId and ReferencedGroupInfo.UserId; VpcId is
// derived from the parent record (same-VPC references are enforced on write).
func sgRuleToSecurityGroupRule(record *SecurityGroupRecord, rule SGRule, isEgress bool, accountID string) *ec2.SecurityGroupRule {
	out := &ec2.SecurityGroupRule{
		SecurityGroupRuleId: aws.String(rule.RuleId),
		GroupId:             aws.String(record.GroupId),
		GroupOwnerId:        aws.String(accountID),
		IsEgress:            aws.Bool(isEgress),
		IpProtocol:          aws.String(rule.IpProtocol),
		FromPort:            aws.Int64(rule.FromPort),
		ToPort:              aws.Int64(rule.ToPort),
	}
	if rule.CidrIp != "" {
		out.CidrIpv4 = aws.String(rule.CidrIp)
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
func (s *VPCServiceImpl) AuthorizeSecurityGroupIngress(input *ec2.AuthorizeSecurityGroupIngressInput, accountID string) (*ec2.AuthorizeSecurityGroupIngressOutput, error) {
	if input.GroupId == nil || *input.GroupId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.requireVPCExists(accountID, record.VpcId); err != nil {
		return nil, err
	}

	newRules, err := ipPermissionsToSGRules(input.IpPermissions, sgParseAuthorize)
	if err != nil {
		slog.Warn("AuthorizeSecurityGroupIngress: invalid rule", "groupId", groupId, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := s.validateSGRuleReferences(accountID, record.VpcId, newRules); err != nil {
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
	if _, err := s.sgKV.Update(key, data, entry.Revision()); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("AuthorizeSecurityGroupIngress completed", "groupId", groupId, "newRules", len(newRules), "accountID", accountID)

	if err := s.requestSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.Error("AuthorizeSecurityGroupIngress: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.AuthorizeSecurityGroupIngressOutput{
		Return: aws.Bool(true),
	}, nil
}

// AuthorizeSecurityGroupEgress adds egress rules to a security group.
func (s *VPCServiceImpl) AuthorizeSecurityGroupEgress(input *ec2.AuthorizeSecurityGroupEgressInput, accountID string) (*ec2.AuthorizeSecurityGroupEgressOutput, error) {
	if input.GroupId == nil || *input.GroupId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.requireVPCExists(accountID, record.VpcId); err != nil {
		return nil, err
	}

	newRules, err := ipPermissionsToSGRules(input.IpPermissions, sgParseAuthorize)
	if err != nil {
		slog.Warn("AuthorizeSecurityGroupEgress: invalid rule", "groupId", groupId, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if err := s.validateSGRuleReferences(accountID, record.VpcId, newRules); err != nil {
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
	if _, err := s.sgKV.Update(key, data, entry.Revision()); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("AuthorizeSecurityGroupEgress completed", "groupId", groupId, "newRules", len(newRules), "accountID", accountID)

	if err := s.requestSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.Error("AuthorizeSecurityGroupEgress: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.AuthorizeSecurityGroupEgressOutput{
		Return: aws.Bool(true),
	}, nil
}

// RevokeSecurityGroupIngress removes ingress rules from a security group.
func (s *VPCServiceImpl) RevokeSecurityGroupIngress(input *ec2.RevokeSecurityGroupIngressInput, accountID string) (*ec2.RevokeSecurityGroupIngressOutput, error) {
	if input.GroupId == nil || *input.GroupId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	revokeRules, err := ipPermissionsToSGRules(input.IpPermissions, sgParseRevoke)
	if err != nil {
		slog.Warn("RevokeSecurityGroupIngress: invalid rule", "groupId", groupId, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(revokeRules) == 0 {
		return &ec2.RevokeSecurityGroupIngressOutput{Return: aws.Bool(true)}, nil
	}
	if err := assertRulesPresent(record.IngressRules, revokeRules); err != nil {
		return nil, err
	}
	record.IngressRules = removeSGRules(record.IngressRules, revokeRules)

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal security group record: %w", err)
	}
	if _, err := s.sgKV.Update(key, data, entry.Revision()); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("RevokeSecurityGroupIngress completed", "groupId", groupId, "revokedRules", len(revokeRules), "accountID", accountID)

	if err := s.requestSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.Error("RevokeSecurityGroupIngress: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.RevokeSecurityGroupIngressOutput{
		Return: aws.Bool(true),
	}, nil
}

// RevokeSecurityGroupEgress removes egress rules from a security group.
func (s *VPCServiceImpl) RevokeSecurityGroupEgress(input *ec2.RevokeSecurityGroupEgressInput, accountID string) (*ec2.RevokeSecurityGroupEgressOutput, error) {
	if input.GroupId == nil || *input.GroupId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	groupId := *input.GroupId
	key := utils.AccountKey(accountID, groupId)

	entry, err := s.sgKV.Get(key)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
	}

	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	revokeRules, err := ipPermissionsToSGRules(input.IpPermissions, sgParseRevoke)
	if err != nil {
		slog.Warn("RevokeSecurityGroupEgress: invalid rule", "groupId", groupId, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(revokeRules) == 0 {
		return &ec2.RevokeSecurityGroupEgressOutput{Return: aws.Bool(true)}, nil
	}
	if err := assertRulesPresent(record.EgressRules, revokeRules); err != nil {
		return nil, err
	}
	record.EgressRules = removeSGRules(record.EgressRules, revokeRules)

	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal security group record: %w", err)
	}
	if _, err := s.sgKV.Update(key, data, entry.Revision()); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("RevokeSecurityGroupEgress completed", "groupId", groupId, "revokedRules", len(revokeRules), "accountID", accountID)

	if err := s.requestSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		slog.Error("RevokeSecurityGroupEgress: vpcd request failed", "groupId", groupId, "err", err)
		return nil, err
	}

	return &ec2.RevokeSecurityGroupEgressOutput{
		Return: aws.Bool(true),
	}, nil
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

// sgParseMode selects how ipPermissionsToSGRules handles IPv6 sources.
// Authorize rejects IPv6; Revoke silently skips it (Terraform auto-revokes
// ::/0 IPv6 egress and must not abort on no-op).
type sgParseMode int

const (
	sgParseAuthorize sgParseMode = iota
	sgParseRevoke
)

// ipPermissionsToSGRules converts AWS IpPermission slice to SGRule slice,
// validating every tenant-supplied CidrIp/SourceSG. IPv6-only permissions
// error in Authorize mode and are silently skipped in Revoke mode.
func ipPermissionsToSGRules(perms []*ec2.IpPermission, mode sgParseMode) ([]SGRule, error) {
	var rules []SGRule
	for _, perm := range perms {
		if perm == nil {
			continue
		}

		proto := "-1"
		if perm.IpProtocol != nil {
			proto = *perm.IpProtocol
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
			if err := validateSGRule(r); err != nil {
				return nil, err
			}
			if mode == sgParseAuthorize {
				r.RuleId = utils.GenerateResourceID("sgr")
			}
			rules = append(rules, r)
			appended = true
		}

		hasIPv6 := false
		for _, r6 := range perm.Ipv6Ranges {
			if r6 != nil && r6.CidrIpv6 != nil {
				hasIPv6 = true
				break
			}
		}
		if hasIPv6 {
			if mode == sgParseAuthorize {
				return nil, errors.New("IPv6 rules are not supported")
			}
			if !appended {
				continue
			}
		}

		if !appended {
			return nil, errors.New("IpPermission must specify at least one IpRange or UserIdGroupPair")
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
			perm.IpRanges = append(perm.IpRanges, &ec2.IpRange{
				CidrIp: aws.String(rule.CidrIp),
			})
		}
		if rule.SourceSG != "" {
			perm.UserIdGroupPairs = append(perm.UserIdGroupPairs, &ec2.UserIdGroupPair{
				GroupId: aws.String(rule.SourceSG),
			})
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
	return fmt.Sprintf("%s:%d:%d:%s:%s", r.IpProtocol, r.FromPort, r.ToPort, r.CidrIp, r.SourceSG)
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
// CreateVpc and EnsureDefaultVPC.
func (s *VPCServiceImpl) createDefaultSecurityGroupInternal(accountID, vpcId string) (string, error) {
	groupId := utils.GenerateResourceID("sg")
	record := SecurityGroupRecord{
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
		return "", fmt.Errorf("marshal default security group: %w", err)
	}
	if _, err := s.sgKV.Put(utils.AccountKey(accountID, groupId), data); err != nil {
		return "", fmt.Errorf("store default security group: %w", err)
	}

	slog.Info("Created default security group", "groupId", groupId, "vpcId", vpcId, "accountID", accountID)

	if err := s.requestSGEvent("vpc.create-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        vpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	}); err != nil {
		return "", fmt.Errorf("vpcd vpc.create-sg: %w", err)
	}
	return groupId, nil
}

// deleteSecurityGroupInternal removes an SG record without the public-API
// CannotDelete guard for default SGs. Used by DeleteVpc to cascade-delete the
// per-VPC default SG. Surfaces vpcd tear-down failures to the caller.
func (s *VPCServiceImpl) deleteSecurityGroupInternal(accountID, groupId string) error {
	key := utils.AccountKey(accountID, groupId)
	entry, err := s.sgKV.Get(key)
	if err != nil {
		return fmt.Errorf("read default security group: %w", err)
	}
	var record SecurityGroupRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return fmt.Errorf("unmarshal default security group: %w", err)
	}
	if err := s.sgKV.Delete(key); err != nil {
		return fmt.Errorf("delete default security group: %w", err)
	}
	return s.requestSGEvent("vpc.delete-sg", SGEvent{
		GroupId: groupId,
		VpcId:   record.VpcId,
	})
}

// FindDefaultSGForVPC scans the account's SG bucket for the SG with
// IsDefault=true and the given VPC. Returns "" if none found (only happens
// when CreateVpc failed mid-flow and left the VPC without a default SG).
func (s *VPCServiceImpl) FindDefaultSGForVPC(accountID, vpcId string) (string, error) {
	prefix := accountID + "."
	keys, err := s.sgKV.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return "", nil
		}
		return "", err
	}
	for _, k := range keys {
		if k == utils.VersionKey || !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := s.sgKV.Get(k)
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
