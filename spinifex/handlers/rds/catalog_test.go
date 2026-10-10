package handlers_rds

//test:in-package — asserts the catalog against the unexported rejectUnimplemented
// and against storageTypeGP3 and the allocated-storage bounds, which are package
// constants deliberately, so the option cannot claim what create refuses.

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runsEverything(string) bool { return true }

// The catalog is a read of the engine package's registry, so a pin bump has to
// fail here rather than in a client that hardcoded the old one.
func TestEngineVersions_ReportWhatTheEngineTableSays(t *testing.T) {
	t.Parallel()
	byEngine := map[string]*rds.DBEngineVersion{}
	for _, row := range EngineVersions(rdsengine.EngineVersionFilter{}) {
		byEngine[aws.StringValue(row.Engine)] = row
	}
	require.Len(t, byEngine, len(rdsengine.SupportedEngines()))

	for _, name := range rdsengine.SupportedEngines() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			engine, err := rdsengine.LookupEngine(name)
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
	for _, row := range EngineVersions(rdsengine.EngineVersionFilter{}) {
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
	options := OrderableOptions(rdsengine.OrderableFilter{}, InstanceSizing(), runsEverything)
	require.Len(t, options, len(rdsengine.SupportedEngines())*len(rdsengine.SupportedInstanceClasses()))

	for _, option := range options {
		label := aws.StringValue(option.Engine) + "/" + aws.StringValue(option.DBInstanceClass)
		engine, err := rdsengine.LookupEngine(aws.StringValue(option.Engine))
		require.NoError(t, err, label)

		assert.Equal(t, engine.EngineVersion(), aws.StringValue(option.EngineVersion), label)
		assert.Equal(t, engine.LicenseModel(), aws.StringValue(option.LicenseModel), label)
		assert.Contains(t, rdsengine.SupportedInstanceClasses(), aws.StringValue(option.DBInstanceClass), label)

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
	option := OrderableOptions(rdsengine.OrderableFilter{}, InstanceSizing(), runsEverything)[0]

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
