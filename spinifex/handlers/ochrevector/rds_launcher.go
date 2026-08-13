package handlers_ochrevector

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// rdsCreateSubject, rdsDescribeSubject and rdsDeleteSubject mirror
// handlers/rds's SubjectCreateDBInstance/SubjectDescribeDBInstances/
// SubjectDeleteDBInstance. Restated as literals rather than imported, so
// this launcher stays decoupled from the rds handler package (it talks to
// RDS only over NATS, like any other client).
const (
	rdsCreateSubject   = "rds.CreateDBInstance"
	rdsDescribeSubject = "rds.DescribeDBInstances"
	rdsDeleteSubject   = "rds.DeleteDBInstance"
)

// rdsAvailableStatus mirrors handlers/rds's StatusAvailable ("available"),
// restated for the same reason as the subjects above.
const rdsAvailableStatus = "available"

// The platform appliance's own fixed sizing: the smallest supported RDS
// instance class and the minimum allocated storage, since it serves
// control-plane vector data rather than a tenant workload.
const (
	applianceEngine           = "postgres"
	applianceInstanceClass    = "db.t3.micro"
	applianceAllocatedStorage = 20
)

// rdsCreateTimeout and rdsDescribeTimeout bound one NATS round trip each,
// mirroring handlers/rds's own NATSService client timeouts for the same
// two calls.
const (
	rdsCreateTimeout   = 5 * time.Minute
	rdsDescribeTimeout = 30 * time.Second
	rdsDeleteTimeout   = 5 * time.Minute
)

// launchPollInterval paces Launch's wait between DescribeDBInstances polls
// while the appliance instance boots toward "available". A var, not a const,
// so tests can shrink it rather than paying the real delay (mirrors
// ingest.go's ingestRetryBackoff).
var launchPollInterval = 5 * time.Second

// rdsLauncher is the real ApplianceLauncher: it provisions the platform
// Postgres appliance by NATS request to RDS's CreateDBInstance, then polls
// DescribeDBInstances until the instance reports available, bounded by the
// caller's context and timeout.
type rdsLauncher struct {
	nc      *nats.Conn
	timeout time.Duration
}

var _ ApplianceLauncher = (*rdsLauncher)(nil)

// NewRDSApplianceLauncher constructs an ApplianceLauncher over nc, bounding
// the whole launch (create-and-poll-to-available) by timeout.
func NewRDSApplianceLauncher(nc *nats.Conn, timeout time.Duration) *rdsLauncher {
	return &rdsLauncher{nc: nc, timeout: timeout}
}

// Launch provisions identifier as a new RDS DB instance under masterUsername/
// masterPassword, then waits for it to become available. It is
// identifier-idempotent: a create that fails because identifier already
// exists (Ensure's re-entrant recovery path calling Launch again for an
// identifier it already launched) is treated as "already launching", not a
// hard failure, and Launch proceeds straight to polling. The password is
// never logged.
func (l *rdsLauncher) Launch(ctx context.Context, identifier, masterUsername, masterPassword string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	if err := l.create(ctx, identifier, masterUsername, masterPassword); err != nil {
		return "", 0, err
	}
	return l.pollUntilAvailable(ctx, identifier)
}

// create sends the CreateDBInstance request. An AWS DBInstanceAlreadyExists
// error is swallowed: the caller only reaches here because it holds (or is
// resuming) the appliance's single-writer claim, so an identifier collision
// means a previous attempt already started this same launch.
func (l *rdsLauncher) create(ctx context.Context, identifier, masterUsername, masterPassword string) error {
	input := &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(identifier),
		Engine:               aws.String(applianceEngine),
		DBInstanceClass:      aws.String(applianceInstanceClass),
		AllocatedStorage:     aws.Int64(applianceAllocatedStorage),
		MasterUsername:       aws.String(masterUsername),
		MasterUserPassword:   aws.String(masterPassword),
	}
	_, err := utils.NATSRequest[rds.CreateDBInstanceOutput](ctx, l.nc, rdsCreateSubject, input, rdsCreateTimeout, utils.GlobalAccountID)
	if err != nil {
		if strings.Contains(err.Error(), awserrors.ErrorDBInstanceAlreadyExists) {
			return nil
		}
		return fmt.Errorf("ochrevector: create appliance db instance %s: %w", identifier, err)
	}
	return nil
}

// Delete removes identifier's backing RDS DB instance, skipping any final
// snapshot: the appliance is a system resource, not customer data worth
// retaining past its own teardown. Idempotent: an identifier already gone
// (or never created) is a no-op success.
func (l *rdsLauncher) Delete(ctx context.Context, identifier string) error {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	input := &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(identifier),
		SkipFinalSnapshot:    aws.Bool(true),
	}
	_, err := utils.NATSRequest[rds.DeleteDBInstanceOutput](ctx, l.nc, rdsDeleteSubject, input, rdsDeleteTimeout, utils.GlobalAccountID)
	if err != nil {
		if strings.Contains(err.Error(), awserrors.ErrorDBInstanceNotFound) {
			return nil
		}
		return fmt.Errorf("ochrevector: delete appliance db instance %s: %w", identifier, err)
	}
	return nil
}

// pollUntilAvailable polls DescribeDBInstances for identifier until it
// reports StatusAvailable with a resolved endpoint, or ctx is done.
func (l *rdsLauncher) pollUntilAvailable(ctx context.Context, identifier string) (string, int, error) {
	for {
		out, err := utils.NATSRequest[rds.DescribeDBInstancesOutput](ctx, l.nc, rdsDescribeSubject,
			&rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(identifier)}, rdsDescribeTimeout, utils.GlobalAccountID)
		if err != nil {
			return "", 0, fmt.Errorf("ochrevector: describe appliance db instance %s: %w", identifier, err)
		}
		if len(out.DBInstances) == 1 {
			inst := out.DBInstances[0]
			if aws.StringValue(inst.DBInstanceStatus) == rdsAvailableStatus &&
				inst.Endpoint != nil && aws.StringValue(inst.Endpoint.Address) != "" {
				return aws.StringValue(inst.Endpoint.Address), int(aws.Int64Value(inst.Endpoint.Port)), nil
			}
		}
		select {
		case <-ctx.Done():
			return "", 0, fmt.Errorf("ochrevector: wait for appliance db instance %s to become available: %w", identifier, ctx.Err())
		case <-time.After(launchPollInterval):
		}
	}
}
