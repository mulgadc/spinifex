package handlers_rds

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A daily window that is open at now, exactly the 30-minute minimum wide.
func openBackupWindow(now time.Time) string {
	start := sinceMidnight(now.Add(-15 * time.Minute))
	return formatClock(start) + "-" + formatClock(start+windowSlot)
}

// A daily window that is closed at now, well clear of it on both sides.
func closedBackupWindow(now time.Time) string {
	start := sinceMidnight(now.Add(2 * time.Hour))
	return formatClock(start) + "-" + formatClock(start+time.Hour)
}

func openMaintenanceWindow(now time.Time) string {
	start := sinceSunday(now.Add(-15 * time.Minute))
	return formatWeekdayClock(start) + "-" + formatWeekdayClock(start+windowSlot)
}

func closedMaintenanceWindow(now time.Time) string {
	start := sinceSunday(now.Add(2 * time.Hour))
	return formatWeekdayClock(start) + "-" + formatWeekdayClock(start+time.Hour)
}

// An instance the scheduler will back up: available, with a data volume, backups
// on, and a window that is open right now.
func backupReadyRecord() DBInstanceRecord {
	rec := availableRecord()
	rec.BackupRetentionPeriod = 7
	rec.PreferredBackupWindow = openBackupWindow(time.Now().UTC())
	rec.PreferredMaintenanceWindow = closedMaintenanceWindow(time.Now().UTC())
	return rec
}

// The revision the record is at, which is what the scheduled passes CAS against.
func instanceRevision(t *testing.T, svc *Service, id string) (*DBInstanceRecord, uint64) {
	t.Helper()
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	rec, rev, err := svc.getDBInstance(t.Context(), kv, id)
	require.NoError(t, err)
	return rec, rev
}

// Runs one backup pass the way the reconciler does: re-read at its revision,
// then fire.
func (h *snapshotHarness) runBackupPass(t *testing.T) bool {
	t.Helper()
	return h.runBackupPassFor(t, testDBID)
}

func (h *snapshotHarness) runBackupPassFor(t *testing.T, id string) bool {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	rec, rev := instanceRevision(t, h.svc, id)
	fired, err := h.svc.runBackupWindow(t.Context(), kv, rev, testAccountID, rec)
	require.NoError(t, err)
	return fired
}

func (h *snapshotHarness) runMaintenancePass(t *testing.T) bool {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	rec, rev := instanceRevision(t, h.svc, testDBID)
	fired, err := h.svc.runMaintenanceWindow(t.Context(), kv, rev, testAccountID, rec)
	require.NoError(t, err)
	return fired
}

func (h *snapshotHarness) automatedStamps(t *testing.T, id string) []string {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	stamps, err := ListAutomatedBackupStamps(t.Context(), kv, id)
	require.NoError(t, err)
	return stamps
}

func TestParseDailyWindow_RejectsWhatAWSRejects(t *testing.T) {
	cases := map[string]string{
		"NoSeparator":      "0300-0400",
		"TooManyParts":     "03:00-04:00-05:00",
		"SingleDigitHour":  "3:00-4:00",
		"NoMinutes":        "03-04",
		"HourOutOfRange":   "24:00-25:00",
		"MinuteOutOfRange": "03:60-04:00",
		"NotDigits":        "aa:bb-cc:dd",
		// A zero-length window is the degenerate reading of a wrap, so it has to
		// fail the minimum rather than be measured as a whole day.
		"ZeroLength":  "03:00-03:00",
		"TooShort":    "03:00-03:15",
		"WeekdayForm": "sun:03:00-sun:04:00",
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseDailyWindow("PreferredBackupWindow", value)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		})
	}
}

func TestParseDailyWindow_AcceptsAWindowThatWrapsMidnight(t *testing.T) {
	window, err := parseDailyWindow("PreferredBackupWindow", "23:45-00:15")
	require.NoError(t, err)
	assert.Equal(t, windowSlot, window.length())
	assert.Equal(t, "23:45-00:15", window.String())

	assert.True(t, window.contains(time.Date(2026, 7, 30, 23, 50, 0, 0, time.UTC)))
	assert.True(t, window.contains(time.Date(2026, 7, 31, 0, 5, 0, 0, time.UTC)))
	assert.False(t, window.contains(time.Date(2026, 7, 30, 23, 40, 0, 0, time.UTC)))
	assert.False(t, window.contains(time.Date(2026, 7, 31, 0, 20, 0, 0, time.UTC)))

	// The instant it opened is yesterday's, which is what stops a backup taken at
	// 23:50 from firing again at 00:05 the next morning.
	assert.Equal(t, time.Date(2026, 7, 30, 23, 45, 0, 0, time.UTC),
		window.openedAt(time.Date(2026, 7, 31, 0, 5, 0, 0, time.UTC)))
}

func TestParseWeeklyWindow_RejectsWhatAWSRejects(t *testing.T) {
	cases := map[string]string{
		"NoDay":          "03:00-04:00",
		"UnknownDay":     "xyz:03:00-xyz:04:00",
		"FullDayName":    "sunday:03:00-sunday:04:00",
		"PartialDayName": "un:03:00-un:04:00",
		"UnpaddedClock":  "sun:3:00-sun:4:00",
		"ZeroLength":     "sun:03:00-sun:03:00",
		"TooShort":       "sun:03:00-sun:03:15",
		"MalformedClock": "mon:03:00-mon:0245",
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseWeeklyWindow("PreferredMaintenanceWindow", value)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		})
	}
}

func TestParseWeeklyWindow_AcceptsAWindowThatWrapsTheWeek(t *testing.T) {
	window, err := parseWeeklyWindow("PreferredMaintenanceWindow", "sat:23:45-sun:00:15")
	require.NoError(t, err)
	assert.Equal(t, windowSlot, window.length())
	assert.Equal(t, "sat:23:45-sun:00:15", window.String())

	// Saturday the 1st of August 2026 at 23:50, and the Sunday minutes after it.
	assert.True(t, window.contains(time.Date(2026, 8, 1, 23, 50, 0, 0, time.UTC)))
	assert.True(t, window.contains(time.Date(2026, 8, 2, 0, 5, 0, 0, time.UTC)))
	assert.False(t, window.contains(time.Date(2026, 8, 2, 0, 20, 0, 0, time.UTC)))
	assert.Equal(t, time.Date(2026, 8, 1, 23, 45, 0, 0, time.UTC),
		window.openedAt(time.Date(2026, 8, 2, 0, 5, 0, 0, time.UTC)))
}

// AWS refuses the pair rather than the individual window, because the two would
// collide the week the maintenance window's day comes round.
func TestValidateWindows_RejectsAnOverlappingPair(t *testing.T) {
	svc := NewService(nil, testRegion)

	_, _, err := svc.validateWindows(testDBID, "03:00-04:00", "mon:03:30-mon:04:30")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterCombination)

	// The same times on a day the backup window cannot reach are still refused: the
	// overlap is in the time of day, whatever weekday it falls on.
	_, _, err = svc.validateWindows(testDBID, "03:00-04:00", "sat:03:30-sat:04:30")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterCombination)
}

// Canonical text, not the customer's: a describe has to read back the string a
// later modify compares against, or every modify would look like a change.
func TestValidateWindows_ReturnsTheCanonicalText(t *testing.T) {
	backup, maintenance, err := NewService(nil, testRegion).
		validateWindows(testDBID, "03:00-04:00", "SUN:05:00-sun:06:00")
	require.NoError(t, err)
	assert.Equal(t, "03:00-04:00", backup)
	assert.Equal(t, "sun:05:00-sun:06:00", maintenance)
}

// The assignment stands in for AWS's random one. It has to be stable, because a
// window that moved whenever the record was rewritten would back up at an hour
// the customer never saw.
func TestValidateWindows_AssignsStableNonOverlappingWindows(t *testing.T) {
	svc := NewService(nil, testRegion)
	block := svc.backupWindowBlock()
	maintenanceBlock := svc.maintenanceWindowBlock()

	assigned := map[string]bool{}
	for _, id := range []string{"orders-db", "billing-db", "sessions-db", "audit-db", "reporting-db"} {
		backup, maintenance, err := svc.validateWindows(id, "", "")
		require.NoError(t, err)

		again, _, err := svc.validateWindows(id, "", "")
		require.NoError(t, err)
		assert.Equal(t, backup, again, "the assignment for %s has to be stable", id)

		backupWindow, err := parseDailyWindow("PreferredBackupWindow", backup)
		require.NoError(t, err)
		maintenanceWindow, err := parseWeeklyWindow("PreferredMaintenanceWindow", maintenance)
		require.NoError(t, err)

		assert.Equal(t, windowSlot, backupWindow.length())
		assert.Equal(t, windowSlot, maintenanceWindow.length())
		assert.True(t, withinSegments(block.segments(), backupWindow.start),
			"%s: the backup window %s should start inside %s", id, backup, block)
		assert.True(t, withinSegments(maintenanceBlock.segments(), maintenanceWindow.start%oneDay),
			"%s: the maintenance window %s should start inside %s", id, maintenance, maintenanceBlock)
		// The two default blocks are disjoint, which is what keeps a derived pair
		// from being the pair AWS itself would refuse.
		assert.False(t, backupWindow.overlaps(maintenanceWindow))

		assigned[backup] = true
	}
	assert.Greater(t, len(assigned), 1, "the fleet's quiesce load should spread across the block")
}

// A window the customer never sent must never be the reason their request is
// refused. Naming one window inside the other's block is the ordinary Terraform
// shape — backup_window set, maintenance_window left out — and assigning the
// second one on top of it would fail the same identifier deterministically,
// forever.
func TestValidateWindows_AssignsAroundACustomerNamedWindow(t *testing.T) {
	svc := NewService(nil, testRegion)
	// Inside the other window's block, which is where a collision is possible at
	// all: an hour of the maintenance block covers two of its assignable slots.
	const insideMaintenanceBlock = "13:00-14:00"
	const insideBackupBlock = "wed:04:00-wed:05:00"

	for i := range 200 {
		id := fmt.Sprintf("orders-db-%d", i)

		backup, maintenance, err := svc.validateWindows(id, insideMaintenanceBlock, "")
		require.NoError(t, err, "%s: a named backup window must not be refused over an assigned one", id)
		assert.Equal(t, insideMaintenanceBlock, backup, "%s: the named window is kept as named", id)
		assertWindowsDoNotOverlap(t, id, backup, maintenance)

		backup, maintenance, err = svc.validateWindows(id, "", insideBackupBlock)
		require.NoError(t, err, "%s: a named maintenance window must not be refused over an assigned one", id)
		assert.Equal(t, insideBackupBlock, maintenance)
		assertWindowsDoNotOverlap(t, id, backup, maintenance)
	}

	// Stable, like every other assignment: stepping off the named window must not
	// make the result depend on when it was resolved.
	first, second, err := svc.validateWindows(testDBID, insideMaintenanceBlock, "")
	require.NoError(t, err)
	again, alsoAgain, err := svc.validateWindows(testDBID, insideMaintenanceBlock, "")
	require.NoError(t, err)
	assert.Equal(t, first, again)
	assert.Equal(t, second, alsoAgain)
}

// A pair the customer named in full is still refused — the assignment moving out
// of the way is for windows the platform chose, not for the ones it was given.
func TestValidateWindows_StillRejectsAPairTheCustomerNamed(t *testing.T) {
	_, _, err := NewService(nil, testRegion).
		validateWindows(testDBID, "13:00-14:00", "wed:13:30-wed:14:30")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterCombination)
}

// The window a record does not carry is derived the same way the request that
// stored the other one derived it. Anything else would schedule the two passes
// over each other on exactly the records that name one window.
func TestResolvedWindows_AssignAroundTheStoredWindow(t *testing.T) {
	svc := NewService(nil, testRegion)

	for i := range 200 {
		id := fmt.Sprintf("orders-db-%d", i)

		backupOnly := &DBInstanceRecord{DBInstanceIdentifier: id, PreferredBackupWindow: "13:00-14:00"}
		assertWindowsDoNotOverlap(t, id, svc.reportedBackupWindow(backupOnly), svc.reportedMaintenanceWindow(backupOnly))

		maintenanceOnly := &DBInstanceRecord{DBInstanceIdentifier: id, PreferredMaintenanceWindow: "wed:04:00-wed:05:00"}
		assertWindowsDoNotOverlap(t, id,
			svc.reportedBackupWindow(maintenanceOnly), svc.reportedMaintenanceWindow(maintenanceOnly))
	}
}

func assertWindowsDoNotOverlap(t *testing.T, id, backup, maintenance string) {
	t.Helper()
	backupWindow, err := parseDailyWindow("PreferredBackupWindow", backup)
	require.NoError(t, err)
	maintenanceWindow, err := parseWeeklyWindow("PreferredMaintenanceWindow", maintenance)
	require.NoError(t, err)
	assert.False(t, backupWindow.overlaps(maintenanceWindow),
		"%s: %s and %s should not overlap", id, backup, maintenance)
}

// An operator's typo cannot be allowed to fail every create on the node, so the
// built-in block is used and the fault is logged instead.
func TestWindowBlock_FallsBackToTheBuiltInBlock(t *testing.T) {
	svc := NewService(nil, testRegion).WithDeps(Deps{Backup: BackupPolicy{
		BackupWindowBlock:      "not-a-window",
		MaintenanceWindowBlock: "19:00",
	}})

	builtIn, err := parseDailyWindow("block", defaultBackupWindowBlock)
	require.NoError(t, err)
	assert.Equal(t, builtIn, svc.backupWindowBlock())

	builtIn, err = parseDailyWindow("block", defaultMaintenanceWindowBlock)
	require.NoError(t, err)
	assert.Equal(t, builtIn, svc.maintenanceWindowBlock())
}

func TestValidateRetentionPeriod_BoundsTheRetention(t *testing.T) {
	svc := NewService(nil, testRegion)
	require.NoError(t, svc.validateRetentionPeriod(0))
	require.NoError(t, svc.validateRetentionPeriod(defaultBackupRetentionCapDays))

	err := svc.validateRetentionPeriod(defaultBackupRetentionCapDays + 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	require.Error(t, svc.validateRetentionPeriod(-1))

	// A cap lowered below the configured default hands new instances the cap, not a
	// retention the same cluster would reject on modify.
	capped := NewService(nil, testRegion).WithDeps(Deps{Backup: BackupPolicy{
		RetentionCapDays: 3,
		RetentionDays:    7,
	}})
	assert.Equal(t, int64(3), capped.defaultRetentionDays())
	require.Error(t, capped.validateRetentionPeriod(4))
}

// The stamp, not a timer: this is what makes the pass fire exactly once per
// window across leader churn and daemon restarts, and what stops a missed window
// from being backfilled.
func TestBackupDue_FiresOncePerWindowAndNeverBackfills(t *testing.T) {
	window, err := parseDailyWindow("PreferredBackupWindow", "03:00-03:30")
	require.NoError(t, err)
	svc := NewService(nil, testRegion)

	inWindow := time.Date(2026, 7, 30, 3, 10, 0, 0, time.UTC)
	opened := window.openedAt(inWindow)
	rec := &DBInstanceRecord{DBInstanceIdentifier: testDBID, BackupRetentionPeriod: 7}

	assert.True(t, svc.backupDue(rec, window, inWindow))

	// Already fired in this window: every later pass inside it, on any node, reads
	// the same stamp and stands down.
	fired := opened.Add(time.Minute)
	rec.LastAutomatedBackupAt = &fired
	assert.False(t, svc.backupDue(rec, window, inWindow))
	assert.False(t, svc.backupDue(rec, window, inWindow.Add(19*time.Minute)))

	// The next day's window is a new one.
	assert.True(t, svc.backupDue(rec, window, inWindow.Add(oneDay)))

	// Outside the window nothing fires, however long ago the last backup was: a
	// window that was missed is never caught up on.
	stale := opened.Add(-30 * oneDay)
	rec.LastAutomatedBackupAt = &stale
	assert.False(t, svc.backupDue(rec, window, inWindow.Add(2*time.Hour)))
	assert.True(t, svc.backupDue(rec, window, inWindow))

	// Retention 0 turns scheduling off outright.
	rec.LastAutomatedBackupAt = nil
	rec.BackupRetentionPeriod = 0
	assert.False(t, svc.backupDue(rec, window, inWindow))
}

// A failure inside the window is retried, but paced: the window is 30 minutes, so
// the backoff is what keeps a broken backup from re-quiescing the engine on every
// two-minute pass.
func TestBackupDue_PacesRetriesInsideTheWindow(t *testing.T) {
	window, err := parseDailyWindow("PreferredBackupWindow", "03:00-03:30")
	require.NoError(t, err)
	svc := NewService(nil, testRegion)

	inWindow := time.Date(2026, 7, 30, 3, 10, 0, 0, time.UTC)
	failed := inWindow
	rec := &DBInstanceRecord{
		DBInstanceIdentifier:         testDBID,
		BackupRetentionPeriod:        7,
		AutomatedBackupFailures:      1,
		LastAutomatedBackupFailureAt: &failed,
	}

	assert.False(t, svc.backupDue(rec, window, inWindow.Add(30*time.Second)))
	assert.True(t, svc.backupDue(rec, window, inWindow.Add(time.Minute)))

	// Doubling per consecutive failure, capped, so a persistently failing backup
	// settles at a handful of attempts per window rather than one per pass.
	assert.Equal(t, time.Minute, backupRetryDelay(1))
	assert.Equal(t, 2*time.Minute, backupRetryDelay(2))
	assert.Equal(t, 8*time.Minute, backupRetryDelay(4))
	assert.Equal(t, 8*time.Minute, backupRetryDelay(40))
	// A deferred backup carries no failure count, and still has to be paced.
	assert.Equal(t, time.Minute, backupRetryDelay(0))
}

func TestRunBackupWindow_TakesIndexesAndStampsTheBackup(t *testing.T) {
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, backupReadyRecord())

	require.True(t, h.runBackupPass(t))

	// One EC2 snapshot, taken through the automated backup path: the engine was quiesced and let
	// go again rather than copied mid-write.
	require.Len(t, h.snaps.created, 1)
	issued := make([]string, 0, 2)
	for _, cmd := range h.agent.received() {
		issued = append(issued, cmd.Type)
	}
	assert.Equal(t, []string{CommandQuiesce, CommandUnquiesce}, issued)

	stamps := h.automatedStamps(t, testDBID)
	require.Len(t, stamps, 1, "the backup has to be indexed, or retention can never find it")

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var entry AutomatedBackupRecord
	found, err := getJSON(t.Context(), kv, AutomatedBackupKey(testDBID, stamps[0]), &entry)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, strings.HasPrefix(entry.DBSnapshotIdentifier, automatedSnapshotPrefix+testDBID+"-"),
		"an automated snapshot takes AWS's own name: %s", entry.DBSnapshotIdentifier)

	snapshot, found := h.snapshot(t, entry.DBSnapshotIdentifier)
	require.True(t, found)
	assert.Equal(t, SnapshotTypeAutomated, snapshot.SnapshotType)
	assert.Equal(t, SnapshotStatusAvailable, snapshot.Status)

	// The instance is where it was found, with the window stamped as fired.
	rec := h.instance(t, testDBID)
	assert.Equal(t, StatusAvailable, rec.Status)
	require.NotNil(t, rec.LastAutomatedBackupAt)
	assert.Zero(t, rec.AutomatedBackupFailures)
	assert.Nil(t, rec.LastAutomatedBackupFailureAt)
}

// The window fires once however many passes run inside it, which is what a
// two-minute reconciler and a leader handover both look like from here.
func TestRunBackupWindow_FiresOncePerWindow(t *testing.T) {
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, backupReadyRecord())

	require.True(t, h.runBackupPass(t))
	assert.False(t, h.runBackupPass(t))
	assert.False(t, h.runBackupPass(t))

	assert.Len(t, h.snaps.created, 1)
	assert.Len(t, h.automatedStamps(t, testDBID), 1)
}

func TestRunBackupWindow_DoesNotFireOutsideTheWindow(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := backupReadyRecord()
	rec.PreferredBackupWindow = closedBackupWindow(time.Now().UTC())
	seedInstance(t, h.svc, rec)

	assert.False(t, h.runBackupPass(t))
	assert.Empty(t, h.snaps.created)
	assert.Empty(t, h.automatedStamps(t, testDBID))
}

func TestRunBackupWindow_DoesNotFireWithBackupsDisabled(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := backupReadyRecord()
	rec.BackupRetentionPeriod = 0
	seedInstance(t, h.svc, rec)

	assert.False(t, h.runBackupPass(t))
	assert.Empty(t, h.snaps.created)
	assert.Nil(t, h.instance(t, testDBID).LastAutomatedBackupAt)
}

// A backup is a point in time, so one taken after the reboot finishes belongs to
// the next window rather than this one. The skip is evented, because a customer
// looking for last night's backup has to be able to see why there is none.
func TestRunBackupWindow_SkipsAnInstanceItCannotBackUp(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := backupReadyRecord()
	rec.Status = StatusRebooting
	seedInstance(t, h.svc, rec)

	assert.False(t, h.runBackupPass(t))
	assert.Empty(t, h.snaps.created)

	stored := h.instance(t, testDBID)
	assert.Equal(t, StatusRebooting, stored.Status, "a skipped backup does not touch the instance")
	assert.Nil(t, stored.LastAutomatedBackupAt)
	require.NotNil(t, stored.LastAutomatedBackupFailureAt)
	assert.Zero(t, stored.AutomatedBackupFailures, "nothing failed; the instance was busy elsewhere")

	messages := eventMessages(h.events(t, EventSourceTypeDBInstance, testDBID))
	assert.Contains(t, messages, "The automated backup was skipped because the DB instance is rebooting.")

	// Evented once per window rather than once per pass, so a long reboot does not
	// bury the event ring.
	assert.False(t, h.runBackupPass(t))
	assert.Len(t, h.events(t, EventSourceTypeDBInstance, testDBID), len(messages))
}

// A failed backup is a backup fault, never an instance fault. The
// database stays available and the failure is counted so it is visible.
func TestRunBackupWindow_CountsAFailureAndLeavesTheInstanceAvailable(t *testing.T) {
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, backupReadyRecord())
	h.snaps.createErr = assert.AnError

	assert.False(t, h.runBackupPass(t))

	rec := h.instance(t, testDBID)
	assert.Equal(t, StatusAvailable, rec.Status)
	assert.Equal(t, 1, rec.AutomatedBackupFailures)
	require.NotNil(t, rec.LastAutomatedBackupFailureAt)
	assert.Nil(t, rec.LastAutomatedBackupAt)
	assert.Empty(t, h.automatedStamps(t, testDBID), "a failed backup leaves nothing in the index")

	messages := strings.Join(eventMessages(h.events(t, EventSourceTypeDBInstance, testDBID)), "\n")
	assert.Contains(t, messages, "The automated backup could not be taken")
}

// A stored window this plane cannot parse is corruption rather than a schedule:
// firing on a guessed window would back up at an hour nobody asked for.
func TestRunBackupWindow_IgnoresAMalformedStoredWindow(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := backupReadyRecord()
	rec.PreferredBackupWindow = "0300-0400"
	seedInstance(t, h.svc, rec)

	assert.False(t, h.runBackupPass(t))
	assert.Empty(t, h.snaps.created)
	assert.Nil(t, h.instance(t, testDBID).LastAutomatedBackupFailureAt)
}

// The other half of the pair is resolved for the overlap check, not to be
// written down. A record that carried no maintenance window before the modify
// still carries none after it: reporting one back would show as drift in the next
// configuration read for a request that never set it.
func TestModifyDBInstance_DoesNotPersistTheWindowItWasNotGiven(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := retainingRecord(7)
	rec.PreferredMaintenanceWindow = ""
	seedInstance(t, h.svc, rec)

	out, err := h.svc.ModifyDBInstance(t.Context(), &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier:  aws.String(testDBID),
		PreferredBackupWindow: aws.String("13:00-14:00"),
	}, testAccountID)
	require.NoError(t, err, "a backup window inside the maintenance block is not a rejection")
	assert.Equal(t, "13:00-14:00", aws.StringValue(out.DBInstance.PreferredBackupWindow))

	stored := h.instance(t, testDBID)
	assert.Equal(t, "13:00-14:00", stored.PreferredBackupWindow)
	assert.Empty(t, stored.PreferredMaintenanceWindow, "a window the request did not name is not stored")
}

// A record written before window fields existed carries no window at all, and still has to be
// backed up — on the window a describe reports for it.
func TestResolvedBackupWindow_DerivesAWindowForARecordWithoutOne(t *testing.T) {
	svc := NewService(nil, testRegion)
	rec := &DBInstanceRecord{DBInstanceIdentifier: testDBID}

	window, err := svc.resolvedBackupWindow(rec)
	require.NoError(t, err)
	assert.Equal(t, assignDailyWindow(svc.backupWindowBlock(), testDBID), window)
	assert.Equal(t, window.String(), svc.reportedBackupWindow(rec))

	maintenance, err := svc.resolvedMaintenanceWindow(rec)
	require.NoError(t, err)
	assert.Equal(t, maintenance.String(), svc.reportedMaintenanceWindow(rec))

	// A malformed stored window is reported verbatim: it is not one the scheduler
	// will honour either, so replacing it with a plausible one would mislead.
	rec.PreferredBackupWindow = "0300-0400"
	assert.Equal(t, "0300-0400", svc.reportedBackupWindow(rec))
}

// AWS applies a deferred class change or storage grow in the maintenance window
// rather than never. Only the transition happens here; the apply itself is the
// reconciler's single drain through applyPendingModifications.
func TestRunMaintenanceWindow_OpensADeferredModify(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := backupReadyRecord()
	rec.FormatAuthorized = true
	rec.PreferredMaintenanceWindow = openMaintenanceWindow(time.Now().UTC())
	rec.PendingModifiedValues = &PendingModifiedValues{
		DBInstanceClass: "db.t3.large",
		RequestedAt:     time.Now().UTC().Add(-time.Hour),
	}
	seedInstance(t, h.svc, rec)

	require.True(t, h.runMaintenancePass(t))

	stored := h.instance(t, testDBID)
	assert.Equal(t, StatusModifying, stored.Status)
	assert.False(t, stored.FormatAuthorized, "deferred replacement must revoke formatting before its boot")
	require.NotNil(t, stored.LastMaintenanceWindowAt)
	require.NotNil(t, stored.TransitionStartedAt)
	require.NotNil(t, stored.PendingModifiedValues, "the values are drained by the reconciler, not here")
	assert.Equal(t, "db.t3.large", stored.PendingModifiedValues.DBInstanceClass)

	messages := strings.Join(eventMessages(h.events(t, EventSourceTypeDBInstance, testDBID)), "\n")
	assert.Contains(t, messages, "Applying the modification recorded earlier")
}

func TestRunMaintenanceWindow_StandsDownWhenThereIsNothingToApply(t *testing.T) {
	now := time.Now().UTC()
	cases := map[string]func(*DBInstanceRecord){
		"NothingPending": func(rec *DBInstanceRecord) { rec.PendingModifiedValues = nil },
		"WindowClosed": func(rec *DBInstanceRecord) {
			rec.PreferredMaintenanceWindow = closedMaintenanceWindow(now)
		},
		"NotAvailable": func(rec *DBInstanceRecord) { rec.Status = StatusRebooting },
		// A grow already past its volume modify is resumed by the reconciler
		// whatever the window says, so it must not be re-opened here.
		"GrowingFilesystem": func(rec *DBInstanceRecord) {
			rec.PendingModifiedValues.FilesystemGrowPending = true
		},
		"MalformedWindow": func(rec *DBInstanceRecord) { rec.PreferredMaintenanceWindow = "sunday:03:00" },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newSnapshotHarness(t, false)
			rec := backupReadyRecord()
			rec.PreferredMaintenanceWindow = openMaintenanceWindow(now)
			rec.PendingModifiedValues = &PendingModifiedValues{DBInstanceClass: "db.t3.large"}
			mutate(&rec)
			seedInstance(t, h.svc, rec)

			assert.False(t, h.runMaintenancePass(t))
			assert.NotEqual(t, StatusModifying, h.instance(t, testDBID).Status)
		})
	}
}

func TestRunMaintenanceWindow_OpensOncePerWindow(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := backupReadyRecord()
	rec.PreferredMaintenanceWindow = openMaintenanceWindow(time.Now().UTC())
	rec.PendingModifiedValues = &PendingModifiedValues{DBInstanceClass: "db.t3.large"}
	seedInstance(t, h.svc, rec)

	require.True(t, h.runMaintenancePass(t))

	// Back to available with the change still pending — the shape a failed apply
	// leaves behind. The window has already fired, so it is not re-opened.
	stored := h.instance(t, testDBID)
	stored.Status = StatusAvailable
	seedInstance(t, h.svc, stored)
	assert.False(t, h.runMaintenancePass(t))
}

func TestAutomatedSnapshotIdentifier_TakesAWSsOwnName(t *testing.T) {
	at := time.Date(2026, 7, 30, 3, 25, 45, 0, time.UTC)
	assert.Equal(t, "rds:orders-db-2026-07-30-03-25", AutomatedSnapshotIdentifier(testDBID, at))
	// Minute-precise, so a retry inside the same window never collides with the
	// attempt that failed.
	assert.NotEqual(t, AutomatedSnapshotIdentifier(testDBID, at),
		AutomatedSnapshotIdentifier(testDBID, at.Add(time.Minute)))
	// Fixed width and UTC, so the index's lexical order is chronological.
	assert.Equal(t, "20260730T032545Z", AutomatedBackupStamp(at))
	assert.Less(t, AutomatedBackupStamp(at), AutomatedBackupStamp(at.Add(time.Minute)))
}

// Automated backups own the rds: namespace, as in AWS, so a customer snapshot can
// never collide with one the scheduler mints.
func TestCreateDBSnapshot_RejectsTheAutomatedNamespace(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)

	in := snapshotInput()
	in.DBSnapshotIdentifier = aws.String("rds:orders-db-2026-07-30-03-25")

	_, err := h.svc.CreateDBSnapshot(t.Context(), in, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.Contains(t, err.Error(), automatedSnapshotPrefix)
	assert.Empty(t, h.snaps.created)
}

func TestDescribeDBInstanceAutomatedBackups_ReportsTheBackupSet(t *testing.T) {
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, backupReadyRecord())

	// Before the first snapshot the backup set exists but has nothing in it, which
	// is what AWS calls creating.
	out, err := h.svc.DescribeDBInstanceAutomatedBackups(t.Context(),
		&rds.DescribeDBInstanceAutomatedBackupsInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBInstanceAutomatedBackups, 1)
	assert.Equal(t, "creating", aws.StringValue(out.DBInstanceAutomatedBackups[0].Status))

	require.True(t, h.runBackupPass(t))

	out, err = h.svc.DescribeDBInstanceAutomatedBackups(t.Context(),
		&rds.DescribeDBInstanceAutomatedBackupsInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBInstanceAutomatedBackups, 1)

	backup := out.DBInstanceAutomatedBackups[0]
	assert.Equal(t, "active", aws.StringValue(backup.Status))
	assert.Equal(t, testDBID, aws.StringValue(backup.DBInstanceIdentifier))
	assert.Equal(t, DBInstanceARN(testRegion, testAccountID, testDBID), aws.StringValue(backup.DBInstanceArn))
	assert.Equal(t, int64(7), aws.Int64Value(backup.BackupRetentionPeriod))
	assert.Equal(t, testRegion, aws.StringValue(backup.Region))

	// This phase backs discrete daily snapshots. A restore window
	// would tell a client it can recover to any instant inside it.
	assert.Nil(t, backup.RestoreWindow)
}

func TestDescribeDBInstanceAutomatedBackups_SkipsAnInstanceWithBackupsOff(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := backupReadyRecord()
	rec.BackupRetentionPeriod = 0
	seedInstance(t, h.svc, rec)

	out, err := h.svc.DescribeDBInstanceAutomatedBackups(t.Context(),
		&rds.DescribeDBInstanceAutomatedBackupsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.DBInstanceAutomatedBackups)
}

func TestDescribeDBInstanceAutomatedBackups_RejectsAnUnknownInstance(t *testing.T) {
	h := newSnapshotHarness(t, false)

	_, err := h.svc.DescribeDBInstanceAutomatedBackups(t.Context(),
		&rds.DescribeDBInstanceAutomatedBackupsInput{DBInstanceIdentifier: aws.String("no-such-db")}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceNotFound)
}

// A filter this phase cannot honour is rejected, because a silently
// unfiltered list reads as a complete answer.
func TestDescribeDBInstanceAutomatedBackups_RejectsUnimplementedFilters(t *testing.T) {
	cases := map[string]*rds.DescribeDBInstanceAutomatedBackupsInput{
		"Filters": {Filters: []*rds.Filter{{
			Name:   aws.String("db-instance-id"),
			Values: aws.StringSlice([]string{testDBID}),
		}}},
		"DbiResourceId":                 {DbiResourceId: aws.String("db-ABCDEF")},
		"DBInstanceAutomatedBackupsArn": {DBInstanceAutomatedBackupsArn: aws.String("arn:aws:rds:::auto-backup:x")},
	}

	h := newSnapshotHarness(t, false)
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.DescribeDBInstanceAutomatedBackups(t.Context(), input, testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		})
	}
}

// The window pass has to be part of the leader's own account loop: a backup
// nothing calls is a backup that is never taken.
func TestReconciler_RunsTheBackupWindowOnItsAccountPass(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := backupReadyRecord()
	now := time.Now().UTC()
	rec.Agent = AgentState{InstanceID: testInstance, EngineHealth: EngineHealthHealthy, LastSeen: &now}
	seedInstance(t, h.svc, rec)

	require.NoError(t, NewReconciler(h.svc, "node-a").reconcileOnce(t.Context()))

	assert.Len(t, h.snaps.created, 1)
	assert.Len(t, h.automatedStamps(t, testDBID), 1)
	assert.NotNil(t, h.instance(t, testDBID).LastAutomatedBackupAt)
}

// The GC framework's cluster-wide gate is the reconciler's own lease. Without it
// the retention sweep would delete customer data from every node at once.
func TestReconciler_ClusterLeaseFollowsLeadership(t *testing.T) {
	h := newReconcileHarness(t)

	release, ok := h.rec.AcquireClusterLease()
	require.NotNil(t, release)
	assert.False(t, ok, "a node that does not hold the lease does not sweep")

	h.rec.evaluateLeadership(t.Context())
	release, ok = h.rec.AcquireClusterLease()
	require.True(t, ok)

	// The lease is held continuously rather than claimed per sweep, so releasing
	// the GC's handle must not hand leadership away.
	release()
	_, ok = h.rec.AcquireClusterLease()
	assert.True(t, ok)
}

// Automated backups are on by default at the full cap: a shorter default would
// pay the whole storage cost of retaining a snapshot for a fraction of the cover.
func TestValidateCreateRequest_DefaultsTheBackupSettings(t *testing.T) {
	svc := newCreateValidator()
	req, err := svc.validateCreateRequest(validCreateInput())
	require.NoError(t, err)

	assert.Equal(t, int64(defaultBackupRetentionDays), req.BackupRetentionPeriod)
	backup, maintenance, err := svc.validateWindows(testDBInstanceID, "", "")
	require.NoError(t, err)
	assert.Equal(t, backup, req.PreferredBackupWindow)
	assert.Equal(t, maintenance, req.PreferredMaintenanceWindow)
}

func TestValidateCreateRequest_AcceptsTheBackupSettings(t *testing.T) {
	input := validCreateInput()
	input.BackupRetentionPeriod = aws.Int64(3)
	input.PreferredBackupWindow = aws.String("03:00-04:00")
	input.PreferredMaintenanceWindow = aws.String("sun:05:00-sun:06:00")

	req, err := newCreateValidator().validateCreateRequest(input)
	require.NoError(t, err)
	assert.Equal(t, int64(3), req.BackupRetentionPeriod)
	assert.Equal(t, "03:00-04:00", req.PreferredBackupWindow)
	assert.Equal(t, "sun:05:00-sun:06:00", req.PreferredMaintenanceWindow)

	// A create asking for 0 explicitly turns automated backups off, which the
	// default may not do on its behalf.
	input.BackupRetentionPeriod = aws.Int64(0)
	req, err = newCreateValidator().validateCreateRequest(input)
	require.NoError(t, err)
	assert.Zero(t, req.BackupRetentionPeriod)
}

// A retention or a window that would fail in a window nobody is watching fails in
// the call that set it instead.
func TestValidateCreateRequest_RejectsBadBackupSettings(t *testing.T) {
	cases := map[string]func(*rds.CreateDBInstanceInput){
		"RetentionOverCap":  func(in *rds.CreateDBInstanceInput) { in.BackupRetentionPeriod = aws.Int64(35) },
		"NegativeRetention": func(in *rds.CreateDBInstanceInput) { in.BackupRetentionPeriod = aws.Int64(-1) },
		"MalformedBackupWindow": func(in *rds.CreateDBInstanceInput) {
			in.PreferredBackupWindow = aws.String("3am-4am")
		},
		"MalformedMaintenanceWindow": func(in *rds.CreateDBInstanceInput) {
			in.PreferredMaintenanceWindow = aws.String("05:00-06:00")
		},
		"OverlappingWindows": func(in *rds.CreateDBInstanceInput) {
			in.PreferredBackupWindow = aws.String("03:00-04:00")
			in.PreferredMaintenanceWindow = aws.String("wed:03:30-wed:04:30")
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := validCreateInput()
			mutate(input)
			_, err := newCreateValidator().validateCreateRequest(input)
			require.Error(t, err)
		})
	}
}

// The window is persisted at create rather than derived on every read, so it
// survives a change to the configured block the way AWS's assigned window does.
func TestCreateDBInstance_PersistsTheBackupSettings(t *testing.T) {
	h := newCreateHarness(t, "")

	_, err := h.svc.CreateDBInstance(t.Context(), validCreateInput(), testAccountID)
	require.NoError(t, err)

	rec := h.record(t, testDBInstanceID)
	assert.Equal(t, int64(defaultBackupRetentionDays), rec.BackupRetentionPeriod)
	assert.NotEmpty(t, rec.PreferredBackupWindow)
	assert.NotEmpty(t, rec.PreferredMaintenanceWindow)

	window, err := parseDailyWindow("PreferredBackupWindow", rec.PreferredBackupWindow)
	require.NoError(t, err)
	assert.Equal(t, windowSlot, window.length())
}

// Retention 0 is an answer, not an absence: a client that sees no
// BackupRetentionPeriod cannot tell "backups are off" from "not reported".
func TestDescribeDBInstances_ReportsTheBackupSettingsIncludingZero(t *testing.T) {
	h := newCreateHarness(t, "")
	rec := seedCreated(t, h, testDBInstanceID)

	out, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testDBInstanceID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBInstances, 1)
	assert.Equal(t, rec.BackupRetentionPeriod, aws.Int64Value(out.DBInstances[0].BackupRetentionPeriod))
	assert.Equal(t, rec.PreferredBackupWindow, aws.StringValue(out.DBInstances[0].PreferredBackupWindow))
	assert.Equal(t, rec.PreferredMaintenanceWindow,
		aws.StringValue(out.DBInstances[0].PreferredMaintenanceWindow))

	rec.BackupRetentionPeriod = 0
	seedInstance(t, h.svc, rec)
	out, err = h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testDBInstanceID),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.DBInstances[0].BackupRetentionPeriod)
	assert.Zero(t, aws.Int64Value(out.DBInstances[0].BackupRetentionPeriod))
}

// Restoring from last night's backup is the whole point of keeping it, so the
// namespace refused wherever a name is minted has to be accepted as a reference.
func TestRestoreDBInstanceFromDBSnapshot_RestoresFromAnAutomatedBackup(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := backupReadyRecord()
	// Everything a restore reproduces from the snapshot alone.
	rec.DBInstanceClass = "db.t3.medium"
	rec.AllocatedStorage = 20
	rec.StorageType = storageTypeGP3
	rec.StorageEncrypted = true
	rec.VpcID = testDefaultVPC
	rec.VpcSecurityGroupIDs = []string{testDefaultSG}
	seedInstance(t, h.svc, rec)
	require.True(t, h.runBackupPass(t))

	stamps := h.automatedStamps(t, testDBID)
	require.Len(t, stamps, 1)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var entry AutomatedBackupRecord
	found, err := getJSON(t.Context(), kv, AutomatedBackupKey(testDBID, stamps[0]), &entry)
	require.NoError(t, err)
	require.True(t, found)

	out, err := h.svc.RestoreDBInstanceFromDBSnapshot(t.Context(), &rds.RestoreDBInstanceFromDBSnapshotInput{
		DBInstanceIdentifier: aws.String(testRestoredID),
		DBSnapshotIdentifier: aws.String(entry.DBSnapshotIdentifier),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.DBInstance)
	assert.Equal(t, entry.DBSnapshotIdentifier, h.instance(t, testRestoredID).RestoredFromDBSnapshot)
}

// Only the retention sweep removes an automated backup, as in AWS: a customer
// delete-db-snapshot against one is refused rather than silently accepted.
func TestDeleteDBSnapshot_RefusesAnAutomatedBackup(t *testing.T) {
	h := newSnapshotHarness(t, false)
	seedInstance(t, h.svc, backupReadyRecord())
	require.True(t, h.runBackupPass(t))

	stamps := h.automatedStamps(t, testDBID)
	require.Len(t, stamps, 1)

	_, err := h.svc.DeleteDBSnapshot(t.Context(), &rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(AutomatedSnapshotIdentifier(testDBID, time.Now().UTC())),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.Len(t, h.automatedStamps(t, testDBID), 1)
}
