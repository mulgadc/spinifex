//go:build e2e

package rds

import (
	"fmt"
	"strconv"
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
	// The table the lifecycle writes once and reads after every transition: a
	// stop, a start and a reboot that lose it have not preserved the datadir.
	lifecycleTable = "e2e_lifecycle"
	lifecycleNote  = "written before the first stop"

	// A static parameter — the engine adopts it only on a restart, which is what
	// makes it the one that can be pending-reboot. Its floor is 6 and its default
	// is derived from the instance class, so a literal well above both is
	// unambiguous in SHOW.
	staticParameterName  = "max_connections"
	staticParameterValue = "137"

	// How long the failure classifier is watched for a false positive: the ladder is
	// 90s of heartbeat staleness plus a 30s grace plus one 15s reconciler pass, so
	// a window past all three is what proves a stop is not read as a failure.
	failureClassifierWindow = 3 * time.Minute
)

// TestLifecycle drives stop, start and reboot against a live engine, and the two
// paths a modify takes when it cannot land immediately: a parameter that waits
// for a restart, and a whole modify that waits for the maintenance window.
//
// The instance is consumed: every assertion here is about a transition, and the
// last one leaves it rebooted rather than pristine.
func TestLifecycle(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	reserveDBVMs(t, dbClass)

	suffix := time.Now().Unix()
	id := fmt.Sprintf("%s-lifecycle-%d", dbInstancePfx, suffix)
	paramGroup := fmt.Sprintf("%s-static-%d", dbInstancePfx, suffix)

	harness.Phase(t, "Creating DB instance %q", id)
	createDBInstance(t, f, id)
	client := rdsClient(t, f)
	system := f.SystemAWS(t)

	instance := waitForAvailable(t, f, id)
	conn := harness.PSQLConnFor(t, instance, dbMasterUser, dbMasterPassword, dbName)
	endpoint := conn.Host

	// Captured while the instance is up: the VM carries no per-instance tag, so
	// after a transition it can only be found again by an ID held from before.
	vmID := aws.StringValue(harness.DBInstanceVM(t, system, id).InstanceId)
	dataVolumeID := aws.StringValue(harness.DBInstanceDataVolume(t, system, id).VolumeId)
	privateIP := aws.StringValue(harness.DBEndpointENI(t, f.AWS, id).PrivateIpAddress)

	harness.PSQL(t, client, conn, fmt.Sprintf(
		"CREATE TABLE %s (id int primary key, note text); INSERT INTO %s VALUES (1, '%s');",
		lifecycleTable, lifecycleTable, lifecycleNote))

	// A stop keeps the data volume, the customer ENI and its address, so a
	// start comes back on the same datadir at the same endpoint. The VM is stopped
	// rather than terminated — only a class change replaces it.
	t.Run("StopKeepsTheDataAndTheAddress", func(t *testing.T) {
		_, err := f.AWS.RDS.StopDBInstance(&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(id)})
		require.NoError(t, err, "stop-db-instance")
		stopped := harness.WaitForDBInstanceStatus(t, f.AWS, id, harness.DBInstanceStopped)

		harness.WaitForInstanceState(t, system, vmID, ec2.InstanceStateNameStopped)
		assert.Equal(t, dataVolumeID, aws.StringValue(harness.DBInstanceDataVolume(t, system, id).VolumeId),
			"the data volume must be retained across a stop")
		assert.Equal(t, privateIP, aws.StringValue(harness.DBEndpointENI(t, f.AWS, id).PrivateIpAddress),
			"the customer ENI keeps its address, or a start would come back on a different endpoint")

		require.NotNil(t, stopped.Endpoint, "a stopped instance still reports where it will be when it comes back")
		assert.Equal(t, endpoint, aws.StringValue(stopped.Endpoint.Address))
	})

	// The failure classifier treats an instance whose heartbeats go stale as failed. A stop the
	// control plane itself performed is the one case where they go stale by design,
	// and reading it as a failure would page an operator for a working database.
	t.Run("AStoppedInstanceIsNotClassifiedFailed", func(t *testing.T) {
		harness.Phase(t, "Watching %q for %s in case the classifier calls a stop a failure", id, failureClassifierWindow)
		deadline := time.Now().Add(failureClassifierWindow)
		for time.Now().Before(deadline) {
			time.Sleep(15 * time.Second)
			current, err := harness.DescribeDBInstance(f.AWS, id)
			require.NoError(t, err, "describe-db-instances")
			require.Equal(t, harness.DBInstanceStopped, aws.StringValue(current.DBInstanceStatus),
				"a stopped instance must stay stopped; the failure classifier has fired on a lifecycle-owned stop")
		}
	})

	t.Run("StartComesBackOnTheSameDatadirAndAddress", func(t *testing.T) {
		_, err := f.AWS.RDS.StartDBInstance(&rds.StartDBInstanceInput{DBInstanceIdentifier: aws.String(id)})
		require.NoError(t, err, "start-db-instance")
		started := waitForAvailable(t, f, id)

		require.NotNil(t, started.Endpoint)
		assert.Equal(t, endpoint, aws.StringValue(started.Endpoint.Address), "the endpoint must survive a stop/start")
		assert.Equal(t, privateIP, aws.StringValue(harness.DBEndpointENI(t, f.AWS, id).PrivateIpAddress),
			"the private address is what the endpoint name resolves to, so it must persist")

		out := harness.PSQL(t, client, conn, fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", lifecycleTable))
		assert.Equal(t, lifecycleNote, strings.TrimSpace(out), "the row written before the stop must still be there")
	})

	t.Run("RebootRestartsTheEngineAndKeepsTheData", func(t *testing.T) {
		before := postmasterStartTime(t, client, conn)

		_, err := f.AWS.RDS.RebootDBInstance(&rds.RebootDBInstanceInput{DBInstanceIdentifier: aws.String(id)})
		require.NoError(t, err, "reboot-db-instance")
		waitForAvailable(t, f, id)

		out := harness.PSQL(t, client, conn, fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", lifecycleTable))
		assert.Equal(t, lifecycleNote, strings.TrimSpace(out), "a reboot must not lose the datadir")

		// The only assertion that separates a reboot from a no-op: an engine that
		// was never restarted reports the same start time.
		after := postmasterStartTime(t, client, conn)
		assert.NotEqual(t, before, after, "pg_postmaster_start_time() did not move, so the engine never restarted")
	})

	// A deferred attachment is drained by the maintenance window. The static
	// parameter it installs then remains pending until a separate reboot.
	t.Run("ADeferredGroupAttachmentWaitsForItsWindow", func(t *testing.T) {
		createParameterGroupWithStatic(t, f, paramGroup)

		harness.Step(t, "Moving %q onto %q, deferred to the maintenance window", id, paramGroup)
		deferred, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: aws.String(id),
			DBParameterGroupName: aws.String(paramGroup),
			ApplyImmediately:     aws.Bool(false),
		})
		require.NoError(t, err, "modify-db-instance")
		// AWS carries a pending group change on the membership rather than in
		// PendingModifiedValues, which holds only storage and class, and the
		// Terraform provider reads it there.
		require.NotEmpty(t, deferred.DBInstance.DBParameterGroups,
			"a deferred group change must be visible, or a customer cannot tell it was accepted")
		assert.Equal(t, "applying", aws.StringValue(deferred.DBInstance.DBParameterGroups[0].ParameterApplyStatus))

		// A deferred modify is drained by the maintenance window, not by a reboot,
		// so the window is moved onto the instance to open now.
		opens := time.Now().UTC().Add(time.Minute)
		_, err = f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier:       aws.String(id),
			PreferredMaintenanceWindow: aws.String(weeklyWindowAt(opens, 30*time.Minute)),
			ApplyImmediately:           aws.Bool(true),
		})
		require.NoError(t, err, "modify-db-instance maintenance window")

		harness.Phase(t, "Waiting for the maintenance window of %q to drain the deferred modify", id)
		var applied *rds.DBInstance
		harness.EventuallyErr(t, func() error {
			current, err := harness.DescribeDBInstance(f.AWS, id)
			if err != nil {
				return err
			}
			if dbParameterGroupName(current) != paramGroup {
				return fmt.Errorf("%s is still on parameter group %q", id, dbParameterGroupName(current))
			}
			if status := aws.StringValue(current.DBInstanceStatus); status != harness.DBInstanceAvailable {
				return fmt.Errorf("%s is %s", id, status)
			}
			applied = current
			return nil
		}, 10*time.Minute, 15*time.Second)

		assert.Equal(t, paramGroup, dbParameterGroupName(applied),
			"the window must leave the instance on the group the modify named")
		require.NotEmpty(t, applied.DBParameterGroups)
		assert.Equal(t, "pending-reboot", aws.StringValue(applied.DBParameterGroups[0].ParameterApplyStatus),
			"a static parameter is accepted by the engine but not in force until it restarts")

		// Still the old value: the whole point of pending-reboot is that the engine
		// is running without it.
		assert.NotEqual(t, staticParameterValue, showInteger(t, client, conn, staticParameterName),
			"a static parameter must not be in force before the restart that applies it")

		_, err = f.AWS.RDS.RebootDBInstance(&rds.RebootDBInstanceInput{DBInstanceIdentifier: aws.String(id)})
		require.NoError(t, err, "reboot-db-instance")
		rebooted := waitForAvailable(t, f, id)

		require.NotEmpty(t, rebooted.DBParameterGroups)
		assert.Equal(t, "in-sync", aws.StringValue(rebooted.DBParameterGroups[0].ParameterApplyStatus),
			"the reboot applied the pending parameters, so nothing is outstanding")
		assert.Equal(t, staticParameterValue, showInteger(t, client, conn, staticParameterName),
			"the engine must be running the static parameter after the reboot that applied it")
	})

	// The events page is the operator's account of what the control plane did, and
	// a lifecycle nobody can reconstruct afterwards is a lifecycle nobody can
	// support.
	t.Run("EventsRecordTheTransitionsInOrder", func(t *testing.T) {
		out, err := f.AWS.RDS.DescribeEvents(&rds.DescribeEventsInput{
			SourceType:       aws.String("db-instance"),
			SourceIdentifier: aws.String(id),
			Duration:         aws.Int64(1440),
		})
		require.NoError(t, err, "describe-events")
		require.NotEmpty(t, out.Events)

		var messages []string
		var last time.Time
		for _, event := range out.Events {
			assert.Equal(t, id, aws.StringValue(event.SourceIdentifier))
			assert.Equal(t, "db-instance", aws.StringValue(event.SourceType))
			assert.Contains(t, aws.StringValue(event.SourceArn), ":db:"+id,
				"an event must name the resource it is about")
			at := aws.TimeValue(event.Date)
			assert.False(t, at.Before(last), "events must be reported oldest first")
			last = at
			messages = append(messages, aws.StringValue(event.Message))
		}
		joined := strings.Join(messages, "\n")
		for _, want := range []string{"stopped", "starting", "restarted"} {
			assert.Contains(t, joined, want, "the events page must record the %s transition", want)
		}

		// The category filter is what a customer subscribes on, so a filter that
		// returns everything is as broken as one that returns nothing. Scoped to the
		// same instance as the page above: without SourceIdentifier this returns
		// every instance in the account and the comparison below is between two
		// different populations, which passes or fails on how many siblings the
		// suite happens to have alive.
		filtered, err := f.AWS.RDS.DescribeEvents(&rds.DescribeEventsInput{
			SourceType:       aws.String("db-instance"),
			SourceIdentifier: aws.String(id),
			EventCategories:  aws.StringSlice([]string{"availability"}),
			Duration:         aws.Int64(1440),
		})
		require.NoError(t, err, "describe-events filtered by category")
		require.NotEmpty(t, filtered.Events, "the availability category covers the stop, start and reboot")
		for _, event := range filtered.Events {
			assert.Equal(t, id, aws.StringValue(event.SourceIdentifier))
			assert.Contains(t, aws.StringValueSlice(event.EventCategories), "availability")
		}
		assert.Less(t, len(filtered.Events), len(out.Events),
			"the unfiltered page carries the configuration-change events as well")

		// AWS's own rule, and the one a client trips over first.
		harness.ExpectError(t, "InvalidParameterCombination", func() error {
			_, err := f.AWS.RDS.DescribeEvents(&rds.DescribeEventsInput{
				SourceIdentifier: aws.String(id),
			})
			return err
		})
	})
}

// A parameter group carrying one static override, and nothing else: the
// assertion is about how that one value reaches the engine.
func createParameterGroupWithStatic(t *testing.T, f *Fixture, name string) {
	t.Helper()
	_, err := f.AWS.RDS.CreateDBParameterGroup(&rds.CreateDBParameterGroupInput{
		DBParameterGroupName:   aws.String(name),
		DBParameterGroupFamily: aws.String(dbParameterGroupFamily),
		Description:            aws.String("rds e2e static parameter group"),
	})
	require.NoError(t, err, "create-db-parameter-group %s", name)
	t.Cleanup(func() {
		if _, err := f.AWS.RDS.DeleteDBParameterGroup(&rds.DeleteDBParameterGroupInput{
			DBParameterGroupName: aws.String(name),
		}); err != nil && !harness.ErrorCodeIs(err, "DBParameterGroupNotFound") {
			t.Logf("cleanup: delete-db-parameter-group %s: %v", name, err)
		}
	})

	_, err = f.AWS.RDS.ModifyDBParameterGroup(&rds.ModifyDBParameterGroupInput{
		DBParameterGroupName: aws.String(name),
		Parameters: []*rds.Parameter{{
			ParameterName:  aws.String(staticParameterName),
			ParameterValue: aws.String(staticParameterValue),
			ApplyMethod:    aws.String("pending-reboot"),
		}},
	})
	require.NoError(t, err, "modify-db-parameter-group %s", name)
}

// The engine's own start time, which moves only across a real restart.
func postmasterStartTime(t *testing.T, tgt harness.SSHTarget, conn harness.PSQLConn) string {
	t.Helper()
	return strings.TrimSpace(harness.PSQL(t, tgt, conn, "SELECT pg_postmaster_start_time();"))
}

// SHOW reports an integer parameter as a plain number, so it round-trips as the
// literal the parameter group set.
func showInteger(t *testing.T, tgt harness.SSHTarget, conn harness.PSQLConn, parameter string) string {
	t.Helper()
	out := strings.TrimSpace(harness.PSQL(t, tgt, conn, "SHOW "+parameter+";"))
	if _, err := strconv.Atoi(out); err != nil {
		t.Fatalf("SHOW %s reported %q, which is not an integer", parameter, out)
	}
	return out
}
