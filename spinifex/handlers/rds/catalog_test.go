package handlers_rds

//test:in-package — asserts the catalog against the unexported rejectUnimplemented
// and against storageTypeGP3 and the allocated-storage bounds, which are package
// constants deliberately, so the option cannot claim what create refuses.

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runsEverything(string) bool { return true }

func runsNothing(string) bool { return false }

// The catalog is a read of engine.go, so a pin bump has to fail here rather than
// in a client that hardcoded the old one.
func TestEngineVersions_ReportWhatTheEngineTableSays(t *testing.T) {
	t.Parallel()
	byEngine := map[string]*rds.DBEngineVersion{}
	for _, row := range EngineVersions(EngineVersionFilter{}) {
		byEngine[aws.StringValue(row.Engine)] = row
	}
	require.Len(t, byEngine, len(SupportedEngines()))

	for _, name := range SupportedEngines() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			engine, err := LookupEngine(name)
			require.NoError(t, err)
			row := byEngine[name]
			require.NotNil(t, row)

			assert.Equal(t, engine.EngineVersion(), aws.StringValue(row.EngineVersion))
			assert.Equal(t, engine.MajorVersion, aws.StringValue(row.MajorEngineVersion))
			assert.Equal(t, engine.ParameterGroupFamily(), aws.StringValue(row.DBParameterGroupFamily))
			assert.Equal(t, engine.Description(), aws.StringValue(row.DBEngineDescription))
			assert.Equal(t, engineVersionStatusAvailable, aws.StringValue(row.Status))

			// The family the row names has to be the one the implicit default
			// parameter group is built from, or a console reading both disagrees.
			assert.Equal(t, defaultParameterGroupPrefix+aws.StringValue(row.DBParameterGroupFamily),
				engine.DefaultParameterGroupName())
		})
	}
}

// v1 offers no upgrade path, no log export and no engine-selectable character
// set or timezone, so every one of these is empty rather than unpopulated.
func TestEngineVersions_ReportNoCapabilityThePlatformLacks(t *testing.T) {
	t.Parallel()
	for _, row := range EngineVersions(EngineVersionFilter{}) {
		name := aws.StringValue(row.Engine)
		assert.Empty(t, row.ValidUpgradeTarget, "%s should offer no upgrade target", name)
		assert.Empty(t, row.ExportableLogTypes, "%s should export no log type", name)
		assert.Empty(t, row.SupportedEngineModes, "%s should offer no engine mode", name)
		assert.Empty(t, row.SupportedCharacterSets, "%s should offer no character set", name)
		assert.Empty(t, row.SupportedTimezones, "%s should offer no timezone", name)
		assert.False(t, aws.BoolValue(row.SupportsReadReplica), "%s should not claim read replicas", name)
		assert.False(t, aws.BoolValue(row.SupportsGlobalDatabases), "%s should not claim global databases", name)
		assert.False(t, aws.BoolValue(row.SupportsLogExportsToCloudwatchLogs), "%s should not claim log export", name)
	}
}

func TestOrderableOptions_CoverEveryEngineAndClass(t *testing.T) {
	t.Parallel()
	options := OrderableOptions(OrderableFilter{}, runsEverything)
	require.Len(t, options, len(SupportedEngines())*len(SupportedInstanceClasses()))

	for _, option := range options {
		label := aws.StringValue(option.Engine) + "/" + aws.StringValue(option.DBInstanceClass)
		engine, err := LookupEngine(aws.StringValue(option.Engine))
		require.NoError(t, err, label)

		assert.Equal(t, engine.EngineVersion(), aws.StringValue(option.EngineVersion), label)
		assert.Equal(t, engine.LicenseModel(), aws.StringValue(option.LicenseModel), label)
		assert.Contains(t, SupportedInstanceClasses(), aws.StringValue(option.DBInstanceClass), label)

		assert.Equal(t, storageTypeGP3, aws.StringValue(option.StorageType), label)
		assert.Equal(t, int64(minAllocatedStorageGiB), aws.Int64Value(option.MinStorageSize), label)
		assert.Equal(t, int64(maxAllocatedStorageGiB), aws.Int64Value(option.MaxStorageSize), label)
		assert.True(t, aws.BoolValue(option.SupportsStorageEncryption), label)
		assert.True(t, aws.BoolValue(option.Vpc), label)
		assert.Equal(t, []string{networkTypeIPv4}, aws.StringValueSlice(option.SupportedNetworkTypes), label)

		// A zone here would invite a create naming it, which validate.go refuses;
		// a processor feature would advertise a knob create accepts and ignores.
		assert.Empty(t, option.AvailabilityZones, label)
		assert.Empty(t, option.AvailableProcessorFeatures, label)
	}
}

// An option claiming a capability the create path refuses is a contradiction the
// catalog should not be able to hold quietly.
func TestOrderableOptions_AgreeWithTheRejectedCreateParameters(t *testing.T) {
	t.Parallel()
	option := OrderableOptions(OrderableFilter{}, runsEverything)[0]

	cases := []struct {
		capability string
		claimed    *bool
		create     *rds.CreateDBInstanceInput
	}{
		{"MultiAZCapable", option.MultiAZCapable, &rds.CreateDBInstanceInput{MultiAZ: aws.Bool(true)}},
		{"SupportsIops", option.SupportsIops, &rds.CreateDBInstanceInput{Iops: aws.Int64(3000)}},
		{"SupportsStorageThroughput", option.SupportsStorageThroughput,
			&rds.CreateDBInstanceInput{StorageThroughput: aws.Int64(125)}},
		{"SupportsStorageAutoscaling", option.SupportsStorageAutoscaling,
			&rds.CreateDBInstanceInput{MaxAllocatedStorage: aws.Int64(100)}},
		{"SupportsIAMDatabaseAuthentication", option.SupportsIAMDatabaseAuthentication,
			&rds.CreateDBInstanceInput{EnableIAMDatabaseAuthentication: aws.Bool(true)}},
		{"SupportsClusters", option.SupportsClusters,
			&rds.CreateDBInstanceInput{DBClusterIdentifier: aws.String("orders-cluster")}},
	}

	for _, tc := range cases {
		t.Run(tc.capability, func(t *testing.T) {
			t.Parallel()
			assert.False(t, aws.BoolValue(tc.claimed), "the option should not claim %s", tc.capability)
			assert.Error(t, rejectUnimplemented(tc.create),
				"create should refuse the parameter %s corresponds to", tc.capability)
		})
	}

	// The one true claim, checked the same way: encrypted storage is not merely
	// offered, unencrypted storage is refused.
	assert.True(t, aws.BoolValue(option.SupportsStorageEncryption))
	assert.Error(t, rejectUnimplemented(&rds.CreateDBInstanceInput{StorageEncrypted: aws.Bool(false)}))
}

// A class the cluster's nodes cannot run is never offered, and a cluster that
// runs none of them is an empty list rather than an error.
func TestOrderableOptions_FilterOnClusterCapability(t *testing.T) {
	t.Parallel()
	onlyMicro := OrderableOptions(OrderableFilter{}, func(instanceType string) bool {
		return instanceType == "t3.micro"
	})
	require.Len(t, onlyMicro, len(SupportedEngines()))
	for _, option := range onlyMicro {
		assert.Equal(t, "db.t3.micro", aws.StringValue(option.DBInstanceClass))
	}

	assert.Empty(t, OrderableOptions(OrderableFilter{}, runsNothing))
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
			instanceType, err := InstanceTypeForClass(class)
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
				got = append(got, aws.StringValue(row.Engine))
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
			assert.Len(t, OrderableOptions(tc.filter(), runsEverything), tc.want)
		})
	}
}
