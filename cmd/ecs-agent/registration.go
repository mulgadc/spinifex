package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// identity is the container instance's stable identity, assembled at boot from
// IMDS (account, instance, AZ) and config (cluster). It is the join key the
// scheduler uses to track this instance.
type identity struct {
	AccountID    string
	ClusterName  string
	InstanceID   string
	AZ           string
	Hostname     string
	Capacity     bus.InstanceCapacity
	AgentVersion string
}

// publisher is the subset of *nats.Conn the agent uses to emit bus messages.
// Narrowed to an interface so tests can drive it without a live server.
type publisher interface {
	Publish(subject string, data []byte) error
}

var _ publisher = (*nats.Conn)(nil)

// registrar emits the boot-time instance-register message.
type registrar struct {
	pub publisher
	id  identity
}

func newRegistrar(pub publisher, id identity) *registrar {
	return &registrar{pub: pub, id: id}
}

// Register publishes the RegisterInstance message on the cluster's register
// subject. The scheduler records the container instance on receipt (Sprint 4e).
func (r *registrar) Register() error {
	msg := bus.RegisterInstance{
		AccountID:    r.id.AccountID,
		ClusterName:  r.id.ClusterName,
		InstanceID:   r.id.InstanceID,
		AZ:           r.id.AZ,
		Hostname:     r.id.Hostname,
		Capacity:     r.id.Capacity,
		AgentVersion: r.id.AgentVersion,
		RegisteredAt: time.Now().UTC(),
	}
	data, err := json.Marshal(&msg)
	if err != nil {
		return fmt.Errorf("marshal register: %w", err)
	}
	subj := bus.RegisterSubject(r.id.AccountID, r.id.ClusterName, r.id.InstanceID)
	if err := r.pub.Publish(subj, data); err != nil {
		return fmt.Errorf("publish register %s: %w", subj, err)
	}
	return nil
}
