package tags

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsSystemManaged(t *testing.T) {
	cases := map[string]struct {
		managedBy string
		want      bool
		why       string
	}{
		"elbv2": {ManagedByELBv2, true, "HAProxy VMs are platform-owned"},
		"eks":   {ManagedByEKS, true, "K3s control-plane VMs are platform-owned"},
		"rds":   {ManagedByRDS, true, "DB engine VMs are platform-owned and live in the system account"},
		// Container instances launched from the ECS node AMI stay customer-owned,
		// so only the AMI carries the tag — never a VM.
		"ecs":      {ManagedByECS, false, "ECS container instances are customer-owned"},
		"customer": {"", false, "an untagged instance is a customer instance"},
		"unknown":  {"redshift", false, "an unrecognised component is not system-managed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// A system VM that reports false never binds its
			// system.TerminateInstance.{id} subject, so a teardown invoked on a
			// non-owning node has no responder.
			assert.Equal(t, tc.want, IsSystemManaged(tc.managedBy), tc.why)
		})
	}
}
