package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// The one primitive a class change, a storage grow that rides the same outage,
// and automatic recovery all reduce to: stop the engine, terminate the VM,
// launch a fresh one at the target size, re-attach the same data volume and
// persisted customer ENI, and rewrite the rds-system index so the superseded
// agent can no longer authenticate as this instance.
//
// The endpoint is untouched throughout: the customer ENI keeps its address, so
// the DNS record and the serving cert's SANs stay correct and clients reconnect
// to the same hostname.

// What the replacement VM has to come back as.
type replaceInput struct {
	// The db.* class and the EC2 instance type behind it. Empty keeps the
	// record's current sizing, which is what auto-recovery wants.
	InstanceClass string
	InstanceType  string
	// A storage grow to perform while no VM holds the volume. Zero grows
	// nothing; ModifyVolume would refuse an attached volume anyway, so a grow
	// requested alongside a class change rides this window rather than opening a
	// second one.
	GrowStorageToGiB int64
	// Recorded on the customer's event ring, so a replace is attributable to the
	// change or the failure that caused it.
	Reason string
}

// Swaps the VM under a DB instance and leaves the record naming the new one.
// Only the VM's identity is written here — the class, the storage size and the
// pending values belong to the caller that asked for the change, so a failure
// part-way through leaves the request still recorded and retryable.
func (s *Service) replaceInstanceVM(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord, in replaceInput) error {
	if rec.ENIID == "" || rec.DataVolumeID == "" {
		return fmt.Errorf("rds: DB instance %s has no persisted ENI and data volume to replace onto",
			rec.DBInstanceIdentifier)
	}
	instanceType := in.InstanceType
	if instanceType == "" {
		resolved, err := InstanceTypeForClass(rec.DBInstanceClass)
		if err != nil {
			return fmt.Errorf("rds: DB instance %s has an unmapped class %q", rec.DBInstanceIdentifier, rec.DBInstanceClass)
		}
		instanceType = resolved
	}
	// Resolve the profile before stopping the engine or terminating its VM. A
	// profile failure must leave the current database serving.
	profileARN, err := ensureInstanceProfile(s.deps.IAM, utils.GlobalAccountID)
	if err != nil {
		return err
	}
	if rec.FormatAuthorized {
		if err := s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
			stored.FormatAuthorized = false
		}); err != nil {
			return fmt.Errorf("revoke the data-volume format grant before replacing %s: %w", rec.DBInstanceIdentifier, err)
		}
		rec.FormatAuthorized = false
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	// The generation this replace bumps is what the staged payload is bound to,
	// and it is never rebound. Withdrawn before the VM is touched, so the
	// ciphertext is gone even if the replace fails half-way.
	if err := s.discardPendingBootstrap(ctx, kv, accountID, rec); err != nil {
		return err
	}

	// Checkpointed first so the replacement boots on a clean datadir rather than
	// one it has to replay a WAL over. A wedged agent degrades this rather than
	// blocking the replace, exactly as a stop does.
	s.stopEngineOrRecordFallback(ctx, accountID, rec, "replacing its VM")

	oldInstanceID := rec.InstanceID
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := s.terminateInstanceVM(ctx, oldInstanceID); err != nil {
		return fmt.Errorf("terminate the VM behind %s: %w", rec.DBInstanceIdentifier, err)
	}
	// The system NIC is disposable and the fresh launch mints its own; leaving
	// this one behind would hold an address in the shared RDS system subnet.
	if err := context.Cause(ctx); err != nil {
		return err
	}
	s.deleteSystemENI(ctx, rec)

	// The only moment the volume is held by nothing, which is the only moment
	// ModifyVolume will take it.
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if in.GrowStorageToGiB > 0 {
		if err := s.growDataVolume(ctx, rec.DataVolumeID, in.GrowStorageToGiB); err != nil {
			return err
		}
	}

	if err := context.Cause(ctx); err != nil {
		return err
	}
	launched, err := s.launchReplacementVM(ctx, accountID, rec, instanceType, profileARN)
	if err != nil {
		return err
	}
	if err := context.Cause(ctx); err != nil {
		s.unwindLaunched(context.WithoutCancel(ctx), launched)
		return err
	}

	// Written before the record so an agent that boots fast enough to call the
	// gateway is resolvable; the old entry goes first, or a superseded VM still
	// authenticates as this instance.
	if err := s.rewriteInstanceIndex(ctx, accountID, rec, oldInstanceID, launched.InstanceID); err != nil {
		return errors.Join(err, s.rollbackReplacementLaunch(ctx, accountID, rec, oldInstanceID, launched))
	}
	if err := context.Cause(ctx); err != nil {
		return errors.Join(err, s.rollbackReplacementLaunch(ctx, accountID, rec, oldInstanceID, launched))
	}

	if err := s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		stored.InstanceID = launched.InstanceID
		stored.SystemENIID = launched.SystemENIID
		stored.DataVolumeSerial = launched.DataVolumeSerial
		stored.VMGeneration++
		stored.FormatAuthorized = false
		// The new VM has never reported, so the old VM's health must not read as
		// this one's — the reconciler would call the replace finished at once.
		stored.Agent = AgentState{}
	}); err != nil {
		return errors.Join(err, s.rollbackReplacementLaunch(ctx, accountID, rec, oldInstanceID, launched))
	}
	// Kept in step so the caller's own record write does not resurrect the old
	// VM's identity on top of this one.
	rec.InstanceID = launched.InstanceID
	rec.SystemENIID = launched.SystemENIID
	rec.DataVolumeSerial = launched.DataVolumeSerial
	rec.VMGeneration++
	rec.FormatAuthorized = false
	rec.Agent = AgentState{}

	slog.InfoContext(ctx, "rds: DB VM replaced",
		"dbInstance", rec.DBInstanceIdentifier, "from", oldInstanceID, "to", launched.InstanceID,
		"instanceType", instanceType, "generation", rec.VMGeneration, "reason", in.Reason)
	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
		"DB instance is being replaced onto a new VM: "+in.Reason, EventCategoryConfigurationChange)
	return nil
}

// A fresh COW clone of the engine AMI, re-attached to the identity the DB
// instance owns. The new agent fetches its bootstrap config in attach mode
// It gets the port, the resolved parameters and a freshly minted serving
// cert, but no master password, and rds-init skips initdb on the datadir it
// finds already initialised.
func (s *Service) launchReplacementVM(ctx context.Context, accountID string, rec *DBInstanceRecord, instanceType, profileARN string) (*LaunchOutput, error) {
	launched, err := LaunchDBInstanceVM(ctx, s.deps.Launch, LaunchInput{
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		AccountID:            accountID,
		SubnetID:             rec.SubnetID,
		SecurityGroupIDs:     rec.VpcSecurityGroupIDs,
		Engine:               rec.Engine,
		EngineVersion:        rec.EngineVersion,
		InstanceType:         instanceType,
		AllocatedStorage:     rec.AllocatedStorage,
		ExistingCustomerENI:  rec.ENIID,
		ExistingDataVolume:   rec.DataVolumeID,
		UserData: buildAgentUserData(agentUserDataInput{
			GatewayURL:           s.deps.GatewayURL,
			GatewayCACert:        s.deps.GatewayCACert,
			Region:               s.region,
			DBInstanceIdentifier: rec.DBInstanceIdentifier,
			EngineVersion:        rec.EngineVersion,
			EnginePort:           rec.Port,
		}),
		IamInstanceProfileArn: profileARN,
	})
	if err != nil {
		return nil, fmt.Errorf("launch the replacement VM for %s: %w", rec.DBInstanceIdentifier, err)
	}
	return launched, nil
}

// The instance index keys off the internal EC2 instance ID, which every replace
// mints anew. The old entry is removed first: while it exists the superseded
// agent's IMDS credentials still resolve to this DB instance.
func (s *Service) rewriteInstanceIndex(ctx context.Context, accountID string, rec *DBInstanceRecord, oldInstanceID, newInstanceID string) error {
	if oldInstanceID != "" && oldInstanceID != newInstanceID {
		if err := s.DeleteInstanceIndex(ctx, oldInstanceID); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("rds: drop the superseded instance index entry for %s: %w", rec.DBInstanceIdentifier, err)
		}
	}
	if err := s.PutInstanceIndex(ctx, newInstanceID, InstanceIndexEntry{
		AccountID:            accountID,
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		VMGeneration:         rec.VMGeneration + 1,
	}); err != nil {
		return fmt.Errorf("rds: write the instance index for %s: %w", rec.DBInstanceIdentifier, err)
	}
	return nil
}

// Restores the index to the record's old VM before unwinding a replacement that
// could not commit. Cleanup is bounded but detached from lease cancellation.
func (s *Service) rollbackReplacementLaunch(
	ctx context.Context,
	accountID string,
	rec *DBInstanceRecord,
	oldInstanceID string,
	launched *LaunchOutput,
) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	var cleanupErrs []error
	if err := s.DeleteInstanceIndex(cleanupCtx, launched.InstanceID); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("delete replacement instance index %s: %w", launched.InstanceID, err))
	}
	if oldInstanceID != "" {
		if err := s.PutInstanceIndex(cleanupCtx, oldInstanceID, InstanceIndexEntry{
			AccountID:            accountID,
			DBInstanceIdentifier: rec.DBInstanceIdentifier,
			VMGeneration:         rec.VMGeneration,
		}); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("restore old instance index %s: %w", oldInstanceID, err))
		}
	}
	s.unwindLaunched(cleanupCtx, launched)
	return errors.Join(cleanupErrs...)
}

// Best-effort: a leaked system NIC costs an address in a platform-owned subnet,
// which is not worth failing a replace that has already terminated the VM.
func (s *Service) deleteSystemENI(ctx context.Context, rec *DBInstanceRecord) {
	if rec.SystemENIID == "" || s.deps.Launch.VPC == nil {
		return
	}
	deleteLaunchENI(ctx, s.deps.Launch.VPC, utils.GlobalAccountID, rec.SystemENIID)
}
