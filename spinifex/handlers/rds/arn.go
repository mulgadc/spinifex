package handlers_rds

import (
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// RDS separates resource type and name with a colon, unlike the slash-separated
// ECS/EKS ARNs, so resource-scoped IAM policies and Terraform state round-trip
// against the real service.

func DBInstanceARN(region, accountID, dbInstanceIdentifier string) string {
	return FormatARN(ResourceKindDBInstance, region, accountID, dbInstanceIdentifier)
}

// Manual and automated snapshots share the one resource type.
func DBSnapshotARN(region, accountID, dbSnapshotIdentifier string) string {
	return FormatARN(ResourceKindDBSnapshot, region, accountID, dbSnapshotIdentifier)
}

func DBSubnetGroupARN(region, accountID, name string) string {
	return FormatARN(ResourceKindDBSubnetGroup, region, accountID, name)
}

func DBParameterGroupARN(region, accountID, name string) string {
	return FormatARN(ResourceKindDBParameterGroup, region, accountID, name)
}

// The one place the ARN shape is written, for callers that already hold a kind
// rather than a resource type of their own.
func FormatARN(kind ResourceKind, region, accountID, identifier string) string {
	return fmt.Sprintf("arn:aws:rds:%s:%s:%s:%s", region, accountID, kind, identifier)
}

// The resource-type segment of an RDS ARN, and the key the tag registry and
// resource-scoped authorization dispatch on.
type ResourceKind string

const (
	ResourceKindDBInstance       ResourceKind = "db"
	ResourceKindDBSnapshot       ResourceKind = "snapshot"
	ResourceKindDBSubnetGroup    ResourceKind = "subgrp"
	ResourceKindDBParameterGroup ResourceKind = "pg"
)

func validResourceKind(kind ResourceKind) bool {
	switch kind {
	case ResourceKindDBInstance, ResourceKindDBSnapshot, ResourceKindDBSubnetGroup, ResourceKindDBParameterGroup:
		return true
	default:
		return false
	}
}

// An ARN split into the parts a caller acts on. Partition and service are
// validated rather than returned: only "arn:aws:rds" is ever accepted.
type ParsedARN struct {
	Region     string
	AccountID  string
	Kind       ResourceKind
	Identifier string
}

// arn:aws:rds:{region}:{accountID}:{kind}:{identifier}
const arnSegmentCount = 7

// Parses one of the four supported RDS ARNs and validates it belongs to the caller.
// Region and account are checked here rather than at policy evaluation, so a
// foreign-account reference never reaches the evaluator at all.
func ParseARN(arn, region, accountID string) (ParsedARN, error) {
	// SplitN leaves an automated snapshot's rds: prefix in the identifier. Other
	// resource kinds still reject that extra separator below.
	parts := strings.SplitN(arn, ":", arnSegmentCount)
	if len(parts) != arnSegmentCount {
		return ParsedARN{}, arnError(arn, "expected the form arn:aws:rds:{region}:{account}:{type}:{name}")
	}
	if parts[0] != "arn" || parts[1] != "aws" || parts[2] != "rds" {
		return ParsedARN{}, arnError(arn, "only arn:aws:rds resources are addressable here")
	}
	if parts[3] != region {
		return ParsedARN{}, arnError(arn, fmt.Sprintf("region %q does not match this endpoint's region %q", parts[3], region))
	}
	if parts[4] != accountID {
		return ParsedARN{}, arnError(arn, "the resource belongs to another account")
	}

	kind := ResourceKind(parts[5])
	if !validResourceKind(kind) {
		return ParsedARN{}, arnError(arn, fmt.Sprintf("%q is not an RDS resource type", parts[5]))
	}
	identifier := parts[6]
	malformed := identifier == "" || strings.Contains(identifier, "/")
	if kind == ResourceKindDBSnapshot {
		malformed = malformed || validateDBSnapshotReference(identifier) != nil
	} else {
		malformed = malformed || strings.Contains(identifier, ":")
	}
	if malformed {
		return ParsedARN{}, arnError(arn, "the resource name is empty or malformed")
	}

	return ParsedARN{Region: parts[3], AccountID: parts[4], Kind: kind, Identifier: identifier}, nil
}

func arnError(arn, why string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%q is not a valid RDS ARN: %s", arn, why)
}
