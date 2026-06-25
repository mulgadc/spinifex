package main

import (
	"fmt"

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

// registrar registers the host as a container instance through the gateway.
type registrar struct {
	cp controlPlane
	id identity
}

func newRegistrar(cp controlPlane, id identity) *registrar {
	return &registrar{cp: cp, id: id}
}

// Register calls the gateway's RegisterContainerInstance. The scheduler records
// (or refreshes) the container instance on receipt.
func (r *registrar) Register() error {
	if err := r.cp.Register(r.id); err != nil {
		return fmt.Errorf("register instance %s: %w", r.id.InstanceID, err)
	}
	return nil
}
