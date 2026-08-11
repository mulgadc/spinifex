package handlers_rds

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRestoredID = "orders-db-restored"

// seedSnapshot takes a real snapshot of a seeded instance, so a restore reads
// the record CreateDBSnapshot actually writes rather than a hand-built one.
func (h *snapshotHarness) seedSnapshot(t *testing.T) DBSnapshotRecord {
	t.Helper()
	h.seedSnapshotSource(t)
	_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.NoError(t, err)
	rec, found := h.snapshot(t, testSnapshotID)
	require.True(t, found)
	return rec
}

func restoreInput() *rds.RestoreDBInstanceFromDBSnapshotInput {
	return &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: aws.String(testRestoredID),
		DBSnapshotIdentifier: aws.String(testSnapshotID),
	}
}

func TestRestoreDBInstanceFromDBSnapshot_BuildsANewInstanceOnTheSnapshotsData(t *testing.T) {
	h := newSnapshotHarness(t, false)
	snapshot := h.seedSnapshot(t)

	out, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.NoError(t, err)

	require.NotNil(t, out.DBInstance)
	assert.Equal(t, testRestoredID, aws.StringValue(out.DBInstance.DBInstanceIdentifier))
	assert.Equal(t, string(StatusCreating), aws.StringValue(out.DBInstance.DBInstanceStatus))

	// The datadir comes from the snapshot rather than from a fresh volume, so the
	// launch attaches a volume the restore created instead of making one.
	created := h.launch.volumes.created
	require.Len(t, created, 1)
	assert.Equal(t, snapshot.SnapshotID, aws.StringValue(created[0].SnapshotId))
	assert.Equal(t, int64(20), aws.Int64Value(created[0].Size))
	assert.Equal(t, tags.ManagedByRDS, tagOf(created[0].TagSpecifications, tags.ManagedByKey))
	// System-owned like every RDS data volume, so the customer reaches it only
	// through the DB instance.
	assert.Equal(t, []string{utils.GlobalAccountID}, h.launch.volumes.accts)

	stored := h.instance(t, testRestoredID)
	assert.Equal(t, "vol-rdsdata01", stored.DataVolumeID)
	assert.Equal(t, vm.VolumeSerial(stored.DataVolumeID), stored.DataVolumeSerial)
	assert.False(t, stored.FormatAuthorized, "snapshot-derived volumes must never receive a format grant")
	assert.Equal(t, testSnapshotID, stored.RestoredFromDBSnapshot)
	assert.True(t, stored.StorageEncrypted)

	// The datadir already holds the master role and its password hash, so
	// the agent's first bootstrap fetch has to attach rather than run initdb.
	assert.Equal(t, BootstrapStateNone, stored.Bootstrap.State)
	assert.Empty(t, stored.Bootstrap.MasterUserPassword)

	require.NotNil(t, h.launch.launcher.input)
	assert.Equal(t, h.iam.profileARN(utils.GlobalAccountID), h.launch.launcher.input.IamInstanceProfileArn)
}

func TestRestoreDBInstanceFromDBSnapshot_IAMFailurePrecedesReservationAndVolume(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)
	iamErr := errors.New("IAM store unavailable")
	h.iam.policyErr = iamErr

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)

	require.Error(t, err)
	assert.ErrorIs(t, err, iamErr)
	assert.False(t, h.instanceExists(t, testRestoredID))
	assert.Empty(t, h.launch.volumes.created)
	assert.Empty(t, h.launch.enis.created)
	assert.Nil(t, h.launch.launcher.input)
}

// Source-instance configuration comes from the snapshot when the restore
// request does not override it; platform-owned settings use current defaults.
func TestRestoreDBInstanceFromDBSnapshot_FallsBackToTheSnapshotsConfiguration(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.NoError(t, err)

	stored := h.instance(t, testRestoredID)
	assert.Equal(t, "db.t3.medium", stored.DBInstanceClass)
	assert.Equal(t, int64(20), stored.AllocatedStorage)
	assert.Equal(t, "postgres", stored.Engine)
	assert.Equal(t, "18.1", stored.EngineVersion)
	assert.Equal(t, "postgres", stored.MasterUsername)
	assert.Equal(t, "orders", stored.DBName)
	assert.Equal(t, int64(5432), stored.Port)
	assert.Equal(t, []string{testDefaultSG}, stored.VpcSecurityGroupIDs)
}

func TestRestoreDBInstanceFromDBSnapshot_DefaultsRetentionAndTakesAnAutomatedBackup(t *testing.T) {
	h := newSnapshotHarness(t, false)
	now := time.Now().UTC()
	h.svc.deps.Backup = BackupPolicy{
		RetentionDays:     3,
		BackupWindowBlock: openBackupWindow(now),
	}
	h.seedSnapshot(t)

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.NoError(t, err)

	stored := h.instance(t, testRestoredID)
	assert.Equal(t, int64(3), h.svc.defaultRetentionDays())
	assert.Equal(t, int64(3), stored.BackupRetentionPeriod)
	assert.Empty(t, stored.PreferredBackupWindow)
	assert.Empty(t, stored.PreferredMaintenanceWindow)
	window, err := h.svc.resolvedBackupWindow(&stored)
	require.NoError(t, err)
	require.True(t, window.contains(now), "the lazily assigned window should be open")

	newStubAgent(t, h.nc, testAccountID, testRestoredID, false)
	stored.Status = StatusAvailable
	seedInstance(t, h.svc, stored)
	require.True(t, h.runBackupPassFor(t, testRestoredID))

	stamps := h.automatedStamps(t, testRestoredID)
	require.Len(t, stamps, 1)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var entry AutomatedBackupRecord
	found, err := getJSON(t.Context(), kv, AutomatedBackupKey(testRestoredID, stamps[0]), &entry)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, testRestoredID, entry.DBInstanceIdentifier)
	backup, found := h.snapshot(t, entry.DBSnapshotIdentifier)
	require.True(t, found)
	assert.Equal(t, testRestoredID, backup.DBInstanceIdentifier)
	assert.Equal(t, SnapshotTypeAutomated, backup.SnapshotType)
	assert.Equal(t, SnapshotStatusAvailable, backup.Status)
	assert.NotNil(t, h.instance(t, testRestoredID).LastAutomatedBackupAt)
}

func TestRestoreDBInstanceFromDBSnapshot_HonoursTheRequestedOverrides(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)

	input := restoreInput()
	input.DBInstanceClass = aws.String("db.t3.large")
	input.AllocatedStorage = aws.Int64(50)
	input.Port = aws.Int64(6432)
	input.CopyTagsToSnapshot = aws.Bool(true)
	input.Tags = []*rds.Tag{{Key: aws.String("env"), Value: aws.String("staging")}}

	out, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), input, testAccountID)
	require.NoError(t, err)

	stored := h.instance(t, testRestoredID)
	assert.Equal(t, "db.t3.large", stored.DBInstanceClass)
	assert.Equal(t, int64(50), stored.AllocatedStorage)
	assert.Equal(t, int64(6432), stored.Port)
	assert.True(t, stored.CopyTagsToSnapshot)
	assert.True(t, aws.BoolValue(out.DBInstance.CopyTagsToSnapshot))
	assert.Equal(t, map[string]string{"env": "staging"}, stored.Tags)
	assert.Equal(t, int64(50), aws.Int64Value(h.launch.volumes.created[0].Size))
}

// CreateVolume refuses a size below the snapshot's, and a shrink has nowhere to
// put the data the snapshot already holds.
func TestRestoreDBInstanceFromDBSnapshot_RejectsStorageBelowTheSnapshots(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)

	input := restoreInput()
	input.AllocatedStorage = aws.Int64(10)

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), input, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.Empty(t, h.launch.volumes.created)
	assert.False(t, h.instanceExists(t, testRestoredID))
}

// The volume holds its source snapshot undeletable, so a restore that never
// produced an instance must not leave one behind.
func TestRestoreDBInstanceFromDBSnapshot_UnwindsTheVolumeAndTheRecordWhenTheLaunchFails(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)
	h.launch.launcher.err = errors.New("no node had capacity")

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.Error(t, err)

	assert.Contains(t, h.launch.volumes.deleted, "vol-rdsdata01")
	assert.False(t, h.instanceExists(t, testRestoredID),
		"the reserved identifier is withdrawn with everything else")
	// The ENIs the launch made are its own to unwind; what matters here is that
	// none survives the failure.
	assert.NotEmpty(t, h.launch.unwind)
}

func TestRestoreDBInstanceFromDBSnapshot_IndexFailureWithdrawsTheRecordedLaunch(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)
	// This invalid KV key makes the instance-index write fail after recordLaunch
	// has advanced the reservation revision.
	h.launch.launcher.instanceID = "invalid instance id"

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.Error(t, err)
	assert.False(t, h.instanceExists(t, testRestoredID))

	h.launch.launcher.instanceID = ""
	_, err = h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.NoError(t, err, "the failed restore must release the identifier for reuse")
}

func TestRestoreDBInstanceFromDBSnapshot_RecordLaunchDoesNotOverwriteAConcurrentReplacement(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)
	var replacement DBInstanceRecord
	h.launch.launcher.onLaunch = func() {
		replacement = replaceInstanceRecord(t, h.svc, testRestoredID)
	}

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.Error(t, err)

	assert.Equal(t, replacement, h.instance(t, testRestoredID))
}

func TestRestoreDBInstanceFromDBSnapshot_RollbackDoesNotDeleteAConcurrentReplacement(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)
	h.launch.launcher.instanceID = "invalid instance id"
	var replacement DBInstanceRecord
	h.launch.launcher.onTerminate = func() {
		assert.Equal(t, "invalid instance id", h.instance(t, testRestoredID).InstanceID,
			"recordLaunch must finish before the replacement race")
		replacement = replaceInstanceRecord(t, h.svc, testRestoredID)
	}

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.Error(t, err)

	assert.Equal(t, replacement, h.instance(t, testRestoredID))
}

func TestRestoreDBInstanceFromDBSnapshot_RejectsATakenIdentifier(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)

	input := restoreInput()
	input.DBInstanceIdentifier = aws.String(testDBID)

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), input, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceAlreadyExists)
	assert.Empty(t, h.launch.volumes.created)
}

func TestRestoreDBInstanceFromDBSnapshot_RejectsASnapshotThatDoesNotExist(t *testing.T) {
	h := newSnapshotHarness(t, false)

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBSnapshotNotFound)
}

// A snapshot still being taken has no data to restore from.
func TestRestoreDBInstanceFromDBSnapshot_RejectsASnapshotStillBeingTaken(t *testing.T) {
	h := newSnapshotHarness(t, false)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	creating := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
		Engine:               "postgres",
	}
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &creating))

	_, err = h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBSnapshotInvalidState)
}

// A parameter that would create a false safety, security or availability
// guarantee is rejected rather than silently dropped.
func TestRestoreDBInstanceFromDBSnapshot_RejectsUnimplementedParameters(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)

	cases := map[string]func(*rds.RestoreDBInstanceFromDBSnapshotInput){
		"MultiAZ":            func(in *rds.RestoreDBInstanceFromDBSnapshotInput) { in.MultiAZ = aws.Bool(true) },
		"PubliclyAccessible": func(in *rds.RestoreDBInstanceFromDBSnapshotInput) { in.PubliclyAccessible = aws.Bool(true) },
		"Iops":               func(in *rds.RestoreDBInstanceFromDBSnapshotInput) { in.Iops = aws.Int64(3000) },
		"AvailabilityZone": func(in *rds.RestoreDBInstanceFromDBSnapshotInput) {
			in.AvailabilityZone = aws.String("ap-southeast-2b")
		},
		"OptionGroupName": func(in *rds.RestoreDBInstanceFromDBSnapshotInput) { in.OptionGroupName = aws.String("custom") },
		"EnableIAMDatabaseAuthentication": func(in *rds.RestoreDBInstanceFromDBSnapshotInput) {
			in.EnableIAMDatabaseAuthentication = aws.Bool(true)
		},
		"DBClusterSnapshotIdentifier": func(in *rds.RestoreDBInstanceFromDBSnapshotInput) {
			in.DBClusterSnapshotIdentifier = aws.String("cluster-snap")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := restoreInput()
			mutate(input)
			_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), input, testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), name)
		})
	}
}

// The datadir is written in one engine's on-disk format and no other can read
// it, so a request naming a different engine is refused rather than honoured.
func TestRestoreDBInstanceFromDBSnapshot_RejectsAnEngineChange(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)

	input := restoreInput()
	input.Engine = aws.String("mysql")

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), input, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// Read from the volume rather than echoed from the snapshot: a cluster whose
// storage key has gone would otherwise report encryption it is not giving.
func TestRestoreDBInstanceFromDBSnapshot_RefusesAnUnencryptedVolumeForAnEncryptedSnapshot(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshot(t)
	h.launch.volumes.encrypted = false

	_, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), restoreInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unencrypted")
	assert.Contains(t, h.launch.volumes.deleted, "vol-rdsdata01")
	assert.False(t, h.instanceExists(t, testRestoredID))
}

func (h *snapshotHarness) instanceExists(t *testing.T, id string) bool {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(id), &rec)
	require.NoError(t, err)
	return found
}
