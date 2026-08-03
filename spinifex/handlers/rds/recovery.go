package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// How long an instance may be observed dark before it is called failed, unless
// the config overrides it. One heartbeat interval, so at least two reconciler
// passes have to agree before a live database can be reported down.
const defaultFailureGrace = HeartbeatInterval

// What the classifier decides about one instance. Reduced from the EKS
// classifier's live/restartable/lost to healthy/failed, because v1.0 RDS reports
// failure rather than repairing it (D13).
type healthVerdict int

const (
	// Some other owner is acting on this instance, or it is legitimately down.
	verdictSkip healthVerdict = iota
	// The engine is serving from the record's current VM.
	verdictHealthy
	// Neither the VM nor the agent is answering.
	verdictFailed
	// Exactly one half is wrong, which is not evidence of a dead database. The
	// failure clock is neither started nor reset.
	verdictDegraded
)

// One pass's view of an instance. The classifier is a pure function over this so
// the (VM state, heartbeat age, RDS status) matrix is testable without a cluster.
type healthObservation struct {
	status Status
	// The record's current VM is reporting a healthy engine. A beat from a
	// superseded VM does not count.
	engineHealthy bool
	// That beat arrived inside the staleness bound.
	heartbeatFresh bool
	// The record's VM, as the fleet sees it.
	vmRunning bool
	// The beat the verdict was formed against; zero when the agent has never
	// reported. Carried for the recorded reason, not for the decision.
	lastSeen time.Time
}

// Detection is "VM not running" AND "heartbeat stale", never either alone: a
// stale beat under a live VM is an agent or network fault, not a dead database,
// and a fresh beat from a stopped VM is a stale lookup rather than a failure.
func classifyHealth(obs healthObservation) healthVerdict {
	// Every other state is owned by something already acting on the VM, and
	// stopped is legitimate rather than a failure. This is the interlock that
	// keeps the classifier from fighting rds-5a/rds-5b over the same VM.
	if obs.status != StatusAvailable && obs.status != StatusFailed {
		return verdictSkip
	}
	switch {
	case obs.engineHealthy && obs.heartbeatFresh && obs.vmRunning:
		return verdictHealthy
	case !obs.vmRunning && !obs.heartbeatFresh:
		return verdictFailed
	default:
		return verdictDegraded
	}
}

// The failure classifier, run once per pass for every settled instance. It
// drives available → failed on a dark one, and failed → available on one whose
// engine reports healthy again — the recovery the AMI's in-guest
// Restart=on-failure and EC2's VM auto-restart provide underneath. v1.0 does no
// repair of its own.
func (r *Reconciler) reconcileHealth(ctx context.Context, kv jetstream.KeyValue, rev uint64, accountID string, rec *DBInstanceRecord) error {
	obs := r.observeAgent(accountID, rec)

	// The VM lookup is a fleet-wide describe fan-out, so it is issued only where
	// its answer could change something. A healthy instance with nothing recorded
	// against it is the overwhelmingly common case and the record alone answers it.
	if obs.engineHealthy && obs.heartbeatFresh && rec.Status == StatusAvailable && rec.UnhealthySince == nil {
		return nil
	}
	running, err := r.vmRunning(ctx, accountID, rec)
	if err != nil {
		return err
	}
	obs.vmRunning = running

	switch classifyHealth(obs) {
	case verdictHealthy:
		return r.clearFailure(ctx, kv, rev, accountID, rec)
	case verdictFailed:
		return r.recordFailure(ctx, kv, rev, accountID, rec, obs)
	default:
		return nil
	}
}

// A healthy heartbeat is the only thing that resets the failure clock. VM state
// alone must not: a VM that boots and immediately wedges would otherwise reset
// it every pass and mask a persistent fault indefinitely.
func (r *Reconciler) clearFailure(ctx context.Context, kv jetstream.KeyValue, rev uint64, accountID string, rec *DBInstanceRecord) error {
	if rec.Status == StatusFailed {
		rec.UnhealthySince = nil
		if err := r.transition(ctx, kv, rev, rec, StatusAvailable, ""); err != nil {
			return err
		}
		r.svc.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
			"DB instance recovered: the database engine is reporting healthy again.",
			EventCategoryRecovery, EventCategoryAvailability)
		return nil
	}
	if rec.UnhealthySince == nil {
		return nil
	}
	rec.UnhealthySince = nil
	return r.persistFailureClock(ctx, kv, rev, rec)
}

// Starts the failure clock on the first dark pass and fails the instance once
// the grace window has elapsed. Both writes carry the clock, so the timestamp a
// later leader measures against is the one the original observation stamped.
func (r *Reconciler) recordFailure(ctx context.Context, kv jetstream.KeyValue, rev uint64, accountID string, rec *DBInstanceRecord, obs healthObservation) error {
	// Terminal for the control plane in v1.0: nothing retries, so re-recording a
	// failure that is already recorded would only churn the record.
	if rec.Status == StatusFailed {
		return nil
	}
	if rec.UnhealthySince == nil {
		since := time.Now().UTC()
		rec.UnhealthySince = &since
	}
	if time.Since(*rec.UnhealthySince) < r.svc.failureGrace() {
		// The clock is the whole write: the instance stays available until the
		// window has passed and a later pass agrees it is still dark.
		return r.persistFailureClock(ctx, kv, rev, rec)
	}

	reason := failureReason(obs)
	if err := r.transition(ctx, kv, rev, rec, StatusFailed, reason); err != nil {
		return err
	}
	r.svc.RecordEvent(ctx, accountID, EventSourceTypeDBInstance, rec.DBInstanceIdentifier,
		fmt.Sprintf("DB instance failed: %s. Recovery is operator-driven.", reason),
		EventCategoryFailure, EventCategoryAvailability)
	return nil
}

// Writes the clock without a status change. A lost revision race is dropped
// rather than retried: the record moved under this pass, and the next one
// re-reads and re-stamps against whatever it moved to.
func (r *Reconciler) persistFailureClock(ctx context.Context, kv jetstream.KeyValue, rev uint64, rec *DBInstanceRecord) error {
	rec.UpdatedAt = time.Now().UTC()
	err := updateJSON(ctx, kv, DBInstanceKey(rec.DBInstanceIdentifier), rev, rec)
	if errors.Is(err, jetstream.ErrKeyExists) {
		slog.DebugContext(ctx, "rds reconciler: failure clock lost a revision race; retrying next pass",
			"dbInstance", rec.DBInstanceIdentifier)
		return nil
	}
	return err
}

// The customer-facing explanation of a failure. It is what makes a failed
// instance something monitoring can alert on rather than an opaque state.
func failureReason(obs healthObservation) string {
	if obs.lastSeen.IsZero() {
		return "the DB instance is not running and its agent has never reported"
	}
	return fmt.Sprintf("the DB instance is not running and its agent has not reported for %s",
		time.Since(obs.lastSeen).Round(time.Second))
}

// The agent half of the observation, read without touching the fleet.
func (r *Reconciler) observeAgent(accountID string, rec *DBInstanceRecord) healthObservation {
	lastSeen, bound := r.lastHeartbeat(accountID, rec)
	return healthObservation{
		status: rec.Status,
		engineHealthy: rec.Agent.EngineHealth == EngineHealthHealthy && rec.InstanceID != "" &&
			rec.Agent.InstanceID == rec.InstanceID,
		heartbeatFresh: !lastSeen.IsZero() && time.Since(lastSeen) <= bound,
		lastSeen:       lastSeen,
	}
}

// The freshest beat this node can see, and the age at which it counts as stale.
// Beats are queue-group delivered, so most land on another node and reach the
// leader only through the record — which a healthy agent refreshes no more often
// than the persist floor.
func (r *Reconciler) lastHeartbeat(accountID string, rec *DBInstanceRecord) (time.Time, time.Duration) {
	var lastSeen time.Time
	var bound time.Duration
	// Judging a persisted beat by the raw stale window would report a steady
	// instance as stale for most of every persist cycle.
	if rec.Agent.LastSeen != nil {
		lastSeen, bound = *rec.Agent.LastSeen, HeartbeatStaleAfter+HeartbeatPersistFloor
	}
	// Not strictly after: a persisting beat writes the record from the same
	// instant it was noted in memory, and losing the tie would hand a beat this
	// node saw itself the slack meant for one it can only see through KV.
	if inMemory, ok := r.svc.LastSeen(accountID, rec.DBInstanceIdentifier); ok && !inMemory.Before(lastSeen) {
		lastSeen, bound = inMemory, HeartbeatStaleAfter
	}
	return lastSeen, bound
}

// Whether the record's VM is running, fanned out across the fleet. A nil
// resolver answers true, which is what leaves failure undetectable on a node
// with EC2 unwired instead of declaring it on the heartbeat alone (D13).
func (r *Reconciler) vmRunning(ctx context.Context, accountID string, rec *DBInstanceRecord) (bool, error) {
	if r.svc.deps.InstanceState == nil || rec.InstanceID == "" {
		return true, nil
	}
	state, err := r.svc.deps.InstanceState.InstanceState(ctx, rec.InstanceID, accountID)
	if err != nil {
		return false, fmt.Errorf("resolve VM state for %s: %w", rec.InstanceID, err)
	}
	return state == instanceStateRunning, nil
}
