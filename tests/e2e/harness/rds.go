//go:build e2e

package harness

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
)

// The statuses a DB instance reports on its way up, and the one it lands in when
// the bootstrap never completes.
const (
	DBInstanceAvailable = "available"
	DBInstanceCreating  = "creating"
	DBInstanceFailed    = "failed"
)

// WaitForDBInstanceAvailable polls DescribeDBInstances until the instance
// reports available. Reaching it means the VM booted, the engine ran initdb and
// the in-guest agent reported healthy, so the default envelope is generous.
func WaitForDBInstanceAvailable(t *testing.T, c *AWSClient, id string, opts ...PollOpt) *rds.DBInstance {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 20 * time.Minute, interval: 10 * time.Second}, opts...)
	var last *rds.DBInstance
	EventuallyErr(t, func() error {
		instance, err := DescribeDBInstance(c, id)
		if err != nil {
			return fmt.Errorf("describe-db-instances %s: %w", id, err)
		}
		last = instance
		switch status := aws.StringValue(instance.DBInstanceStatus); status {
		case DBInstanceAvailable:
			return nil
		case DBInstanceFailed:
			// Terminal: the control plane already gave up waiting for the engine,
			// so polling on would only burn the rest of the timeout.
			t.Fatalf("DB instance %s entered %s", id, DBInstanceFailed)
			return nil
		default:
			return fmt.Errorf("%s status=%s want=%s", id, status, DBInstanceAvailable)
		}
	}, cfg.timeout, cfg.interval)
	t.Logf("DB instance %s reached status %s", id, DBInstanceAvailable)
	return last
}

// WaitForDBInstanceGone polls until the instance no longer exists. A group an
// instance still references cannot be deleted, and a deleting instance still
// counts, so a teardown that frees a group has to wait for the record to go.
func WaitForDBInstanceGone(t *testing.T, c *AWSClient, id string, opts ...PollOpt) {
	t.Helper()
	cfg := applyOpts(pollCfg{timeout: 10 * time.Minute, interval: 10 * time.Second}, opts...)
	EventuallyErr(t, func() error {
		instance, err := DescribeDBInstance(c, id)
		if ErrorCodeIs(err, "DBInstanceNotFound") {
			return nil
		}
		if err != nil {
			return fmt.Errorf("describe-db-instances %s: %w", id, err)
		}
		return fmt.Errorf("%s still exists with status=%s", id, aws.StringValue(instance.DBInstanceStatus))
	}, cfg.timeout, cfg.interval)
	t.Logf("DB instance %s is gone", id)
}

// DescribeDBInstance returns the single named DB instance. A named instance that
// does not exist is an error from the API, not an empty list.
func DescribeDBInstance(c *AWSClient, id string) (*rds.DBInstance, error) {
	out, err := c.RDS.DescribeDBInstances(&rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(id),
	})
	if err != nil {
		return nil, err
	}
	if len(out.DBInstances) != 1 {
		return nil, fmt.Errorf("describe %s returned %d instances, want 1", id, len(out.DBInstances))
	}
	return out.DBInstances[0], nil
}
