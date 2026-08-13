//go:build e2e

package rds

import (
	"fmt"
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
	// The table the point-in-time assertion is made on. One row goes in before the
	// snapshot and one after, so a restore that holds both is reading the live
	// volume rather than a copy of it, and one that holds neither restored the
	// wrong thing.
	snapshotTable      = "e2e_snapshot"
	rowBeforeSnapshot  = "before the snapshot"
	rowAfterSnapshot   = "after the snapshot"
	snapshotRowsBefore = 1

	// The message a snapshot taken over a running engine must not carry: the
	// engine was available, so the quiesce had every opportunity to work, and the
	// crash-consistent fallback is for the case where it does not.
	crashConsistentNotice = "could not be quiesced"

	// How long a retained volume is given to go once its last snapshot has: the
	// delete is synchronous in the snapshot path, but the volume store's own
	// bookkeeping settles behind it.
	volumeReleaseTimeout = 2 * time.Minute
)

// TestSnapshotRestore drives a manual snapshot and the instance restored from it.
// What only a live cluster can prove is that the snapshot is a copy of the datadir
// at the moment it was taken rather than a pointer at the live volume, that the
// restored engine comes up on that datadir with the roles it already held, and
// that the restore is a wholly separate instance — its own volume, its own ENI,
// its own endpoint.
//
// Both instances are consumed: deleting the source proves the retained volume
// is released, which is an assertion that cannot be made while it exists.
func TestSnapshotRestore(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	// The source and the instance restored from its snapshot, which have to be
	// alive together for the restore to be provably a point in time. The restore
	// exercises the class override, so it costs more than the source it came from.
	reserveDBVMs(t, dbClass, grownClass)

	suffix := time.Now().Unix()
	sourceID := fmt.Sprintf("%s-snapsrc-%d", dbInstancePfx, suffix)
	restoredID := fmt.Sprintf("%s-restored-%d", dbInstancePfx, suffix)
	snapshotID := fmt.Sprintf("%s-snapshot-%d", dbInstancePfx, suffix)
	stoppedSnapshotID := fmt.Sprintf("%s-stopped-snap-%d", dbInstancePfx, suffix)
	subnetGroup := fmt.Sprintf("%s-restore-subnets-%d", dbInstancePfx, suffix)

	harness.Phase(t, "Creating source DB instance %q", sourceID)
	createDBInstance(t, f, sourceID)
	client := rdsClient(t, f)
	system := f.SystemAWS(t)

	source := waitForAvailable(t, f, sourceID)
	sourceConn := harness.PSQLConnFor(t, source, dbMasterUser, dbMasterPassword, dbName)
	sourceEndpoint := sourceConn.Host
	sourceVolumeID := aws.StringValue(harness.DBInstanceDataVolume(t, system, sourceID).VolumeId)

	harness.PSQL(t, client, sourceConn, fmt.Sprintf(
		"CREATE TABLE %s (id int primary key, note text); INSERT INTO %s VALUES (1, '%s');",
		snapshotTable, snapshotTable, rowBeforeSnapshot))

	harness.Phase(t, "Snapshotting %q as %q", sourceID, snapshotID)
	snapshot := createDBSnapshot(t, f, sourceID, snapshotID)

	// The snapshot has to carry enough of the instance's configuration to rebuild
	// it, because the instance may be gone by the time it is used — as it is by the
	// end of this test.
	assert.Equal(t, harness.DBSnapshotAvailable, aws.StringValue(snapshot.Status),
		"CreateDBSnapshot returns once the data is captured, so it is available on the response")
	assert.Equal(t, sourceID, aws.StringValue(snapshot.DBInstanceIdentifier))
	assert.Equal(t, "manual", aws.StringValue(snapshot.SnapshotType))
	assert.Equal(t, dbEngine, aws.StringValue(snapshot.Engine))
	assert.Equal(t, int64(dbStorageGiB), aws.Int64Value(snapshot.AllocatedStorage))
	assert.Equal(t, dbMasterUser, aws.StringValue(snapshot.MasterUsername))
	assert.Equal(t, int64(harness.PostgresEnginePort), aws.Int64Value(snapshot.Port))
	assert.True(t, aws.BoolValue(snapshot.Encrypted), "the snapshot inherits the data volume's encryption")
	assert.Equal(t, int64(100), aws.Int64Value(snapshot.PercentProgress),
		"a snapshot reported as available must not also report itself part-done")
	assert.Contains(t, aws.StringValue(snapshot.DBSnapshotArn), ":snapshot:"+snapshotID)

	// Written after the snapshot and therefore the row that must be missing from
	// the restore. Inserted before the restore starts so it is unambiguously
	// committed to the live volume first.
	harness.PSQL(t, client, sourceConn, fmt.Sprintf(
		"INSERT INTO %s VALUES (2, '%s');", snapshotTable, rowAfterSnapshot))

	t.Run("ADuplicateSnapshotIdentifierIsRefused", func(t *testing.T) {
		harness.ExpectError(t, "DBSnapshotAlreadyExists", func() error {
			_, err := f.AWS.RDS.CreateDBSnapshot(&rds.CreateDBSnapshotInput{
				DBInstanceIdentifier: aws.String(sourceID),
				DBSnapshotIdentifier: aws.String(snapshotID),
			})
			return err
		})

		// The refusal has to come before the instance is moved into backing-up, or a
		// misspelled snapshot leaves a database that cannot be modified or stopped.
		current, err := harness.DescribeDBInstance(f.AWS, sourceID)
		require.NoError(t, err)
		assert.Equal(t, harness.DBInstanceAvailable, aws.StringValue(current.DBInstanceStatus),
			"a rejected snapshot must leave the instance where it found it")
	})

	// The restore is a new instance in every respect but its data: its own volume,
	// ENI, endpoint and certificate. The overrides are the two a customer actually
	// reaches for — a different class, and somewhere else to put it.
	t.Run("ARestoreHoldsTheDataAsOfTheSnapshot", func(t *testing.T) {
		createSubnetGroupOverVPC(t, f, subnetGroup)

		harness.Phase(t, "Restoring %q from %q on %s", restoredID, snapshotID, grownClass)
		restored := restoreFromSnapshot(t, f, restoredID, snapshotID,
			func(in *rds.RestoreDBInstanceFromDBSnapshotInput) {
				in.DBInstanceClass = aws.String(grownClass)
				in.DBSubnetGroupName = aws.String(subnetGroup)
			})
		assert.Equal(t, harness.DBInstanceCreating, aws.StringValue(restored.DBInstanceStatus))

		instance := waitForAvailable(t, f, restoredID)
		assert.Equal(t, grownClass, aws.StringValue(instance.DBInstanceClass),
			"the class override must be honoured; a restore is where a customer resizes")
		assert.Equal(t, subnetGroup, dbSubnetGroupName(instance))
		// Inherited from the snapshot rather than defaulted, because the datadir is
		// what it is: the database and the master role already exist inside it.
		assert.Equal(t, dbMasterUser, aws.StringValue(instance.MasterUsername))
		assert.Equal(t, int64(dbStorageGiB), aws.Int64Value(instance.AllocatedStorage))

		require.NotNil(t, instance.Endpoint)
		assert.NotEqual(t, sourceEndpoint, aws.StringValue(instance.Endpoint.Address),
			"the restore is its own instance and must publish its own endpoint")
		assert.NotEqual(t, sourceVolumeID, aws.StringValue(harness.DBInstanceDataVolume(t, system, restoredID).VolumeId),
			"the restore reads a volume created from the snapshot, never the source's own")

		// No password was supplied to the restore and none could be — the role
		// and its hash came from the datadir, so the source's credentials are the
		// restore's. A restore that needed new credentials would be unusable.
		conn := harness.PSQLConnFor(t, instance, dbMasterUser, dbMasterPassword, dbName)
		rows := harness.PSQL(t, client, conn, fmt.Sprintf("SELECT count(*) FROM %s;", snapshotTable))
		assert.Equal(t, fmt.Sprint(snapshotRowsBefore), strings.TrimSpace(rows),
			"the restore must hold the rows the snapshot captured and nothing written after it")

		note := harness.PSQL(t, client, conn, fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", snapshotTable))
		assert.Equal(t, rowBeforeSnapshot, strings.TrimSpace(note))

		// The row the source has and the snapshot does not. Its absence is what
		// separates a point-in-time copy from a second reference to the live volume.
		absent := harness.PSQL(t, client, conn,
			fmt.Sprintf("SELECT count(*) FROM %s WHERE note = '%s';", snapshotTable, rowAfterSnapshot))
		assert.Equal(t, "0", strings.TrimSpace(absent),
			"a row written after the snapshot must not appear in the restore")

		// The source is untouched by the restore reading from its snapshot.
		live := harness.PSQL(t, client, sourceConn, fmt.Sprintf("SELECT count(*) FROM %s;", snapshotTable))
		assert.Equal(t, "2", strings.TrimSpace(live), "the source instance keeps both rows")
	})

	t.Run("TheEventsAccountForBothHalves", func(t *testing.T) {
		created := dbSnapshotEvents(t, f, snapshotID)
		require.NotEmpty(t, created, "a snapshot with no event is a backup a customer cannot audit")
		assert.Contains(t, strings.Join(created, "\n"), "DB snapshot created")

		// An unquiesced snapshot is reported rather than refused, which makes the
		// notice the only signal that a backup is crash consistent. The engine was
		// available here, so its absence is the assertion.
		instanceEvents := dbInstanceEventMessages(t, f, sourceID)
		assert.NotContains(t, strings.Join(instanceEvents, "\n"), crashConsistentNotice,
			"the engine was available, so the snapshot must have been taken over a real checkpoint")

		restoreEvents := dbInstanceEventMessages(t, f, restoredID)
		assert.Contains(t, strings.Join(restoreEvents, "\n"), "Restored from DB snapshot "+snapshotID,
			"the restored instance's history must name where its data came from")
	})

	// A stopped instance was sealed by its own graceful stop, so there is no engine
	// to hold at a checkpoint and no reason to refuse the snapshot. It is also the
	// cheapest correct way to back up a database that is not in use.
	t.Run("ASnapshotOfAStoppedInstanceIsAllowed", func(t *testing.T) {
		_, err := f.AWS.RDS.StopDBInstance(&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(sourceID)})
		require.NoError(t, err, "stop-db-instance")
		harness.WaitForDBInstanceStatus(t, f.AWS, sourceID, harness.DBInstanceStopped)

		stopped := createDBSnapshot(t, f, sourceID, stoppedSnapshotID)
		assert.Equal(t, harness.DBSnapshotAvailable, aws.StringValue(stopped.Status))
		assert.Equal(t, int64(dbStorageGiB), aws.Int64Value(stopped.AllocatedStorage))

		// The instance goes back to where the snapshot found it, not to available: a
		// backup must not restart a database its owner deliberately stopped.
		current, err := harness.DescribeDBInstance(f.AWS, sourceID)
		require.NoError(t, err)
		assert.Equal(t, harness.DBInstanceStopped, aws.StringValue(current.DBInstanceStatus))
	})

	// Snapshots are per-account, like the instances they come from. A tenant that
	// could name another tenant's snapshot could restore their database.
	t.Run("AnotherTenantCannotSeeOrRestoreTheSnapshot", func(t *testing.T) {
		carousel := harness.NewAccountCarousel()
		tenant := carousel.Add(t, f.Env, "rds-e2e-snapshot-tenant",
			harness.SpxAdminAccountCreate(t, fmt.Sprintf("RDS E2E Snapshot Tenant %d", suffix), ""))

		harness.ExpectError(t, "DBSnapshotNotFound", func() error {
			_, err := tenant.Client.RDS.DescribeDBSnapshots(&rds.DescribeDBSnapshotsInput{
				DBSnapshotIdentifier: aws.String(snapshotID),
			})
			return err
		})

		// Not AccessDenied: the snapshot is not in this tenant's namespace at all, and
		// they are an administrator of their own account.
		harness.ExpectError(t, "DBSnapshotNotFound", func() error {
			_, err := tenant.Client.RDS.RestoreDBInstanceFromDBSnapshot(&rds.RestoreDBInstanceFromDBSnapshotInput{
				DBInstanceIdentifier: aws.String(restoredID + "-crossaccount"),
				DBSnapshotIdentifier: aws.String(snapshotID),
			})
			return err
		})

		// The suite's own describe is unaffected, so the isolation is the tenant's
		// view rather than the snapshot having gone.
		mine, err := harness.DescribeDBSnapshot(f.AWS, snapshotID)
		require.NoError(t, err)
		assert.Equal(t, snapshotID, aws.StringValue(mine.DBSnapshotIdentifier))
	})

	// A data volume whose chunks a snapshot still references outlives its DB
	// instance, and is reclaimed by the delete of the last snapshot holding it. The
	// failure this catches is the expensive one — a volume nothing points at, paid
	// for indefinitely, invisible to the customer who deleted the database.
	t.Run("DeletingTheLastSnapshotReleasesTheRetainedVolume", func(t *testing.T) {
		// First, so the snapshots below are not being deleted from under a volume
		// created from one of them.
		harness.Step(t, "Deleting the restored instance %q", restoredID)
		deleteInstance(t, f, restoredID)

		harness.Step(t, "Deleting the source instance %q, whose volume two snapshots hold", sourceID)
		deleteInstance(t, f, sourceID)
		requireVolumeExists(t, system, sourceVolumeID,
			"the data volume must be retained while snapshots reference its chunks")

		deleteDBSnapshot(t, f, stoppedSnapshotID)
		requireVolumeExists(t, system, sourceVolumeID,
			"one snapshot is gone but the other still holds the volume")

		deleteDBSnapshot(t, f, snapshotID)
		requireVolumeGone(t, system, sourceVolumeID)
	})
}

// createDBSnapshot takes a snapshot and registers its teardown. CreateDBSnapshot
// is synchronous — it returns once the volume has been captured — so there is
// nothing to wait for, and a wait here would hide a status that never settled.
func createDBSnapshot(t *testing.T, f *Fixture, dbInstanceID, snapshotID string) *rds.DBSnapshot {
	t.Helper()
	out, err := f.AWS.RDS.CreateDBSnapshot(&rds.CreateDBSnapshotInput{
		DBInstanceIdentifier: aws.String(dbInstanceID),
		DBSnapshotIdentifier: aws.String(snapshotID),
	})
	require.NoError(t, err, "create-db-snapshot %s", snapshotID)
	require.NotNil(t, out.DBSnapshot)
	t.Cleanup(func() { deleteDBSnapshot(t, f, snapshotID) })
	return out.DBSnapshot
}

// Idempotent teardown for one snapshot, so the assertions above can delete
// snapshots in the order the retention behavior is proven without the cleanups then failing.
func deleteDBSnapshot(t *testing.T, f *Fixture, snapshotID string) {
	t.Helper()
	if _, err := f.AWS.RDS.DeleteDBSnapshot(&rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(snapshotID),
	}); err != nil && !harness.ErrorCodeIs(err, "DBSnapshotNotFound") {
		t.Errorf("delete-db-snapshot %s: %v (left behind for manual teardown)", snapshotID, err)
	}
}

// restoreFromSnapshot restores an instance and registers the same teardown and
// failure-only diagnostics a create does: a restored instance is a DB VM like any
// other, and one left behind is charged to the next run.
func restoreFromSnapshot(t *testing.T, f *Fixture, id, snapshotID string,
	opts ...func(*rds.RestoreDBInstanceFromDBSnapshotInput)) *rds.DBInstance {
	t.Helper()
	in := &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: aws.String(id),
		DBSnapshotIdentifier: aws.String(snapshotID),
	}
	for _, opt := range opts {
		opt(in)
	}
	markDBCreateStarted(id)
	out, err := f.AWS.RDS.RestoreDBInstanceFromDBSnapshot(in) //nolint:staticcheck // e2e:allow-create — the restored instance under test
	require.NoError(t, err, "restore-db-instance-from-db-snapshot %s", id)
	require.NotNil(t, out.DBInstance)
	t.Cleanup(func() { deleteInstance(t, f, id) })
	harness.CaptureDBDiagnostics(t, f.dbDiag(t), id)
	return out.DBInstance
}

// A DB subnet group over every subnet of the default VPC, which is what makes it
// a placement a restore can be pointed at.
func createSubnetGroupOverVPC(t *testing.T, f *Fixture, name string) {
	t.Helper()
	vpcID, _, _ := harness.DiscoverDefaultVPC(t, f.AWS)
	subnets := subnetsInVPC(t, f.AWS, vpcID)
	require.NotEmpty(t, subnets, "the default VPC %s has no subnets to build a DB subnet group from", vpcID)

	subnetIDs := make([]string, 0, len(subnets))
	for _, subnet := range subnets {
		subnetIDs = append(subnetIDs, aws.StringValue(subnet.SubnetId))
	}
	_, err := f.AWS.RDS.CreateDBSubnetGroup(&rds.CreateDBSubnetGroupInput{
		DBSubnetGroupName:        aws.String(name),
		DBSubnetGroupDescription: aws.String("rds e2e restore subnet group"),
		SubnetIds:                aws.StringSlice(subnetIDs),
	})
	require.NoError(t, err, "create-db-subnet-group %s", name)
	t.Cleanup(func() {
		if _, err := f.AWS.RDS.DeleteDBSubnetGroup(&rds.DeleteDBSubnetGroupInput{
			DBSubnetGroupName: aws.String(name),
		}); err != nil && !harness.ErrorCodeIs(err, "DBSubnetGroupNotFoundFault") {
			t.Logf("cleanup: delete-db-subnet-group %s: %v", name, err)
		}
	})
}

// The event messages recorded against one DB snapshot.
func dbSnapshotEvents(t *testing.T, f *Fixture, snapshotID string) []string {
	t.Helper()
	out, err := f.AWS.RDS.DescribeEvents(&rds.DescribeEventsInput{
		SourceType:       aws.String("db-snapshot"),
		SourceIdentifier: aws.String(snapshotID),
		Duration:         aws.Int64(1440),
	})
	require.NoError(t, err, "describe-events for snapshot %s", snapshotID)

	messages := make([]string, 0, len(out.Events))
	for _, event := range out.Events {
		assert.Equal(t, "db-snapshot", aws.StringValue(event.SourceType))
		assert.Contains(t, aws.StringValue(event.SourceArn), ":snapshot:"+snapshotID)
		messages = append(messages, aws.StringValue(event.Message))
	}
	return messages
}

// The event messages recorded against one DB instance.
func dbInstanceEventMessages(t *testing.T, f *Fixture, id string) []string {
	t.Helper()
	out, err := f.AWS.RDS.DescribeEvents(&rds.DescribeEventsInput{
		SourceType:       aws.String("db-instance"),
		SourceIdentifier: aws.String(id),
		Duration:         aws.Int64(1440),
	})
	require.NoError(t, err, "describe-events for instance %s", id)

	messages := make([]string, 0, len(out.Events))
	for _, event := range out.Events {
		messages = append(messages, aws.StringValue(event.Message))
	}
	return messages
}

func requireVolumeExists(t *testing.T, system *harness.AWSClient, volumeID, why string) {
	t.Helper()
	out, err := system.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
		VolumeIds: aws.StringSlice([]string{volumeID}),
	})
	require.NoError(t, err, "describe-volumes %s: %s", volumeID, why)
	require.Len(t, out.Volumes, 1, why)
}

// Polled: the snapshot delete releases the volume synchronously, but the volume
// store settles its own index behind that.
func requireVolumeGone(t *testing.T, system *harness.AWSClient, volumeID string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		out, err := system.EC2.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: aws.StringSlice([]string{volumeID}),
		})
		if err != nil {
			if harness.ErrorCodeIs(err, "InvalidVolume.NotFound") {
				return nil
			}
			return fmt.Errorf("describe-volumes %s: %w", volumeID, err)
		}
		if len(out.Volumes) == 0 {
			return nil
		}
		return fmt.Errorf("retained volume %s is still %s after its last snapshot was deleted",
			volumeID, aws.StringValue(out.Volumes[0].State))
	}, volumeReleaseTimeout, 5*time.Second)
}
