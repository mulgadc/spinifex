//test:in-package — prunable and PruneScope are unexported, and what this test
//pins is which zone the service-endpoint class is allowed to act on.

package dns

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const testServiceZone = "spinifex.internal"

// prunableForServiceZone mirrors prunableForZones with the service-endpoint
// zone configured alongside the base and private zones.
func prunableForServiceZone(scope PruneScope) func(zone, label string) bool {
	r := &Reconciler{baseDomain: testBase, internalDomain: testPrivate, serviceZone: testServiceZone}
	return r.prunable(scope)
}

// The service-endpoint class must be matched on zone identity alone, never on
// a label substring. A label shaped like "ec2.<region>" sits in the same base
// zone as EC2's own "ec2-"-prefixed labels, so if this class were ever matched
// by substring instead of zone, a future EC2 prefix change could make the two
// collide and delete each other's records.
func TestPrunable_ServiceEndpointMatchesOnZoneNotLabel(t *testing.T) {
	prune := prunableForServiceZone(PruneScope{ServiceEndpoint: true, EC2: true})

	assert.True(t, prune(testServiceZone, "ec2.ap-southeast-2"),
		"a service-endpoint label in its own zone is prunable with ServiceEndpoint authority")
	assert.False(t, prune(testBase, "ec2.ap-southeast-2"),
		"a label shaped like a service endpoint in the BASE zone must not be treated as one")

	// ServiceEndpoint authority alone (no EC2) must not reach into the base
	// zone just because a label happens to look service-endpoint-shaped.
	serviceOnly := prunableForServiceZone(PruneScope{ServiceEndpoint: true})
	assert.False(t, serviceOnly(testBase, "ec2-3-3-3-3.ap-southeast-2.compute."),
		"ServiceEndpoint authority does not extend to the base zone at all")
}

// Without ServiceEndpoint authority nothing in that zone is prunable, even
// when every other class is authoritative this cycle.
func TestPrunable_ServiceEndpointRequiresItsOwnAuthority(t *testing.T) {
	prune := prunableForServiceZone(PruneScope{EC2: true, ELB: true, EKS: true, RDS: true})
	assert.False(t, prune(testServiceZone, "ec2.ap-southeast-2"),
		"other classes' authority does not extend to the service-endpoint zone")
}

// A misconfigured suffix equal to the base domain must not disable ELB/EKS/RDS
// pruning on the shared zone; the collision degrades to "service-endpoint
// records are never pruned" instead.
func TestPrunable_ServiceEndpointZoneEqualToBaseDomainDoesNotShadowOtherClasses(t *testing.T) {
	r := &Reconciler{baseDomain: testBase, internalDomain: testPrivate, serviceZone: testBase}
	prune := r.prunable(PruneScope{ServiceEndpoint: true, ELB: true})
	assert.True(t, prune(testBase, "app-web-abc.ap-southeast-2.elb."),
		"ELB pruning on the base zone must survive a suffix misconfigured to equal it")
}
