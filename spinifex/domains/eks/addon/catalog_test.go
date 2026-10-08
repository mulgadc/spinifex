package addon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookup_KnownAndUnknown(t *testing.T) {
	t.Parallel()
	spec, ok := Lookup("aws-load-balancer-controller")
	require.True(t, ok)
	assert.Equal(t, "aws-load-balancer-controller", spec.Name)
	assert.True(t, spec.RequiresIRSA)

	_, ok = Lookup("does-not-exist")
	assert.False(t, ok)
}

func TestSpec_DefaultVersionIsNewest(t *testing.T) {
	t.Parallel()
	for _, spec := range Specs() {
		require.NotEmpty(t, spec.Versions, "addon %s must list versions", spec.Name)
		assert.Equal(t, spec.Versions[0], spec.DefaultVersion,
			"addon %s default version must be the newest (first) version", spec.Name)
	}
}

func TestSpec_SupportsVersion(t *testing.T) {
	t.Parallel()
	spec, ok := Lookup("aws-load-balancer-controller")
	require.True(t, ok)
	assert.True(t, spec.SupportsVersion(spec.DefaultVersion))
	assert.False(t, spec.SupportsVersion("0.0.0-nope"))
}

func TestSpecs_SortedByName(t *testing.T) {
	t.Parallel()
	specs := Specs()
	require.Len(t, specs, len(catalog))
	for i := 1; i < len(specs); i++ {
		assert.LessOrEqual(t, specs[i-1].Name, specs[i].Name, "catalog must be name-sorted")
	}
}

func TestValidateCatalog(t *testing.T) {
	tests := []struct {
		name    string
		catalog map[string]Spec
		wantErr string
	}{
		{
			name:    "valid catalog",
			catalog: buildCatalog(newSpec("valid", false, "valid add-on", "1.0.0")),
		},
		{
			name:    "no versions",
			catalog: buildCatalog(newSpec("broken", false, "no versions")),
			wantErr: `add-on "broken" has no versions`,
		},
		{
			name: "mismatched name",
			catalog: map[string]Spec{
				"catalog-name": newSpec("spec-name", false, "mismatched name", "1.0.0"),
			},
			wantErr: `add-on catalog key "catalog-name" does not match spec name "spec-name"`,
		},
		{
			name: "unsupported default",
			catalog: map[string]Spec{
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
			err := validateCatalog(tt.catalog)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

// Not parallel: it swaps the package-wide catalogue.
func TestValidateCatalog_ChecksTheBundledCatalog(t *testing.T) {
	require.NoError(t, ValidateCatalog())

	previous := catalog
	catalog = buildCatalog(newSpec("broken", false, "no versions"))
	t.Cleanup(func() { catalog = previous })
	require.EqualError(t, ValidateCatalog(), `add-on "broken" has no versions`)
}
