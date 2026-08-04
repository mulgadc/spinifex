package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

var errFinalSnapshotInProgress = errors.New("rds: final DB snapshot creation is still in progress")

type finalSnapshotReservation struct {
	creator  bool
	revision uint64
}

// Tears the DB instance down. AWS requires the caller to choose explicitly
// between a final snapshot and none, so neither an accidental data loss nor an
// accidental retained volume can happen by omission.
//
// The record is moved to deleting first and every step below tolerates a
// missing resource, so a retried call or a reconciler-resumed teardown
// converges on the same end state rather than failing on what it already did.
func (s *Service) DeleteDBInstance(ctx context.Context, input *rds.DeleteDBInstanceInput, accountID string) (*rds.DeleteDBInstanceOutput, error) {
	finalSnapshot, err := validateDeleteRequest(input)
	if err != nil {
		return nil, err
	}
	id := aws.StringValue(input.DBInstanceIdentifier)

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, rev, err := s.getDBInstance(ctx, kv, id)
	if err != nil {
		return nil, err
	}
	if rec.DeletionProtection {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterCombination,
			"DB instance %s cannot be deleted because deletion protection is enabled", id)
	}
	if err := s.checkFinalSnapshotAvailable(ctx, kv, rec, finalSnapshot); err != nil {
		return nil, err
	}

	// A retry of a delete already under way has to repeat its snapshot choice.
	// Keeping the first one silently would answer a caller who asked for a final
	// snapshot with no snapshot, or snapshot for one who asked to skip.
	if rec.Status == StatusDeleting && finalSnapshot != rec.FinalSnapshotIdentifier {
		return nil, awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
			"DB instance %s is already being deleted with %s; a retry must repeat that choice", id, snapshotChoice(rec.FinalSnapshotIdentifier))
	}

	// Reserve the final snapshot before the instance enters deleting. A collision
	// therefore fails while the VM and data volume are still intact, and a crash
	// afterwards leaves a creating record the reconciler can resolve.
	reservation, err := s.reserveFinalSnapshot(ctx, kv, accountID, rec, finalSnapshot)
	if err != nil {
		return nil, err
	}

	// A repeat of a delete already under way re-runs the teardown rather than
	// being rejected: the first call may have died partway through it.
	if rec.Status != StatusDeleting {
		now := time.Now().UTC()
		rec.Status = StatusDeleting
		rec.FailureReason = ""
		rec.FinalSnapshotIdentifier = finalSnapshot
		rec.TransitionStartedAt = &now
		rec.UpdatedAt = now
		if err := updateJSON(ctx, kv, DBInstanceKey(id), rev, rec); err != nil {
			if reservation.creator {
				s.rollbackFinalSnapshotReservation(ctx, kv, finalSnapshot, reservation.revision)
			}
			if errors.Is(err, jetstream.ErrKeyExists) {
				return nil, awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
					"DB instance %s changed state concurrently; retry the delete", id)
			}
			return nil, err
		}
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, id, "DB instance deleted.", EventCategoryDeletion)
	}

	if err := s.teardownDBInstance(ctx, kv, accountID, rec, reservation.creator); err != nil {
		return nil, err
	}

	// The record is gone, so the customer is answered with the last state it
	// held — as AWS does, which reports the instance as deleting.
	return &rds.DeleteDBInstanceOutput{DBInstance: s.projectDBInstance(rec)}, nil
}

// One of the two is required, exactly as AWS: omitting both is the request that
// would otherwise silently destroy the only copy of the data.
func validateDeleteRequest(input *rds.DeleteDBInstanceInput) (string, error) {
	if input == nil {
		return "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	if aws.StringValue(input.DBInstanceIdentifier) == "" {
		return "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBInstanceIdentifier is required")
	}

	skip := aws.BoolValue(input.SkipFinalSnapshot)
	identifier := aws.StringValue(input.FinalDBSnapshotIdentifier)
	switch {
	case skip && identifier != "":
		return "", awserrors.Errorf(awserrors.ErrorInvalidParameterCombination,
			"FinalDBSnapshotIdentifier cannot be supplied with SkipFinalSnapshot")
	case !skip && identifier == "":
		return "", awserrors.Errorf(awserrors.ErrorInvalidParameterCombination,
			"FinalDBSnapshotIdentifier is required unless SkipFinalSnapshot is set")
	}
	if identifier != "" {
		if err := validateDBSnapshotIdentifier(identifier); err != nil {
			return "", err
		}
	}
	return identifier, nil
}

// How the in-flight delete's snapshot choice reads back to the customer, in the
// terms they supplied it in.
func snapshotChoice(identifier string) string {
	if identifier == "" {
		return "SkipFinalSnapshot"
	}
	return "FinalDBSnapshotIdentifier " + identifier
}

// A snapshot record outlives the DB instance record it was taken from, so an
// identifier reused by a later instance of the same name collides with the
// earlier one. Only a record naming this same instance and its current data
// volume is a resumed teardown's own work.
func finalSnapshotIsOurs(existing *DBSnapshotRecord, rec *DBInstanceRecord) bool {
	sameSource := existing.DBInstanceIdentifier == rec.DBInstanceIdentifier &&
		existing.SourceVolumeID == rec.DataVolumeID
	legacyFinal := rec.Status == StatusDeleting &&
		rec.FinalSnapshotIdentifier == existing.DBSnapshotIdentifier
	return sameSource && (existing.FinalSnapshot || legacyFinal)
}

// Rejects a taken identifier before anything is torn down, as AWS does. Leaving
// it to the snapshot step would terminate the VM first and then fail on an
// error no retry can clear.
func (s *Service) checkFinalSnapshotAvailable(ctx context.Context, kv jetstream.KeyValue, rec *DBInstanceRecord, identifier string) error {
	if identifier == "" {
		return nil
	}
	var existing DBSnapshotRecord
	found, err := getJSON(ctx, kv, DBSnapshotKey(identifier), &existing)
	if err != nil || !found {
		return err
	}
	if finalSnapshotIsOurs(&existing, rec) {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorDBSnapshotAlreadyExists, "DB snapshot %s already exists", identifier)
}

// The teardown chain. Ordering is forced by what holds what: the VM has to go
// before its ENIs and volume can be released, and the final snapshot is taken
// once the VM is gone so it reads a sealed data volume rather than one a live
// engine is still writing to.
//
// Every step treats a missing resource as done, so this is safe to re-run from
// any point it stopped at.
func (s *Service) teardownDBInstance(ctx context.Context, kv jetstream.KeyValue, accountID string,
	rec *DBInstanceRecord, finalSnapshotCreator bool) error {
	// Asked to stop cleanly even though the VM is about to go: it is what makes
	// the final snapshot a clean checkpoint rather than one needing WAL replay.
	if rec.FinalSnapshotIdentifier != "" {
		s.stopEngineOrRecordFallback(ctx, accountID, rec, "deleting")
	}

	if err := s.terminateInstanceVM(ctx, rec.InstanceID); err != nil {
		return fmt.Errorf("rds: terminate the VM behind %s: %w", rec.DBInstanceIdentifier, err)
	}
	if err := s.takeFinalSnapshot(ctx, kv, accountID, rec, finalSnapshotCreator); err != nil {
		return err
	}
	// Before the volume is released, so the automated backups are no longer holding
	// its chunks when that decides between deleting and retaining it. AWS keeps
	// automated backups after a delete as a separate resource; doing that here
	// would pin the data volume indefinitely under the retention rule.
	if err := s.purgeAutomatedBackups(ctx, kv, accountID, rec.DBInstanceIdentifier); err != nil {
		return fmt.Errorf("rds: sweep the automated backups of %s: %w", rec.DBInstanceIdentifier, err)
	}
	if err := s.releaseDataVolume(ctx, kv, accountID, rec); err != nil {
		return err
	}

	s.deleteInstanceENIs(ctx, accountID, rec)
	s.publishDNS(ctx, accountID, rec, handlers_dns.ActionDelete)

	if rec.InstanceID != "" {
		if err := s.DeleteInstanceIndex(ctx, rec.InstanceID); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("rds: delete the instance index entry for %s: %w", rec.DBInstanceIdentifier, err)
		}
	}
	// Last: while it exists the instance is still nameable, and everything above
	// is reachable only through it.
	if err := kv.Delete(ctx, DBInstanceKey(rec.DBInstanceIdentifier)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("rds: delete the DB instance record for %s: %w", rec.DBInstanceIdentifier, err)
	}

	slog.InfoContext(ctx, "rds: DB instance deleted",
		"dbInstance", rec.DBInstanceIdentifier, "accountId", accountID,
		"finalSnapshot", rec.FinalSnapshotIdentifier)
	return nil
}

// A VM no node owns is already terminated, which is the state this is trying to
// reach.
func (s *Service) terminateInstanceVM(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return nil
	}
	launcher := s.deps.Launch.Instance
	if launcher == nil {
		return errors.New("rds: no system-instance launcher configured")
	}
	if err := launcher.TerminateSystemInstance(instanceID); err != nil &&
		!errors.Is(err, sysinstance.ErrSystemInstanceNotFound) {
		return err
	}
	return nil
}

// Reserves the final snapshot name before deletion becomes destructive. The
// creator is the only worker allowed to cut new data; retries may adopt data it
// already cut, but cannot race it into making a duplicate snapshot.
func (s *Service) reserveFinalSnapshot(ctx context.Context, kv jetstream.KeyValue, accountID string,
	rec *DBInstanceRecord, identifier string) (finalSnapshotReservation, error) {
	if identifier == "" || rec.DataVolumeID == "" {
		return finalSnapshotReservation{}, nil
	}

	// Manual, deliberately: the customer named it, so only the customer removes it
	// — which is also what keeps the retention sweep away from it.
	record := newDBSnapshotRecord(accountID, rec, &validatedSnapshot{
		DBSnapshotIdentifier: identifier,
		SnapshotType:         SnapshotTypeManual,
		Tags:                 rec.Tags,
	})
	record.FinalSnapshot = true
	rev, err := createJSONRevision(ctx, kv, DBSnapshotKey(identifier), &record)
	if err == nil {
		return finalSnapshotReservation{creator: true, revision: rev}, nil
	}
	if !errors.Is(err, jetstream.ErrKeyExists) {
		return finalSnapshotReservation{}, err
	}

	var existing DBSnapshotRecord
	_, found, err := getJSONRevision(ctx, kv, DBSnapshotKey(identifier), &existing)
	if err != nil {
		return finalSnapshotReservation{}, err
	}
	if !found || !finalSnapshotIsOurs(&existing, rec) {
		return finalSnapshotReservation{}, awserrors.Errorf(awserrors.ErrorDBSnapshotAlreadyExists,
			"DB snapshot %s already exists", identifier)
	}
	return finalSnapshotReservation{}, nil
}

// Removes a reservation when the instance never entered deleting or the EC2
// create failed. The revision guard prevents cleanup from deleting a record
// another worker has already completed.
func (s *Service) rollbackFinalSnapshotReservation(ctx context.Context, kv jetstream.KeyValue,
	identifier string, rev uint64) {
	if identifier == "" || rev == 0 {
		return
	}
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := kv.Delete(rbCtx, DBSnapshotKey(identifier), jetstream.LastRevision(rev)); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		slog.WarnContext(rbCtx, "rds: rollback of a final snapshot reservation failed",
			"dbSnapshot", identifier, "err", err)
	}
}

// Records the final snapshot under the reservation written before teardown. A
// retry first adopts matching EC2 data left by a dead creator; it never cuts a
// second snapshot while that creator may still be running.
func (s *Service) takeFinalSnapshot(ctx context.Context, kv jetstream.KeyValue, accountID string,
	rec *DBInstanceRecord, creator bool) error {
	if rec.FinalSnapshotIdentifier == "" || rec.DataVolumeID == "" {
		return nil
	}
	if s.deps.Snapshots == nil {
		return errors.New("rds: no snapshot service configured")
	}

	key := DBSnapshotKey(rec.FinalSnapshotIdentifier)
	var record DBSnapshotRecord
	rev, found, err := getJSONRevision(ctx, kv, key, &record)
	if err != nil {
		return err
	}
	if !found {
		reservation, err := s.reserveFinalSnapshot(ctx, kv, accountID, rec, rec.FinalSnapshotIdentifier)
		if err != nil {
			return err
		}
		creator = reservation.creator
		rev, found, err = getJSONRevision(ctx, kv, key, &record)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("rds: final snapshot reservation %s is missing", rec.FinalSnapshotIdentifier)
		}
	}
	if !finalSnapshotIsOurs(&record, rec) {
		return awserrors.Errorf(awserrors.ErrorDBSnapshotAlreadyExists,
			"DB snapshot %s already exists", rec.FinalSnapshotIdentifier)
	}
	if record.Status == SnapshotStatusAvailable && record.SnapshotID != "" {
		return nil
	}
	if record.Status != SnapshotStatusCreating {
		return awserrors.Errorf(awserrors.ErrorDBSnapshotInvalidState,
			"DB snapshot %s is %s; expected %s", rec.FinalSnapshotIdentifier, record.Status, SnapshotStatusCreating)
	}

	if !creator {
		snapshotID, err := s.findEC2SnapshotFor(ctx, accountID, rec.FinalSnapshotIdentifier)
		if err != nil {
			return err
		}
		if snapshotID == "" {
			return errFinalSnapshotInProgress
		}
		completed, err := s.completeFinalSnapshot(ctx, kv, rec, snapshotID)
		if err != nil {
			return err
		}
		if completed {
			s.recordFinalSnapshotCreated(ctx, accountID, rec, snapshotID)
		}
		return nil
	}

	// System-owned, like the volume it is taken from: the customer addresses it
	// by its RDS identifier, never by the EC2 snapshot underneath.
	snapshot, err := s.deps.Snapshots.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{
		VolumeId:    aws.String(rec.DataVolumeID),
		Description: aws.String("RDS final snapshot for " + rec.DBInstanceIdentifier),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("snapshot"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByRDS)},
				{Key: aws.String(rdsInstanceTagKey), Value: aws.String(rec.DBInstanceIdentifier)},
				{Key: aws.String(rdsSnapshotTagKey), Value: aws.String(rec.FinalSnapshotIdentifier)},
				{Key: aws.String(rdsSnapshotAccountTagKey), Value: aws.String(accountID)},
			},
		}},
	}, utils.GlobalAccountID)
	if err != nil {
		s.rollbackFinalSnapshotReservation(ctx, kv, rec.FinalSnapshotIdentifier, rev)
		return fmt.Errorf("rds: take the final snapshot of %s: %w", rec.DBInstanceIdentifier, err)
	}
	if snapshot == nil || aws.StringValue(snapshot.SnapshotId) == "" {
		s.rollbackFinalSnapshotReservation(ctx, kv, rec.FinalSnapshotIdentifier, rev)
		return fmt.Errorf("rds: take the final snapshot of %s: empty snapshot id", rec.DBInstanceIdentifier)
	}
	snapshotID := aws.StringValue(snapshot.SnapshotId)
	completed, err := s.completeFinalSnapshot(ctx, kv, rec, snapshotID)
	if err != nil {
		return fmt.Errorf("rds: record the final snapshot of %s: %w", rec.DBInstanceIdentifier, err)
	}
	if completed {
		s.recordFinalSnapshotCreated(ctx, accountID, rec, snapshotID)
	}
	return nil
}

// Completes only the snapshot fields under CAS, preserving tags changed while
// the EC2 snapshot was being cut.
func (s *Service) completeFinalSnapshot(ctx context.Context, kv jetstream.KeyValue,
	source *DBInstanceRecord, snapshotID string) (bool, error) {
	key := DBSnapshotKey(source.FinalSnapshotIdentifier)
	for range tagWriteAttempts {
		var record DBSnapshotRecord
		rev, found, err := getJSONRevision(ctx, kv, key, &record)
		if err != nil {
			return false, err
		}
		if !found {
			return false, fmt.Errorf("final snapshot reservation %s is missing", source.FinalSnapshotIdentifier)
		}
		if !finalSnapshotIsOurs(&record, source) {
			return false, awserrors.Errorf(awserrors.ErrorDBSnapshotAlreadyExists,
				"DB snapshot %s already exists", source.FinalSnapshotIdentifier)
		}
		if record.Status == SnapshotStatusAvailable {
			if record.SnapshotID != snapshotID {
				return false, fmt.Errorf("final snapshot %s already points to %s, not %s",
					source.FinalSnapshotIdentifier, record.SnapshotID, snapshotID)
			}
			return false, nil
		}
		if record.Status != SnapshotStatusCreating {
			return false, awserrors.Errorf(awserrors.ErrorDBSnapshotInvalidState,
				"DB snapshot %s is %s; expected %s", source.FinalSnapshotIdentifier,
				record.Status, SnapshotStatusCreating)
		}

		record.SnapshotID = snapshotID
		record.Status = SnapshotStatusAvailable
		if err := updateJSON(ctx, kv, key, rev, &record); err == nil {
			return true, nil
		} else if !errors.Is(err, jetstream.ErrKeyExists) {
			return false, err
		}
	}
	return false, fmt.Errorf("rds: completing final snapshot %s contended after %d attempts",
		source.FinalSnapshotIdentifier, tagWriteAttempts)
}

func (s *Service) recordFinalSnapshotCreated(ctx context.Context, accountID string,
	rec *DBInstanceRecord, snapshotID string) {
	s.RecordEvent(ctx, accountID, EventSourceTypeDBSnapshot, rec.FinalSnapshotIdentifier,
		"Final DB snapshot created.", EventCategoryBackup, EventCategoryCreation)
	slog.InfoContext(ctx, "rds: final DB snapshot created",
		"dbSnapshot", rec.FinalSnapshotIdentifier, "dbInstance", rec.DBInstanceIdentifier,
		"snapshotId", snapshotID)
}

// A viperblock snapshot references its source volume's chunk files rather than
// copying them, so the volume cannot be deleted while any snapshot survives —
// including the final one just taken. It is retained instead, recorded with the
// snapshots holding it so the last DeleteDBSnapshot can release it.
func (s *Service) releaseDataVolume(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord) error {
	if rec.DataVolumeID == "" {
		return nil
	}
	holders, err := s.snapshotsHolding(ctx, rec.DataVolumeID)
	if err != nil {
		return err
	}
	if len(holders) > 0 {
		return s.retainDataVolume(ctx, kv, accountID, rec, holders, false)
	}

	if s.deps.Launch.Volume == nil {
		return errors.New("rds: no volume service configured")
	}
	_, err = s.deps.Launch.Volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
		VolumeId: aws.String(rec.DataVolumeID),
	}, utils.GlobalAccountID)
	switch {
	case err == nil || awserrors.IsNotFound(err):
		return nil
	case awserrors.IsErrorCode(err, awserrors.ErrorVolumeInUse):
		// The volume store's own snapshot index sees a reference the enumeration
		// above did not. Retaining is the safe reading of that disagreement;
		// returning the error would wedge the delete on every retry instead.
		return s.retainDataVolume(ctx, kv, accountID, rec, holders, true)
	default:
		return fmt.Errorf("rds: delete the data volume of %s: %w", rec.DBInstanceIdentifier, err)
	}
}

// Records a volume the teardown left behind, with the snapshots holding it so
// the last DeleteDBSnapshot can release it. holdersUnresolved marks the
// case where the volume store refused the delete but named nobody, so a release
// has to re-check rather than trust an empty list.
func (s *Service) retainDataVolume(ctx context.Context, kv jetstream.KeyValue, accountID string,
	rec *DBInstanceRecord, holders []string, holdersUnresolved bool) error {
	retained := RetainedVolumeRecord{
		VolumeID:             rec.DataVolumeID,
		AccountID:            accountID,
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		Snapshots:            holders,
		HoldersUnresolved:    holdersUnresolved,
		RetainedAt:           time.Now().UTC(),
	}
	if err := putJSON(ctx, kv, RetainedVolumeKey(rec.DataVolumeID), &retained); err != nil {
		return err
	}
	slog.InfoContext(ctx, "rds: data volume retained; snapshots still reference its chunks",
		"dbInstance", rec.DBInstanceIdentifier, "volumeId", rec.DataVolumeID,
		"snapshots", holders, "holdersUnresolved", holdersUnresolved)
	return nil
}

// The RDS snapshot identifiers whose EC2 snapshot references volumeID. Read
// from EC2 rather than from the RDS key space, because that is what
// DeleteVolume itself enforces against.
func (s *Service) snapshotsHolding(ctx context.Context, volumeID string) ([]string, error) {
	if s.deps.Snapshots == nil {
		return nil, errors.New("rds: no snapshot service configured")
	}
	out, err := s.deps.Snapshots.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("volume-id"),
			Values: aws.StringSlice([]string{volumeID}),
		}},
	}, utils.GlobalAccountID)
	if err != nil {
		if awserrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("rds: list the snapshots of %s: %w", volumeID, err)
	}
	if out == nil {
		return nil, nil
	}
	holders := make([]string, 0, len(out.Snapshots))
	for _, snapshot := range out.Snapshots {
		if id := aws.StringValue(snapshot.SnapshotId); id != "" {
			holders = append(holders, id)
		}
	}
	return holders, nil
}

// The customer ENI is deleted rather than retained: it is the endpoint of an
// instance that no longer exists. Both NICs are removed explicitly because the
// launch created them explicitly, and a leaked ENI holds its subnet address.
func (s *Service) deleteInstanceENIs(ctx context.Context, accountID string, rec *DBInstanceRecord) {
	vpcSvc := s.deps.Launch.VPC
	if vpcSvc == nil {
		return
	}
	if rec.ENIID != "" {
		deleteLaunchENI(ctx, vpcSvc, accountID, rec.ENIID)
	}
	if rec.SystemENIID != "" {
		deleteLaunchENI(ctx, vpcSvc, utils.GlobalAccountID, rec.SystemENIID)
	}
}
