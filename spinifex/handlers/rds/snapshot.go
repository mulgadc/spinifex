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
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// The EC2 snapshot surface the RDS control plane drives. A DB snapshot is an
// ec2.CreateSnapshot of the instance's data volume wrapped in an agent quiesce
// consistent; DescribeSnapshots is also what answers whether a volume is still
// referenced, which is what decides between deleting and retaining it.
type snapshotProvider interface {
	CreateSnapshot(ctx context.Context, input *ec2.CreateSnapshotInput, accountID string) (*ec2.Snapshot, error)
	DescribeSnapshots(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error)
	// Unlike the customer-facing describe, this fails on any unreadable snapshot
	// metadata so reconciliation never mistakes a partial result for absence.
	DescribeSnapshotsStrict(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error)
	DeleteSnapshot(ctx context.Context, input *ec2.DeleteSnapshotInput, accountID string) (*ec2.DeleteSnapshotOutput, error)
}

// AWS's bound. The character rules match a DB instance identifier's so a name
// accepted here is also accepted as a FinalDBSnapshotIdentifier at delete.
const maxDBSnapshotIdentifierLen = 255

// Links the EC2 snapshot back to the account-scoped RDS identity that owns it,
// so a record left stuck in creating cannot adopt another tenant's snapshot.
const (
	rdsSnapshotTagKey        = "spinifex:rds-db-snapshot"
	rdsSnapshotAccountTagKey = "spinifex:rds-db-snapshot-account"
)

// Takes a customer-requested snapshot of the instance's data volume. The engine
// is held at a checkpoint for the length of it, so the captured datadir is a
// checkpoint rather than a mid-write state — and if it cannot be, the snapshot
// is still taken and reported as crash consistent.
func (s *Service) CreateDBSnapshot(ctx context.Context, input *rds.CreateDBSnapshotInput, accountID string) (*rds.CreateDBSnapshotOutput, error) {
	req, err := validateCreateSnapshotRequest(input)
	if err != nil {
		return nil, err
	}
	if s.deps.Snapshots == nil {
		return nil, errors.New("rds: no snapshot service configured")
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}

	rec, rev, err := s.getDBInstance(ctx, kv, req.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	record, err := s.snapshotDBInstance(ctx, kv, rev, accountID, rec, req)
	if err != nil {
		return nil, err
	}
	return &rds.CreateDBSnapshotOutput{DBSnapshot: s.projectDBSnapshot(record)}, nil
}

// The one snapshot path. A customer's CreateDBSnapshot and an automated
// backup differ only in who asks and in the type stamped on the record;
// the quiesce, the node-addressed EC2 snapshot, the db-snapshots record and the
// per-instance in-flight guard are the same for both.
//
// rev is the revision the caller read the DB instance at: the CAS that moves it
// into backing-up is what serialises the two kinds of snapshot against each other
// and against a lifecycle op.
func (s *Service) snapshotDBInstance(ctx context.Context, kv jetstream.KeyValue, rev uint64,
	accountID string, rec *DBInstanceRecord, req *validatedSnapshot) (*DBSnapshotRecord, error) {
	if s.deps.Snapshots == nil {
		return nil, errors.New("rds: no snapshot service configured")
	}
	if rec.DataVolumeID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
			"DB instance %s has no data volume to snapshot", rec.DBInstanceIdentifier)
	}
	// Checked before the instance is moved to backing-up, so a request naming a
	// taken identifier leaves the instance where it was.
	if err := s.checkDBSnapshotAvailable(ctx, kv, req.DBSnapshotIdentifier); err != nil {
		return nil, err
	}

	// The same CAS that moves the instance to backing-up records the snapshot
	// holding it, so a second snapshot request or a lifecycle op is rejected
	// rather than entering the same window.
	resume, err := s.beginSnapshotOperation(ctx, kv, rev, rec, req.DBSnapshotIdentifier)
	if err != nil {
		return nil, err
	}
	defer s.endSnapshotOperation(ctx, kv, rec.DBInstanceIdentifier, resume)

	// Written before the EC2 call, so a crash in between leaves a reconcilable
	// record rather than an untracked EC2 snapshot.
	record := newDBSnapshotRecord(accountID, rec, req)
	key := DBSnapshotKey(req.DBSnapshotIdentifier)
	if err := createJSON(ctx, kv, key, &record); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, awserrors.Errorf(awserrors.ErrorDBSnapshotAlreadyExists,
				"DB snapshot %s already exists", req.DBSnapshotIdentifier)
		}
		return nil, err
	}

	snapshotID, crashConsistent, err := s.snapshotDataVolume(ctx, accountID, rec, resume, req.DBSnapshotIdentifier)
	if err != nil {
		// The record only ever described a snapshot that now does not exist, so it
		// goes with it rather than being left for the reconciler to puzzle over.
		s.discardSnapshotRecord(ctx, kv, req.DBSnapshotIdentifier)
		return nil, err
	}

	record.SnapshotID = snapshotID
	record.Status = SnapshotStatusAvailable
	record.CrashConsistent = crashConsistent
	if err := putJSON(ctx, kv, key, &record); err != nil {
		return nil, err
	}

	s.RecordEvent(ctx, accountID, EventSourceTypeDBSnapshot, req.DBSnapshotIdentifier,
		"DB snapshot created.", EventCategoryBackup, EventCategoryCreation)
	slog.InfoContext(ctx, "rds: DB snapshot created",
		"dbSnapshot", req.DBSnapshotIdentifier, "dbInstance", rec.DBInstanceIdentifier,
		"snapshotType", record.SnapshotType, "snapshotId", snapshotID, "crashConsistent", crashConsistent)

	return &record, nil
}

// Quiesces the engine, snapshots the data volume, and releases the engine again
// however the snapshot went. Reports the EC2 snapshot and whether the engine was
// still writing when it was taken.
func (s *Service) snapshotDataVolume(ctx context.Context, accountID string, rec *DBInstanceRecord,
	resume Status, dbSnapshotIdentifier string) (snapshotID string, crashConsistent bool, err error) {
	// A stopped instance has no engine to hold: its datadir was sealed by the
	// graceful stop, which is the checkpoint a quiesce would be forcing.
	if resume == StatusAvailable {
		if quiesceErr := s.quiesceEngine(ctx, accountID, rec.DBInstanceIdentifier, dbSnapshotIdentifier); quiesceErr != nil {
			// Still a restorable backup, so it is taken and reported honestly
			// rather than refused. The engine replays WAL on restore.
			crashConsistent = true
			slog.WarnContext(ctx, "rds: the engine could not be quiesced; taking a crash-consistent snapshot",
				"dbInstance", rec.DBInstanceIdentifier, "dbSnapshot", dbSnapshotIdentifier, "err", quiesceErr)
			s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
				crashConsistentSnapshotMessage(ctx, rec.Engine),
				EventCategoryNotification, EventCategoryBackup)
		} else {
			defer s.releaseQuiesce(ctx, accountID, rec.DBInstanceIdentifier)
		}
	}

	// System-owned, like the volume it is taken from: the customer addresses it by
	// its RDS identifier and can neither see nor delete the EC2 snapshot.
	snapshot, err := s.deps.Snapshots.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{
		VolumeId:    aws.String(rec.DataVolumeID),
		Description: aws.String("RDS snapshot " + dbSnapshotIdentifier + " of " + rec.DBInstanceIdentifier),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("snapshot"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByRDS)},
				{Key: aws.String(rdsInstanceTagKey), Value: aws.String(rec.DBInstanceIdentifier)},
				{Key: aws.String(rdsSnapshotTagKey), Value: aws.String(dbSnapshotIdentifier)},
				{Key: aws.String(rdsSnapshotAccountTagKey), Value: aws.String(accountID)},
			},
		}},
	}, utils.GlobalAccountID)
	if err != nil {
		return "", crashConsistent, fmt.Errorf("rds: snapshot the data volume of %s: %w", rec.DBInstanceIdentifier, err)
	}
	if snapshot == nil || aws.StringValue(snapshot.SnapshotId) == "" {
		return "", crashConsistent, fmt.Errorf("rds: snapshot the data volume of %s: empty snapshot id", rec.DBInstanceIdentifier)
	}
	return aws.StringValue(snapshot.SnapshotId), crashConsistent, nil
}

// The half of the warning that holds for every engine. What restoring such a
// snapshot then recovers does not: PostgreSQL replays every table from its
// write-ahead log, MariaDB only its InnoDB ones.
const crashConsistentSnapshotWarning = "The database engine could not be quiesced before the snapshot; " +
	"the snapshot is crash consistent."

func crashConsistentSnapshotMessage(ctx context.Context, engineName string) string {
	engine, err := LookupEngine(engineName)
	if err != nil {
		// The snapshot has already been taken, so the customer still gets the half
		// of the warning that does not depend on knowing the engine.
		slog.ErrorContext(ctx, "rds: the DB instance names an engine this build does not offer",
			"engine", engineName, "err", err)
		return crashConsistentSnapshotWarning
	}
	return crashConsistentSnapshotWarning + " " + engine.crashRecoveryNote
}

// Releases the quiesce on a context detached from the caller's, so a snapshot
// that failed because the request deadline expired still lets the engine out of
// backup mode rather than leaving it to the agent's own deadline.
func (s *Service) releaseQuiesce(ctx context.Context, accountID, dbInstanceIdentifier string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unquiesceTimeout)
	defer cancel()
	if err := s.unquiesceEngine(releaseCtx, accountID, dbInstanceIdentifier); err != nil {
		slog.ErrorContext(ctx, "rds: releasing the engine quiesce failed; the agent will release it on its own deadline",
			"dbInstance", dbInstanceIdentifier, "err", err)
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, dbInstanceIdentifier,
			fmt.Sprintf("The database engine could not be released from backup mode after the snapshot; the instance agent releases it automatically within %s.", quiesceHold),
			EventCategoryNotification, EventCategoryBackup)
	}
}

// The customer's view of one account's snapshots, filtered the three ways AWS
// scopes them. A named snapshot that does not exist is an error rather than an
// empty list, matching AWS: a client polling a create would otherwise read
// "gone" for "not ready".
func (s *Service) DescribeDBSnapshots(ctx context.Context, input *rds.DescribeDBSnapshotsInput, accountID string) (*rds.DescribeDBSnapshotsOutput, error) {
	snapshotType, err := validateDescribeSnapshotsRequest(input)
	if err != nil {
		return nil, err
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}

	if id := aws.StringValue(input.DBSnapshotIdentifier); id != "" {
		rec, _, err := s.getDBSnapshot(ctx, kv, id)
		if err != nil {
			return nil, err
		}
		if !snapshotMatches(rec, aws.StringValue(input.DBInstanceIdentifier), snapshotType) {
			return &rds.DescribeDBSnapshotsOutput{DBSnapshots: []*rds.DBSnapshot{}}, nil
		}
		return &rds.DescribeDBSnapshotsOutput{DBSnapshots: []*rds.DBSnapshot{s.projectDBSnapshot(rec)}}, nil
	}

	ids, err := ListDBSnapshotIDs(ctx, kv)
	if err != nil {
		return nil, err
	}
	slices.Sort(ids)

	snapshots := make([]*rds.DBSnapshot, 0, len(ids))
	for _, id := range ids {
		var rec DBSnapshotRecord
		found, err := getJSON(ctx, kv, DBSnapshotKey(id), &rec)
		if err != nil {
			return nil, err
		}
		// A snapshot deleted between the key listing and this read is simply gone,
		// which is what a describe one tick later would report too.
		if !found || !snapshotMatches(&rec, aws.StringValue(input.DBInstanceIdentifier), snapshotType) {
			continue
		}
		snapshots = append(snapshots, s.projectDBSnapshot(&rec))
	}
	return &rds.DescribeDBSnapshotsOutput{DBSnapshots: snapshots}, nil
}

// An empty filter matches everything, as AWS does.
func snapshotMatches(rec *DBSnapshotRecord, dbInstanceIdentifier, snapshotType string) bool {
	if dbInstanceIdentifier != "" && rec.DBInstanceIdentifier != dbInstanceIdentifier {
		return false
	}
	return snapshotType == "" || rec.SnapshotType == snapshotType
}

// Removes the snapshot and, when it was the last thing holding a data volume
// its DB instance already released, that volume too. A snapshot a restored
// instance still reads from is refused with the instance named, so the customer
// knows what to remove first.
func (s *Service) DeleteDBSnapshot(ctx context.Context, input *rds.DeleteDBSnapshotInput, accountID string) (*rds.DeleteDBSnapshotOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	id := aws.StringValue(input.DBSnapshotIdentifier)
	if err := validateDBSnapshotIdentifier(id); err != nil {
		return nil, err
	}
	if s.deps.Snapshots == nil {
		return nil, errors.New("rds: no snapshot service configured")
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, _, err := s.getDBSnapshot(ctx, kv, id)
	if err != nil {
		return nil, err
	}
	if err := s.removeDBSnapshot(ctx, kv, accountID, rec); err != nil {
		return nil, err
	}
	// AWS answers with the snapshot as it last stood, the way a delete of a DB
	// instance answers with the record it just removed.
	return &rds.DeleteDBSnapshotOutput{DBSnapshot: s.projectDBSnapshot(rec)}, nil
}

// Removes a DB snapshot, its EC2 data and — when this was the last reference — the
// data volume behind it. Shared with the retention sweep, which cannot go
// through DeleteDBSnapshot: that rejects the rds: namespace automated snapshots
// live in, so a customer can never delete one by hand.
func (s *Service) removeDBSnapshot(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBSnapshotRecord) error {
	if s.deps.Snapshots == nil {
		return errors.New("rds: no snapshot service configured")
	}
	id := rec.DBSnapshotIdentifier
	// A snapshot still being taken has no data to remove and an in-flight writer
	// that would recreate the record; the reconciler resolves it either way.
	if rec.Status != SnapshotStatusAvailable {
		return awserrors.Errorf(awserrors.ErrorDBSnapshotInvalidState,
			"DB snapshot %s is %s; it must be %s to be deleted", id, rec.Status, SnapshotStatusAvailable)
	}

	if err := s.deleteEC2Snapshot(ctx, kv, accountID, rec); err != nil {
		return err
	}
	// After the EC2 snapshot, so the volume store no longer sees this reference
	// when it decides whether the volume is still held.
	if err := s.releaseRetainedVolume(ctx, kv, rec); err != nil {
		return err
	}
	// Last: while it exists the snapshot is still nameable, and a retry re-runs
	// the steps above, each of which tolerates work it has already done.
	if err := kv.Delete(ctx, DBSnapshotKey(id)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("rds: delete the DB snapshot record for %s: %w", id, err)
	}

	s.RecordEvent(ctx, accountID, EventSourceTypeDBSnapshot, id, "DB snapshot deleted.", EventCategoryDeletion)
	slog.InfoContext(ctx, "rds: DB snapshot deleted", "dbSnapshot", id, "snapshotId", rec.SnapshotID)
	return nil
}

// A snapshot with a volume still reading through it cannot go, which is exactly
// the case where an instance restored from it is still running. That is
// translated into an RDS fault naming the instance rather than surfaced as the
// raw EC2 code, which says nothing a customer can act on.
func (s *Service) deleteEC2Snapshot(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBSnapshotRecord) error {
	if rec.SnapshotID == "" {
		return nil
	}
	_, err := s.deps.Snapshots.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(rec.SnapshotID),
	}, utils.GlobalAccountID)
	switch {
	case err == nil || awserrors.IsNotFound(err):
		return nil
	case awserrors.IsErrorCode(err, awserrors.ErrorInvalidSnapshotInUse):
		return awserrors.Errorf(awserrors.ErrorDBSnapshotInvalidState,
			"DB snapshot %s cannot be deleted while %s reads from it", rec.DBSnapshotIdentifier,
			s.describeSnapshotDependents(ctx, kv, rec.DBSnapshotIdentifier))
	default:
		return fmt.Errorf("rds: delete the EC2 snapshot behind %s: %w", rec.DBSnapshotIdentifier, err)
	}
}

// What the customer has to remove before the snapshot can go. A restored
// instance is named; anything else is a volume outside RDS's own bookkeeping,
// which is reported as such rather than guessed at.
func (s *Service) describeSnapshotDependents(ctx context.Context, kv jetstream.KeyValue, dbSnapshotIdentifier string) string {
	ids, err := ListDBInstanceIDs(ctx, kv)
	if err != nil {
		slog.WarnContext(ctx, "rds: listing the instances restored from a snapshot failed",
			"dbSnapshot", dbSnapshotIdentifier, "err", err)
		return "a volume created from it"
	}
	var dependents []string
	for _, id := range ids {
		var rec DBInstanceRecord
		found, err := getJSON(ctx, kv, DBInstanceKey(id), &rec)
		if err == nil && found && rec.RestoredFromDBSnapshot == dbSnapshotIdentifier {
			dependents = append(dependents, id)
		}
	}
	if len(dependents) == 0 {
		return "a volume created from it"
	}
	slices.Sort(dependents)
	return "DB instance " + joinNames(dependents) + ", restored from it,"
}

// Deletes a data volume its DB instance already gave up, once this was the last
// snapshot holding its chunks. The holders are re-read from the volume store
// rather than taken from the retained record: that index is what DeleteVolume
// enforces against, and a record written before another snapshot was taken would
// otherwise read as "nothing holds it".
func (s *Service) releaseRetainedVolume(ctx context.Context, kv jetstream.KeyValue, rec *DBSnapshotRecord) error {
	if rec.SourceVolumeID == "" {
		return nil
	}
	var retained RetainedVolumeRecord
	found, err := getJSON(ctx, kv, RetainedVolumeKey(rec.SourceVolumeID), &retained)
	if err != nil || !found {
		return err
	}
	_, err = s.reclaimRetainedVolume(ctx, kv, &retained)
	return err
}

// Deletes a retained data volume once nothing holds it, or records the holders it
// still has. Reports whether the volume actually went, which is what lets the
// reaper count the ones it reclaimed.
func (s *Service) reclaimRetainedVolume(ctx context.Context, kv jetstream.KeyValue,
	retained *RetainedVolumeRecord) (bool, error) {
	holders, err := s.snapshotsHolding(ctx, retained.VolumeID)
	if err != nil {
		return false, err
	}
	if len(holders) > 0 {
		retained.Snapshots = holders
		retained.HoldersUnresolved = false
		return false, putJSON(ctx, kv, RetainedVolumeKey(retained.VolumeID), retained)
	}

	if s.deps.Launch.Volume == nil {
		return false, errors.New("rds: no volume service configured")
	}
	_, err = s.deps.Launch.Volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
		VolumeId: aws.String(retained.VolumeID),
	}, utils.GlobalAccountID)
	switch {
	case err == nil || awserrors.IsNotFound(err):
	case awserrors.IsErrorCode(err, awserrors.ErrorVolumeInUse):
		// The volume store's index sees a reference the enumeration above did not.
		// The disagreement is recorded rather than returned, so the volume is
		// re-checked on a later pass instead of failing this caller — which for a
		// DeleteDBSnapshot has already removed the snapshot by now.
		retained.HoldersUnresolved = true
		return false, putJSON(ctx, kv, RetainedVolumeKey(retained.VolumeID), retained)
	default:
		return false, fmt.Errorf("rds: delete the retained data volume %s: %w", retained.VolumeID, err)
	}
	if err := kv.Delete(ctx, RetainedVolumeKey(retained.VolumeID)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, fmt.Errorf("rds: clear the retained-volume record for %s: %w", retained.VolumeID, err)
	}
	slog.InfoContext(ctx, "rds: retained data volume reclaimed; its last snapshot is gone",
		"volumeId", retained.VolumeID, "dbInstance", retained.DBInstanceIdentifier)
	return true, nil
}

// The request as CreateDBSnapshot resolved it, or as the scheduler and the
// delete path's final-snapshot reservation construct it. SnapshotType is what the
// record is stamped with, and the only thing separating an automated backup from
// a manual snapshot once it exists.
type validatedSnapshot struct {
	DBSnapshotIdentifier string
	DBInstanceIdentifier string
	SnapshotType         string
	Tags                 map[string]string
}

func validateCreateSnapshotRequest(input *rds.CreateDBSnapshotInput) (*validatedSnapshot, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	if err := validateDBSnapshotIdentifier(aws.StringValue(input.DBSnapshotIdentifier)); err != nil {
		return nil, err
	}
	if err := validateDBInstanceIdentifier(aws.StringValue(input.DBInstanceIdentifier)); err != nil {
		return nil, err
	}
	// Rejected before the instance is touched, so a snapshot with bad tags does
	// not leave it in backing-up.
	tagMap, err := validateTags(input.Tags)
	if err != nil {
		return nil, err
	}
	return &validatedSnapshot{
		DBSnapshotIdentifier: aws.StringValue(input.DBSnapshotIdentifier),
		DBInstanceIdentifier: aws.StringValue(input.DBInstanceIdentifier),
		SnapshotType:         SnapshotTypeManual,
		Tags:                 tagMap,
	}, nil
}

// Returns the snapshot type to filter on, empty for "any". A filter this
// phase cannot honour is rejected rather than dropped, because a silently
// unfiltered list reads as a complete answer.
func validateDescribeSnapshotsRequest(input *rds.DescribeDBSnapshotsInput) (string, error) {
	if input == nil {
		return "", nil
	}
	if len(input.Filters) > 0 {
		return "", unimplemented("Filters", "DescribeDBSnapshots filters on DBSnapshotIdentifier, DBInstanceIdentifier and SnapshotType only")
	}
	if aws.StringValue(input.DbiResourceId) != "" {
		return "", unimplemented("DbiResourceId", "a DB instance has no resource ID distinct from its identifier here")
	}
	if aws.BoolValue(input.IncludeShared) || aws.BoolValue(input.IncludePublic) {
		return "", unimplemented("IncludeShared/IncludePublic", "cross-account snapshot sharing is not offered")
	}
	switch snapshotType := aws.StringValue(input.SnapshotType); snapshotType {
	case "", SnapshotTypeManual, SnapshotTypeAutomated:
		return snapshotType, nil
	default:
		return "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"SnapshotType %q is not offered; use %q or %q", snapshotType, SnapshotTypeManual, SnapshotTypeAutomated)
	}
}

// A name the caller is minting: a customer snapshot, or a final snapshot at
// delete. The rds: namespace belongs to automated backups, so it is refused
// wherever a name is created — and accepted by validateDBSnapshotReference, which
// is what a restore from an automated backup goes through.
func validateDBSnapshotIdentifier(id string) error {
	// Ahead of the name rules, which would reject the colon too but as an opaque
	// "may contain only lowercase letters, digits and hyphens".
	if err := rejectAutomatedNamespace(id); err != nil {
		return err
	}
	return validateDBSnapshotName(id)
}

// A reference to a snapshot that already exists. An automated backup carries the
// prefix this plane minted it with, and restoring from one is the whole point of
// keeping it, so the namespace is accepted here.
func validateDBSnapshotReference(id string) error {
	if rest, ok := strings.CutPrefix(id, automatedSnapshotPrefix); ok {
		return validateDBSnapshotName(rest)
	}
	return validateDBSnapshotName(id)
}

// AWS's own rules. The character set is deliberately the DB instance
// identifier's rather than AWS's laxer one, so every name a snapshot can be
// created under is also a name DeleteDBInstance accepts for a final snapshot.
func validateDBSnapshotName(id string) error {
	switch {
	case id == "":
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBSnapshotIdentifier is required")
	case len(id) > maxDBSnapshotIdentifierLen:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBSnapshotIdentifier must be at most %d characters", maxDBSnapshotIdentifierLen)
	case !isLetter(rune(id[0])):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBSnapshotIdentifier must begin with a letter")
	case strings.HasSuffix(id, "-"):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBSnapshotIdentifier may not end with a hyphen")
	case strings.Contains(id, "--"):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBSnapshotIdentifier may not contain consecutive hyphens")
	}
	for _, r := range id {
		if !isDigit(r) && r != '-' && (r < 'a' || r > 'z') {
			return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"DBSnapshotIdentifier may contain only lowercase letters, digits and hyphens")
		}
	}
	return nil
}

// Everything a restore needs, copied rather than referenced: the DB instance
// this describes may be gone by the time the snapshot is used.
func newDBSnapshotRecord(accountID string, rec *DBInstanceRecord, req *validatedSnapshot) DBSnapshotRecord {
	return DBSnapshotRecord{
		DBSnapshotIdentifier:    req.DBSnapshotIdentifier,
		DBInstanceIdentifier:    rec.DBInstanceIdentifier,
		AccountID:               accountID,
		SnapshotType:            req.SnapshotType,
		Status:                  SnapshotStatusCreating,
		SourceVolumeID:          rec.DataVolumeID,
		Engine:                  rec.Engine,
		EngineVersion:           rec.EngineVersion,
		DBInstanceClass:         rec.DBInstanceClass,
		AllocatedStorage:        rec.AllocatedStorage,
		StorageType:             rec.StorageType,
		StorageEncrypted:        rec.StorageEncrypted,
		DBName:                  rec.DBName,
		MasterUsername:          rec.MasterUsername,
		Port:                    rec.Port,
		VpcID:                   rec.VpcID,
		VpcSecurityGroupIDs:     rec.VpcSecurityGroupIDs,
		DBSubnetGroupName:       rec.DBSubnetGroupName,
		DBParameterGroupName:    rec.DBParameterGroupName,
		MasterPasswordUpdatedAt: rec.MasterPasswordUpdatedAt,
		Tags:                    req.Tags,
		CreatedAt:               time.Now().UTC(),
	}
}

// Returns the record plus its revision, for callers that follow with a CAS.
func (s *Service) getDBSnapshot(ctx context.Context, kv jetstream.KeyValue, id string) (*DBSnapshotRecord, uint64, error) {
	var rec DBSnapshotRecord
	rev, found, err := getJSONRevision(ctx, kv, DBSnapshotKey(id), &rec)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nil, 0, awserrors.Errorf(awserrors.ErrorDBSnapshotNotFound, "DB snapshot %s not found", id)
	}
	return &rec, rev, nil
}

// Rejects a taken identifier before any work starts. A snapshot record outlives
// the instance it came from, so a name can be held by a snapshot of an instance
// that no longer exists.
func (s *Service) checkDBSnapshotAvailable(ctx context.Context, kv jetstream.KeyValue, id string) error {
	var existing DBSnapshotRecord
	found, err := getJSON(ctx, kv, DBSnapshotKey(id), &existing)
	if err != nil {
		return err
	}
	if found {
		return awserrors.Errorf(awserrors.ErrorDBSnapshotAlreadyExists, "DB snapshot %s already exists", id)
	}
	return nil
}

// Withdraws a creating record whose snapshot never happened, on a context
// detached from the caller's so an expired request deadline still clears it.
func (s *Service) discardSnapshotRecord(ctx context.Context, kv jetstream.KeyValue, id string) {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := kv.Delete(rbCtx, DBSnapshotKey(id)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		slog.WarnContext(rbCtx, "rds: withdrawing the record of a snapshot that failed left it behind",
			"dbSnapshot", id, "err", err)
	}
}

// Moves the instance into backing-up and records the snapshot holding it in one
// CAS, returning the status the instance goes back to. That write is the
// per-instance guard: a second snapshot, an automated one or a lifecycle
// op finds the instance already backing-up and is rejected rather than queued.
func (s *Service) beginSnapshotOperation(ctx context.Context, kv jetstream.KeyValue, rev uint64,
	rec *DBInstanceRecord, dbSnapshotIdentifier string) (Status, error) {
	legal := []Status{StatusAvailable, StatusStopped}
	if !slices.Contains(legal, rec.Status) {
		return "", awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
			"DB instance %s is %s; a snapshot requires it to be %s",
			rec.DBInstanceIdentifier, rec.Status, joinStatuses(legal))
	}

	now := time.Now().UTC()
	resume := rec.Status
	rec.SnapshotOperation = &SnapshotOperation{
		DBSnapshotIdentifier: dbSnapshotIdentifier,
		ResumeStatus:         resume,
		StartedAt:            now,
	}
	rec.Status = StatusBackingUp
	rec.TransitionStartedAt = &now
	rec.UpdatedAt = now

	if err := updateJSON(ctx, kv, DBInstanceKey(rec.DBInstanceIdentifier), rev, rec); err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return "", awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
				"DB instance %s changed state concurrently; retry the snapshot", rec.DBInstanceIdentifier)
		}
		return "", err
	}
	return resume, nil
}

// Returns the instance to where the snapshot found it, so a failed snapshot
// leaves a usable instance rather than one stuck in backing-up. A delete
// accepted while the snapshot ran owns the record by now, so the resume only
// applies to an instance still in backing-up.
func (s *Service) endSnapshotOperation(ctx context.Context, kv jetstream.KeyValue, id string, resume Status) {
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultTimeout)
	defer cancel()
	err := s.updateInstance(endCtx, kv, id, func(stored *DBInstanceRecord) {
		if stored.Status == StatusBackingUp {
			stored.Status = resume
			stored.TransitionStartedAt = nil
		}
		stored.SnapshotOperation = nil
	})
	if err != nil {
		slog.ErrorContext(ctx, "rds: returning an instance out of backing-up failed; the reconciler will finish it",
			"dbInstance", id, "resume", resume, "err", err)
	}
}

// The customer-facing view. Only fields this phase actually backs are set, so an
// unset one is honestly absent rather than a fabricated default.
func (s *Service) projectDBSnapshot(rec *DBSnapshotRecord) *rds.DBSnapshot {
	if rec == nil {
		return nil
	}
	out := &rds.DBSnapshot{
		DBSnapshotIdentifier: aws.String(rec.DBSnapshotIdentifier),
		DBSnapshotArn:        aws.String(DBSnapshotARN(s.region, rec.AccountID, rec.DBSnapshotIdentifier)),
		DBInstanceIdentifier: aws.String(rec.DBInstanceIdentifier),
		SnapshotType:         aws.String(rec.SnapshotType),
		Status:               aws.String(rec.Status),
		Engine:               aws.String(rec.Engine),
		EngineVersion:        aws.String(rec.EngineVersion),
		AllocatedStorage:     aws.Int64(rec.AllocatedStorage),
		StorageType:          aws.String(rec.StorageType),
		Encrypted:            aws.Bool(rec.StorageEncrypted),
		MasterUsername:       aws.String(rec.MasterUsername),
		Port:                 aws.Int64(rec.Port),
		SnapshotCreateTime:   aws.Time(rec.CreatedAt),
		// The Terraform provider reads tags from the describe as well as from
		// ListTagsForResource, so the two have to agree.
		TagList: tagsToAWS(rec.Tags),
	}
	if rec.VpcID != "" {
		out.VpcId = aws.String(rec.VpcID)
	}
	// A snapshot still being taken has no data yet, so reporting full progress
	// would have a client restore from something that does not exist.
	if rec.Status == SnapshotStatusAvailable {
		out.PercentProgress = aws.Int64(100)
	}
	return out
}

// Reads as a sentence: the fault this feeds names what the customer has to
// remove first, and one dependent is the overwhelmingly common case.
func joinNames(names []string) string {
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}
