package handlers_ecs

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/nats-io/nats.go/jetstream"
)

// Unexported hooks and in-package fixtures the external test package needs.
// Task status is reported by the agent, so these tests seed KV directly
// rather than drive a live agent through RunTask.
const TestAccountID = testAccountID

var (
	NewTestService   = newTestService
	RegisterInstance = registerInstance
	PutJSON          = putJSON
	GetJSON          = getJSON
	ServiceTaskGroup = serviceTaskGroup
)

func (s *Service) Region() string { return s.region }

func (s *Service) Bucket(ctx context.Context, accountID string) (jetstream.KeyValue, error) {
	return s.bucket(ctx, accountID)
}

func (s *Service) UpsertInstance(ctx context.Context, kv jetstream.KeyValue, accountID, cluster, instanceID string, mutate func(*InstanceRecord)) (*InstanceRecord, error) {
	return s.upsertInstance(ctx, kv, accountID, cluster, instanceID, mutate)
}

func (s *Service) RecordTaskState(ctx context.Context, msg *bus.TaskState) error {
	return s.recordTaskState(ctx, msg)
}
