package gateway

import (
	"sort"

	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	gateway_ecs "github.com/mulgadc/spinifex/spinifex/gateway/ecs"
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
		"sts": {
			Registered: mapKeys(stsActions),
		},
	}
}

func mapKeys[K ~string, V any](values map[K]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	return keys
}

func without(values, excluded []string) []string {
	exclude := make(map[string]bool, len(excluded))
	for _, value := range excluded {
		exclude[value] = true
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !exclude[value] {
			result = append(result, value)
		}
	}
	return result
}

func union(left, right []string) []string {
	set := make(map[string]bool, len(left)+len(right))
	for _, value := range left {
		set[value] = true
	}
	for _, value := range right {
		set[value] = true
	}
	return mapKeys(set)
}
