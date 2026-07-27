package handlers_rds

import "fmt"

// ARN builders for the RDS resource types (rds-v1.md D17). The shapes are AWS
// exact — RDS uses a colon separator between resource type and name, unlike the
// slash-separated ECS/EKS ARNs — so tags, resource-scoped IAM policies and
// Terraform state round-trip against the real service.

// DBInstanceARN returns the ARN of a DB instance.
func DBInstanceARN(region, accountID, dbInstanceIdentifier string) string {
	return fmt.Sprintf("arn:aws:rds:%s:%s:db:%s", region, accountID, dbInstanceIdentifier)
}

// DBSnapshotARN returns the ARN of a DB snapshot. Manual and automated
// snapshots share the one resource type.
func DBSnapshotARN(region, accountID, dbSnapshotIdentifier string) string {
	return fmt.Sprintf("arn:aws:rds:%s:%s:snapshot:%s", region, accountID, dbSnapshotIdentifier)
}

// DBSubnetGroupARN returns the ARN of a DB subnet group.
func DBSubnetGroupARN(region, accountID, name string) string {
	return fmt.Sprintf("arn:aws:rds:%s:%s:subgrp:%s", region, accountID, name)
}

// DBParameterGroupARN returns the ARN of a DB parameter group.
func DBParameterGroupARN(region, accountID, name string) string {
	return fmt.Sprintf("arn:aws:rds:%s:%s:pg:%s", region, accountID, name)
}
