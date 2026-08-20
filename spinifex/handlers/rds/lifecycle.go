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
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The DB VM's power state is driven over the per-instance EC2 command path
// under the system account, not over a system.* subject: the system dispatch
// surface is launch and terminate only, and the ownership check on ec2.cmd
// passes because the VM is system-owned.
type instanceCommander interface {
	StopInstance(ctx context.Context, instanceID string) error
	RebootInstance(ctx context.Context, instanceID string) error
	// Starts a VM the owning node still holds. Returns ErrInstanceNotOnNode when
	// no node has it in memory, which is the normal case after a node restart.
	StartInstance(ctx context.Context, instanceID string) error
	// Relaunches a VM whose owning node no longer holds it, from its persisted
	// stopped-instance record.
	StartStoppedInstance(ctx context.Context, instanceID string) error
}

// No node subscribes ec2.cmd.{instanceID} for this VM, so its power state can
// only be changed through the stopped-instance path.
var ErrInstanceNotOnNode = errors.New("rds: no node is holding this DB VM")

// How long a VM stop, start or reboot may take before the command is treated as
// lost. A stop drains and seals the data volume, which is the long pole.
const instanceCommandTimeout = 90 * time.Second

// How often the fleet is re-read while a stop is settling.
const vmStopPollInterval = 500 * time.Millisecond

// Reboots the engine, applying any static parameters stored pending-reboot.
// ForceFailover is rejected outright: there is no standby to fail over
// to, and silently ignoring it would report a failover that never happened.
func (s *Service) RebootDBInstance(ctx context.Context, input *rds.RebootDBInstanceInput, accountID string) (*rds.RebootDBInstanceOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	if aws.BoolValue(input.ForceFailover) {
		return nil, unimplemented("ForceFailover", "this platform is single-AZ; there is no standby to fail over to")
	}

	rec, kv, err := s.beginTransition(ctx, accountID, aws.StringValue(input.DBInstanceIdentifier),
		StatusRebooting, StatusAvailable, StatusFailed)
	if err != nil {
		return nil, err
	}

	// The engine is asked to stop cleanly first so the reboot is a restart of a
	// checkpointed cluster rather than a crash it has to replay.
	s.stopEngineOrRecordFallback(ctx, accountID, rec, "rebooting")

	if err := s.deps.Instances.RebootInstance(ctx, rec.InstanceID); err != nil {
		return nil, s.failTransition(ctx, kv, accountID, rec,
			fmt.Sprintf("the DB instance could not be rebooted: %v", err))
	}

	// The parameters are already in the engine's config on the data volume, so
	// the restart is what applies them; the record only stops advertising them.
	if len(rec.PendingRebootParameters) > 0 {
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
			"Applied the parameters that were pending a reboot.", EventCategoryConfigurationChange)
	}
	if err := s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		stored.PendingRebootParameters = nil
	}); err != nil {
		return nil, err
	}

	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
		"DB instance restarted.", EventCategoryAvailability)

	// Returned as rebooting rather than available: the engine has to come back
	// and report healthy before the reconciler calls it that.
	return &rds.RebootDBInstanceOutput{DBInstance: s.projectDBInstance(rec)}, nil
}

// Stops the engine and then the VM. The data volume, the customer ENI and its
// IP, and the DNS record are all retained, so a start comes back on the same
// datadir at the same address.
func (s *Service) StopDBInstance(ctx context.Context, input *rds.StopDBInstanceInput, accountID string) (*rds.StopDBInstanceOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	// A pre-stop snapshot is already in progress; accepting it here would report a snapshot
	// the customer would then not find.
	if aws.StringValue(input.DBSnapshotIdentifier) != "" {
		return nil, unimplemented("DBSnapshotIdentifier", "taking a snapshot as part of a stop is not implemented yet")
	}

	rec, kv, err := s.beginTransition(ctx, accountID, aws.StringValue(input.DBInstanceIdentifier),
		StatusStopping, StatusAvailable, StatusFailed)
	if err != nil {
		return nil, err
	}

	s.stopEngineOrRecordFallback(ctx, accountID, rec, "stopping")

	if err := s.stopInstanceVM(ctx, accountID, rec.InstanceID); err != nil {
		return nil, s.failTransition(ctx, kv, accountID, rec,
			fmt.Sprintf("the DB instance could not be stopped: %v", err))
	}

	stopped, err := s.completeTransition(ctx, kv, rec.DBInstanceIdentifier, StatusStopped)
	if err != nil {
		return nil, err
	}
	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
		"DB instance stopped.", EventCategoryAvailability)

	return &rds.StopDBInstanceOutput{DBInstance: s.projectDBInstance(stopped)}, nil
}

// Brings a stopped instance back on its retained data volume and customer ENI.
// The engine replays WAL when the graceful stop did not complete.
func (s *Service) StartDBInstance(ctx context.Context, input *rds.StartDBInstanceInput, accountID string) (*rds.StartDBInstanceOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}

	rec, kv, err := s.beginTransition(ctx, accountID, aws.StringValue(input.DBInstanceIdentifier),
		StatusStarting, StatusStopped, StatusFailed)
	if err != nil {
		return nil, err
	}

	if err := s.startInstanceVM(ctx, rec.InstanceID); err != nil {
		return nil, s.failTransition(ctx, kv, accountID, rec,
			fmt.Sprintf("the DB instance could not be started: %v", err))
	}
	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
		"DB instance starting.", EventCategoryAvailability)

	// Returned as starting: the reconciler flips it to available on the first
	// healthy heartbeat from the restarted agent.
	return &rds.StartDBInstanceOutput{DBInstance: s.projectDBInstance(rec)}, nil
}

// Stops the VM and returns once the fleet reports it stopped. Shared with the
// storage grow, which needs the volume detached before ModifyVolume will touch
// it, and the detach is the last thing the stop does.
func (s *Service) stopInstanceVM(ctx context.Context, accountID, instanceID string) error {
	err := s.deps.Instances.StopInstance(ctx, instanceID)
	// A command no node answered usually means the VM is already down, which is
	// where the stop was going anyway; a command a node accepted says nothing
	// about the VM yet. Both are settled by the same fleet-wide wait.
	if err != nil && !errors.Is(err, ErrInstanceNotOnNode) {
		return err
	}
	return s.waitForVMStopped(ctx, accountID, instanceID)
}

// Polls the fleet until the VM reports stopped or the budget expires. The node
// accepts a stop command in milliseconds but takes seconds to drain and detach
// the data volume, and a caller that acts on the acceptance alone — the storage
// grow above all — acts on a volume the guest still holds.
func (s *Service) waitForVMStopped(ctx context.Context, accountID, instanceID string) error {
	timeout := s.vmStopTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Scaled to the budget so a short one is still polled rather than read once.
	interval := min(vmStopPollInterval, timeout/4)
	for {
		err := s.confirmVMStopped(ctx, accountID, instanceID)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the DB VM %s did not stop within %s: %w", instanceID, timeout, err)
		case <-time.After(interval):
			slog.InfoContext(ctx, "rds: waiting for the DB VM to stop",
				"instanceId", instanceID, "err", err)
		}
	}
}

// The owning node's in-memory command path first, since it is the one that can
// re-attach the volumes it already has mapped. A node restart drops that, and
// the stopped-instance path relaunches from the persisted record instead.
func (s *Service) startInstanceVM(ctx context.Context, instanceID string) error {
	err := s.deps.Instances.StartInstance(ctx, instanceID)
	if err == nil || !errors.Is(err, ErrInstanceNotOnNode) {
		return err
	}
	slog.InfoContext(ctx, "rds: no node holds the DB VM; relaunching it from its stopped-instance record",
		"instanceId", instanceID)
	return s.deps.Instances.StartStoppedInstance(ctx, instanceID)
}

// One reading of the VM's state across the fleet. Neither an accepted stop nor
// an unanswered one says anything about the VM itself — a partitioned or
// restarting node looks identical to one that never held it — so this view has
// to agree before the record says stopped.
//
// A nil resolver disables the check, matching the reconciler's health path.
func (s *Service) confirmVMStopped(ctx context.Context, accountID, instanceID string) error {
	if s.deps.InstanceState == nil {
		return nil
	}
	state, err := s.deps.InstanceState.InstanceState(ctx, instanceID, accountID)
	if err != nil {
		return fmt.Errorf("the DB VM's state could not be resolved: %w", err)
	}
	// Settled down, not merely no longer running: the node reports stopping
	// within a millisecond of accepting the command and spends seconds after
	// that draining the guest and detaching the data volume.
	switch state {
	case instanceStateStopped, instanceStateTerminated:
		return nil
	case "":
		// A VM the fleet cannot find anywhere is down and holds nothing.
		return nil
	}
	return fmt.Errorf("the DB VM is in state %q, not stopped", state)
}

// Asks the engine to shut down cleanly, and records an event rather than
// failing the call when it cannot. Refusing to stop the VM over it would leave
// the customer unable to stop an instance whose agent is wedged.
func (s *Service) stopEngineOrRecordFallback(ctx context.Context, accountID string, rec *DBInstanceRecord, operation string) {
	if err := s.stopEngine(ctx, accountID, rec.DBInstanceIdentifier); err != nil {
		slog.WarnContext(ctx, "rds: graceful engine stop failed; continuing",
			"dbInstance", rec.DBInstanceIdentifier, "operation", operation, "err", err)
		s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
			uncleanStopMessage(ctx, rec.Engine, operation),
			EventCategoryNotification, EventCategoryAvailability)
	}
}

// The half of the warning that holds for every engine. What the next start then
// recovers does not: PostgreSQL replays every table from its write-ahead log,
// MariaDB only its InnoDB ones.
func uncleanStopMessage(ctx context.Context, engineName, operation string) string {
	warning := fmt.Sprintf("The database engine could not be shut down cleanly before %s.", operation)
	engine, err := LookupEngine(engineName)
	if err != nil {
		// The VM is going down either way, so the customer still gets the half of
		// the warning that does not depend on knowing the engine.
		slog.ErrorContext(ctx, "rds: the DB instance names an engine this build does not offer",
			"engine", engineName, "err", err)
		return warning
	}
	return warning + " " + engine.uncleanStopNote
}

// Moves the instance into a transitional state under CAS and returns the record
// as written. from lists the statuses the operation is legal from, so a stop of
// an already-stopping instance is rejected as a state error rather than racing
// the stop already running.
func (s *Service) beginTransition(ctx context.Context, accountID, id string, to Status, from ...Status) (*DBInstanceRecord, jetstream.KeyValue, error) {
	if id == "" {
		return nil, nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBInstanceIdentifier is required")
	}
	if s.deps.Instances == nil {
		return nil, nil, errors.New("rds: no instance command path configured")
	}
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, nil, err
	}
	rec, rev, err := s.getDBInstance(ctx, kv, id)
	if err != nil {
		return nil, nil, err
	}
	if !slices.Contains(from, rec.Status) {
		return nil, nil, awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
			"DB instance %s is %s; the operation requires it to be %s", id, rec.Status, joinStatuses(from))
	}
	if rec.InstanceID == "" {
		return nil, nil, awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
			"DB instance %s has no VM to act on", id)
	}

	now := time.Now().UTC()
	rec.Status = to
	rec.FailureReason = ""
	// An explicit lifecycle op supersedes whatever the classifier had observed,
	// so a clock left over from an earlier outage cannot shorten the grace on a
	// fault the customer has just acted on.
	rec.UnhealthySince = nil
	// No lifecycle operation is an initial create. Revoke before issuing a
	// command that can boot the guest, so an unrecognized existing volume can
	// never inherit the create-time format grant.
	rec.FormatAuthorized = false
	rec.TransitionStartedAt = &now
	rec.UpdatedAt = now
	if err := updateJSON(ctx, kv, DBInstanceKey(id), rev, rec); err != nil {
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return nil, nil, awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState,
				"DB instance %s changed state concurrently; retry the operation", id)
		}
		return nil, nil, err
	}
	return rec, kv, nil
}

// Lands the instance in its post-transition state and clears the transition
// stamp, returning the record as written.
func (s *Service) completeTransition(ctx context.Context, kv jetstream.KeyValue, id string, to Status) (*DBInstanceRecord, error) {
	var stored *DBInstanceRecord
	err := s.updateInstance(ctx, kv, id, func(rec *DBInstanceRecord) {
		rec.Status = to
		rec.TransitionStartedAt = nil
		stored = rec
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// Records why an operation failed and returns the error the caller surfaces, so
// a failed transition cannot be left looking like one still in progress.
func (s *Service) failTransition(ctx context.Context, kv jetstream.KeyValue, accountID string, rec *DBInstanceRecord, reason string) error {
	if err := s.updateInstance(ctx, kv, rec.DBInstanceIdentifier, func(stored *DBInstanceRecord) {
		stored.Status = StatusFailed
		stored.FailureReason = reason
		stored.TransitionStartedAt = nil
	}); err != nil {
		slog.ErrorContext(ctx, "rds: marking a failed lifecycle transition failed",
			"dbInstance", rec.DBInstanceIdentifier, "err", err)
	}
	s.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier, reason, EventCategoryFailure)
	return awserrors.Errorf(awserrors.ErrorDBInstanceInvalidState, "%s", reason)
}

// A read-modify-write under CAS, replayed on contention so a concurrent agent
// heartbeat cannot make a lifecycle write disappear.
func (s *Service) updateInstance(ctx context.Context, kv jetstream.KeyValue, id string, mutate func(*DBInstanceRecord)) error {
	_, err := s.updateInstanceIf(ctx, kv, id, func(rec *DBInstanceRecord) bool {
		mutate(rec)
		return true
	})
	return err
}

// updateInstance for a caller that can only decide whether to write once it has
// seen the record it would overwrite — a lease claim, whose whole question is
// whether someone else holds it. Reports whether the write happened; a mutate
// that returns false leaves the record untouched and is not an error.
func (s *Service) updateInstanceIf(ctx context.Context, kv jetstream.KeyValue, id string, mutate func(*DBInstanceRecord) bool) (bool, error) {
	key := DBInstanceKey(id)
	for range tagWriteAttempts {
		rec, rev, err := s.getDBInstance(ctx, kv, id)
		if err != nil {
			return false, err
		}
		if !mutate(rec) {
			return false, nil
		}
		rec.UpdatedAt = time.Now().UTC()

		err = updateJSON(ctx, kv, key, rev, rec)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			return false, err
		}
	}
	return false, fmt.Errorf("rds: update of %s contended after %d attempts", key, tagWriteAttempts)
}

func joinStatuses(statuses []Status) string {
	out := ""
	for i, status := range statuses {
		switch {
		case i == 0:
			out = string(status)
		case i == len(statuses)-1:
			out += " or " + string(status)
		default:
			out += ", " + string(status)
		}
	}
	return out
}

// Only the node running the VM subscribes ec2.cmd.{instanceID}, so a power
// command routes itself; the fallback subject relaunches a VM no node holds.
type natsInstanceCommander struct {
	nc      *nats.Conn
	timeout time.Duration
}

var _ instanceCommander = (*natsInstanceCommander)(nil)

func NewNATSInstanceCommander(nc *nats.Conn) instanceCommander {
	return &natsInstanceCommander{nc: nc, timeout: instanceCommandTimeout}
}

func (c *natsInstanceCommander) StopInstance(ctx context.Context, instanceID string) error {
	return c.send(ctx, instanceID, types.EC2CommandAttributes{StopInstance: true})
}

func (c *natsInstanceCommander) StartInstance(ctx context.Context, instanceID string) error {
	return c.send(ctx, instanceID, types.EC2CommandAttributes{StartInstance: true})
}

func (c *natsInstanceCommander) RebootInstance(ctx context.Context, instanceID string) error {
	return c.send(ctx, instanceID, types.EC2CommandAttributes{RebootInstance: true})
}

// The VM runs in the system account, so every command is issued there — the
// ownership check on the far side compares against that same account.
func (c *natsInstanceCommander) send(ctx context.Context, instanceID string, attrs types.EC2CommandAttributes) error {
	cmd := types.EC2InstanceCommand{ID: instanceID, Attributes: attrs}
	_, err := utils.NATSRequest[struct{}](ctx, c.nc, "ec2.cmd."+instanceID, cmd, c.timeout, utils.GlobalAccountID)
	if err == nil {
		return nil
	}
	// No subscriber, or a subscriber that does not hold this VM: both mean the
	// command has to go through the stopped-instance path instead.
	if errors.Is(err, nats.ErrNoResponders) ||
		strings.Contains(err.Error(), awserrors.ErrorInvalidInstanceIDNotFound) {
		return fmt.Errorf("%w: %s", ErrInstanceNotOnNode, instanceID)
	}
	return err
}

func (c *natsInstanceCommander) StartStoppedInstance(ctx context.Context, instanceID string) error {
	_, err := utils.NATSRequest[handlers_ec2_instance.StartStoppedInstanceOutput](ctx, c.nc, "ec2.start",
		handlers_ec2_instance.StartStoppedInstanceInput{InstanceID: instanceID}, c.timeout, utils.GlobalAccountID)
	return err
}
