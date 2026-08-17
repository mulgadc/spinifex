package handlers_rds

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Seeds one automated backup exactly as a fired window leaves it: the snapshot
// record, an EC2 snapshot holding the source volume, and the index entry the
// retention sweep drives from.
func seedAutomatedBackup(t *testing.T, svc *Service, snaps *fakeSnapshots,
	accountID, dbID string, age time.Duration, status string) string {
	t.Helper()
	kv, err := svc.bucket(t.Context(), accountID)
	require.NoError(t, err)

	createdAt := time.Now().UTC().Add(-age)
	id := AutomatedSnapshotIdentifier(dbID, createdAt)
	stamp := AutomatedBackupStamp(createdAt)
	ec2Snapshot := "snap-" + stamp

	snaps.holding = append(snaps.holding, ec2Snapshot)
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(id), &DBSnapshotRecord{
		DBSnapshotIdentifier: id,
		DBInstanceIdentifier: dbID,
		AccountID:            accountID,
		SnapshotType:         SnapshotTypeAutomated,
		Status:               status,
		SnapshotID:           ec2Snapshot,
		SourceVolumeID:       "vol-rdsdata01",
		CreatedAt:            createdAt,
	}))
	require.NoError(t, putJSON(t.Context(), kv, AutomatedBackupKey(dbID, stamp), &AutomatedBackupRecord{
		DBInstanceIdentifier: dbID,
		DBSnapshotIdentifier: id,
		CreatedAt:            createdAt,
	}))
	return id
}

func (h *snapshotHarness) seedBackup(t *testing.T, age time.Duration) string {
	t.Helper()
	return seedAutomatedBackup(t, h.svc, h.snaps, testAccountID, testDBID, age, SnapshotStatusAvailable)
}

func (h *snapshotHarness) sweep(t *testing.T) int {
	t.Helper()
	reaped, err := h.svc.NewBackupRetentionReaper().Sweep(t.Context())
	require.NoError(t, err)
	return reaped
}

func (h *snapshotHarness) snapshotExists(t *testing.T, id string) bool {
	t.Helper()
	_, found := h.snapshot(t, id)
	return found
}

// An instance with backups on, whose window is closed so the sweep is the only
// thing acting on its backup set.
func retainingRecord(days int64) DBInstanceRecord {
	rec := availableRecord()
	rec.BackupRetentionPeriod = days
	rec.PreferredBackupWindow = closedBackupWindow(time.Now().UTC())
	return rec
}

// The GC framework gives the sweep the two properties it needs: it is skipped
// while KV is unhealthy, and cluster-wide scope is leader-gated by the framework
// rather than by anything here.
func TestBackupRetentionReaper_IsAClusterWideReaper(t *testing.T) {
	t.Parallel()
	reaper := NewService(nil, testRegion).NewBackupRetentionReaper()
	assert.Equal(t, "rds-backup-retention", reaper.Class())
	assert.Equal(t, vm.ScopeClusterWide, reaper.Scope())
	assert.Equal(t, defaultSweepDeleteLimit, reaper.limit)

	tuned := NewService(nil, testRegion).WithDeps(Deps{Backup: BackupPolicy{SweepDeleteLimit: 4}})
	assert.Equal(t, 4, tuned.NewBackupRetentionReaper().limit)
}

func TestSweep_RemovesOnlyTheBackupsPastRetention(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, retainingRecord(7))

	fresh := h.seedBackup(t, oneDay)
	recent := h.seedBackup(t, 3*oneDay)
	stale := h.seedBackup(t, 8*oneDay)
	ancient := h.seedBackup(t, 30*oneDay)

	assert.Equal(t, 2, h.sweep(t))

	assert.True(t, h.snapshotExists(t, fresh))
	assert.True(t, h.snapshotExists(t, recent))
	assert.False(t, h.snapshotExists(t, stale))
	assert.False(t, h.snapshotExists(t, ancient))

	// The index goes with the snapshot, or the next pass would keep finding a
	// backup that no longer exists.
	assert.Len(t, h.automatedStamps(t, testDBID), 2)
	// And the EC2 snapshots behind them, which is what actually frees the chunks.
	assert.Len(t, h.snaps.deleted, 2)
}

// If backup creation has been failing for a week, strict retention would delete
// the whole backup set at the moment a backup matters most.
func TestSweep_NeverDeletesTheNewestBackupWhileBackupsAreOn(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, retainingRecord(1))

	newest := h.seedBackup(t, 5*oneDay)
	older := h.seedBackup(t, 9*oneDay)

	assert.Equal(t, 1, h.sweep(t))
	assert.True(t, h.snapshotExists(t, newest), "the last restorable copy is kept even past its retention")
	assert.False(t, h.snapshotExists(t, older))

	// And it stays kept: a sweep that eventually got round to it would be the same
	// data loss one pass later.
	assert.Equal(t, 0, h.sweep(t))
	assert.True(t, h.snapshotExists(t, newest))
}

// A snapshot still being cut is not something the customer could restore from, so
// keeping it instead of the newest available one would leave them with nothing.
func TestSweep_KeepsTheNewestAvailableRatherThanTheNewest(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, retainingRecord(1))

	creating := seedAutomatedBackup(t, h.svc, h.snaps, testAccountID, testDBID, 3*oneDay, SnapshotStatusCreating)
	available := h.seedBackup(t, 5*oneDay)

	assert.Equal(t, 0, h.sweep(t))
	assert.True(t, h.snapshotExists(t, available), "the newest available backup is the one retention keeps")
	assert.True(t, h.snapshotExists(t, creating), "a snapshot still being cut is skipped, not failed")
	assert.Empty(t, h.snaps.deleted)
}

// The point of turning automated backups off is to make the data volume
// GC-eligible again, so the last one goes too.
func TestSweep_RemovesEverythingWhenBackupsAreOff(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, retainingRecord(0))

	newest := h.seedBackup(t, time.Hour)
	older := h.seedBackup(t, 2*oneDay)

	assert.Equal(t, 2, h.sweep(t))
	assert.False(t, h.snapshotExists(t, newest))
	assert.False(t, h.snapshotExists(t, older))
	assert.Empty(t, h.automatedStamps(t, testDBID))
}

// An index outliving its instance is a teardown that did not finish. Nothing can
// restore an instance that no longer exists, and the alternative is a data volume
// pinned for good.
func TestSweep_RemovesEverythingWhenTheInstanceIsGone(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)

	newest := h.seedBackup(t, time.Hour)
	older := h.seedBackup(t, 2*oneDay)

	assert.Equal(t, 2, h.sweep(t))
	assert.False(t, h.snapshotExists(t, newest))
	assert.False(t, h.snapshotExists(t, older))
}

// A KV read served by a lagging replica reads exactly like a finished teardown,
// and the branch it would select deletes a live instance's whole backup set. The
// bucket listing is the corroboration that stops it.
func TestSweep_LeavesTheBackupSetWhenTheRecordCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, retainingRecord(7))
	newest := h.seedBackup(t, time.Hour)
	older := h.seedBackup(t, 30*oneDay)

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	stale := missingKey{KeyValue: kv, key: DBInstanceKey(testDBID)}

	reaped, err := h.svc.sweepInstanceBackups(t.Context(), stale, testAccountID, testDBID,
		h.automatedStamps(t, testDBID), defaultSweepDeleteLimit)
	require.Error(t, err, "an unreadable record is not proof the instance is gone")
	assert.Zero(t, reaped)
	assert.True(t, h.snapshotExists(t, newest))
	assert.True(t, h.snapshotExists(t, older), "not even the over-retention one goes on an unread record")
	assert.Len(t, h.automatedStamps(t, testDBID), 2)
}

// A bucket read that misses one key while every other read succeeds, which is
// what a stale replica looks like from here.
type missingKey struct {
	jetstream.KeyValue

	key string
}

func (m missingKey) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if key == m.key {
		return nil, jetstream.ErrKeyNotFound
	}
	return m.KeyValue.Get(ctx, key)
}

// An instance restored from the snapshot is still reading through it. That is a
// skip rather than a failure: the next pass retries, and the index entry stays so
// the backup is not lost from it.
func TestSweep_SkipsABackupStillInUse(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, retainingRecord(0))
	inUse := h.seedBackup(t, 2*oneDay)
	h.snaps.deleteErr = awserrors.Errorf(awserrors.ErrorInvalidSnapshotInUse,
		"the snapshot is in use by vol-restored01")

	reaped, err := h.svc.NewBackupRetentionReaper().Sweep(t.Context())
	require.NoError(t, err, "a snapshot in use is not a sweep failure")
	assert.Zero(t, reaped)
	assert.True(t, h.snapshotExists(t, inUse))
	assert.Len(t, h.automatedStamps(t, testDBID), 1, "the entry stays so the backup is still findable")

	// Once the reader is gone the next pass collects it.
	h.snaps.deleteErr = nil
	assert.Equal(t, 1, h.sweep(t))
	assert.False(t, h.snapshotExists(t, inUse))
}

// A pass that under-collects is corrected by the next one two minutes later; a
// pass that walks an unbounded number of snapshots is not.
func TestSweep_IsBoundedPerPassAndResumes(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	h.svc = h.svc.WithDeps(Deps{
		LoadCA:    newTestCA(t),
		MasterKey: testMasterKey,
		Launch:    h.launch.deps(),
		Network:   h.network,
		Snapshots: h.snaps,
		Backup:    BackupPolicy{SweepDeleteLimit: 1},
	})
	seedInstance(t, h.svc, retainingRecord(0))
	for _, age := range []time.Duration{oneDay, 2 * oneDay, 3 * oneDay} {
		h.seedBackup(t, age)
	}

	assert.Equal(t, 1, h.sweep(t))
	assert.Len(t, h.automatedStamps(t, testDBID), 2)
	assert.Equal(t, 1, h.sweep(t))
	assert.Equal(t, 1, h.sweep(t))
	assert.Empty(t, h.automatedStamps(t, testDBID))
	assert.Equal(t, 0, h.sweep(t))
}

// A half-finished delete: the snapshot went and the entry did not. The entry is
// cleared so the index stops naming it, and nothing is counted as reaped because
// no data went with it.
func TestSweep_DropsAnIndexEntryWhoseSnapshotIsGone(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, retainingRecord(7))

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	createdAt := time.Now().UTC().Add(-2 * oneDay)
	stamp := AutomatedBackupStamp(createdAt)
	require.NoError(t, putJSON(t.Context(), kv, AutomatedBackupKey(testDBID, stamp), &AutomatedBackupRecord{
		DBInstanceIdentifier: testDBID,
		DBSnapshotIdentifier: AutomatedSnapshotIdentifier(testDBID, createdAt),
		CreatedAt:            createdAt,
	}))

	assert.Equal(t, 0, h.sweep(t))
	assert.Empty(t, h.automatedStamps(t, testDBID))
}

// Every tenant's retention is enforced on the same pass, so one account's backlog
// cannot leave another's unenforced.
func TestSweep_CoversEveryAccount(t *testing.T) {
	t.Parallel()
	const otherAccount = "210987654321"
	h := newSnapshotHarness(t, false)

	mine := h.seedBackup(t, 2*oneDay)
	theirs := seedAutomatedBackup(t, h.svc, h.snaps, otherAccount, "billing-db", 2*oneDay, SnapshotStatusAvailable)

	assert.Equal(t, 2, h.sweep(t))
	assert.False(t, h.snapshotExists(t, mine))

	kv, err := h.svc.bucket(t.Context(), otherAccount)
	require.NoError(t, err)
	var rec DBSnapshotRecord
	found, err := getJSON(t.Context(), kv, DBSnapshotKey(theirs), &rec)
	require.NoError(t, err)
	assert.False(t, found)
}

// A truncated listing must never read as "no accounts": that would leave every
// tenant's retention unenforced while looking like a clean pass.
func TestSweep_FailsWhenTheAccountBucketsCannotBeEnumerated(t *testing.T) {
	t.Parallel()
	_, err := NewService(nil, testRegion).NewBackupRetentionReaper().Sweep(t.Context())
	require.Error(t, err)
}

// The backstop for a crash between the last DeleteDBSnapshot and the inline
// volume delete that follows it: nothing else references the volume by then.
func TestSweep_ReclaimsAnOrphanedRetainedVolume(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(t.Context(), kv, RetainedVolumeKey("vol-orphan01"), &RetainedVolumeRecord{
		VolumeID:             "vol-orphan01",
		AccountID:            testAccountID,
		DBInstanceIdentifier: testDBID,
		RetainedAt:           time.Now().UTC().Add(-time.Hour),
	}))

	assert.Equal(t, 1, h.sweep(t))
	assert.Equal(t, []string{"vol-orphan01"}, h.launch.volumes.deleted)

	var retained RetainedVolumeRecord
	found, err := getJSON(t.Context(), kv, RetainedVolumeKey("vol-orphan01"), &retained)
	require.NoError(t, err)
	assert.False(t, found)
}

// The volume store's index is the authority, not the holder list the record was
// written with: a snapshot taken since would read as "nothing holds it", and one
// deleted since would pin the volume for good.
func TestSweep_ReChecksTheHoldersRatherThanTrustingTheRecord(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(t.Context(), kv, RetainedVolumeKey("vol-held01"), &RetainedVolumeRecord{
		VolumeID:  "vol-held01",
		AccountID: testAccountID,
		Snapshots: []string{"snap-gone01"},
	}))
	// A holder the record does not name, taken after it was written.
	h.snaps.holding = append(h.snaps.holding, "snap-9999")

	assert.Equal(t, 0, h.sweep(t))
	assert.Empty(t, h.launch.volumes.deleted, "a volume a snapshot still reads through must not go")

	var stored RetainedVolumeRecord
	found, err := getJSON(t.Context(), kv, RetainedVolumeKey("vol-held01"), &stored)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []string{"snap-9999"}, stored.Snapshots)
}

// The volume store refusing a delete without naming a holder means "unknown", not
// "nobody". It is recorded and re-checked rather than failing the pass, so a
// disagreement between the two indexes never wedges the sweep.
func TestSweep_RetainsAVolumeTheVolumeStoreRefusesToRelease(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(t.Context(), kv, RetainedVolumeKey("vol-held01"), &RetainedVolumeRecord{
		VolumeID:  "vol-held01",
		AccountID: testAccountID,
		Snapshots: []string{"snap-gone01"},
	}))
	h.launch.volumes.deleteErr = awserrors.Errorf(awserrors.ErrorVolumeInUse, "the volume is in use")

	assert.Equal(t, 0, h.sweep(t))

	var stored RetainedVolumeRecord
	found, err := getJSON(t.Context(), kv, RetainedVolumeKey("vol-held01"), &stored)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, stored.HoldersUnresolved, "the disagreement is recorded so the next pass re-checks")
}

// Turning automated backups off sweeps the set that exists rather than leaving it
// to expire against a retention that is now zero.
func TestModifyDBInstance_SweepsTheAutomatedBackupsWhenRetentionGoesToZero(t *testing.T) {
	t.Parallel()
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, retainingRecord(7))
	swept := h.seedBackup(t, time.Hour)

	_, err := h.svc.ModifyDBInstance(t.Context(), &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier:  aws.String(testDBID),
		BackupRetentionPeriod: aws.Int64(0),
	}, testAccountID)
	require.NoError(t, err)

	assert.Zero(t, h.instance(t, testDBID).BackupRetentionPeriod)
	assert.False(t, h.snapshotExists(t, swept))
	assert.Empty(t, h.automatedStamps(t, testDBID))
}

// An automated backup outliving its instance would pin the instance's data
// volume for good, so teardown sweeps the set before releasing the volume.
func TestDeleteDBInstance_SweepsTheAutomatedBackups(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, availableRecord())
	swept := seedAutomatedBackup(t, h.svc, h.snaps, testAccountID, testDBID, oneDay, SnapshotStatusAvailable)

	_, err := h.svc.DeleteDBInstance(t.Context(), skipFinalSnapshot(), testAccountID)
	require.NoError(t, err)

	_, found := h.snapshotRecord(t, swept)
	assert.False(t, found)
	// Nothing holds the data volume once the automated set is gone, so it is
	// deleted rather than retained.
	assert.Equal(t, []string{"vol-rdsdata01"}, h.volumes.deleted)
	_, found = h.retainedVolume(t, "vol-rdsdata01")
	assert.False(t, found)
}
