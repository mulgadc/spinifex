package gateway

import (
	"maps"
	"slices"

	gateway_ecs "github.com/mulgadc/spinifex/spinifex/gateway/ecs"
	gateway_rds "github.com/mulgadc/spinifex/spinifex/gateway/rds"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// ServiceOperationInventory describes the authoritative dispatch state for an
// AWS-compatible service. Registered operations minus Stubbed and Unsupported
// are implemented handlers.
type ServiceOperationInventory struct {
	Registered  []string
	Stubbed     []string
	Unsupported []string
}

// AWSOperationInventory returns a fresh, stable snapshot of the gateway's
// legacy dispatch tables merged with registered, the declared inventory of each
// registered service. S3 is intentionally absent: Spinifex does not dispatch S3
// operations and delegates that REST surface to Predastore.
func AWSOperationInventory(registered map[string]dispatch.Inventory) map[string]ServiceOperationInventory {
	inventory := map[string]ServiceOperationInventory{
		"ec2": {
			Registered: mapKeys(ec2Actions),
		},
		"ecs": {
			Registered: mapKeys(gateway_ecs.Actions),
			Stubbed:    gateway_ecs.StubbedActionNames(),
		},
		"eks": {
			Registered: eksActionNames(),
		},
		"elasticloadbalancingv2": {
			Registered: mapKeys(elbv2Actions),
		},
		"iam": {
			Registered: mapKeys(iamActions),
		},
		"rds": {
			Registered:  gateway_rds.ActionNames(),
			Unsupported: gateway_rds.UnsupportedActionNames(),
		},
		"sts": {
			Registered: mapKeys(stsActions),
		},
	}
	for name, inv := range registered {
		inventory[name] = ServiceOperationInventory{
			Registered:  slices.Clone(inv.Registered),
			Stubbed:     slices.Clone(inv.Stubbed),
			Unsupported: slices.Clone(inv.Unsupported),
		}
	}
	return inventory
}

func mapKeys[V any](values map[string]V) []string {
	return slices.Sorted(maps.Keys(values))
}
