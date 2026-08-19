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

func TestValidateAddonCatalog(t *testing.T) {
	tests := []struct {
		name    string
		catalog map[string]AddonSpec
		wantErr string
	}{
		{
			name:    "valid catalog",
			catalog: buildAddonCatalog(newAddonSpec("valid", false, "valid add-on", "1.0.0")),
		},
		{
			name:    "no versions",
			catalog: buildAddonCatalog(newAddonSpec("broken", false, "no versions")),
			wantErr: `add-on "broken" has no versions`,
		},
		{
			name: "mismatched name",
			catalog: map[string]AddonSpec{
				"catalog-name": newAddonSpec("spec-name", false, "mismatched name", "1.0.0"),
			},
			wantErr: `add-on catalog key "catalog-name" does not match spec name "spec-name"`,
		},
		{
			name: "unsupported default",
			catalog: map[string]AddonSpec{
				"broken": {
					Name:           "broken",
					Versions:       []string{"1.0.0"},
					DefaultVersion: "2.0.0",
				},
			},
			wantErr: `add-on "broken" default version "2.0.0" is not supported`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAddonCatalog(tt.catalog)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestNewEKSServiceImpl_RejectsInvalidAddonCatalog(t *testing.T) {
	previousCatalog := addonCatalog
	addonCatalog = buildAddonCatalog(newAddonSpec("broken", false, "no versions"))
	t.Cleanup(func() { addonCatalog = previousCatalog })

	svc, err := NewEKSServiceImpl(EKSServiceDeps{})
	require.EqualError(t, err, `eks: validate add-on catalog: add-on "broken" has no versions`)
	assert.Nil(t, svc)
}
