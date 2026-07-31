package handlers_rds

import (
	"context"
	"errors"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go/jetstream"
)

// Named DB instances that do not exist are an error, matching AWS: a client
// polling a create would otherwise read an empty list as "gone" rather than
// "not ready".
func (s *Service) DescribeDBInstances(ctx context.Context, input *rds.DescribeDBInstancesInput, accountID string) (*rds.DescribeDBInstancesOutput, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}

	if input != nil {
		if id := aws.StringValue(input.DBInstanceIdentifier); id != "" {
			rec, _, err := s.getDBInstance(ctx, kv, id)
			if err != nil {
				return nil, err
			}
			return &rds.DescribeDBInstancesOutput{DBInstances: []*rds.DBInstance{s.projectDBInstance(rec)}}, nil
		}
	}

	ids, err := ListDBInstanceIDs(ctx, kv)
	if err != nil {
		return nil, err
	}
	slices.Sort(ids)

	instances := make([]*rds.DBInstance, 0, len(ids))
	for _, id := range ids {
		var rec DBInstanceRecord
		found, err := getJSON(ctx, kv, DBInstanceKey(id), &rec)
		if err != nil {
			return nil, err
		}
		// A record deleted between the key listing and this read is simply gone,
		// which is the same answer a describe one tick later would give.
		if !found {
			continue
		}
		instances = append(instances, s.projectDBInstance(&rec))
	}
	return &rds.DescribeDBInstancesOutput{DBInstances: instances}, nil
}

// Returns the record plus its revision, for callers that follow with a CAS.
func (s *Service) getDBInstance(ctx context.Context, kv jetstream.KeyValue, id string) (*DBInstanceRecord, uint64, error) {
	var rec DBInstanceRecord
	rev, found, err := getJSONRevision(ctx, kv, DBInstanceKey(id), &rec)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, errors.New(awserrors.ErrorDBInstanceNotFound)
	}
	return &rec, rev, nil
}

// The customer-facing view. Only fields this phase actually backs are set —
// an unset field is honestly absent rather than a fabricated default.
func (s *Service) projectDBInstance(rec *DBInstanceRecord) *rds.DBInstance {
	if rec == nil {
		return nil
	}
	out := &rds.DBInstance{
		DBInstanceIdentifier: aws.String(rec.DBInstanceIdentifier),
		DBInstanceArn:        aws.String(DBInstanceARN(s.region, rec.AccountID, rec.DBInstanceIdentifier)),
		DBInstanceStatus:     aws.String(string(rec.Status)),
		Engine:               aws.String(rec.Engine),
		EngineVersion:        aws.String(rec.EngineVersion),
		DBInstanceClass:      aws.String(rec.DBInstanceClass),
		AllocatedStorage:     aws.Int64(rec.AllocatedStorage),
		StorageType:          aws.String(rec.StorageType),
		StorageEncrypted:     aws.Bool(rec.StorageEncrypted),
		MasterUsername:       aws.String(rec.MasterUsername),
		MultiAZ:              aws.Bool(false),
		PubliclyAccessible:   aws.Bool(false),
		DeletionProtection:   aws.Bool(rec.DeletionProtection),
		InstanceCreateTime:   aws.Time(rec.CreatedAt),
		// The Terraform provider reads tags from the describe as well as from
		// ListTagsForResource, so the two have to agree.
		TagList: tagsToAWS(rec.Tags),
	}
	if rec.DBName != "" {
		out.DBName = aws.String(rec.DBName)
	}
	// The Terraform provider reads db_subnet_group_name off the describe, so an
	// instance placed from a named group has to report it: an empty read-back is a
	// perpetual diff on an attribute ModifyDBInstance then refuses to change.
	if rec.VpcID != "" || rec.DBSubnetGroupName != "" {
		out.DBSubnetGroup = &rds.DBSubnetGroup{}
		if rec.VpcID != "" {
			out.DBSubnetGroup.VpcId = aws.String(rec.VpcID)
		}
		if rec.DBSubnetGroupName != "" {
			out.DBSubnetGroup.DBSubnetGroupName = aws.String(rec.DBSubnetGroupName)
			out.DBSubnetGroup.SubnetGroupStatus = aws.String(subnetGroupStatusComplete)
		}
	}
	for _, groupID := range rec.VpcSecurityGroupIDs {
		out.VpcSecurityGroups = append(out.VpcSecurityGroups, &rds.VpcSecurityGroupMembership{
			VpcSecurityGroupId: aws.String(groupID),
			Status:             aws.String("active"),
		})
	}
	// Always reported, zero included: the Terraform provider reads all three back,
	// and an absent backup_retention_period on an instance with backups disabled is
	// a perpetual diff rather than an honest omission.
	out.BackupRetentionPeriod = aws.Int64(rec.BackupRetentionPeriod)
	out.PreferredBackupWindow = aws.String(s.reportedBackupWindow(rec))
	out.PreferredMaintenanceWindow = aws.String(s.reportedMaintenanceWindow(rec))
	// AWS has no dedicated failure-reason field on a DB instance, so the reason a
	// failed instance carries is reported the one place a human-readable status
	// message fits. Absent while the instance is healthy, as AWS leaves it.
	if rec.FailureReason != "" {
		out.StatusInfos = []*rds.DBInstanceStatusInfo{{
			StatusType: aws.String("instance"),
			Status:     aws.String(string(rec.Status)),
			Normal:     aws.Bool(false),
			Message:    aws.String(rec.FailureReason),
		}}
	}
	out.PendingModifiedValues = projectPendingModifiedValues(rec.PendingModifiedValues)
	out.DBParameterGroups = projectParameterGroup(rec)
	// Absent until the ENI exists: an Endpoint with no address would have a
	// client dial an empty host rather than wait for the instance to come up.
	if rec.EndpointAddress != "" {
		out.Endpoint = &rds.Endpoint{
			Address: aws.String(rec.EndpointAddress),
			Port:    aws.Int64(rec.Port),
		}
	}
	return out
}

// What a modify asked for and has not yet delivered. Nil when nothing is
// outstanding, so a client polling a deferred change sees the field appear and
// disappear rather than an empty element it has to interpret.
//
// A pending filesystem grow is deliberately absent: the volume is already at the
// new size, so the customer's AllocatedStorage is the new one and reporting it
// as still pending would show Terraform drift on a change that has landed. A
// pending parameter group is absent too — AWS reports that on the parameter
// group's own apply status, not here.
func projectPendingModifiedValues(pending *PendingModifiedValues) *rds.PendingModifiedValues {
	if pending.empty() || (pending.AllocatedStorage == nil && pending.DBInstanceClass == "") {
		return nil
	}
	out := &rds.PendingModifiedValues{}
	if pending.AllocatedStorage != nil {
		out.AllocatedStorage = aws.Int64(*pending.AllocatedStorage)
	}
	if pending.DBInstanceClass != "" {
		out.DBInstanceClass = aws.String(pending.DBInstanceClass)
	}
	return out
}

// AWS reports a parameter group's state on the membership rather than in
// PendingModifiedValues, and the Terraform provider reads it there: applying
// while a modify is draining, pending-reboot while static settings await the
// restart that adopts them (D16).
func projectParameterGroup(rec *DBInstanceRecord) []*rds.DBParameterGroupStatus {
	if rec.DBParameterGroupName == "" {
		return nil
	}
	status := "in-sync"
	switch {
	case rec.PendingModifiedValues != nil && rec.PendingModifiedValues.DBParameterGroupName != "":
		status = "applying"
	case len(rec.PendingRebootParameters) > 0:
		status = "pending-reboot"
	}
	return []*rds.DBParameterGroupStatus{{
		DBParameterGroupName: aws.String(rec.DBParameterGroupName),
		ParameterApplyStatus: aws.String(status),
	}}
}
