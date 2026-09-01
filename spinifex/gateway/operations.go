package gateway

import (
	"maps"
	"slices"

	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	gateway_ecs "github.com/mulgadc/spinifex/spinifex/gateway/ecs"
	gateway_rds "github.com/mulgadc/spinifex/spinifex/gateway/rds"
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
// operation dispatch tables. S3 is intentionally absent: Spinifex does not
// dispatch S3 operations and delegates that REST surface to Predastore.
func AWSOperationInventory() map[string]ServiceOperationInventory {
	ecrInline := mapKeys(ecrInlineActions)
	ecrRegistered := union(mapKeys(gateway_ecrapi.Actions), ecrInline)
	ecrStubbed := without(gateway_ecrapi.StubbedActionNames(), ecrInline)
	return map[string]ServiceOperationInventory{
		"acm": {
			Registered: mapKeys(acmActions),
		},
		"ec2": {
			Registered: mapKeys(ec2Actions),
		},
		"ecr": {
			Registered:  ecrRegistered,
			Stubbed:     ecrStubbed,
			Unsupported: gateway_ecrapi.UnsupportedActionNames(),
		},
		"ecs": {
			Registered: mapKeys(gateway_ecs.Actions),
			Stubbed:    gateway_ecs.StubbedActionNames(),
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
}

func mapKeys[V any](values map[string]V) []string {
	return slices.Sorted(maps.Keys(values))
}

func without(values, excluded []string) []string {
	return slices.DeleteFunc(slices.Clone(values), func(value string) bool {
		return slices.Contains(excluded, value)
	})
}

func union(left, right []string) []string {
	merged := slices.Concat(left, right)
	slices.Sort(merged)
	return slices.Compact(merged)
}
