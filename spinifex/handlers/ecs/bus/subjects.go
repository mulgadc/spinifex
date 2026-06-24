// Package bus defines the Layer-2 internal scheduler↔agent NATS subjects and
// wire payloads for the ECS control plane (ecs-v1.md Q14). Both the ecs-agent
// (publisher) and the spinifex scheduler (subscriber, Sprint 4e) import it so the
// subject shape and message schema have a single source of truth.
//
// Cluster identity is "<accountID>.<clusterName>" (matching the KV layout), not a
// UUID. The Phase 5 per-AZ NATS cutover inserts "{azID}" after "ecs." — keep
// these builders the one place that knows the shape so that change stays local.
package bus

import "fmt"

// Prefix returns "ecs.bus.<accountID>.<clusterName>".
func Prefix(accountID, clusterName string) string {
	return fmt.Sprintf("ecs.bus.%s.%s", accountID, clusterName)
}

// RegisterSubject is published by the agent at boot (agent → scheduler).
func RegisterSubject(accountID, clusterName, instanceID string) string {
	return fmt.Sprintf("%s.instance-register.%s", Prefix(accountID, clusterName), instanceID)
}

// HeartbeatSubject is published by the agent every 30s (agent → scheduler).
func HeartbeatSubject(accountID, clusterName, instanceID string) string {
	return fmt.Sprintf("%s.instance-heartbeat.%s", Prefix(accountID, clusterName), instanceID)
}

// AssignSubject is the per-instance task-assignment subject (scheduler → agent).
// The agent does not subscribe to it until Sprint 4e.
func AssignSubject(accountID, clusterName, instanceID string) string {
	return fmt.Sprintf("%s.assign.%s", Prefix(accountID, clusterName), instanceID)
}

// TaskStateSubject is the per-task state-report subject (agent → scheduler),
// published from Sprint 4e.
func TaskStateSubject(accountID, clusterName, taskID string) string {
	return fmt.Sprintf("%s.task-state.%s", Prefix(accountID, clusterName), taskID)
}

// ServiceReconcileSubject is the scheduler-internal reconcile tick.
func ServiceReconcileSubject(accountID, clusterName string) string {
	return fmt.Sprintf("%s.service-reconcile", Prefix(accountID, clusterName))
}
