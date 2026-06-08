package handlers_eks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupAddon_KnownAndUnknown(t *testing.T) {
	spec, ok := lookupAddon("aws-load-balancer-controller")
	require.True(t, ok)
	assert.Equal(t, "aws-load-balancer-controller", spec.Name)
	assert.True(t, spec.RequiresIRSA)

	_, ok = lookupAddon("does-not-exist")
	assert.False(t, ok)
}

func TestAddonSpec_DefaultVersionIsNewest(t *testing.T) {
	for name, spec := range addonCatalog {
		require.NotEmpty(t, spec.Versions, "addon %s must list versions", name)
		assert.Equal(t, spec.Versions[0], spec.DefaultVersion,
			"addon %s default version must be the newest (first) version", name)
	}
}

func TestAddonSpec_SupportsVersion(t *testing.T) {
	spec, ok := lookupAddon("aws-load-balancer-controller")
	require.True(t, ok)
	assert.True(t, spec.supportsVersion(spec.DefaultVersion))
	assert.False(t, spec.supportsVersion("0.0.0-nope"))
}

func TestCatalogSpecs_SortedByName(t *testing.T) {
	specs := catalogSpecs()
	require.Len(t, specs, len(addonCatalog))
	for i := 1; i < len(specs); i++ {
		assert.LessOrEqual(t, specs[i-1].Name, specs[i].Name, "catalog must be name-sorted")
	}
}

func TestNewAddonSpec_PanicsWithoutVersions(t *testing.T) {
	assert.Panics(t, func() {
		newAddonSpec("broken", false, "no versions")
	})
}
