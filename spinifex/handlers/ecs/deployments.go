package handlers_ecs

import (
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/google/uuid"
)

// deploymentStartedByPrefix is the AWS StartedBy prefix a service stamps on its
// tasks; the suffix is the deployment ID, so the reconciler maps a task back to
// the deployment that launched it.
const deploymentStartedByPrefix = "ecs-svc/"

// deploymentSupersededReason is the stop reason for an old-deployment task drained
// during a rolling update.
const deploymentSupersededReason = "Superseded by new deployment"

func deploymentStartedBy(id string) string { return deploymentStartedByPrefix + id }

func deploymentIDFromStartedBy(startedBy string) string {
	id, _ := strings.CutPrefix(startedBy, deploymentStartedByPrefix)
	return id
}

// ceilPercent returns count*percent/100 rounded up — used for minimumHealthyPercent
// (AWS rounds the healthy floor up).
func ceilPercent(count, percent int) int {
	if count <= 0 {
		return 0
	}
	return (count*percent + 99) / 100
}

// applyDeploymentConfig sets the service's deploymentConfiguration, defaulting to
// the AWS REPLICA defaults (100% healthy / 200% max) when a field is omitted.
func applyDeploymentConfig(rec *ServiceRecord, dc *ecs.DeploymentConfiguration) {
	rec.MinimumHealthyPercent = defaultMinimumHealthyPercent
	rec.MaximumPercent = defaultMaximumPercent
	if dc == nil {
		return
	}
	if dc.MinimumHealthyPercent != nil {
		rec.MinimumHealthyPercent = int(*dc.MinimumHealthyPercent)
	}
	if dc.MaximumPercent != nil {
		rec.MaximumPercent = int(*dc.MaximumPercent)
	}
	if cb := dc.DeploymentCircuitBreaker; cb != nil {
		rec.CircuitBreakerEnable = aws.BoolValue(cb.Enable)
		rec.CircuitBreakerRollback = aws.BoolValue(cb.Rollback)
	}
}

// normalizeDeploymentConfig backfills the deploymentConfiguration defaults for a
// legacy service record written before rolling deployments existed.
func (r *ServiceRecord) normalizeDeploymentConfig() {
	if r.MinimumHealthyPercent == 0 {
		r.MinimumHealthyPercent = defaultMinimumHealthyPercent
	}
	if r.MaximumPercent == 0 {
		r.MaximumPercent = defaultMaximumPercent
	}
}

// newPrimaryDeployment builds an IN_PROGRESS PRIMARY deployment for a task def.
func newPrimaryDeployment(id string, td *TaskDefRecord, desired int) Deployment {
	now := time.Now().UTC()
	return Deployment{
		ID:              id,
		Status:          DeploymentStatusPrimary,
		TaskDefARN:      td.ARN,
		TaskDefFamily:   td.Family,
		TaskDefRevision: td.Revision,
		DesiredCount:    desired,
		RolloutState:    RolloutStateInProgress,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// ensurePrimaryDeployment synthesizes a PRIMARY deployment for a legacy service
// record that predates deployment tracking, keyed by its existing DeploymentID so
// its already-running tasks (StartedBy=ecs-svc/{DeploymentID}) map to it.
func (r *ServiceRecord) ensurePrimaryDeployment() {
	if r.primaryDeployment() != nil {
		return
	}
	r.Deployments = append(r.Deployments, Deployment{
		ID:              r.DeploymentID,
		Status:          DeploymentStatusPrimary,
		TaskDefARN:      r.TaskDefARN,
		TaskDefFamily:   r.TaskDefFamily,
		TaskDefRevision: r.TaskDefRevision,
		DesiredCount:    r.DesiredCount,
		RolloutState:    RolloutStateInProgress,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       time.Now().UTC(),
	})
	if r.LastGoodTaskDefARN == "" {
		r.LastGoodTaskDefARN = r.TaskDefARN
	}
}

// startDeployment demotes the current PRIMARY to ACTIVE (draining) and installs a
// new PRIMARY for td, mirroring the taskdef onto the service record.
func (r *ServiceRecord) startDeployment(td *TaskDefRecord) {
	r.demotePrimary()
	r.DeploymentID = uuid.NewString()
	r.TaskDefFamily = td.Family
	r.TaskDefRevision = td.Revision
	r.TaskDefARN = td.ARN
	r.NetworkMode = resolveNetworkMode(td)
	r.Deployments = append(r.Deployments, newPrimaryDeployment(r.DeploymentID, td, r.DesiredCount))
}

func (r *ServiceRecord) demotePrimary() {
	for i := range r.Deployments {
		if r.Deployments[i].Status == DeploymentStatusPrimary {
			r.Deployments[i].Status = DeploymentStatusActive
			r.Deployments[i].UpdatedAt = time.Now().UTC()
		}
	}
}

// pruneCompletedDeployments drops the drained (non-PRIMARY) deployments once the
// rollout has completed, leaving a single PRIMARY at steady state.
func (r *ServiceRecord) pruneCompletedDeployments() {
	r.Deployments = slices.DeleteFunc(r.Deployments, func(d Deployment) bool {
		return d.Status != DeploymentStatusPrimary
	})
}

// tallyDeployments zeroes then recomputes each deployment's RUNNING/PENDING counts
// from the service's live tasks and returns the service-wide totals.
func tallyDeployments(svc *ServiceRecord, tasks []TaskRecord) (running, pending int) {
	byDep := make(map[string]*Deployment, len(svc.Deployments))
	for i := range svc.Deployments {
		svc.Deployments[i].RunningCount = 0
		svc.Deployments[i].PendingCount = 0
		byDep[svc.Deployments[i].ID] = &svc.Deployments[i]
	}
	for i := range tasks {
		d := byDep[deploymentIDFromStartedBy(tasks[i].StartedBy)]
		switch tasks[i].LastStatus {
		case TaskStatusRunning:
			running++
			if d != nil {
				d.RunningCount++
			}
		case TaskStatusPending:
			pending++
			if d != nil {
				d.PendingCount++
			}
		}
	}
	return running, pending
}

// updateRolloutState advances the PRIMARY deployment's rollout state. It completes
// (and prunes drained deployments, recording the taskdef as last-good) once the
// primary is fully up and no old tasks remain; otherwise it stays IN_PROGRESS. A
// FAILED deployment is left as-is (the circuit breaker owns that transition).
func updateRolloutState(svc *ServiceRecord, primary *Deployment, desired int) {
	primary.DesiredCount = desired
	if primary.RolloutState == RolloutStateFailed {
		return
	}
	remainingOld := 0
	for i := range svc.Deployments {
		if svc.Deployments[i].Status != DeploymentStatusPrimary {
			remainingOld += svc.Deployments[i].RunningCount + svc.Deployments[i].PendingCount
		}
	}
	if primary.RunningCount >= desired && remainingOld == 0 {
		if primary.RolloutState != RolloutStateCompleted {
			primary.RolloutState = RolloutStateCompleted
			primary.RolloutReason = "ECS deployment completed."
			primary.UpdatedAt = time.Now().UTC()
			slog.Info("ECS deployment completed", "service", svc.Name, "deployment", primary.ID, "taskDef", primary.TaskDefARN)
		}
		svc.LastGoodTaskDefARN = primary.TaskDefARN
		svc.pruneCompletedDeployments()
		return
	}
	primary.RolloutState = RolloutStateInProgress
}

// tripCircuitBreaker fails a PRIMARY deployment whose task launches have failed
// past the threshold when the breaker is enabled, optionally rolling back to the
// last-good task definition. Returns true when it acted so the caller can persist
// and defer further launches to the next tick.
func tripCircuitBreaker(svc *ServiceRecord, primary *Deployment) bool {
	if !svc.CircuitBreakerEnable || primary.RolloutState == RolloutStateFailed {
		return false
	}
	if primary.FailedTasks < circuitBreakerFailureThreshold {
		return false
	}
	primary.RolloutState = RolloutStateFailed
	primary.RolloutReason = "ECS deployment circuit breaker: tasks failed to start."
	primary.UpdatedAt = time.Now().UTC()
	slog.Warn("ECS deployment circuit breaker tripped", "service", svc.Name,
		"deployment", primary.ID, "failedTasks", primary.FailedTasks)

	if !svc.CircuitBreakerRollback || svc.LastGoodTaskDefARN == "" || svc.LastGoodTaskDefARN == primary.TaskDefARN {
		return true
	}
	svc.rollbackToLastGood()
	return true
}

// rollbackToLastGood demotes the failed PRIMARY and starts a fresh PRIMARY from
// the last-good task definition ARN.
func (r *ServiceRecord) rollbackToLastGood() {
	family, rev := parseTaskDefRef(r.LastGoodTaskDefARN)
	r.demotePrimary()
	now := time.Now().UTC()
	r.DeploymentID = uuid.NewString()
	r.Deployments = append(r.Deployments, Deployment{
		ID:              r.DeploymentID,
		Status:          DeploymentStatusPrimary,
		TaskDefARN:      r.LastGoodTaskDefARN,
		TaskDefFamily:   family,
		TaskDefRevision: rev,
		DesiredCount:    r.DesiredCount,
		RolloutState:    RolloutStateInProgress,
		RolloutReason:   "ECS deployment rolled back to last good task definition.",
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	r.TaskDefARN = r.LastGoodTaskDefARN
	r.TaskDefFamily = family
	r.TaskDefRevision = rev
}
