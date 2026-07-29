package handlers_rds

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/nats-io/nats.go/jetstream"
)

// The single drain for everything a modify recorded but has not delivered. Both
// the ApplyImmediately path and the reconciler's resume of an interrupted
// modify come through here, so a deferred change is applied by exactly the code
// an immediate one uses — the failure mode this closes is a maintenance-window
// modify that quietly does something different from the one the customer
// watched happen.
//
// rds-9 owns the trigger: its window machinery — parsing, deterministic
// assignment, a persisted last-fired stamp, exactly-once firing across leader
// churn — is the same mechanism a maintenance window needs, so it is built once
// there and calls this.
//
// The caller has already moved the instance into modifying; the record is left
// there, because the engine has to come back and report healthy before the
// reconciler calls it available.
func (s *Service) applyPendingModifications(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord) error {
	pending := rec.PendingModifiedValues
	if pending.empty() {
		return nil
	}

	// Parameters first, while the engine this modify started against is still
	// the one running: the disruptive step below restarts it, which is also what
	// puts any statically-scoped setting into effect.
	if pending.DBParameterGroupName != "" {
		if err := s.applyParameterGroup(ctx, kv, accountID, rec, pending.DBParameterGroupName); err != nil {
			return err
		}
	}

	// A class change and a storage grow both take the engine down, so they share
	// one outage: the volume is grown in the window between the old VM being
	// terminated and the new one launching, which is the only window ModifyVolume
	// will accept it in.
	grewStorage := false
	switch {
	case pending.DBInstanceClass != "":
		instanceType, err := InstanceTypeForClass(pending.DBInstanceClass)
		if err != nil {
			return fmt.Errorf("rds: DBInstanceClass %q is not supported", pending.DBInstanceClass)
		}
		if err := s.replaceInstanceVM(ctx, kv, accountID, rec, replaceInput{
			InstanceClass:    pending.DBInstanceClass,
			InstanceType:     instanceType,
			GrowStorageToGiB: aws.Int64Value(pending.AllocatedStorage),
			Reason:           "the instance class changed to " + pending.DBInstanceClass,
		}); err != nil {
			return err
		}
		grewStorage = pending.AllocatedStorage != nil
	case pending.AllocatedStorage != nil:
		if err := s.growInstanceStorage(ctx, accountID, rec, *pending.AllocatedStorage); err != nil {
			return err
		}
		grewStorage = true
	}

	applied := *pending
	return s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		if applied.DBInstanceClass != "" {
			stored.DBInstanceClass = applied.DBInstanceClass
		}
		if applied.AllocatedStorage != nil {
			stored.AllocatedStorage = *applied.AllocatedStorage
		}
		if applied.DBParameterGroupName != "" {
			stored.DBParameterGroupName = applied.DBParameterGroupName
		}
		// The volume is at its new size but the guest's filesystem is not yet on
		// it, so the last step stays outstanding until the agent is back to run
		// it. Everything else is now in effect.
		if grewStorage || applied.FilesystemGrowPending {
			stored.PendingModifiedValues = &PendingModifiedValues{
				FilesystemGrowPending: true,
				RequestedAt:           applied.RequestedAt,
			}
			return
		}
		stored.PendingModifiedValues = nil
	})
}

// Installs the resolved parameter set into the engine's config and reloads it,
// recording the settings the engine accepted but will not honour until it
// restarts (D16). The set lives on the data volume, so it survives the VM
// replace a class change performs and needs no second apply afterwards.
func (s *Service) applyParameterGroup(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord, group string) error {
	pendingReboot, err := s.applyParameters(ctx, accountID, rec.DBInstanceIdentifier, rec.Bootstrap.ResolvedParameters)
	if err != nil {
		return fmt.Errorf("apply the parameters of %s to %s: %w", group, rec.DBInstanceIdentifier, err)
	}
	return s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		stored.PendingRebootParameters = pendingReboot
	})
}

// The last step of a grow, run once the restarted or replaced agent is back:
// the control plane has already grown the volume, and this extends the guest's
// filesystem onto the capacity that is now there. Both ext4 and XFS grow while
// mounted, so it needs no ordering against the engine start.
func (s *Service) finishFilesystemGrow(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord) error {
	if err := s.growFilesystem(ctx, accountID, rec.DBInstanceIdentifier); err != nil {
		return fmt.Errorf("extend the filesystem of %s onto its grown volume: %w", rec.DBInstanceIdentifier, err)
	}
	if err := s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		stored.PendingModifiedValues = nil
	}); err != nil {
		return err
	}
	rec.PendingModifiedValues = nil

	slog.InfoContext(ctx, "rds: filesystem extended onto the grown data volume",
		"dbInstance", rec.DBInstanceIdentifier, "allocatedStorage", rec.AllocatedStorage)
	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
		"DB instance storage grown.", EventCategoryConfigurationChange)
	return nil
}
