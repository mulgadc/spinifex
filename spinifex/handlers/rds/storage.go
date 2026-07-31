package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// D12: ModifyVolume refuses a volume a running VM holds, so a storage grow is
// stop the engine, stop the VM, grow the volume, start the VM, and extend the
// filesystem inside the guest. There is no online grow in v1, and a shrink is
// rejected outright — as AWS rejects it.

// The volume-level half of a grow, kept apart from the launch provisioner
// because a grow reads the volume's current size before modifying it and a
// launch never does.
type volumeResizer interface {
	DescribeVolumes(ctx context.Context, input *ec2.DescribeVolumesInput, accountID string) (*ec2.DescribeVolumesOutput, error)
	ModifyVolume(ctx context.Context, input *ec2.ModifyVolumeInput, accountID string) (*ec2.ModifyVolumeOutput, error)
}

// Everything a resize request can be wrong about, checked before the instance
// moves out of available so a bad request never stops a running database. A
// request that merely repeats the current size is not an error: Terraform sends
// the whole body on every apply, so rejecting it would fail every unrelated
// modify. It is dropped from the plan instead, which changes no state.
func validateStorageGrow(current, requested int64) error {
	switch {
	case requested < minAllocatedStorageGiB || requested > maxAllocatedStorageGiB:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"AllocatedStorage must be between %d and %d GiB", minAllocatedStorageGiB, maxAllocatedStorageGiB)
	case requested < current:
		return awserrors.Errorf(awserrors.ErrorInvalidParameterCombination,
			"AllocatedStorage cannot be reduced from %d to %d GiB; storage is grow-only", current, requested)
	}
	return nil
}

// The stop/grow/start cycle for a grow with no class change alongside it. A
// class change opens the same outage and grows the volume inside it instead,
// so the two never run back to back.
func (s *Service) growInstanceStorage(ctx context.Context, accountID string, rec *DBInstanceRecord, targetGiB int64) error {
	if s.deps.Instances == nil {
		return errors.New("rds: no instance command path configured")
	}
	// The engine is checkpointed first so the filesystem the grow extends is not
	// one a live postmaster is still writing into.
	s.stopEngineOrRecordFallback(ctx, accountID, rec, "growing its storage")

	if err := s.stopInstanceVM(ctx, accountID, rec.InstanceID); err != nil {
		return fmt.Errorf("stop the DB VM before growing its storage: %w", err)
	}
	if err := s.growDataVolume(ctx, rec.DataVolumeID, targetGiB); err != nil {
		return err
	}
	if err := s.startInstanceVM(ctx, rec.InstanceID); err != nil {
		return fmt.Errorf("restart the DB VM after growing its storage: %w", err)
	}
	return nil
}

// Grows the data volume itself. Idempotent: a resumed grow re-reads the volume
// and returns without a second modify once it is already at the target, which
// ModifyVolume would otherwise reject as a shrink.
func (s *Service) growDataVolume(ctx context.Context, volumeID string, targetGiB int64) error {
	if volumeID == "" {
		return errors.New("rds: the DB instance has no data volume to grow")
	}
	if s.deps.Storage == nil {
		return errors.New("rds: no volume resize path configured")
	}

	current, err := s.dataVolumeSize(ctx, volumeID)
	if err != nil {
		return err
	}
	if current >= targetGiB {
		slog.InfoContext(ctx, "rds: data volume is already at the requested size; skipping the modify",
			"volumeId", volumeID, "sizeGiB", current, "targetGiB", targetGiB)
		return nil
	}

	if _, err := s.deps.Storage.ModifyVolume(ctx, &ec2.ModifyVolumeInput{
		VolumeId: aws.String(volumeID),
		Size:     aws.Int64(targetGiB),
	}, utils.GlobalAccountID); err != nil {
		return fmt.Errorf("grow the data volume %s to %d GiB: %w", volumeID, targetGiB, err)
	}
	slog.InfoContext(ctx, "rds: data volume grown",
		"volumeId", volumeID, "fromGiB", current, "toGiB", targetGiB)
	return nil
}

// The volume's own reported size, which is what makes the grow idempotent. A
// volume the store cannot describe is an error rather than a zero, since zero
// would read as "smaller than the target" and trigger a modify.
func (s *Service) dataVolumeSize(ctx context.Context, volumeID string) (int64, error) {
	out, err := s.deps.Storage.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: aws.StringSlice([]string{volumeID}),
	}, utils.GlobalAccountID)
	if err != nil {
		return 0, fmt.Errorf("read the data volume %s: %w", volumeID, err)
	}
	for _, volume := range out.Volumes {
		if aws.StringValue(volume.VolumeId) == volumeID {
			return aws.Int64Value(volume.Size), nil
		}
	}
	return 0, fmt.Errorf("rds: data volume %s no longer exists", volumeID)
}
