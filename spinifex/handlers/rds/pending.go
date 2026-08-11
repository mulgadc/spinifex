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
// The backup scheduler owns the trigger: its window machinery — parsing, deterministic
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
	if err := context.Cause(ctx); err != nil {
		return err
	}

	// Parameters first, while the engine this modify started against is still
	// the one running: the disruptive step below restarts it, which is also what
	// puts any statically-scoped setting into effect.
	//
	// A class change re-derives every size-derived default, so the set is
	// recomputed when either the group or the class moves. It is resolved
	// against the class the instance is becoming: the include lives on the data
	// volume, so it is the replacement VM that adopts it.
	if pending.DBParameterGroupName != "" || pending.DBInstanceClass != "" {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		group := pending.DBParameterGroupName
		if group == "" {
			group = rec.DBParameterGroupName
		}
		class := pending.DBInstanceClass
		if class == "" {
			class = rec.DBInstanceClass
		}
		if err := s.applyParameterGroup(ctx, kv, accountID, rec, group, class); err != nil {
			return err
		}
	}

	// A class change and a storage grow both take the engine down, so they share
	// one outage: the volume is grown in the window between the old VM being
	// terminated and the new one launching, which is the only window ModifyVolume
	// will accept it in.
	grewStorage := false
	restarted := false
	if err := context.Cause(ctx); err != nil {
		return err
	}
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
		restarted = true
	case pending.AllocatedStorage != nil:
		if err := s.growInstanceStorage(ctx, accountID, rec, *pending.AllocatedStorage); err != nil {
			return err
		}
		grewStorage = true
		restarted = true
	}

	// The outage above is the restart the statically-scoped parameters were
	// waiting for — the replacement VM boots on the set, and a grow's stop/start
	// re-reads it — so the record stops advertising them. A group change with
	// neither a class nor a storage move alongside it restarts nothing, and its
	// parameters stay pending until RebootDBInstance.
	appliedPendingReboot := restarted && len(rec.PendingRebootParameters) > 0
	if appliedPendingReboot {
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
			"Applied the parameters that were pending a reboot.", EventCategoryConfigurationChange)
		rec.PendingRebootParameters = nil
	}

	applied := *pending
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		if appliedPendingReboot {
			stored.PendingRebootParameters = nil
		}
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

// Re-resolves the effective set and installs it into the engine's config,
// recording the settings the engine accepted but will not honour until it
// restarts. The set lives on the data volume, so it survives the VM
// replace a class change performs and needs no second apply afterwards.
//
// A re-resolve, never a merge: the whole set is recomputed from the catalog and
// the new group's overrides, so a parameter the old group set and the new one
// does not reverts to its default rather than lingering.
func (s *Service) applyParameterGroup(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord, group, instanceClass string) error {
	engine, err := LookupEngine(rec.Engine)
	if err != nil {
		return err
	}
	// Reached from a deferred modify, whose group was checked at request time,
	// and from group propagation, where the binding was checked at attach. A
	// family mismatch here is corrupt state rather than a bad request.
	resolved, err := s.resolveGroupParameters(ctx, kv, accountID, engine, group, instanceClass)
	if err != nil {
		return err
	}
	pendingReboot, err := s.applyParameters(ctx, accountID, rec.DBInstanceIdentifier, resolved)
	if err != nil {
		return fmt.Errorf("apply the parameters of %s to %s: %w", group, rec.DBInstanceIdentifier, err)
	}
	// Stored so a later VM replace boots against the same set the engine is
	// already running, rather than re-deriving it from a group that may since
	// have changed underneath the instance.
	rec.Bootstrap.ResolvedParameters = resolved
	// Kept in step so the caller decides on the set this apply produced rather
	// than on the one the instance carried in.
	rec.PendingRebootParameters = pendingReboot
	rec.ParametersRolledBack = false
	return s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		stored.Bootstrap.ResolvedParameters = resolved
		stored.PendingRebootParameters = pendingReboot
		stored.ParametersRolledBack = false
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
