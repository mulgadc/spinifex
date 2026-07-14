package handlers_ecs

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/nats-io/nats.go"
)

// PollAssignmentsInput is the agent's poll request. ContainerInstance names the
// instance whose inbox to drain; AckTaskIDs are assignments the agent accepted on
// a prior poll and that may now be removed (at-least-once: an unacked assign is
// re-delivered after an agent crash). This is an internal agent↔gateway shape,
// not an AWS SDK action.
type PollAssignmentsInput struct {
	Cluster           string   `json:"cluster"`
	ContainerInstance string   `json:"containerInstance"`
	AckTaskIDs        []string `json:"ackTaskIds,omitempty"`
	// AckStopIDs are stop directives the agent reaped on a prior poll; they may now
	// be removed. Optional — stop delivery is idempotent and the STOPPED transition
	// deletes the entry, so a dropped ack only re-delivers a no-op reap.
	AckStopIDs []string `json:"ackStopIds,omitempty"`
}

// PollAssignmentsOutput carries the instance's currently pending assignments and
// stop directives.
type PollAssignmentsOutput struct {
	Assignments []bus.Assign        `json:"assignments,omitempty"`
	Stops       []bus.StopDirective `json:"stops,omitempty"`
}

// PollAssignments acks any completed assignments (deletes their inbox keys) then
// returns the instance's still-pending assignments. The scheduler is the only
// writer of the inbox; the agent is the only reader/acker. Account is
// authoritative from accountID (SigV4 caller), so an instance cannot drain
// another account's inbox.
func (s *Service) PollAssignments(_ context.Context, input *PollAssignmentsInput, accountID string) (*PollAssignmentsOutput, error) {
	cluster := clusterShortName(input.Cluster)
	instanceID := containerInstanceShortID(input.ContainerInstance)
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}

	for _, taskID := range input.AckTaskIDs {
		if taskID == "" {
			continue
		}
		_ = kv.Delete(AssignmentKey(cluster, instanceID, taskShortID(taskID)))
	}
	for _, taskID := range input.AckStopIDs {
		if taskID == "" {
			continue
		}
		_ = kv.Delete(StopKey(cluster, instanceID, taskShortID(taskID)))
	}

	keys, err := keysWithPrefix(kv, AssignmentsPrefix(cluster, instanceID))
	if err != nil {
		return nil, err
	}
	out := &PollAssignmentsOutput{}
	for _, key := range keys {
		var as bus.Assign
		found, err := getJSON(kv, key, &as)
		if err != nil {
			return nil, err
		}
		if found {
			out.Assignments = append(out.Assignments, as)
		}
	}

	stopKeys, err := keysWithPrefix(kv, StopsPrefix(cluster, instanceID))
	if err != nil {
		return nil, err
	}
	for _, key := range stopKeys {
		var sd bus.StopDirective
		found, err := getJSON(kv, key, &sd)
		if err != nil {
			return nil, err
		}
		if found {
			out.Stops = append(out.Stops, sd)
		}
	}
	return out, nil
}

// reclaimAssignInbox removes a task's inbox entry on the STOPPED transition so a
// stopped task is never re-delivered to the agent. Best-effort.
func (s *Service) reclaimAssignInbox(kv nats.KeyValue, cluster, instanceID, taskID string) {
	if instanceID == "" || taskID == "" {
		return
	}
	_ = kv.Delete(AssignmentKey(cluster, instanceID, taskID))
}

// reclaimStopInbox removes a task's stop-inbox entry on the STOPPED transition so
// a reaped task is never re-delivered as a stop. Best-effort.
func (s *Service) reclaimStopInbox(kv nats.KeyValue, cluster, instanceID, taskID string) {
	if instanceID == "" || taskID == "" {
		return
	}
	_ = kv.Delete(StopKey(cluster, instanceID, taskID))
}
