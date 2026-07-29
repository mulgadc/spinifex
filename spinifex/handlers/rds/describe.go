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
		InstanceCreateTime:   aws.Time(rec.CreatedAt),
	}
	if rec.DBName != "" {
		out.DBName = aws.String(rec.DBName)
	}
	if rec.VpcID != "" {
		out.DBSubnetGroup = &rds.DBSubnetGroup{VpcId: aws.String(rec.VpcID)}
	}
	for _, groupID := range rec.VpcSecurityGroupIDs {
		out.VpcSecurityGroups = append(out.VpcSecurityGroups, &rds.VpcSecurityGroupMembership{
			VpcSecurityGroupId: aws.String(groupID),
			Status:             aws.String("active"),
		})
	}
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
