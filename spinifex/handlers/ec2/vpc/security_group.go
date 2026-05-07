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

// validateSGRule rejects values that could break out of an OVN ACL match-expression token.
// CidrIp must be IPv4 and round-trip to canonical form (so "10.0.0.5/8" with host bits set is
// rejected, as is anything containing operators/whitespace that net.ParseCIDR would not accept).
// IPv6 is rejected because the ACL builder in vpcd/acl.go is IPv4-only — accepting an IPv6 CIDR
// would persist a rule that OVN can never program.
// SourceSG must match the spinifex SG-ID format. At least one source must be specified.
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
	KVBucketSecurityGroupsVersion = 1

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
type SGRule struct {
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

	// Verify VPC exists
	if _, err := s.vpcKV.Get(utils.AccountKey(accountID, vpcId)); err != nil {
		return nil, errors.New(awserrors.ErrorInvalidVpcIDNotFound)
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
			continue
		}
		var existing SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &existing); err != nil {
			continue
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
		{IpProtocol: "-1", FromPort: 0, ToPort: 0, CidrIp: "0.0.0.0/0"},
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

	// Publish vpc.create-sg event for vpcd
	s.publishSGEvent("vpc.create-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        vpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	})

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
		return nil, errors.New(awserrors.ErrorInvalidGroupNotFound)
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

	// Publish vpc.delete-sg event for vpcd
	s.publishSGEvent("vpc.delete-sg", SGEvent{
		GroupId: groupId,
		VpcId:   record.VpcId,
	})

	return &ec2.DeleteSecurityGroupOutput{}, nil
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
			continue
		}
		var eni ENIRecord
		if err := json.Unmarshal(entry.Value(), &eni); err != nil {
			continue
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
			continue
		}
		var other SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &other); err != nil {
			continue
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
	var groups []*ec2.SecurityGroup

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

	newRules, err := ipPermissionsToSGRules(input.IpPermissions)
	if err != nil {
		slog.Warn("AuthorizeSecurityGroupIngress: invalid rule", "groupId", groupId, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	for _, nr := range newRules {
		if slices.Contains(record.IngressRules, nr) {
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

	// Publish vpc.update-sg event for vpcd
	s.publishSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	})

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

	newRules, err := ipPermissionsToSGRules(input.IpPermissions)
	if err != nil {
		slog.Warn("AuthorizeSecurityGroupEgress: invalid rule", "groupId", groupId, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	for _, nr := range newRules {
		if slices.Contains(record.EgressRules, nr) {
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

	s.publishSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	})

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

	revokeRules, err := ipPermissionsToSGRules(input.IpPermissions)
	if err != nil {
		slog.Warn("RevokeSecurityGroupIngress: invalid rule", "groupId", groupId, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
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

	s.publishSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	})

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

	revokeRules, err := ipPermissionsToSGRules(input.IpPermissions)
	if err != nil {
		slog.Warn("RevokeSecurityGroupEgress: invalid rule", "groupId", groupId, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
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

	s.publishSGEvent("vpc.update-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        record.VpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	})

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

// ipPermissionsToSGRules converts AWS IpPermission slice to SGRule slice, validating
// every tenant-supplied CidrIp/SourceSG. This is the only path that constructs SGRule
// from external input — validating here makes it impossible for a future handler to
// bypass the check.
func ipPermissionsToSGRules(perms []*ec2.IpPermission) ([]SGRule, error) {
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
			rules = append(rules, r)
			appended = true
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

// publishSGEvent publishes a security group lifecycle event to NATS for vpcd consumption.
func (s *VPCServiceImpl) publishSGEvent(topic string, evt SGEvent) {
	utils.PublishEvent(s.natsConn, topic, evt)
}

// createDefaultSecurityGroupInternal provisions the per-VPC default SG with
// AWS-equivalent rules: allow all inbound from self, allow all outbound to
// 0.0.0.0/0. Bypasses the public-API "default" reserved-name guard. Used by
// CreateVpc and EnsureDefaultVPC.
func (s *VPCServiceImpl) createDefaultSecurityGroupInternal(accountID, vpcId string) (string, error) {
	groupId := utils.GenerateResourceID("sg")
	record := SecurityGroupRecord{
		GroupId:     groupId,
		GroupName:   defaultSecurityGroupName,
		Description: defaultSecurityGroupDescription,
		VpcId:       vpcId,
		IngressRules: []SGRule{
			{IpProtocol: "-1", FromPort: 0, ToPort: 0, SourceSG: groupId},
		},
		EgressRules: []SGRule{
			{IpProtocol: "-1", FromPort: 0, ToPort: 0, CidrIp: "0.0.0.0/0"},
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

	s.publishSGEvent("vpc.create-sg", SGEvent{
		GroupId:      groupId,
		VpcId:        vpcId,
		IngressRules: record.IngressRules,
		EgressRules:  record.EgressRules,
	})
	return groupId, nil
}

// deleteSecurityGroupInternal removes an SG record without the public-API
// CannotDelete guard for default SGs. Used by DeleteVpc to cascade-delete the
// per-VPC default SG.
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
	s.publishSGEvent("vpc.delete-sg", SGEvent{
		GroupId: groupId,
		VpcId:   record.VpcId,
	})
	return nil
}

// findDefaultSGForVPC scans the account's SG bucket for the SG with
// IsDefault=true and the given VPC. Returns "" if none found (e.g., VPC still
// in pending state because default SG creation failed).
func (s *VPCServiceImpl) findDefaultSGForVPC(accountID, vpcId string) (string, error) {
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
