package handlers_rds

import "fmt"

// RDS separates resource type and name with a colon, unlike the slash-separated
// ECS/EKS ARNs, so resource-scoped IAM policies and Terraform state round-trip
// against the real service.

func DBInstanceARN(region, accountID, dbInstanceIdentifier string) string {
	return fmt.Sprintf("arn:aws:rds:%s:%s:db:%s", region, accountID, dbInstanceIdentifier)
}

// Manual and automated snapshots share the one resource type.
func DBSnapshotARN(region, accountID, dbSnapshotIdentifier string) string {
	return fmt.Sprintf("arn:aws:rds:%s:%s:snapshot:%s", region, accountID, dbSnapshotIdentifier)
}

func DBSubnetGroupARN(region, accountID, name string) string {
	return fmt.Sprintf("arn:aws:rds:%s:%s:subgrp:%s", region, accountID, name)
}

func DBParameterGroupARN(region, accountID, name string) string {
	return fmt.Sprintf("arn:aws:rds:%s:%s:pg:%s", region, accountID, name)
}
