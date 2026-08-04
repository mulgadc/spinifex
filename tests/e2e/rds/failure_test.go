//go:build e2e

package rds

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// Three missed 30s heartbeats, the 30s failure grace and a reconciler pass
	// fit inside this bound with enough room for the e2e poll intervals.
	failureDetectionBudget = 4 * time.Minute
	failureRecoveryTimeout = 5 * time.Minute
)

// TestFailureDetection takes the engine VM down without using the RDS lifecycle
// API, so the failure classifier has to detect and report the dark instance.
// Starting that same VM again proves a healthy heartbeat clears the failure.
func TestFailureDetection(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	reserveDBVMs(t, dbClass)

	id := fmt.Sprintf("%s-failure-%d", dbInstancePfx, time.Now().Unix())

	harness.Phase(t, "Creating DB instance %q", id)
	createDBInstance(t, f, id)
	client := rdsClient(t, f)
	system := f.SystemAWS(t)

	instance := waitForAvailable(t, f, id)
	conn := harness.PSQLConnFor(t, instance, dbMasterUser, dbMasterPassword, dbName)
	assert.Equal(t, "1", strings.TrimSpace(harness.PSQL(t, client, conn, "SELECT 1;")),
		"the engine must accept a connection before its VM is stopped")

	// Resolve the untagged VM while its tagged data volume still names the
	// attachment. Once stopped, that attachment is no longer a reliable lookup.
	vmID := aws.StringValue(harness.DBInstanceVM(t, system, id).InstanceId)
	require.NotEmpty(t, vmID, "the DB instance must have a backing VM")

	harness.Phase(t, "Stopping DB VM %s out of band", vmID)
	failureStarted := time.Now()
	_, err := system.EC2.StopInstances(&ec2.StopInstancesInput{
		InstanceIds: aws.StringSlice([]string{vmID}),
	})
	require.NoError(t, err, "stop DB VM out of band")
	harness.WaitForInstanceState(t, system, vmID, ec2.InstanceStateNameStopped)

	failed := harness.WaitForDBInstanceStatus(t, f.AWS, id, harness.DBInstanceFailed,
		harness.WithTimeout(failureDetectionBudget))
	detectionTime := time.Since(failureStarted)
	require.LessOrEqual(t, detectionTime, failureDetectionBudget,
		"failure detection exceeded the allowed bound")
	t.Logf("DB instance %s was classified failed after %s", id, detectionTime.Round(time.Second))

	require.Len(t, failed.StatusInfos, 1, "a failed instance must explain its status")
	statusInfo := failed.StatusInfos[0]
	assert.Equal(t, "instance", aws.StringValue(statusInfo.StatusType))
	assert.Equal(t, harness.DBInstanceFailed, aws.StringValue(statusInfo.Status))
	assert.False(t, aws.BoolValue(statusInfo.Normal))
	reason := aws.StringValue(statusInfo.Message)
	require.NotEmpty(t, reason, "a failed instance must report a reason")
	assert.Contains(t, reason, "the DB instance is not running")
	assert.Contains(t, reason, "its agent has not reported for")

	// The status write precedes the best-effort event append, so poll briefly
	// rather than racing an immediate DescribeEvents against the reconciler.
	var failureEvent *rds.Event
	harness.EventuallyErr(t, func() error {
		out, err := f.AWS.RDS.DescribeEvents(&rds.DescribeEventsInput{
			SourceType:       aws.String("db-instance"),
			SourceIdentifier: aws.String(id),
			Duration:         aws.Int64(1440),
		})
		if err != nil {
			return fmt.Errorf("describe-events %s: %w", id, err)
		}
		for _, event := range out.Events {
			message := aws.StringValue(event.Message)
			categories := aws.StringValueSlice(event.EventCategories)
			if strings.Contains(message, reason) && slices.Contains(categories, "failure") {
				failureEvent = event
				return nil
			}
		}
		return fmt.Errorf("no failure event for %s contains reason %q", id, reason)
	}, 30*time.Second, 2*time.Second)

	assert.Equal(t, id, aws.StringValue(failureEvent.SourceIdentifier))
	assert.Equal(t, "db-instance", aws.StringValue(failureEvent.SourceType))
	assert.Contains(t, aws.StringValue(failureEvent.SourceArn), ":db:"+id)
	assert.Contains(t, aws.StringValue(failureEvent.Message), "DB instance failed")
	assert.Contains(t, aws.StringValueSlice(failureEvent.EventCategories), "availability")

	harness.Phase(t, "Restarting DB VM %s out of band", vmID)
	_, err = system.EC2.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: aws.StringSlice([]string{vmID}),
	})
	require.NoError(t, err, "start DB VM out of band")
	harness.WaitForInstanceState(t, system, vmID, ec2.InstanceStateNameRunning)

	// WaitForDBInstanceAvailable fails fast on failed, which is normally useful
	// but not while this test is deliberately waiting for failed -> available.
	var recovered *rds.DBInstance
	harness.EventuallyErr(t, func() error {
		current, err := harness.DescribeDBInstance(f.AWS, id)
		if err != nil {
			return fmt.Errorf("describe-db-instances %s: %w", id, err)
		}
		if status := aws.StringValue(current.DBInstanceStatus); status != harness.DBInstanceAvailable {
			return fmt.Errorf("%s status=%s want=%s", id, status, harness.DBInstanceAvailable)
		}
		recovered = current
		return nil
	}, failureRecoveryTimeout, 10*time.Second)

	assert.Empty(t, recovered.StatusInfos, "the recovered instance must not retain a stale failure reason")
	assert.Equal(t, "1", strings.TrimSpace(harness.PSQL(t, client, conn, "SELECT 1;")),
		"the client must reconnect after the backing VM recovers")
}
