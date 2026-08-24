package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go/jetstream"
)

// AWS's own name rules. The name is a KV key rather than a DNS label, so the
// looser AWS character set is accepted as-is — but "default" is reserved.
const (
	maxDBGroupNameLen        = 255
	maxDBGroupDescriptionLen = 255
	// AWS's cap on the subnets one group may hold.
	maxSubnetsPerGroup = 20
)

// The status AWS reports on a usable group. There is no asynchronous
// provisioning behind a subnet group here, so it is complete the moment it is
// written.
const subnetGroupStatusComplete = "Complete"

// AWS requires a DB subnet group to span at least two AZs. This platform is
// single-AZ — every subnet reports the one zone — and multi-AZ is a V2
// milestone, so the AZ-count rule is not enforced: rejecting on it would fail
// every stock Terraform module for no safety benefit. The constraint that does
// matter is enforced below, because a group spanning two VPCs cannot host an
// instance at all.
func (s *Service) CreateDBSubnetGroup(ctx context.Context, input *rds.CreateDBSubnetGroupInput, accountID string) (*rds.CreateDBSubnetGroupOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	name := aws.StringValue(input.DBSubnetGroupName)
	if err := validateDBGroupName("DBSubnetGroupName", name); err != nil {
		return nil, err
	}
	description := aws.StringValue(input.DBSubnetGroupDescription)
	if err := validateDBGroupDescription("DBSubnetGroupDescription", description); err != nil {
		return nil, err
	}
	tags, err := validateTags(input.Tags)
	if err != nil {
		return nil, err
	}

	subnets, vpcID, err := s.resolveGroupSubnets(ctx, accountID, aws.StringValueSlice(input.SubnetIds))
	if err != nil {
		return nil, err
	}

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := DBSubnetGroupRecord{
		Name:        name,
		AccountID:   accountID,
		Description: description,
		Subnets:     subnets,
		VpcID:       vpcID,
		Tags:        tags,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// Create rather than Put, so two concurrent creates of one name have exactly
	// one winner instead of both reporting success over each other's subnets.
	if err := createJSON(ctx, kv, DBSubnetGroupKey(name), &rec); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, awserrors.Errorf(awserrors.ErrorDBSubnetGroupAlreadyExists,
				"DB subnet group %s already exists", name)
		}
		return nil, err
	}

	slog.InfoContext(ctx, "rds: DB subnet group created",
		"dbSubnetGroup", name, "accountId", accountID, "vpcId", vpcID, "subnets", len(subnets))
	return &rds.CreateDBSubnetGroupOutput{DBSubnetGroup: s.projectSubnetGroup(&rec)}, nil
}

// A named group that does not exist is an error, matching AWS; an unnamed
// request lists the account's groups.
func (s *Service) DescribeDBSubnetGroups(ctx context.Context, input *rds.DescribeDBSubnetGroupsInput, accountID string) (*rds.DescribeDBSubnetGroupsOutput, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if input != nil {
		if name := aws.StringValue(input.DBSubnetGroupName); name != "" {
			rec, _, err := getDBSubnetGroup(ctx, kv, name)
			if err != nil {
				return nil, err
			}
			return &rds.DescribeDBSubnetGroupsOutput{DBSubnetGroups: []*rds.DBSubnetGroup{s.projectSubnetGroup(rec)}}, nil
		}
	}

	names, err := listNames(ctx, kv, DBSubnetGroupsPrefix())
	if err != nil {
		return nil, err
	}
	slices.Sort(names)

	groups := make([]*rds.DBSubnetGroup, 0, len(names))
	for _, name := range names {
		var rec DBSubnetGroupRecord
		found, err := getJSON(ctx, kv, DBSubnetGroupKey(name), &rec)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		groups = append(groups, s.projectSubnetGroup(&rec))
	}
	return &rds.DescribeDBSubnetGroupsOutput{DBSubnetGroups: groups}, nil
}

// Refused while any instance still names the group, including one that is only
// deleting: releasing it early would let a teardown lose the record of where its
// ENI was placed, and would make destroy ordering ambiguous.
func (s *Service) DeleteDBSubnetGroup(ctx context.Context, input *rds.DeleteDBSubnetGroupInput, accountID string) (*rds.DeleteDBSubnetGroupOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	name := aws.StringValue(input.DBSubnetGroupName)
	if name == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBSubnetGroupName is required")
	}

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if _, _, err := getDBSubnetGroup(ctx, kv, name); err != nil {
		return nil, err
	}
	users, err := instancesUsingGroup(ctx, kv, func(rec *DBInstanceRecord) bool {
		return rec.DBSubnetGroupName == name
	})
	if err != nil {
		return nil, err
	}
	if len(users) > 0 {
		return nil, awserrors.Errorf(awserrors.ErrorDBSubnetGroupInvalidState,
			"DB subnet group %s is still used by %s", name, strings.Join(users, ", "))
	}

	if err := kv.Delete(ctx, DBSubnetGroupKey(name)); err != nil {
		return nil, fmt.Errorf("rds: delete DB subnet group %s: %w", name, err)
	}
	slog.InfoContext(ctx, "rds: DB subnet group deleted", "dbSubnetGroup", name, "accountId", accountID)
	return &rds.DeleteDBSubnetGroupOutput{}, nil
}

// Validates the requested subnets against the caller's own account and returns
// them with the AZ each subnet itself records. The describe is issued as the
// caller, so a subnet in another account simply does not come back and is
// reported as not found rather than leaked.
func (s *Service) resolveGroupSubnets(ctx context.Context, accountID string, requested []string) ([]DBSubnetGroupSubnet, string, error) {
	if s.deps.Network == nil {
		return nil, "", awserrors.Errorf(awserrors.ErrorServerInternal, "RDS networking is not wired on this node")
	}
	switch {
	case len(requested) == 0:
		return nil, "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"SubnetIds must name at least one subnet")
	case len(requested) > maxSubnetsPerGroup:
		return nil, "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"a DB subnet group may hold at most %d subnets, got %d", maxSubnetsPerGroup, len(requested))
	}

	out, err := s.deps.Network.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: aws.StringSlice(requested),
	}, accountID)
	if err != nil {
		return nil, "", fmt.Errorf("rds: describe the subnets of the DB subnet group: %w", err)
	}

	found := map[string]*ec2.Subnet{}
	if out != nil {
		for _, subnet := range out.Subnets {
			if id := aws.StringValue(subnet.SubnetId); id != "" {
				found[id] = subnet
			}
		}
	}

	vpcID := ""
	subnets := make([]DBSubnetGroupSubnet, 0, len(requested))
	seen := make(map[string]bool, len(requested))
	for _, id := range requested {
		if seen[id] {
			return nil, "", awserrors.Errorf(awserrors.ErrorDBSubnetInvalid, "subnet %s is named more than once", id)
		}
		seen[id] = true

		subnet, ok := found[id]
		if !ok {
			return nil, "", awserrors.Errorf(awserrors.ErrorDBSubnetInvalid,
				"subnet %s does not exist in this account", id)
		}
		subnetVPC := aws.StringValue(subnet.VpcId)
		if vpcID == "" {
			vpcID = subnetVPC
		}
		if subnetVPC != vpcID {
			return nil, "", awserrors.Errorf(awserrors.ErrorDBSubnetInvalid,
				"subnet %s is in VPC %s while the group's other subnets are in %s; "+
					"a DB subnet group must span one VPC", id, subnetVPC, vpcID)
		}
		// Read off the subnet rather than stamped from the platform's single zone,
		// so the response needs no change when V2 makes AZs real.
		subnets = append(subnets, DBSubnetGroupSubnet{
			SubnetID:         id,
			AvailabilityZone: aws.StringValue(subnet.AvailabilityZone),
		})
	}
	if vpcID == "" {
		return nil, "", awserrors.Errorf(awserrors.ErrorDBSubnetInvalid, "the named subnets report no VPC")
	}
	return subnets, vpcID, nil
}

// The record plus its revision. A missing group raises AWS's own fault, so a
// well-formed name that resolves to nothing is distinguishable from a bad one.
func getDBSubnetGroup(ctx context.Context, kv jetstream.KeyValue, name string) (*DBSubnetGroupRecord, uint64, error) {
	var rec DBSubnetGroupRecord
	rev, found, err := getJSONRevision(ctx, kv, DBSubnetGroupKey(name), &rec)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, awserrors.Errorf(awserrors.ErrorDBSubnetGroupNotFound, "DB subnet group %s not found", name)
	}
	return &rec, rev, nil
}

// The identifiers of every instance in the account matching uses, sorted. Both
// group deletes share it: an in-use guard that missed one instance would strand
// a live database's configuration.
func instancesUsingGroup(ctx context.Context, kv jetstream.KeyValue, uses func(*DBInstanceRecord) bool) ([]string, error) {
	ids, err := ListDBInstanceIDs(ctx, kv)
	if err != nil {
		return nil, err
	}
	slices.Sort(ids)

	var users []string
	for _, id := range ids {
		var rec DBInstanceRecord
		found, err := getJSON(ctx, kv, DBInstanceKey(id), &rec)
		if err != nil {
			return nil, err
		}
		if found && uses(&rec) {
			users = append(users, id)
		}
	}
	return users, nil
}

func (s *Service) projectSubnetGroup(rec *DBSubnetGroupRecord) *rds.DBSubnetGroup {
	if rec == nil {
		return nil
	}
	out := &rds.DBSubnetGroup{
		DBSubnetGroupName:        aws.String(rec.Name),
		DBSubnetGroupDescription: aws.String(rec.Description),
		DBSubnetGroupArn:         aws.String(FormatARN(ResourceKindDBSubnetGroup, s.region, rec.AccountID, rec.Name)),
		SubnetGroupStatus:        aws.String(subnetGroupStatusComplete),
		VpcId:                    aws.String(rec.VpcID),
	}
	for _, subnet := range rec.Subnets {
		member := &rds.Subnet{
			SubnetIdentifier: aws.String(subnet.SubnetID),
			SubnetStatus:     aws.String("Active"),
		}
		if subnet.AvailabilityZone != "" {
			member.SubnetAvailabilityZone = &rds.AvailabilityZone{Name: aws.String(subnet.AvailabilityZone)}
		}
		out.Subnets = append(out.Subnets, member)
	}
	return out
}

// Shared by both group types: AWS applies the same name rules to each, and
// "default" is reserved on both because a default parameter group is implicit.
func validateDBGroupName(field, name string) error {
	switch {
	case name == "":
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s is required", field)
	case len(name) > maxDBGroupNameLen:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s must be at most %d characters", field, maxDBGroupNameLen)
	case !isLetter(rune(name[0])):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s must begin with a letter", field)
	case strings.EqualFold(name, "default"):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s may not be \"default\", which the service reserves", field)
	}
	for _, r := range name {
		if !isLetter(r) && !isDigit(r) && r != '-' {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"%s may contain only letters, digits and hyphens", field)
		}
	}
	return nil
}

func validateDBGroupDescription(field, description string) error {
	switch {
	case description == "":
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%s is required", field)
	case len(description) > maxDBGroupDescriptionLen:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"%s must be at most %d characters", field, maxDBGroupDescriptionLen)
	}
	return nil
}
