package handlers_rds

import (
	"net/netip"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemVPCNameIsPerRegion(t *testing.T) {
	assert.Equal(t, "rds-system-ap-southeast-2", SystemVPCName("ap-southeast-2"))
	assert.NotEqual(t, SystemVPCName("us-east-1"), SystemVPCName("ap-southeast-2"),
		"one system VPC per region, so two regions must not resolve to the same VPC")
}

func TestSystemVPCSpecDefaults(t *testing.T) {
	spec := SystemVPCSpec(nil, "ap-southeast-2")

	assert.Equal(t, handlers_systemvpc.Spec{
		Owner: handlers_systemvpc.Owner{
			Name:        "rds-system-ap-southeast-2",
			ManagedBy:   tags.ManagedByRDS,
			OwnerTagKey: rdsSystemVPCTagKey,
			RoleTagKey:  rdsSystemVPCRoleTagKey,
		},
		Region:         "ap-southeast-2",
		RolePrefix:     rdsSystemVPCRolePrefix,
		Supernet:       config.RDSDefaultSystemVPCSupernet,
		PrivateSubnets: 1,
	}, spec)

	// An empty RDSConfig is what an operator who never wrote an rds block has,
	// so it must produce the same VPC as no config at all.
	assert.Equal(t, spec, SystemVPCSpec(&config.RDSConfig{}, "ap-southeast-2"))
}

func TestSystemVPCSpecHonoursOperatorOverrides(t *testing.T) {
	spec := SystemVPCSpec(&config.RDSConfig{
		SystemVPCSupernet:       "172.16.0.0/14",
		SystemVPCPrivateSubnets: 3,
	}, "us-east-1")

	assert.Equal(t, "172.16.0.0/14", spec.Supernet)
	assert.Equal(t, 3, spec.PrivateSubnets)
	assert.Equal(t, tags.ManagedByRDS, spec.ManagedBy, "an override must not change who owns the VPC")
}

func TestSystemVPCTagsAreRDSOwn(t *testing.T) {
	spec := SystemVPCSpec(nil, "ap-southeast-2")

	// The tag keys are the whole isolation mechanism: EKS's teardown and its
	// orphan reaper sweep on the EKS keys, so RDS resources carrying them would
	// be reclaimed out from under a running database.
	assert.NotEqual(t, "spinifex:eks-cluster", spec.OwnerTagKey)
	assert.NotEqual(t, "spinifex:eks-role", spec.RoleTagKey)
	assert.Equal(t, "rds-vpc", spec.Roles().VPC)
	assert.Equal(t, "rds-private", spec.Roles().PrivateSubnet)
}

func TestSystemVPCSupernetIsUsableAndDisjointFromEKS(t *testing.T) {
	supernet, err := netip.ParsePrefix(config.RDSDefaultSystemVPCSupernet)
	require.NoError(t, err)
	assert.Equal(t, supernet.Masked(), supernet, "a supernet with host bits set is rejected by the builder")
	assert.Equal(t, 14, supernet.Bits(), "the builder carves a /22 out of a /14 and requires that shape")

	// EKS's control-plane supernet. A collision is survivable but leaves an
	// operator unable to tell from an address which component owns it.
	eks := netip.MustParsePrefix("10.252.0.0/14")
	assert.False(t, supernet.Overlaps(eks),
		"the RDS system VPC space %s must not overlap the EKS control-plane space %s", supernet, eks)
}
