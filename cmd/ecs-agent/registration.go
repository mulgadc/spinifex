package main

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
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
	InstanceType string
	Hostname     string
	Capacity     bus.InstanceCapacity
	AgentVersion string
}

// instanceAttributes builds the built-in container-instance attributes from the
// host's identity. Only attributes whose value was actually discovered are
// reported, so an empty one is absent rather than present and blank.
func instanceAttributes(id identity) []*ecs.Attribute {
	var attrs []*ecs.Attribute
	add := func(name, value string) {
		if value == "" {
			return
		}
		attrs = append(attrs, &ecs.Attribute{Name: aws.String(name), Value: aws.String(value)})
	}
	add(handlers_ecs.AttrInstanceType, id.InstanceType)
	add(handlers_ecs.AttrAvailabilityZone, id.AZ)
	return attrs
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
