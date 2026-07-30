//go:build e2e

package rds

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The namespace automated snapshots own, as in AWS.
const automatedSnapshotPrefix = "rds:"

// TestAutomatedBackups drives what only a live cluster can prove about rds-9:
// the leader's window pass actually fires against a real engine, the snapshot it
// takes is a real quiesced copy of the data volume, it fires once and not once per
// pass, and turning retention off sweeps the set through the cluster-wide reaper.
//
// A window is the only trigger — there is no "back up now" API — so the window is
// moved onto the instance once it is available rather than at create: a bootstrap
// slower than the window would otherwise miss it and there is no catch-up.
func TestAutomatedBackups(t *testing.T) {
	f := requireRDSFixture(t)
	id := fmt.Sprintf("%s-backup-%d", dbInstancePfx, time.Now().Unix())

	harness.Phase(t, "Creating DB instance %q with automated backups on", id)
	_, err := f.AWS.RDS.CreateDBInstance(&rds.CreateDBInstanceInput{ //nolint:staticcheck // e2e:allow-create — the instance under test
		DBInstanceIdentifier:  aws.String(id),
		Engine:                aws.String(dbEngine),
		DBInstanceClass:       aws.String(dbClass),
		AllocatedStorage:      aws.Int64(dbStorageGiB),
		DBName:                aws.String(dbName),
		MasterUsername:        aws.String(dbMasterUser),
		MasterUserPassword:    aws.String(dbMasterPassword),
		BackupRetentionPeriod: aws.Int64(1),
	})
	require.NoError(t, err, "create-db-instance")
	t.Cleanup(func() { deleteInstance(t, f, id) })

	instance := harness.WaitForDBInstanceAvailable(t, f.AWS, id)
	assert.Equal(t, int64(1), aws.Int64Value(instance.BackupRetentionPeriod))
	// A create that names no window is assigned one, as AWS does, so a customer is
	// never left believing nothing is scheduled.
	assert.Regexp(t, `^\d\d:\d\d-\d\d:\d\d$`, aws.StringValue(instance.PreferredBackupWindow))
	assert.Regexp(t, `^[a-z]{3}:\d\d:\d\d-[a-z]{3}:\d\d:\d\d$`,
		aws.StringValue(instance.PreferredMaintenanceWindow))

	opens := time.Now().UTC().Add(time.Minute)
	backupWindow := dailyWindowAt(opens, 30*time.Minute)
	// Well clear of the backup window: the API refuses an overlapping pair.
	maintenanceWindow := weeklyWindowAt(opens.Add(4*time.Hour), 30*time.Minute)

	harness.Phase(t, "Moving the backup window of %q to %s", id, backupWindow)
	modified, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
		DBInstanceIdentifier:       aws.String(id),
		PreferredBackupWindow:      aws.String(backupWindow),
		PreferredMaintenanceWindow: aws.String(maintenanceWindow),
		ApplyImmediately:           aws.Bool(true),
	})
	require.NoError(t, err, "modify-db-instance")
	require.NotNil(t, modified.DBInstance)
	assert.Equal(t, backupWindow, aws.StringValue(modified.DBInstance.PreferredBackupWindow),
		"the window is reported back in AWS's canonical form")
	assert.Equal(t, maintenanceWindow, aws.StringValue(modified.DBInstance.PreferredMaintenanceWindow))

	var snapshot string
	t.Run("TakesASnapshotInsideTheWindow", func(t *testing.T) {
		harness.Phase(t, "Waiting for the automated backup of %q", id)
		// The window may still be ahead of us, and the reconciler's pass is on its
		// own cadence once it opens.
		harness.EventuallyErr(t, func() error {
			snapshots, err := automatedSnapshots(f, id)
			if err != nil {
				return err
			}
			if len(snapshots) == 0 {
				return fmt.Errorf("no automated snapshot of %s yet", id)
			}
			snapshot = aws.StringValue(snapshots[0].DBSnapshotIdentifier)
			return nil
		}, 8*time.Minute, 15*time.Second)

		assert.True(t, strings.HasPrefix(snapshot, automatedSnapshotPrefix),
			"an automated snapshot takes AWS's own name, got %q", snapshot)

		described, err := f.AWS.RDS.DescribeDBSnapshots(&rds.DescribeDBSnapshotsInput{
			DBSnapshotIdentifier: aws.String(snapshot),
		})
		require.NoError(t, err, "describe-db-snapshots")
		require.Len(t, described.DBSnapshots, 1)
		assert.Equal(t, "automated", aws.StringValue(described.DBSnapshots[0].SnapshotType))
		assert.Equal(t, "available", aws.StringValue(described.DBSnapshots[0].Status))
		assert.Equal(t, id, aws.StringValue(described.DBSnapshots[0].DBInstanceIdentifier))
	})

	t.Run("ReportsTheAutomatedBackupSet", func(t *testing.T) {
		requireSnapshot(t, snapshot)
		out, err := f.AWS.RDS.DescribeDBInstanceAutomatedBackups(&rds.DescribeDBInstanceAutomatedBackupsInput{
			DBInstanceIdentifier: aws.String(id),
		})
		require.NoError(t, err, "describe-db-instance-automated-backups")
		require.Len(t, out.DBInstanceAutomatedBackups, 1)

		backup := out.DBInstanceAutomatedBackups[0]
		assert.Equal(t, id, aws.StringValue(backup.DBInstanceIdentifier))
		assert.Equal(t, "active", aws.StringValue(backup.Status))
		assert.Equal(t, int64(1), aws.Int64Value(backup.BackupRetentionPeriod))
		// This phase backs discrete daily snapshots, so there is no restore window
		// to report; reporting one would promise point-in-time recovery.
		assert.Nil(t, backup.RestoreWindow)
	})

	// The reconciler passes every few seconds while the window stays open, so a
	// window that fired per pass rather than per window shows up here immediately.
	t.Run("FiresOncePerWindow", func(t *testing.T) {
		requireSnapshot(t, snapshot)
		harness.Phase(t, "Watching %q for a duplicate backup in the same window", id)
		for range 8 {
			time.Sleep(20 * time.Second)
			snapshots, err := automatedSnapshots(f, id)
			require.NoError(t, err)
			require.Len(t, snapshots, 1, "the window has already fired; a second backup means it fired per pass")
		}
	})

	// Turning retention off is what makes the data volume GC-eligible again, so it
	// has to remove the set rather than leave it to expire.
	t.Run("RetentionZeroSweepsTheSet", func(t *testing.T) {
		requireSnapshot(t, snapshot)
		_, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier:  aws.String(id),
			BackupRetentionPeriod: aws.Int64(0),
			ApplyImmediately:      aws.Bool(true),
		})
		require.NoError(t, err, "modify-db-instance")

		harness.Phase(t, "Waiting for the automated backups of %q to be swept", id)
		harness.EventuallyErr(t, func() error {
			snapshots, err := automatedSnapshots(f, id)
			if err != nil {
				return err
			}
			if len(snapshots) > 0 {
				return fmt.Errorf("%s still has %d automated snapshots", id, len(snapshots))
			}
			return nil
		}, 6*time.Minute, 15*time.Second)

		out, err := f.AWS.RDS.DescribeDBInstanceAutomatedBackups(&rds.DescribeDBInstanceAutomatedBackupsInput{
			DBInstanceIdentifier: aws.String(id),
		})
		require.NoError(t, err, "describe-db-instance-automated-backups")
		assert.Empty(t, out.DBInstanceAutomatedBackups,
			"an instance with backups off has no automated backup set")
	})
}

// The automated snapshots of one instance, newest first, which is what the
// snapshot-type filter has to answer without listing the manual ones.
func automatedSnapshots(f *Fixture, id string) ([]*rds.DBSnapshot, error) {
	out, err := f.AWS.RDS.DescribeDBSnapshots(&rds.DescribeDBSnapshotsInput{
		DBInstanceIdentifier: aws.String(id),
		SnapshotType:         aws.String("automated"),
	})
	if err != nil {
		return nil, fmt.Errorf("describe-db-snapshots %s: %w", id, err)
	}
	return out.DBSnapshots, nil
}

func requireSnapshot(t *testing.T, snapshot string) {
	t.Helper()
	if snapshot == "" {
		t.Skip("no automated snapshot was taken (TakesASnapshotInsideTheWindow failed)")
	}
}

// AWS's hh24:mi-hh24:mi in UTC.
func dailyWindowAt(start time.Time, length time.Duration) string {
	return start.UTC().Format("15:04") + "-" + start.UTC().Add(length).Format("15:04")
}

// AWS's ddd:hh24:mi-ddd:hh24:mi in UTC.
func weeklyWindowAt(start time.Time, length time.Duration) string {
	return weekdayClock(start) + "-" + weekdayClock(start.Add(length))
}

func weekdayClock(at time.Time) string {
	at = at.UTC()
	return strings.ToLower(at.Format("Mon")) + ":" + at.Format("15:04")
}

func deleteInstance(t *testing.T, f *Fixture, id string) {
	t.Helper()
	_, err := f.AWS.RDS.DeleteDBInstance(&rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(id),
		SkipFinalSnapshot:    aws.Bool(true),
	})
	if err != nil {
		t.Logf("delete-db-instance %s: %v (left behind for manual teardown)", id, err)
		return
	}
	harness.WaitForDBInstanceGone(t, f.AWS, id)
}
