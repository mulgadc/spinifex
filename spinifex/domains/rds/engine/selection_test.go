package engine

//test:in-package — reaches the unexported engines/dbInstanceClasses registries
// and engineVersionStatusAvailable, none of which this package exports.

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instancetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runsEverything(string) bool { return true }

func runsNothing(string) bool { return false }

// Both are reported verbatim by the two catalogs, and the licence model is
// filterable, so an engine registering neither would answer a --license-model
// filter with an empty row set rather than with its own licence.
func TestEngines_RegisterADescriptionAndALicenceModel(t *testing.T) {
	t.Parallel()
	for _, name := range SupportedEngines() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			engine, err := LookupEngine(name)
			require.NoError(t, err)
			assert.NotEmpty(t, engine.Description())
			assert.NotEmpty(t, engine.LicenseModel())
		})
	}
}

// db.X is the EC2 instance type X, which is what lets a client read vCPU and
// memory from ec2:DescribeInstanceTypes for the identically named type. Asserted
// by name rather than by footprint: t3.large and m5.large are both 2 vCPU and
// 8 GiB, so repointing db.t3.large at m5.large would break the documented
// identity and pass a footprint comparison unchanged.
func TestInstanceTypeForClass_IsTheClassNameWithoutItsPrefix(t *testing.T) {
	t.Parallel()
	for _, class := range SupportedInstanceClasses() {
		t.Run(class, func(t *testing.T) {
			t.Parallel()
			instanceType, err := testSizing.InstanceTypeForClass(class)
			require.NoError(t, err)
			assert.Equal(t, strings.TrimPrefix(class, "db."), instanceType)

			// Both lookups prove the name is known to instancetypes, which is what
			// makes SmallestInstanceClass safe: it silently skips a class whose
			// footprint will not resolve.
			_, knownVCPUs := instancetypes.DefaultVCPUs(instanceType)
			assert.True(t, knownVCPUs, "instancetypes should know the vCPU count of %s", instanceType)
			_, knownMemory := instancetypes.DefaultMemoryMiB(instanceType)
			assert.True(t, knownMemory, "instancetypes should know the memory of %s", instanceType)
		})
	}
}

func TestEngineVersionFilter_NarrowsByEveryRecognisedField(t *testing.T) {
	t.Parallel()
	postgres, err := LookupEngine("postgres")
	require.NoError(t, err)

	cases := []struct {
		name   string
		filter func() EngineVersionFilter
		want   []string
	}{
		{"unfiltered", func() EngineVersionFilter { return EngineVersionFilter{} }, SupportedEngines()},
		{"engine", func() (f EngineVersionFilter) {
			f.Engine.AddParam("postgres")
			return f
		}, []string{"postgres"}},
		{"engine is case insensitive", func() (f EngineVersionFilter) {
			f.Engine.AddFilter([]string{"PostgreSQL", "POSTGRES"})
			return f
		}, []string{"postgres"}},
		{"engine version", func() (f EngineVersionFilter) {
			f.EngineVersion.AddParam(postgres.MajorVersion)
			return f
		}, []string{"postgres"}},
		{"parameter group family", func() (f EngineVersionFilter) {
			f.ParameterGroupFamily.AddParam(postgres.ParameterGroupFamily())
			return f
		}, []string{"postgres"}},
		{"status", func() (f EngineVersionFilter) {
			f.Status.AddFilter([]string{engineVersionStatusAvailable})
			return f
		}, SupportedEngines()},
		{"deprecated status matches nothing", func() (f EngineVersionFilter) {
			f.Status.AddFilter([]string{"deprecated"})
			return f
		}, nil},
		{"unknown engine matches nothing", func() (f EngineVersionFilter) {
			f.Engine.AddParam("oracle-ee")
			return f
		}, nil},
		{"unpinned version matches nothing", func() (f EngineVersionFilter) {
			f.EngineVersion.AddParam("17")
			return f
		}, nil},
		// Two sources naming different engines is a conjunction, not a union: a
		// row has to satisfy both, and no row is two engines.
		{"typed parameter and filter are both applied", func() (f EngineVersionFilter) {
			f.Engine.AddParam("postgres")
			f.Engine.AddFilter([]string{"mariadb"})
			return f
		}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, row := range EngineVersions(tc.filter()) {
				got = append(got, row.Name)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestOrderableFilter_NarrowsByEveryRecognisedField(t *testing.T) {
	t.Parallel()
	postgres, err := LookupEngine("postgres")
	require.NoError(t, err)
	classes := len(SupportedInstanceClasses())

	cases := []struct {
		name   string
		filter func() OrderableFilter
		want   int
	}{
		{"unfiltered", func() OrderableFilter { return OrderableFilter{} }, len(SupportedEngines()) * classes},
		{"engine", func() (f OrderableFilter) {
			f.Engine.AddParam("postgres")
			return f
		}, classes},
		{"engine version", func() (f OrderableFilter) {
			f.EngineVersion.AddParam(postgres.MajorVersion)
			return f
		}, classes},
		{"instance class", func() (f OrderableFilter) {
			f.DBInstanceClass.AddParam("db.t3.micro")
			return f
		}, len(SupportedEngines())},
		{"license model", func() (f OrderableFilter) {
			f.LicenseModel.AddParam(postgres.LicenseModel())
			return f
		}, classes},
		{"vpc true", func() (f OrderableFilter) {
			f.Vpc.AddParam("true")
			return f
		}, len(SupportedEngines()) * classes},
		// Every endpoint is a private VPC address, so asking for a non-VPC option
		// is an empty list rather than the whole catalog.
		{"vpc false", func() (f OrderableFilter) {
			f.Vpc.AddParam("false")
			return f
		}, 0},
		{"unknown class matches nothing", func() (f OrderableFilter) {
			f.DBInstanceClass.AddParam("db.r5.24xlarge")
			return f
		}, 0},
		{"unpinned version matches nothing", func() (f OrderableFilter) {
			f.EngineVersion.AddParam("17")
			return f
		}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Len(t, OrderableOptions(tc.filter(), testSizing, runsEverything), tc.want)
		})
	}
}

// A class the cluster's nodes cannot run is never offered, and a cluster that
// runs none of them is an empty list rather than an error.
func TestOrderableOptions_FilterOnClusterCapability(t *testing.T) {
	t.Parallel()
	onlyMicro := OrderableOptions(OrderableFilter{}, testSizing, func(instanceType string) bool {
		return instanceType == "t3.micro"
	})
	require.Len(t, onlyMicro, len(SupportedEngines()))
	for _, option := range onlyMicro {
		assert.Equal(t, "db.t3.micro", option.DBInstanceClass)
	}

	assert.Empty(t, OrderableOptions(OrderableFilter{}, testSizing, runsNothing))
}
