package accountteardown

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/nats-io/nats.go"
)

// RDSReapers returns the RDS-backed reapers.
//
// Instances go in the compute stage alongside everything else holding a volume
// and an address. The configuration groups outlive them and cannot be removed
// while an instance still references one, so they wait for the platform stage.
func RDSReapers(nc *nats.Conn) []Reaper {
	svc := handlers_rds.NewNATSService(nc)
	return []Reaper{
		&rdsInstanceReaper{svc: svc},
		&rdsSubnetGroupReaper{svc: svc},
		&rdsParameterGroupReaper{svc: svc},
	}
}

// rdsDefaultParameterGroupPrefix marks the groups RDS synthesises per engine.
// They appear in every listing whether or not anything created them, and
// DeleteDBParameterGroup refuses them — so a reaper that offered them up would
// leave the platform stage unable to drain for any account at all.
const rdsDefaultParameterGroupPrefix = "default."

type rdsInstanceReaper struct {
	svc *handlers_rds.NATSService
}

func (r *rdsInstanceReaper) Kind() string { return "rds-instance" }
func (r *rdsInstanceReaper) Stage() Stage { return StageCompute }

func (r *rdsInstanceReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := r.svc.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{}, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, instance := range out.DBInstances {
		if instance == nil || instance.DBInstanceIdentifier == nil {
			continue
		}
		// A deleting instance still holds its VM, volume and ENI, so it stays
		// in the listing until it is actually gone rather than being counted
		// as drained the moment it was asked to go.
		found = append(found, Resource{
			Kind:   r.Kind(),
			ID:     aws.StringValue(instance.DBInstanceIdentifier),
			Detail: aws.StringValue(instance.DBInstanceStatus),
		})
	}
	return found, nil
}

// Delete always skips the final snapshot. The AWS-faithful default is to take
// one, which inside an account being deleted either forces the storage stage to
// remove it again or, if it lands after that stage, leaks a snapshot with no
// owner to attribute it to.
func (r *rdsInstanceReaper) Delete(ctx context.Context, accountID string, resource Resource, force bool) error {
	err := r.deleteInstance(ctx, accountID, resource.ID)
	if err == nil || !force || !isDeletionProtected(err) {
		return err
	}

	// Force path: deletion protection is RDS's version of the deadlock the
	// force flag exists for — the tenant is gone and nobody is left to clear
	// the flag through the ordinary API.
	if _, mErr := r.svc.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: aws.String(resource.ID),
		DeletionProtection:   aws.Bool(false),
		ApplyImmediately:     aws.Bool(true),
	}, accountID); mErr != nil {
		return ignoreAlreadyGone(mErr)
	}
	return r.deleteInstance(ctx, accountID, resource.ID)
}

func (r *rdsInstanceReaper) deleteInstance(ctx context.Context, accountID, id string) error {
	_, err := r.svc.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(id),
		SkipFinalSnapshot:    aws.Bool(true),
	}, accountID)
	return ignoreAlreadyGone(err)
}

// isDeletionProtected reports the one refusal the force path can do something
// about. Every other invalid-state answer means the instance is busy, and
// retrying the delete is what the drain loop already does.
func isDeletionProtected(err error) bool {
	return err != nil && strings.Contains(err.Error(), "deletion protection is enabled")
}

type rdsSubnetGroupReaper struct {
	svc *handlers_rds.NATSService
}

func (r *rdsSubnetGroupReaper) Kind() string { return "rds-subnet-group" }
func (r *rdsSubnetGroupReaper) Stage() Stage { return StagePlatform }

func (r *rdsSubnetGroupReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := r.svc.DescribeDBSubnetGroups(ctx, &rds.DescribeDBSubnetGroupsInput{}, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, group := range out.DBSubnetGroups {
		if group == nil || group.DBSubnetGroupName == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: aws.StringValue(group.DBSubnetGroupName)})
	}
	return found, nil
}

func (r *rdsSubnetGroupReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteDBSubnetGroup(ctx, &rds.DeleteDBSubnetGroupInput{
		DBSubnetGroupName: aws.String(resource.ID),
	}, accountID)
	return ignoreAlreadyGone(err)
}

type rdsParameterGroupReaper struct {
	svc *handlers_rds.NATSService
}

func (r *rdsParameterGroupReaper) Kind() string { return "rds-parameter-group" }
func (r *rdsParameterGroupReaper) Stage() Stage { return StagePlatform }

func (r *rdsParameterGroupReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := r.svc.DescribeDBParameterGroups(ctx, &rds.DescribeDBParameterGroupsInput{}, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, group := range out.DBParameterGroups {
		if group == nil || group.DBParameterGroupName == nil {
			continue
		}
		name := aws.StringValue(group.DBParameterGroupName)
		if isRDSDefaultParameterGroup(name) {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: name})
	}
	return found, nil
}

func (r *rdsParameterGroupReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteDBParameterGroup(ctx, &rds.DeleteDBParameterGroupInput{
		DBParameterGroupName: aws.String(resource.ID),
	}, accountID)
	return ignoreAlreadyGone(err)
}

func isRDSDefaultParameterGroup(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), rdsDefaultParameterGroupPrefix)
}
