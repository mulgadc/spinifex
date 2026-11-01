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

// buildSpec is a two-version spec whose newest version is on EKS build 2, so
// a test cannot pass by hardcoding build 1.
func buildSpec() Spec {
	return Spec{
		Name:     "fixture",
		Versions: []string{"2.0.0", "1.0.0"},
		Builds:   map[string]int{"2.0.0": 2, "1.0.0": 1},
	}
}

func TestSpec_AWSVersionUsesTheCatalogBuildNumber(t *testing.T) {
	t.Parallel()
	spec := buildSpec()
	assert.Equal(t, "v2.0.0-eksbuild.2", spec.AWSVersion("2.0.0"))
	assert.Equal(t, "v1.0.0-eksbuild.1", spec.AWSVersion("1.0.0"))
}

func TestSpec_AWSVersionIsIdempotent(t *testing.T) {
	t.Parallel()
	spec := buildSpec()
	assert.Equal(t, "v2.0.0-eksbuild.2", spec.AWSVersion("v2.0.0-eksbuild.2"),
		"a record already holding the AWS form must not be wrapped twice")
}

func TestSpec_AWSVersionsAreNewestFirst(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"v2.0.0-eksbuild.2", "v1.0.0-eksbuild.1"}, buildSpec().AWSVersions())
}

func TestSpec_ParseVersionAcceptsOnlyTheExactForms(t *testing.T) {
	t.Parallel()
	spec := buildSpec()
	for input, want := range map[string]string{
		"v2.0.0-eksbuild.2": "2.0.0",
		"2.0.0":             "2.0.0",
		"v1.0.0-eksbuild.1": "1.0.0",
		"1.0.0":             "1.0.0",
	} {
		got, ok := spec.ParseVersion(input)
		assert.True(t, ok, "%q must be accepted", input)
		assert.Equal(t, want, got, "%q", input)
	}
}

func TestSpec_ParseVersionRefusesEverythingElse(t *testing.T) {
	t.Parallel()
	spec := buildSpec()
	for _, input := range []string{
		"v2.0.0-eksbuild.1", // wrong build for 2.0.0
		"v2.0.0-eksbuild.3",
		"v1.0.0-eksbuild.2", // wrong build for 1.0.0
		"v2.0.0",            // missing build
		"2.0",               // partial
		"v2.0.0-eksbuild.",
		"2.0.0-eksbuild.2", // missing v
		"V2.0.0-eksbuild.2",
		" 2.0.0",
		"9.9.9",
		"",
	} {
		_, ok := spec.ParseVersion(input)
		assert.False(t, ok, "%q must be refused", input)
	}
}

func TestCatalog_EveryVersionHasAnExplicitBuild(t *testing.T) {
	t.Parallel()
	for _, spec := range Specs() {
		for _, v := range spec.Versions {
			assert.Equal(t, 1, spec.Builds[v], "%s %s needs an explicit build number", spec.Name, v)
			assert.Equal(t, "v"+v+"-eksbuild.1", spec.AWSVersion(v), "%s %s", spec.Name, v)
		}
	}
}
