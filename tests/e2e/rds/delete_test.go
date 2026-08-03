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

const (
	// The row the final snapshot has to come back with. Written while the instance
	// is available, so a final snapshot that captured nothing, or captured a datadir
	// the engine had not checkpointed, fails on it rather than on a status field.
	deleteTable = "e2e_delete"
	deleteNote  = "written before the final snapshot"

	// The DNS records are published with a 60s TTL, and the guest's resolver caches
	// for it — so a withdrawal is given three TTLs before it counts as not having
	// happened.
	dnsWithdrawalTimeout = 3 * time.Minute
)

// TestDelete drives the two deletes a customer has to choose between and the
// guarantees each one makes: a final snapshot that is genuinely restorable, and a
// skip that genuinely removes everything — the volume, the endpoint ENI and the
// DNS name, which are the three things that go on costing or colliding if a
// teardown stops half way.
//
// Two instances are created up front so their boots overlap, plus one that is
// deleted while it is still creating.
func TestDelete(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	// Two alive at once, not the three this test gets through: the restore is
	// created after the protected instance it came from is gone, and so is the
	// one deleted mid-create.
	reserveDBVMs(t, dbClass, dbClass)

	suffix := time.Now().Unix()
	protectedID := fmt.Sprintf("%s-final-%d", dbInstancePfx, suffix)
	skippedID := fmt.Sprintf("%s-skipped-%d", dbInstancePfx, suffix)
	finalSnapshotID := fmt.Sprintf("%s-finalsnap-%d", dbInstancePfx, suffix)
	restoredID := fmt.Sprintf("%s-fromfinal-%d", dbInstancePfx, suffix)

	// Scoped to the whole test, not to the subtest that creates the snapshot: the
	// restore below and the removal after it both consume it, so a subtest-scoped
	// cleanup deletes it before either runs. Registered before the snapshot exists
	// because the teardown tolerates one that was never created, which also covers
	// a failure part way through the delete that leaves a snapshot behind.
	t.Cleanup(func() { deleteDBSnapshot(t, f, finalSnapshotID) })

	harness.Phase(t, "Creating DB instances %q (protected) and %q", protectedID, skippedID)
	createDBInstance(t, f, protectedID, func(in *rds.CreateDBInstanceInput) {
		in.DeletionProtection = aws.Bool(true)
	})
	// Registered after the create's own teardown so it runs before it: a failure
	// while protection is still on would otherwise leave an instance no cleanup can
	// delete.
	t.Cleanup(func() { clearDeletionProtection(t, f, protectedID) })
	createDBInstance(t, f, skippedID)

	client := rdsClient(t, f)
	system := f.SystemAWS(t)

	protected := waitForAvailable(t, f, protectedID)
	assert.True(t, aws.BoolValue(protected.DeletionProtection),
		"the flag supplied at create must be reported back, or a customer cannot tell they are protected")
	protectedVMID := aws.StringValue(harness.DBInstanceVM(t, system, protectedID).InstanceId)
	protectedVolumeID := aws.StringValue(harness.DBInstanceDataVolume(t, system, protectedID).VolumeId)

	conn := harness.PSQLConnFor(t, protected, dbMasterUser, dbMasterPassword, dbName)
	harness.PSQL(t, client, conn, fmt.Sprintf(
		"CREATE TABLE %s (id int primary key, note text); INSERT INTO %s VALUES (1, '%s');",
		deleteTable, deleteTable, deleteNote))

	// AWS makes the caller say which they want, so neither destroying the only copy
	// of the data nor keeping a snapshot they will be billed for can happen by
	// omission. Asserted against an identifier that does not exist, which proves the
	// choice is validated before anything is looked up.
	t.Run("TheSnapshotChoiceIsRequired", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			input *rds.DeleteDBInstanceInput
		}{
			{"Neither", &rds.DeleteDBInstanceInput{
				DBInstanceIdentifier: aws.String(protectedID + "-nonexistent"),
			}},
			{"Both", &rds.DeleteDBInstanceInput{
				DBInstanceIdentifier:      aws.String(protectedID + "-nonexistent"),
				SkipFinalSnapshot:         aws.Bool(true),
				FinalDBSnapshotIdentifier: aws.String(finalSnapshotID),
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				harness.ExpectError(t, "InvalidParameterCombination", func() error {
					_, err := f.AWS.RDS.DeleteDBInstance(tc.input)
					return err
				})
			})
		}
	})

	// The whole point of the flag: a delete that would have worked is refused, and
	// the database is still there afterwards.
	t.Run("DeletionProtectionBlocksTheDelete", func(t *testing.T) {
		harness.ExpectError(t, "InvalidParameterCombination", func() error {
			_, err := f.AWS.RDS.DeleteDBInstance(&rds.DeleteDBInstanceInput{
				DBInstanceIdentifier: aws.String(protectedID),
				SkipFinalSnapshot:    aws.Bool(true),
			})
			return err
		})

		current, err := harness.DescribeDBInstance(f.AWS, protectedID)
		require.NoError(t, err, "a refused delete must leave the instance describable")
		assert.Equal(t, harness.DBInstanceAvailable, aws.StringValue(current.DBInstanceStatus),
			"a refused delete must not have moved the instance towards deleting")

		harness.Step(t, "Clearing deletion protection on %q", protectedID)
		cleared, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: aws.String(protectedID),
			DeletionProtection:   aws.Bool(false),
			ApplyImmediately:     aws.Bool(true),
		})
		require.NoError(t, err, "modify-db-instance clearing deletion protection")
		assert.False(t, aws.BoolValue(cleared.DBInstance.DeletionProtection),
			"clearing protection is a record write, so it lands on the response")
		assert.Equal(t, harness.DBInstanceAvailable, aws.StringValue(cleared.DBInstance.DBInstanceStatus),
			"clearing a flag must not take the engine down")
	})

	// The engine is stopped cleanly before the VM goes, so the final snapshot is a
	// checkpoint rather than a datadir needing WAL replay. The volume it was cut
	// from outlives the instance, because the snapshot references its chunks (D18).
	t.Run("AFinalSnapshotIsTakenAndTheVolumeRetained", func(t *testing.T) {
		harness.Phase(t, "Deleting %q with final snapshot %q", protectedID, finalSnapshotID)
		out, err := f.AWS.RDS.DeleteDBInstance(&rds.DeleteDBInstanceInput{
			DBInstanceIdentifier:      aws.String(protectedID),
			FinalDBSnapshotIdentifier: aws.String(finalSnapshotID),
		})
		require.NoError(t, err, "delete-db-instance with a final snapshot")
		require.NotNil(t, out.DBInstance)
		assert.Equal(t, "deleting", aws.StringValue(out.DBInstance.DBInstanceStatus),
			"AWS answers a delete with the instance as it last stood")

		harness.WaitForDBInstanceGone(t, f.AWS, protectedID)
		harness.AssertVMGone(t, system, protectedVMID)

		snapshot, err := harness.DescribeDBSnapshot(f.AWS, finalSnapshotID)
		require.NoError(t, err, "the final snapshot must exist once the delete has returned")
		assert.Equal(t, harness.DBSnapshotAvailable, aws.StringValue(snapshot.Status))
		// Manual, not automated: the customer named it, so only the customer removes
		// it — and rds-9's retention sweep must not.
		assert.Equal(t, "manual", aws.StringValue(snapshot.SnapshotType))
		assert.Equal(t, protectedID, aws.StringValue(snapshot.DBInstanceIdentifier),
			"the snapshot must name the instance it outlived")
		assert.Equal(t, int64(dbStorageGiB), aws.Int64Value(snapshot.AllocatedStorage))

		requireVolumeExists(t, system, protectedVolumeID,
			"the data volume must be retained while the final snapshot references its chunks")
	})

	// A final snapshot nobody can restore from is worse than no snapshot: it reads
	// as protection that is not there.
	t.Run("TheFinalSnapshotRestoresTheRows", func(t *testing.T) {
		harness.Phase(t, "Restoring %q from the final snapshot %q", restoredID, finalSnapshotID)
		restoreFromSnapshot(t, f, restoredID, finalSnapshotID)
		instance := waitForAvailable(t, f, restoredID)

		restoredConn := harness.PSQLConnFor(t, instance, dbMasterUser, dbMasterPassword, dbName)
		note := harness.PSQL(t, client, restoredConn,
			fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", deleteTable))
		assert.Equal(t, deleteNote, strings.TrimSpace(note),
			"the row committed before the delete must come back out of the final snapshot")
	})

	// The end state of the whole final-snapshot path: once the customer removes the
	// snapshot they asked for, nothing of the instance is left to be billed for.
	t.Run("RemovingTheFinalSnapshotReleasesTheVolume", func(t *testing.T) {
		deleteInstance(t, f, restoredID)
		deleteDBSnapshot(t, f, finalSnapshotID)

		requireVolumeGone(t, system, protectedVolumeID)
		harness.AssertNoRDSRemnants(t, f.dbDiag(t), protectedID)
	})

	// The other choice, and the one that has to be complete: the volume, the
	// endpoint ENI and the DNS name all go. A leaked ENI holds an address in the
	// customer's subnet; a leaked record resolves to an address that is now
	// somebody else's.
	t.Run("SkipFinalSnapshotRemovesEverything", func(t *testing.T) {
		skipped := waitForAvailable(t, f, skippedID)
		require.NotNil(t, skipped.Endpoint)
		endpoint := aws.StringValue(skipped.Endpoint.Address)
		vmID := aws.StringValue(harness.DBInstanceVM(t, system, skippedID).InstanceId)
		volumeID := aws.StringValue(harness.DBInstanceDataVolume(t, system, skippedID).VolumeId)

		harness.Phase(t, "Deleting %q with SkipFinalSnapshot", skippedID)
		_, err := f.AWS.RDS.DeleteDBInstance(&rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: aws.String(skippedID),
			SkipFinalSnapshot:    aws.Bool(true),
		})
		require.NoError(t, err, "delete-db-instance skipping the final snapshot")

		harness.WaitForDBInstanceGone(t, f.AWS, skippedID)
		harness.AssertVMGone(t, system, vmID)
		// Nothing references the volume's chunks, so this delete releases it rather
		// than retaining it.
		requireVolumeGone(t, system, volumeID)
		harness.AssertNoRDSRemnants(t, f.dbDiag(t), skippedID)

		// A repeat of a delete that has finished has nothing left to name.
		t.Run("ASecondDeleteReportsNotFound", func(t *testing.T) {
			harness.ExpectError(t, "DBInstanceNotFound", func() error {
				_, err := f.AWS.RDS.DeleteDBInstance(&rds.DeleteDBInstanceInput{
					DBInstanceIdentifier: aws.String(skippedID),
					SkipFinalSnapshot:    aws.Bool(true),
				})
				return err
			})
		})

		// D6: the record only proves anything from inside a guest, and its withdrawal
		// is the half nothing else in the suite covers.
		t.Run("TheEndpointNameStopsResolvingInTheGuest", func(t *testing.T) {
			requireEndpointName(t, endpoint)
			harness.EventuallyErr(t, func() error {
				if addrs := harness.ResolveInGuest(t, client, endpoint); len(addrs) > 0 {
					return fmt.Errorf("%s still resolves to %v", endpoint, addrs)
				}
				return nil
			}, dnsWithdrawalTimeout, 10*time.Second)
		})
	})

	// The window a delete is most likely to leak in: the volume and the ENI exist,
	// the VM may or may not, and the record is not yet available. Nothing is waited
	// for beyond the create's own return, which is the point.
	t.Run("ADeleteWhileCreatingLeavesNothingBehind", func(t *testing.T) {
		id := fmt.Sprintf("%s-earlydel-%d", dbInstancePfx, suffix)
		created := createDBInstance(t, f, id)
		require.Equal(t, harness.DBInstanceCreating, aws.StringValue(created.DBInstanceStatus))

		harness.Step(t, "Deleting %q while it is still creating", id)
		_, err := f.AWS.RDS.DeleteDBInstance(&rds.DeleteDBInstanceInput{
			DBInstanceIdentifier: aws.String(id),
			SkipFinalSnapshot:    aws.Bool(true),
		})
		require.NoError(t, err, "a delete during create must be accepted, not deferred to available")

		harness.WaitForDBInstanceGone(t, f.AWS, id)
		harness.AssertNoRDSRemnants(t, f.dbDiag(t), id)
	})
}

// Clears deletion protection so the standard teardown can run. Best-effort: an
// instance already gone is the state this is trying to reach.
func clearDeletionProtection(t *testing.T, f *Fixture, id string) {
	t.Helper()
	if _, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: aws.String(id),
		DeletionProtection:   aws.Bool(false),
		ApplyImmediately:     aws.Bool(true),
	}); err != nil && !harness.ErrorCodeIs(err, "DBInstanceNotFound") {
		t.Logf("cleanup: clearing deletion protection on %s: %v", id, err)
	}
}
