package tags

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsSystemManaged(t *testing.T) {
	for value, want := range map[string]bool{
		ManagedByELBv2: true,
		ManagedByEKS:   true,
		// An RDS DB instance is a VM in the system account; this is what keeps it
		// out of the customer's EC2 API and binds its cluster-wide terminate
		// subject.
		ManagedByRDS: true,
		// The ECS value marks the node AMI only — container instances launched
		// from it stay customer-owned, so it must not be treated as a system VM.
		ManagedByECS: false,
		// An untagged resource is a customer's.
		"":        false,
		"unknown": false,
	} {
		assert.Equal(t, want, IsSystemManaged(value), "IsSystemManaged(%q)", value)
	}
}
