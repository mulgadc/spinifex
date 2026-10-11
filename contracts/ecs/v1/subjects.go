// Package ecsv1 defines version 1 of the ECS scheduler-agent NATS subjects and
// wire payloads. The ecs-agent and the spinifex scheduler both consume this
// shared vocabulary; the README's route table records which routes are live.
//
// Cluster identity is "<accountID>.<clusterName>" (matching the KV layout), not a
// UUID; these builders are the one place that knows the subject shape.
package ecsv1

import "fmt"

// Prefix returns "ecs.bus.<accountID>.<clusterName>".
func Prefix(accountID, clusterName string) string {
	return fmt.Sprintf("ecs.bus.%s.%s", accountID, clusterName)
}

// RegisterSubject is the per-instance registration subject (agent → scheduler).
// The scheduler subscribes to it; nothing publishes on it today.
func RegisterSubject(accountID, clusterName, instanceID string) string {
	return fmt.Sprintf("%s.instance-register.%s", Prefix(accountID, clusterName), instanceID)
}

// HeartbeatSubject is the per-instance liveness subject (agent → scheduler).
// The scheduler subscribes to it; nothing publishes on it today.
func HeartbeatSubject(accountID, clusterName, instanceID string) string {
	return fmt.Sprintf("%s.instance-heartbeat.%s", Prefix(accountID, clusterName), instanceID)
}

// AssignSubject is the per-instance task-assignment subject (scheduler → agent).
// No production code subscribes to it today; assignments are delivered through
// the AWS-API gateway poll path instead.
func AssignSubject(accountID, clusterName, instanceID string) string {
	return fmt.Sprintf("%s.assign.%s", Prefix(accountID, clusterName), instanceID)
}

// TaskStateSubject is the per-task state-report subject (agent → scheduler).
// The scheduler subscribes to it; nothing publishes on it today.
func TaskStateSubject(accountID, clusterName, taskID string) string {
	return fmt.Sprintf("%s.task-state.%s", Prefix(accountID, clusterName), taskID)
}

// ServiceReconcileSubject wakes the scheduler's service reconcile. It carries no
// payload — it says only that something the reconcile reads has changed — and it
// crosses nodes, because the API worker that saw the change is rarely the leader.
func ServiceReconcileSubject(accountID, clusterName string) string {
	return fmt.Sprintf("%s.service-reconcile", Prefix(accountID, clusterName))
}
