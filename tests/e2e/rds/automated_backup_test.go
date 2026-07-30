//go:build e2e

package rds

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The namespace automated snapshots own, as in AWS.
	automatedSnapshotPrefix = "rds:"

	// The retention reaper runs every two minutes. Three passes leave room for a
	// pass already in flight when the snapshot ages are changed.
	retentionSweepTimeout = 7 * time.Minute
)

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
	t.Parallel()
	// The instance being backed up, plus the restore that holds one of its
	// snapshots open across the sweep.
	reserveDBVMs(t, 2)

	id := fmt.Sprintf("%s-backup-%d", dbInstancePfx, time.Now().Unix())

	harness.Phase(t, "Creating DB instance %q with automated backups on", id)
	createDBInstance(t, f, id, func(in *rds.CreateDBInstanceInput) {
		in.BackupRetentionPeriod = aws.Int64(1)
	})

	instance := waitForAvailable(t, f, id)
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

	// One sweep over three aged backups proves all retention rules together: the
	// oldest is deleted, the middle snapshot is held by a restore, and the newest
	// survives even though it is beyond the configured retention.
	t.Run("RetentionSweepRules", func(t *testing.T) {
		requireSnapshot(t, snapshot)
		oldest := snapshot
		inUse := waitForNextAutomatedSnapshot(t, f, id)
		newest := waitForNextAutomatedSnapshot(t, f, id)

		restoredID := fmt.Sprintf("%s-backup-reader-%d", dbInstancePfx, time.Now().Unix())
		harness.Phase(t, "Restoring %q from automated snapshot %q", restoredID, inUse)
		restoreFromSnapshot(t, f, restoredID, inUse)
		waitForAvailable(t, f, restoredID)

		for _, snapshotID := range []string{oldest, inUse, newest} {
			harness.AgeAutomatedBackup(t, f.Env, f.Account, snapshotID, 2*24*time.Hour)
		}

		harness.Phase(t, "Waiting for the retention sweep of %q", id)
		harness.EventuallyErr(t, func() error {
			snapshots, err := automatedSnapshots(f, id)
			if err != nil {
				return err
			}
			ids := automatedSnapshotIDs(snapshots)
			if !slices.Contains(ids, inUse) {
				t.Fatalf("in-use automated snapshot %s was swept", inUse)
			}
			if !slices.Contains(ids, newest) {
				t.Fatalf("newest automated snapshot %s was swept", newest)
			}
			if slices.Contains(ids, oldest) {
				return fmt.Errorf("over-retention automated snapshot %s still exists", oldest)
			}
			return nil
		}, retentionSweepTimeout, 15*time.Second)

		t.Run("AnOverRetentionSnapshotIsSwept", func(t *testing.T) {
			_, err := harness.DescribeDBSnapshot(f.AWS, oldest)
			harness.AssertAWSError(t, err, "DBSnapshotNotFound")
		})
		t.Run("AnInUseSnapshotIsSkipped", func(t *testing.T) {
			_, err := harness.DescribeDBSnapshot(f.AWS, inUse)
			require.NoError(t, err, "the restore still reads through this snapshot")
		})
		t.Run("TheNewestIsKeptRegardless", func(t *testing.T) {
			_, err := harness.DescribeDBSnapshot(f.AWS, newest)
			require.NoError(t, err, "the newest backup must survive beyond retention")
		})

		// Release the restored volume before retention zero asks the next sweep to
		// remove every snapshot, including the one that was in use above.
		deleteInstance(t, f, restoredID)
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

// Moves the window forward and returns the one new snapshot it produces. The
// existing identifiers distinguish it from every earlier fired window.
func waitForNextAutomatedSnapshot(t *testing.T, f *Fixture, id string) string {
	t.Helper()
	before, err := automatedSnapshots(f, id)
	require.NoError(t, err, "describe automated snapshots before moving the window")
	beforeIDs := automatedSnapshotIDs(before)

	instance, err := harness.DescribeDBInstance(f.AWS, id)
	require.NoError(t, err, "describe-db-instances before moving the backup window")
	currentWindow := aws.StringValue(instance.PreferredBackupWindow)
	opens := time.Now().UTC().Add(time.Minute)
	nextWindow := dailyWindowAt(opens, 30*time.Minute)
	if nextWindow == currentWindow {
		opens = opens.Add(time.Minute)
		nextWindow = dailyWindowAt(opens, 30*time.Minute)
	}

	harness.Phase(t, "Moving the backup window of %q to %s", id, nextWindow)
	_, err = f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
		DBInstanceIdentifier:  aws.String(id),
		PreferredBackupWindow: aws.String(nextWindow),
		ApplyImmediately:      aws.Bool(true),
	})
	require.NoError(t, err, "modify-db-instance backup window")

	var created string
	harness.EventuallyErr(t, func() error {
		snapshots, err := automatedSnapshots(f, id)
		if err != nil {
			return err
		}
		for _, candidate := range automatedSnapshotIDs(snapshots) {
			if !slices.Contains(beforeIDs, candidate) {
				created = candidate
				return nil
			}
		}
		return fmt.Errorf("no new automated snapshot of %s yet", id)
	}, 8*time.Minute, 15*time.Second)
	return created
}

func automatedSnapshotIDs(snapshots []*rds.DBSnapshot) []string {
	ids := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		ids = append(ids, aws.StringValue(snapshot.DBSnapshotIdentifier))
	}
	return ids
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
