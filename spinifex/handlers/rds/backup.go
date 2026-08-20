package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go/jetstream"
)

// Automated backups: one snapshot per instance per day inside its
// backup window, taken through the shared snapshot path and swept past
// BackupRetentionPeriod. PITR, WAL archiving and LatestRestorableTime are Phase B
// and stay unreported, so nothing here implies continuous recovery.

const (
	// AWS's own bound is 35 days; this platform's is 7. The cost of
	// a retained snapshot here is metadata and restore surface, not chunks: any
	// snapshot latches viperblock chunk GC off for the life of the source volume,
	// so one day and seven pin exactly the same data.
	defaultBackupRetentionCapDays = 7

	// Automated backups are on by default, at the full cap. A shorter default
	// would pay the whole storage cost above for a fraction of the coverage.
	defaultBackupRetentionDays = 7

	// How many automated snapshots one retention pass may delete. A pass that
	// under-collects is corrected by the next one two minutes later; a pass that
	// walks an unbounded number of snapshots is not.
	defaultSweepDeleteLimit = 32

	// Automated snapshots own this identifier namespace, as in AWS. A customer
	// identifier beginning with it is rejected so the two cannot collide.
	automatedSnapshotPrefix = "rds:"
	// AWS's own stamp: rds:{db}-{YYYY-MM-DD-HH-MM}.
	automatedSnapshotTimeLayout = "2006-01-02-15-04"

	// The index key's stamp. Fixed width and UTC, so lexical key order is
	// chronological order and the sweep needs no per-entry read to sort.
	automatedBackupKeyLayout = "20060102T150405Z"

	// How long after a failed or skipped backup the next attempt inside the same
	// window is allowed, doubling per consecutive failure up to eight minutes.
	// The window is 30 minutes, so this is a handful of attempts and then silence
	// until the next one — a missed window is never backfilled.
	automatedBackupRetryBase     = time.Minute
	automatedBackupRetryShiftCap = 3
)

// The UTC blocks an unnamed window is assigned inside. Deliberately disjoint,
// so derived backup and maintenance windows cannot overlap.
var (
	defaultBackupWindowBlock      = dailyWindow{start: 3 * time.Hour, end: 11 * time.Hour}
	defaultMaintenanceWindowBlock = dailyWindow{start: 11 * time.Hour, end: 19 * time.Hour}
)

// The operator-tunable backup settings: bounds and defaults, never a per-instance
// setting — those live on the DB instance record.
type BackupPolicy struct {
	// The upper bound ModifyDBInstance and CreateDBInstance accept. Zero takes
	// defaultBackupRetentionCapDays.
	RetentionCapDays int64
	// What a create that names no BackupRetentionPeriod gets. Zero takes
	// defaultBackupRetentionDays; a create asking for 0 explicitly still disables
	// automated backups.
	RetentionDays int64
	// The daily UTC blocks unnamed windows are assigned inside, in the same
	// hh24:mi-hh24:mi form the customer's own window uses. Empty takes the
	// defaults above.
	BackupWindowBlock      string
	MaintenanceWindowBlock string
	// The per-pass bound on automated-snapshot deletions. Zero takes
	// defaultSweepDeleteLimit.
	SweepDeleteLimit int
}

func (s *Service) retentionCapDays() int64 {
	if s.deps.Backup.RetentionCapDays > 0 {
		return s.deps.Backup.RetentionCapDays
	}
	return defaultBackupRetentionCapDays
}

// Clamped to the cap, so lowering the cap on an existing cluster cannot hand new
// instances a retention the same cluster would reject on modify.
func (s *Service) defaultRetentionDays() int64 {
	if s.deps.Backup.RetentionDays > 0 {
		return min(s.deps.Backup.RetentionDays, s.retentionCapDays())
	}
	return min(defaultBackupRetentionDays, s.retentionCapDays())
}

func (s *Service) sweepDeleteLimit() int {
	if s.deps.Backup.SweepDeleteLimit > 0 {
		return s.deps.Backup.SweepDeleteLimit
	}
	return defaultSweepDeleteLimit
}

// A misconfigured block falls back to the built-in one rather than failing every
// create on the node: the block is an operator setting, and refusing to place a
// window because of it would take the whole RDS surface down.
func (s *Service) backupWindowBlock() dailyWindow {
	return s.windowBlock(s.deps.Backup.BackupWindowBlock, defaultBackupWindowBlock, "backup")
}

func (s *Service) maintenanceWindowBlock() dailyWindow {
	return s.windowBlock(s.deps.Backup.MaintenanceWindowBlock, defaultMaintenanceWindowBlock, "maintenance")
}

func (s *Service) windowBlock(configured string, fallback dailyWindow, kind string) dailyWindow {
	if configured != "" {
		if block, err := parseDailyWindow("block", configured); err == nil {
			return block
		}
		slog.Warn("rds: the configured "+kind+" window block is malformed; using the built-in block",
			"block", configured, "fallback", fallback.String())
	}
	return fallback
}

// The window in force for this instance. A record that names one uses it; one
// that does not — a record written before these settings existed, or an instance whose create
// named neither — takes the deterministic assignment, which is the same value
// this phase persists at create.
func (s *Service) resolvedBackupWindow(rec *DBInstanceRecord) (dailyWindow, error) {
	if rec.PreferredBackupWindow != "" {
		return parseDailyWindow("PreferredBackupWindow", rec.PreferredBackupWindow)
	}
	// Assigned clear of the window the record does name, the same way the request
	// that stored one and not the other resolved the pair — otherwise the two
	// passes would be scheduled over each other on exactly those records.
	block := s.backupWindowBlock()
	maintenance, ok := storedMaintenanceWindow(rec)
	if !ok {
		return assignDailyWindow(block, rec.DBInstanceIdentifier), nil
	}
	return assignDailyWindowClearOf(block, rec.DBInstanceIdentifier, maintenance), nil
}

func (s *Service) resolvedMaintenanceWindow(rec *DBInstanceRecord) (weeklyWindow, error) {
	if rec.PreferredMaintenanceWindow != "" {
		return parseWeeklyWindow("PreferredMaintenanceWindow", rec.PreferredMaintenanceWindow)
	}
	block := s.maintenanceWindowBlock()
	backup, ok := s.scheduledBackupWindow(rec)
	if !ok {
		return assignWeeklyWindow(block, rec.DBInstanceIdentifier), nil
	}
	return assignWeeklyWindowClearOf(block, rec.DBInstanceIdentifier, backup), nil
}

// The windows the passes will actually fire on, when there are any. A window
// neither named nor parseable is nothing to assign around: the pass that owns it
// will not fire on it either.
func storedMaintenanceWindow(rec *DBInstanceRecord) (weeklyWindow, bool) {
	if rec.PreferredMaintenanceWindow == "" {
		return weeklyWindow{}, false
	}
	window, err := parseWeeklyWindow("PreferredMaintenanceWindow", rec.PreferredMaintenanceWindow)
	return window, err == nil
}

func (s *Service) scheduledBackupWindow(rec *DBInstanceRecord) (dailyWindow, bool) {
	window, err := s.resolvedBackupWindow(rec)
	return window, err == nil
}

// The windows as a describe reports them. A record written before these settings existed carries
// neither, so the derived window is reported rather than an empty string: it is
// the window the scheduler will actually use, and reporting nothing would have the
// customer believe no backup is scheduled.
func (s *Service) reportedBackupWindow(rec *DBInstanceRecord) string {
	window, err := s.resolvedBackupWindow(rec)
	if err != nil {
		// Malformed and therefore not a window the scheduler will honour either, so
		// it is reported verbatim rather than replaced with a plausible one.
		return rec.PreferredBackupWindow
	}
	return window.String()
}

func (s *Service) reportedMaintenanceWindow(rec *DBInstanceRecord) string {
	window, err := s.resolvedMaintenanceWindow(rec)
	if err != nil {
		return rec.PreferredMaintenanceWindow
	}
	return window.String()
}

// The states a backup can be taken from: available needs a quiesce, and stopped
// was sealed by its own graceful stop and is backed up in place.
var backupCapableStatuses = []Status{StatusAvailable, StatusStopped}

// Fires this instance's automated backup when its window is open and none has
// succeeded since the window opened, reporting whether it took one. Called from
// the leader-elected reconciler pass, which is what makes it cluster-singular;
// the per-instance in-flight guard is what makes it safe anyway.
func (s *Service) runBackupWindow(ctx context.Context, kv jetstream.KeyValue, rev uint64,
	accountID string, rec *DBInstanceRecord) (bool, error) {
	now := time.Now().UTC()
	window, err := s.resolvedBackupWindow(rec)
	if err != nil {
		// A stored window this plane cannot parse is corruption, not a schedule:
		// firing on a guessed window would back up at an hour the customer never
		// asked for, so the instance is left alone and the fault is logged.
		slog.WarnContext(ctx, "rds: the stored backup window is malformed; no automated backup will be taken",
			"dbInstance", rec.DBInstanceIdentifier, "window", rec.PreferredBackupWindow, "err", err)
		return false, nil
	}
	if !s.backupDue(rec, window, now) {
		return false, nil
	}

	// Skipped rather than queued: a backup is a point-in-time copy, so one taken
	// after a reboot finishes is the next window's backup, not this one's.
	if !slices.Contains(backupCapableStatuses, rec.Status) {
		return false, s.recordBackupDeferred(ctx, kv, accountID, rec, window.openedAt(now),
			fmt.Sprintf("The automated backup was skipped because the DB instance is %s.", rec.Status))
	}
	if rec.DataVolumeID == "" {
		return false, s.recordBackupDeferred(ctx, kv, accountID, rec, window.openedAt(now),
			"The automated backup was skipped because the DB instance has no data volume.")
	}

	if err := s.takeAutomatedBackup(ctx, kv, rev, accountID, rec, now); err != nil {
		// The database is healthy and stays available. The failure is
		// counted and evented so it is visible, and retried while the window is
		// open; it never reaches the instance recovery path.
		return false, s.recordBackupFailure(ctx, kv, accountID, rec, err)
	}
	return true, nil
}

// Whether the window is open and nothing has already fired in it. Retention 0
// disables scheduling outright, which is also what makes the sweep remove the
// instance's whole automated set.
func (s *Service) backupDue(rec *DBInstanceRecord, window dailyWindow, now time.Time) bool {
	if rec.BackupRetentionPeriod <= 0 || !window.contains(now) {
		return false
	}
	opened := window.openedAt(now)
	// The stamp, not a timer: leader churn, a daemon restart, or two nodes briefly
	// believing they hold the lease all read the same "already fired".
	if rec.LastAutomatedBackupAt != nil && !rec.LastAutomatedBackupAt.Before(opened) {
		return false
	}
	if rec.LastAutomatedBackupFailureAt != nil && !rec.LastAutomatedBackupFailureAt.Before(opened) {
		return !now.Before(rec.LastAutomatedBackupFailureAt.Add(backupRetryDelay(rec.AutomatedBackupFailures)))
	}
	return true
}

func backupRetryDelay(failures int) time.Duration {
	return automatedBackupRetryBase << min(max(failures-1, 0), automatedBackupRetryShiftCap)
}

// Takes the snapshot through the shared path, indexes it, and stamps the window as
// fired. The index is written before the stamp: an indexed backup with no stamp
// costs one duplicate snapshot at worst and is swept on schedule, while a stamped
// backup with no index would be invisible to retention for the life of the
// instance.
func (s *Service) takeAutomatedBackup(ctx context.Context, kv jetstream.KeyValue, rev uint64,
	accountID string, rec *DBInstanceRecord, now time.Time) error {
	record, err := s.snapshotDBInstance(ctx, kv, rev, accountID, rec, &validatedSnapshot{
		DBSnapshotIdentifier: AutomatedSnapshotIdentifier(rec.DBInstanceIdentifier, now),
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		SnapshotType:         SnapshotTypeAutomated,
	})
	if err != nil {
		return err
	}

	entry := AutomatedBackupRecord{
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		DBSnapshotIdentifier: record.DBSnapshotIdentifier,
		CreatedAt:            record.CreatedAt,
	}
	key := AutomatedBackupKey(rec.DBInstanceIdentifier, AutomatedBackupStamp(record.CreatedAt))
	if err := putJSON(ctx, kv, key, &entry); err != nil {
		return err
	}

	return s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		stored.LastAutomatedBackupAt = &now
		stored.AutomatedBackupFailures = 0
		stored.LastAutomatedBackupFailureAt = nil
	})
}

// Counts the failure and reports it against the DB instance, leaving its status
// alone. The stamp is what paces the retry inside the window; the count is what
// makes a backup that has been failing for days visible and testable.
func (s *Service) recordBackupFailure(ctx context.Context, kv jetstream.KeyValue, accountID string,
	rec *DBInstanceRecord, cause error) error {
	now := time.Now().UTC()
	slog.WarnContext(ctx, "rds: an automated backup failed; the DB instance is unaffected",
		"dbInstance", rec.DBInstanceIdentifier, "accountId", accountID,
		"failures", rec.AutomatedBackupFailures+1, "err", cause)
	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
		fmt.Sprintf("The automated backup could not be taken: %v", cause),
		EventCategoryBackup, EventCategoryFailure)
	return s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		stored.AutomatedBackupFailures++
		stored.LastAutomatedBackupFailureAt = &now
	})
}

// A window this instance could not be backed up in, evented once and then paced
// by the same backoff a failure is. Reported without incrementing the failure
// count: nothing about the backup failed, the instance was busy elsewhere.
func (s *Service) recordBackupDeferred(ctx context.Context, kv jetstream.KeyValue, accountID string,
	rec *DBInstanceRecord, opened time.Time, message string) error {
	now := time.Now().UTC()
	if rec.LastAutomatedBackupFailureAt == nil || rec.LastAutomatedBackupFailureAt.Before(opened) {
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
			message, EventCategoryBackup, EventCategoryNotification)
	}
	return s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		stored.LastAutomatedBackupFailureAt = &now
	})
}

// Drains a deferred modify inside the maintenance window, matching AWS: a class
// change or a storage grow recorded with ApplyImmediately=false takes the
// database down here rather than never. Reports whether it started one.
//
// Only the transition into modifying happens on this pass. The apply itself is
// left to the reconciler's own resume of a modifying instance, which is the
// single drain through applyPendingModifications — so a deferred change is
// applied by exactly the code an immediate one uses, and one instance's VM
// replace does not hold up the whole fleet's pass.
func (s *Service) runMaintenanceWindow(ctx context.Context, kv jetstream.KeyValue, rev uint64,
	accountID string, rec *DBInstanceRecord) (bool, error) {
	pending := rec.PendingModifiedValues
	if pending.empty() || pending.growingFilesystem() || rec.Status != StatusAvailable {
		return false, nil
	}
	now := time.Now().UTC()
	window, err := s.resolvedMaintenanceWindow(rec)
	if err != nil {
		slog.WarnContext(ctx, "rds: the stored maintenance window is malformed; the deferred modify is not applied",
			"dbInstance", rec.DBInstanceIdentifier, "window", rec.PreferredMaintenanceWindow, "err", err)
		return false, nil
	}
	if !window.contains(now) {
		return false, nil
	}
	opened := window.openedAt(now)
	if rec.LastMaintenanceWindowAt != nil && !rec.LastMaintenanceWindowAt.Before(opened) {
		return false, nil
	}

	rec.LastMaintenanceWindowAt = &now
	rec.TransitionStartedAt = &now
	rec.FormatAuthorized = false
	rec.Status = StatusModifying
	rec.UpdatedAt = now
	if err := updateJSON(ctx, kv, DBInstanceKey(rec.DBInstanceIdentifier), rev, rec); err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			// Something else moved the record between the read and here; the next
			// pass re-reads and the window is still open.
			return false, nil
		}
		return false, err
	}

	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
		"Applying the modification recorded earlier; the database is unavailable while it is applied.",
		EventCategoryConfigurationChange, EventCategoryMaintenance)
	slog.InfoContext(ctx, "rds: maintenance window opened a deferred modify",
		"dbInstance", rec.DBInstanceIdentifier, "accountId", accountID, "window", window.String())
	return true, nil
}

// AWS's own name for an automated snapshot, minute-precise so a retry inside the
// same window never collides with the attempt that failed.
func AutomatedSnapshotIdentifier(dbInstanceIdentifier string, at time.Time) string {
	return automatedSnapshotPrefix + dbInstanceIdentifier + "-" + at.UTC().Format(automatedSnapshotTimeLayout)
}

func AutomatedBackupStamp(at time.Time) string {
	return at.UTC().Format(automatedBackupKeyLayout)
}

// The customer's view of the automated backup set: one entry per instance that
// has automated backups, as AWS reports it. The individual snapshots stay
// listable through DescribeDBSnapshots --snapshot-type automated.
func (s *Service) DescribeDBInstanceAutomatedBackups(ctx context.Context,
	input *rds.DescribeDBInstanceAutomatedBackupsInput, accountID string) (*rds.DescribeDBInstanceAutomatedBackupsOutput, error) {
	wanted, err := validateDescribeAutomatedBackupsRequest(input)
	if err != nil {
		return nil, err
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}

	ids := []string{wanted}
	if wanted == "" {
		if ids, err = ListDBInstanceIDs(ctx, kv); err != nil {
			return nil, err
		}
		slices.Sort(ids)
	}

	backups := make([]*rds.DBInstanceAutomatedBackup, 0, len(ids))
	for _, id := range ids {
		var rec DBInstanceRecord
		found, err := getJSON(ctx, kv, DBInstanceKey(id), &rec)
		if err != nil {
			return nil, err
		}
		// A named instance that does not exist is an error, as it is on every other
		// RDS describe; one deleted between the listing and this read is simply gone.
		if !found {
			if wanted != "" {
				return nil, awserrors.Errorf(awserrors.ErrorDBInstanceNotFound, "DB instance %s not found", id)
			}
			continue
		}
		if rec.BackupRetentionPeriod <= 0 {
			continue
		}
		stamps, err := ListAutomatedBackupStamps(ctx, kv, id)
		if err != nil {
			return nil, err
		}
		backups = append(backups, s.projectAutomatedBackup(&rec, len(stamps)))
	}
	return &rds.DescribeDBInstanceAutomatedBackupsOutput{DBInstanceAutomatedBackups: backups}, nil
}

// A filter this phase cannot honour is rejected rather than dropped, since a
// silently unfiltered list reads as a complete answer. Returns the DB instance to
// report on, empty for every one.
func validateDescribeAutomatedBackupsRequest(input *rds.DescribeDBInstanceAutomatedBackupsInput) (string, error) {
	if input == nil {
		return "", nil
	}
	if len(input.Filters) > 0 {
		return "", unimplemented("Filters", "DescribeDBInstanceAutomatedBackups filters on DBInstanceIdentifier only")
	}
	if aws.StringValue(input.DbiResourceId) != "" {
		return "", unimplemented("DbiResourceId", "a DB instance has no resource ID distinct from its identifier here")
	}
	if aws.StringValue(input.DBInstanceAutomatedBackupsArn) != "" {
		return "", unimplemented("DBInstanceAutomatedBackupsArn",
			"cross-region automated backup replication is not offered, so no replicated backup has an ARN")
	}
	return aws.StringValue(input.DBInstanceIdentifier), nil
}

// RestoreWindow and LatestRestorableTime are deliberately absent: this phase
// backs discrete daily snapshots, and reporting a restore window would tell a
// client it can recover to any instant inside it.
func (s *Service) projectAutomatedBackup(rec *DBInstanceRecord, snapshots int) *rds.DBInstanceAutomatedBackup {
	// AWS's own vocabulary: creating until the first snapshot exists, active once
	// one does. retained — an automated backup outliving its instance — is not
	// offered, because it would pin the source data volume indefinitely.
	status := "creating"
	if snapshots > 0 {
		status = "active"
	}
	out := &rds.DBInstanceAutomatedBackup{
		DBInstanceIdentifier:  aws.String(rec.DBInstanceIdentifier),
		DBInstanceArn:         aws.String(DBInstanceARN(s.region, rec.AccountID, rec.DBInstanceIdentifier)),
		Region:                aws.String(s.region),
		Status:                aws.String(status),
		Engine:                aws.String(rec.Engine),
		EngineVersion:         aws.String(rec.EngineVersion),
		AllocatedStorage:      aws.Int64(rec.AllocatedStorage),
		StorageType:           aws.String(rec.StorageType),
		Encrypted:             aws.Bool(rec.StorageEncrypted),
		MasterUsername:        aws.String(rec.MasterUsername),
		Port:                  aws.Int64(rec.Port),
		BackupRetentionPeriod: aws.Int64(rec.BackupRetentionPeriod),
		InstanceCreateTime:    aws.Time(rec.CreatedAt),
	}
	if rec.VpcID != "" {
		out.VpcId = aws.String(rec.VpcID)
	}
	return out
}

// The bounds a customer-supplied retention has to be inside. Checked wherever it
// is accepted, so a value that would fail in a window nobody is watching fails in
// the call that set it instead.
func (s *Service) validateRetentionPeriod(days int64) error {
	if days < 0 || days > s.retentionCapDays() {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"BackupRetentionPeriod must be between 0 and %d days", s.retentionCapDays())
	}
	return nil
}

// The pair, validated together: AWS refuses a backup window that overlaps the
// maintenance window, and the non-overlap check needs both to exist — which is
// also why an unnamed window is assigned rather than left empty.
func (s *Service) validateWindows(identifier, backup, maintenance string) (string, string, error) {
	var backupWindow dailyWindow
	if backup != "" {
		parsed, err := parseDailyWindow("PreferredBackupWindow", backup)
		if err != nil {
			return "", "", err
		}
		backupWindow = parsed
	}
	var maintenanceWindow weeklyWindow
	if maintenance != "" {
		parsed, err := parseWeeklyWindow("PreferredMaintenanceWindow", maintenance)
		if err != nil {
			return "", "", err
		}
		maintenanceWindow = parsed
	}
	// A window the customer named is placed first and an assigned one goes around
	// it. Assigning both independently and then rejecting the overlap would fail a
	// request over a window the customer never sent — and deterministically, so the
	// same identifier would fail forever.
	switch {
	case backup == "" && maintenance == "":
		backupWindow = assignDailyWindow(s.backupWindowBlock(), identifier)
		maintenanceWindow = assignWeeklyWindowClearOf(s.maintenanceWindowBlock(), identifier, backupWindow)
	case backup == "":
		backupWindow = assignDailyWindowClearOf(s.backupWindowBlock(), identifier, maintenanceWindow)
	case maintenance == "":
		maintenanceWindow = assignWeeklyWindowClearOf(s.maintenanceWindowBlock(), identifier, backupWindow)
	}
	if backupWindow.overlaps(maintenanceWindow) {
		return "", "", awserrors.Errorf(awserrors.ErrorInvalidParameterCombination,
			"PreferredBackupWindow %s overlaps PreferredMaintenanceWindow %s",
			backupWindow.String(), maintenanceWindow.String())
	}
	// Returned in AWS's canonical text rather than as the customer typed it, so a
	// describe reads back the same string a later modify would compare against.
	return backupWindow.String(), maintenanceWindow.String(), nil
}

// Rejects the namespace automated snapshots own, so a customer snapshot can
// never collide with one the scheduler mints.
func rejectAutomatedNamespace(id string) error {
	if strings.HasPrefix(id, automatedSnapshotPrefix) {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBSnapshotIdentifier may not begin with %q; that namespace belongs to automated backups",
			automatedSnapshotPrefix)
	}
	return nil
}
