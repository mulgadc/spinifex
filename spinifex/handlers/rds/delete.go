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

// Takes the D18 final snapshot and answers whether a volume is still
// referenced by one, which is what decides between deleting and retaining it.
type snapshotProvider interface {
	CreateSnapshot(ctx context.Context, input *ec2.CreateSnapshotInput, accountID string) (*ec2.Snapshot, error)
	DescribeSnapshots(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error)
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
		return nil, fmt.Errorf("%s: DB instance %s cannot be deleted because deletion protection is enabled",
			awserrors.ErrorInvalidParameterCombination, id)
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
			if errors.Is(err, jetstream.ErrKeyExists) {
				return nil, fmt.Errorf("%s: DB instance %s changed state concurrently; retry the delete",
					awserrors.ErrorDBInstanceInvalidState, id)
			}
			return nil, err
		}
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, id, "DB instance deleted.", EventCategoryDeletion)
	}

	if err := s.teardownDBInstance(ctx, kv, accountID, rec); err != nil {
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
		return "", fmt.Errorf("%s: empty request", awserrors.ErrorInvalidParameterValue)
	}
	if aws.StringValue(input.DBInstanceIdentifier) == "" {
		return "", fmt.Errorf("%s: DBInstanceIdentifier is required", awserrors.ErrorInvalidParameterValue)
	}

	skip := aws.BoolValue(input.SkipFinalSnapshot)
	identifier := aws.StringValue(input.FinalDBSnapshotIdentifier)
	switch {
	case skip && identifier != "":
		return "", fmt.Errorf("%s: FinalDBSnapshotIdentifier cannot be supplied with SkipFinalSnapshot",
			awserrors.ErrorInvalidParameterCombination)
	case !skip && identifier == "":
		return "", fmt.Errorf("%s: FinalDBSnapshotIdentifier is required unless SkipFinalSnapshot is set",
			awserrors.ErrorInvalidParameterCombination)
	}
	if identifier != "" {
		if err := validateDBInstanceIdentifier(identifier); err != nil {
			return "", err
		}
	}
	return identifier, nil
}

// The teardown chain. Ordering is forced by what holds what: the VM has to go
// before its ENIs and volume can be released, and the final snapshot is taken
// once the VM is gone so it reads a sealed data volume rather than one a live
// engine is still writing to.
//
// Every step treats a missing resource as done, so this is safe to re-run from
// any point it stopped at.
func (s *Service) teardownDBInstance(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord) error {
	// Asked to stop cleanly even though the VM is about to go: it is what makes
	// the final snapshot a clean checkpoint rather than one needing WAL replay.
	if rec.FinalSnapshotIdentifier != "" {
		s.stopEngineOrRecordFallback(ctx, accountID, rec, "deleting")
	}

	if err := s.terminateInstanceVM(ctx, rec.InstanceID); err != nil {
		return fmt.Errorf("rds: terminate the VM behind %s: %w", rec.DBInstanceIdentifier, err)
	}
	if err := s.takeFinalSnapshot(ctx, kv, accountID, rec); err != nil {
		return err
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

// Records the D18 final snapshot under db-snapshots/{id}. The record is written
// with Create, so a resumed teardown that already took the snapshot does not
// take a second one.
func (s *Service) takeFinalSnapshot(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord) error {
	if rec.FinalSnapshotIdentifier == "" || rec.DataVolumeID == "" {
		return nil
	}
	key := DBSnapshotKey(rec.FinalSnapshotIdentifier)
	var existing DBSnapshotRecord
	found, err := getJSON(ctx, kv, key, &existing)
	if err != nil {
		return err
	}
	if found {
		if existing.DBInstanceIdentifier != rec.DBInstanceIdentifier {
			return fmt.Errorf("%s: DB snapshot %s already exists",
				awserrors.ErrorDBSnapshotAlreadyExists, rec.FinalSnapshotIdentifier)
		}
		return nil
	}
	if s.deps.Snapshots == nil {
		return errors.New("rds: no snapshot service configured")
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
			},
		}},
	}, utils.GlobalAccountID)
	if err != nil {
		return fmt.Errorf("rds: take the final snapshot of %s: %w", rec.DBInstanceIdentifier, err)
	}
	if snapshot == nil || aws.StringValue(snapshot.SnapshotId) == "" {
		return fmt.Errorf("rds: take the final snapshot of %s: empty snapshot id", rec.DBInstanceIdentifier)
	}

	// Everything a restore needs is copied here rather than referenced: the DB
	// instance record is deleted moments later.
	record := DBSnapshotRecord{
		DBSnapshotIdentifier: rec.FinalSnapshotIdentifier,
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		AccountID:            accountID,
		SnapshotType:         SnapshotTypeManual,
		Status:               SnapshotStatusAvailable,
		SnapshotID:           aws.StringValue(snapshot.SnapshotId),
		SourceVolumeID:       rec.DataVolumeID,
		Engine:               rec.Engine,
		EngineVersion:        rec.EngineVersion,
		AllocatedStorage:     rec.AllocatedStorage,
		StorageType:          rec.StorageType,
		StorageEncrypted:     rec.StorageEncrypted,
		MasterUsername:       rec.MasterUsername,
		Port:                 rec.Port,
		VpcID:                rec.VpcID,
		Tags:                 rec.Tags,
		CreatedAt:            time.Now().UTC(),
	}
	if err := createJSON(ctx, kv, key, &record); err != nil && !errors.Is(err, jetstream.ErrKeyExists) {
		return fmt.Errorf("rds: record the final snapshot of %s: %w", rec.DBInstanceIdentifier, err)
	}
	s.RecordEvent(ctx, accountID, EventSourceTypeDBSnapshot, rec.FinalSnapshotIdentifier,
		"Final DB snapshot created.", EventCategoryBackup, EventCategoryCreation)
	return nil
}

// A viperblock snapshot references its source volume's chunk files rather than
// copying them, so the volume cannot be deleted while any snapshot survives —
// including the final one just taken. It is retained instead, recorded with the
// snapshots holding it so the last DeleteDBSnapshot can release it (D10).
func (s *Service) releaseDataVolume(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord) error {
	if rec.DataVolumeID == "" {
		return nil
	}
	holders, err := s.snapshotsHolding(ctx, rec.DataVolumeID)
	if err != nil {
		return err
	}
	if len(holders) > 0 {
		retained := RetainedVolumeRecord{
			VolumeID:             rec.DataVolumeID,
			AccountID:            accountID,
			DBInstanceIdentifier: rec.DBInstanceIdentifier,
			Snapshots:            holders,
			RetainedAt:           time.Now().UTC(),
		}
		if err := putJSON(ctx, kv, RetainedVolumeKey(rec.DataVolumeID), &retained); err != nil {
			return err
		}
		slog.InfoContext(ctx, "rds: data volume retained; snapshots still reference its chunks",
			"dbInstance", rec.DBInstanceIdentifier, "volumeId", rec.DataVolumeID, "snapshots", holders)
		return nil
	}

	if s.deps.Launch.Volume == nil {
		return errors.New("rds: no volume service configured")
	}
	if _, err := s.deps.Launch.Volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
		VolumeId: aws.String(rec.DataVolumeID),
	}, utils.GlobalAccountID); err != nil && !awserrors.IsNotFound(err) {
		return fmt.Errorf("rds: delete the data volume of %s: %w", rec.DBInstanceIdentifier, err)
	}
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
