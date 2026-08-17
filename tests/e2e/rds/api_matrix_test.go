//go:build e2e

package rds

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// A real AWS class the platform's sizing table deliberately does not map, so
	// it is refused at validation rather than surfacing as a launch failure after
	// a volume and an ENI exist.
	unmappedDBClass = "db.r5.large"
	// Until this is supported, it has to be refused rather than half-served.
	unsupportedDBEngine = "mysql"
	// One day over the retention cap.
	overCapRetentionDays = 8
)

// outOfScopeRDSActions are recognised by the gateway so a client is told the
// action is not offered rather than that it typo'd the name. Written as a
// literal instead of read from the gateway's own table, so an action quietly
// dropped from that table fails here rather than silently redefining the surface.
var outOfScopeRDSActions = []string{
	"CreateDBInstanceReadReplica",
	"PromoteReadReplica",
	"CreateDBCluster",
	"ModifyDBCluster",
	"DeleteDBCluster",
	"DescribeDBClusters",
	"FailoverDBCluster",
	"CreateOptionGroup",
	"ModifyOptionGroup",
	"DeleteOptionGroup",
	"DescribeOptionGroups",
	"RestoreDBInstanceToPointInTime",
}

// An action the gateway has never heard of, as distinct from one it recognises
// and does not offer. Kept so the two stay distinguishable to a client.
var unregisteredRDSActions = []string{
	"NotAnRDSAction",
}

// TestAPIMatrix is the negative matrix: validation and unsupported requests,
// actions that are recognised but not offered, and the parameters that are
// accepted as no-ops. It runs first and boots almost nothing, so a broken API
// surface fails the suite in seconds rather than after a VM boot.
//
// One instance is created and deleted without ever being waited for, because
// three assertions need a record to exist and none of them needs an engine: the
// no-op parameters as the API reports them back, a duplicate identifier, and a
// storage shrink.
func TestAPIMatrix(t *testing.T) {
	f := requireRDSFixture(t)
	// Deliberately not parallel: it is the cheapest test in the suite and the one
	// whose failures are the API's own, so it runs to completion before Go
	// releases the parallel tests and the first DB VM boots.
	reserveDBVMs(t, dbClass)

	suffix := time.Now().Unix()
	rejectedID := fmt.Sprintf("%s-rejected-%d", dbInstancePfx, suffix)

	// A parameter whose omission would create a false safety,
	// security or availability guarantee is refused outright.
	t.Run("RejectsUnimplementedCreateParameters", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func(*rds.CreateDBInstanceInput)
		}{
			{"PubliclyAccessible", func(in *rds.CreateDBInstanceInput) { in.PubliclyAccessible = aws.Bool(true) }},
			{"MultiAZ", func(in *rds.CreateDBInstanceInput) { in.MultiAZ = aws.Bool(true) }},
			{"StorageEncryptedFalse", func(in *rds.CreateDBInstanceInput) { in.StorageEncrypted = aws.Bool(false) }},
			{"EngineVersionOtherThanThePin", func(in *rds.CreateDBInstanceInput) { in.EngineVersion = aws.String("16.4") }},
			{"MinorEngineVersion", func(in *rds.CreateDBInstanceInput) { in.EngineVersion = aws.String("18.4") }},
			{"MaxAllocatedStorage", func(in *rds.CreateDBInstanceInput) { in.MaxAllocatedStorage = aws.Int64(100) }},
			{"StorageThroughput", func(in *rds.CreateDBInstanceInput) { in.StorageThroughput = aws.Int64(250) }},
			{"IAMDatabaseAuthentication", func(in *rds.CreateDBInstanceInput) {
				in.EnableIAMDatabaseAuthentication = aws.Bool(true)
			}},
			{"CustomerManagedKey", func(in *rds.CreateDBInstanceInput) {
				in.KmsKeyId = aws.String("arn:aws:kms:ap-southeast-2:000000000001:key/not-a-key")
			}},
			{"AvailabilityZone", func(in *rds.CreateDBInstanceInput) { in.AvailabilityZone = aws.String("ap-southeast-2a") }},
			{"ProvisionedIops", func(in *rds.CreateDBInstanceInput) { in.Iops = aws.Int64(3000) }},
			{"CloudwatchLogsExports", func(in *rds.CreateDBInstanceInput) {
				in.EnableCloudwatchLogsExports = aws.StringSlice([]string{"postgresql"})
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				in := validCreateInput(rejectedID)
				tc.mutate(in)
				expectCreateRefused(t, f, f.AWS, "InvalidParameterValue", in)
			})
		}
	})

	t.Run("RejectsInvalidCreateRequests", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			code   string
			mutate func(*rds.CreateDBInstanceInput)
		}{
			{"UnmappedInstanceClass", "InvalidParameterValue", func(in *rds.CreateDBInstanceInput) {
				in.DBInstanceClass = aws.String(unmappedDBClass)
			}},
			{"StorageBelowTheFloor", "InvalidParameterValue", func(in *rds.CreateDBInstanceInput) {
				in.AllocatedStorage = aws.Int64(dbStorageGiB - 1)
			}},
			{"StorageAboveTheCap", "InvalidParameterValue", func(in *rds.CreateDBInstanceInput) {
				in.AllocatedStorage = aws.Int64(65537)
			}},
			{"UnsupportedEngine", "InvalidParameterValue", func(in *rds.CreateDBInstanceInput) {
				in.Engine = aws.String(unsupportedDBEngine)
			}},
			{"UnknownSubnetGroup", "DBSubnetGroupNotFoundFault", func(in *rds.CreateDBInstanceInput) {
				in.DBSubnetGroupName = aws.String("no-such-subnet-group")
			}},
			{"MalformedBackupWindow", "InvalidParameterValue", func(in *rds.CreateDBInstanceInput) {
				in.PreferredBackupWindow = aws.String("03:00")
			}},
			{"MalformedMaintenanceWindow", "InvalidParameterValue", func(in *rds.CreateDBInstanceInput) {
				in.PreferredMaintenanceWindow = aws.String("funday:03:00-funday:04:00")
			}},
			// The overlap rule is about the pair, so both are named: an assigned
			// window is placed clear of a named one and could never overlap it.
			{"OverlappingWindows", "InvalidParameterCombination", func(in *rds.CreateDBInstanceInput) {
				in.PreferredBackupWindow = aws.String("03:00-04:00")
				in.PreferredMaintenanceWindow = aws.String("mon:03:30-mon:04:30")
			}},
			{"RetentionAboveTheCap", "InvalidParameterValue", func(in *rds.CreateDBInstanceInput) {
				in.BackupRetentionPeriod = aws.Int64(overCapRetentionDays)
			}},
			{"ReservedMasterUsername", "InvalidParameterValue", func(in *rds.CreateDBInstanceInput) {
				in.MasterUsername = aws.String("postgres")
			}},
			{"ForbiddenPasswordCharacters", "InvalidParameterValue", func(in *rds.CreateDBInstanceInput) {
				in.MasterUserPassword = aws.String("e2e@Sup3rSecret1")
			}},
			{"MalformedIdentifier", "InvalidParameterValue", func(in *rds.CreateDBInstanceInput) {
				in.DBInstanceIdentifier = aws.String("9-starts-with-a-digit")
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				in := validCreateInput(rejectedID)
				tc.mutate(in)
				expectCreateRefused(t, f, f.AWS, tc.code, in)
			})
		}
	})

	// Every one of these is refused before the record is ever read, which is why
	// they are asserted against an identifier that does not exist: a
	// DBInstanceNotFound here would mean the parameter is only checked on
	// instances that happen to be present.
	t.Run("RejectsUnimplementedModifyParameters", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func(*rds.ModifyDBInstanceInput)
		}{
			{"DBSubnetGroupName", func(in *rds.ModifyDBInstanceInput) { in.DBSubnetGroupName = aws.String("any-group") }},
			{"NewDBInstanceIdentifier", func(in *rds.ModifyDBInstanceInput) {
				in.NewDBInstanceIdentifier = aws.String(rejectedID + "-renamed")
			}},
			{"EngineVersionUpgrade", func(in *rds.ModifyDBInstanceInput) { in.EngineVersion = aws.String("19") }},
			{"DBPortNumber", func(in *rds.ModifyDBInstanceInput) { in.DBPortNumber = aws.Int64(6543) }},
			{"StorageAutoscaling", func(in *rds.ModifyDBInstanceInput) { in.MaxAllocatedStorage = aws.Int64(100) }},
			{"NonGP3StorageType", func(in *rds.ModifyDBInstanceInput) { in.StorageType = aws.String("io2") }},
			{"ManagedMasterPassword", func(in *rds.ModifyDBInstanceInput) { in.ManageMasterUserPassword = aws.Bool(true) }},
			{"PubliclyAccessible", func(in *rds.ModifyDBInstanceInput) { in.PubliclyAccessible = aws.Bool(true) }},
			{"MultiAZ", func(in *rds.ModifyDBInstanceInput) { in.MultiAZ = aws.Bool(true) }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				in := &rds.ModifyDBInstanceInput{
					DBInstanceIdentifier: aws.String(rejectedID),
					ApplyImmediately:     aws.Bool(true),
				}
				tc.mutate(in)
				harness.ExpectError(t, "InvalidParameterValue", func() error {
					_, err := f.AWS.RDS.ModifyDBInstance(in)
					return err
				})
			})
		}
	})

	// Registered so a client is told the action is not offered. The distinction
	// matters to a provider: OperationNotSupportedException is a platform answer,
	// InvalidAction is a client-side mistake.
	t.Run("UnsupportedActionsFailLoudly", func(t *testing.T) {
		for _, action := range outOfScopeRDSActions {
			t.Run(action, func(t *testing.T) {
				status, body, code := harness.PostRDSAction(t, f.Env, f.AWS, action, nil)
				assert.Equal(t, 400, status, "body: %s", body)
				assert.Equal(t, "OperationNotSupportedException", code, "body: %s", body)
			})
		}
	})

	t.Run("UnregisteredActionsAreInvalidAction", func(t *testing.T) {
		for _, action := range unregisteredRDSActions {
			t.Run(action, func(t *testing.T) {
				status, body, code := harness.PostRDSAction(t, f.Env, f.AWS, action, nil)
				assert.Equal(t, 400, status, "body: %s", body)
				assert.Equal(t, "InvalidAction", code, "body: %s", body)
			})
		}
	})

	// The catalogs are read from the same tables the create path validates
	// against, so these assertions are the only place a version pin or a class
	// map can be checked against what the deployment will actually accept.
	t.Run("CatalogDescribesAnswerFromTheLiveTables", func(t *testing.T) {
		t.Run("EveryEngineVersionIsAvailable", func(t *testing.T) {
			out, err := f.AWS.RDS.DescribeDBEngineVersions(&rds.DescribeDBEngineVersionsInput{})
			require.NoError(t, err)
			require.NotEmpty(t, out.DBEngineVersions)
			for _, row := range out.DBEngineVersions {
				assert.NotEmpty(t, aws.StringValue(row.EngineVersion))
				assert.NotEmpty(t, aws.StringValue(row.DBParameterGroupFamily))
				assert.Equal(t, "available", aws.StringValue(row.Status))
			}
		})

		t.Run("EngineNarrowsToOneRow", func(t *testing.T) {
			out, err := f.AWS.RDS.DescribeDBEngineVersions(&rds.DescribeDBEngineVersionsInput{
				Engine: aws.String(dbEngine),
			})
			require.NoError(t, err)
			require.Len(t, out.DBEngineVersions, 1)
			assert.Equal(t, dbEngine, aws.StringValue(out.DBEngineVersions[0].Engine))
		})

		// The suite boots instances of dbClass, so a cluster that runs this suite
		// runs that class: a catalog that omits it is under-reporting capability.
		t.Run("TheClassThisSuiteLaunchesIsOrderable", func(t *testing.T) {
			out, err := f.AWS.RDS.DescribeOrderableDBInstanceOptions(&rds.DescribeOrderableDBInstanceOptionsInput{
				Engine: aws.String(dbEngine),
			})
			require.NoError(t, err)
			classes := make([]string, 0, len(out.OrderableDBInstanceOptions))
			for _, option := range out.OrderableDBInstanceOptions {
				assert.Equal(t, dbEngine, aws.StringValue(option.Engine))
				assert.Equal(t, "gp3", aws.StringValue(option.StorageType))
				assert.True(t, aws.BoolValue(option.Vpc))
				classes = append(classes, aws.StringValue(option.DBInstanceClass))
			}
			assert.Contains(t, classes, dbClass)
			assert.NotContains(t, classes, unmappedDBClass)
		})

		t.Run("OrderableOptionsRequireAnEngine", func(t *testing.T) {
			harness.ExpectError(t, "MissingParameter", func() error {
				_, err := f.AWS.RDS.DescribeOrderableDBInstanceOptions(
					&rds.DescribeOrderableDBInstanceOptionsInput{})
				return err
			})
		})

		// These two exist only to be filtered, so an unrecognised filter name is
		// refused rather than dropped: a dropped one returns rows the caller
		// asked not to see and cannot detect.
		t.Run("AnUnrecognisedFilterNameIsRefused", func(t *testing.T) {
			harness.ExpectError(t, "InvalidParameterValue", func() error {
				_, err := f.AWS.RDS.DescribeOrderableDBInstanceOptions(
					&rds.DescribeOrderableDBInstanceOptionsInput{
						Engine:  aws.String(dbEngine),
						Filters: []*rds.Filter{{Name: aws.String("status"), Values: aws.StringSlice([]string{"available"})}},
					})
				return err
			})
		})

		// Neither action ever issues a Marker, so one in a request can only have
		// been fabricated and answering it as page one would be a silent lie.
		t.Run("AMarkerIsRefused", func(t *testing.T) {
			harness.ExpectError(t, "InvalidParameterValue", func() error {
				_, err := f.AWS.RDS.DescribeDBEngineVersions(&rds.DescribeDBEngineVersionsInput{
					Marker: aws.String("fabricated"),
				})
				return err
			})
		})
	})

	t.Run("ARejectedCreateLeavesNoRecord", func(t *testing.T) {
		harness.ExpectError(t, "DBInstanceNotFound", func() error {
			_, err := harness.DescribeDBInstance(f.AWS, rejectedID)
			return err
		})
	})

	// The record every assertion below needs, and nothing more: it is deleted
	// without ever being waited for, so the VM behind it lives for seconds.
	probeID := fmt.Sprintf("%s-matrix-%d", dbInstancePfx, suffix)
	harness.Phase(t, "Creating DB instance %q to probe the record-level rejections", probeID)
	probe := validCreateInput(probeID)
	// Above the floor so a shrink request is reachable: a shrink to anything below
	// the floor is refused by the floor rule first.
	probe.AllocatedStorage = aws.Int64(dbStorageGiB + 10)
	// The inert parameters accepted rather than rejected. Each one names a
	// feature this platform does not have, but omitting it promises nothing.
	probe.AutoMinorVersionUpgrade = aws.Bool(true)
	probe.EnablePerformanceInsights = aws.Bool(true)
	probe.MonitoringInterval = aws.Int64(60)
	probe.CopyTagsToSnapshot = aws.Bool(true)

	created, err := f.AWS.RDS.CreateDBInstance(probe) //nolint:staticcheck // e2e:allow-create — the record the rejections below need
	require.NoError(t, err, "create-db-instance")
	require.NotNil(t, created.DBInstance)
	t.Cleanup(func() { deleteInstance(t, f, probeID) })

	// Accepted inert parameters are echoed so Terraform does not plan changes no
	// modify can deliver.
	t.Run("InertParametersAreAcceptedAndEchoed", func(t *testing.T) {
		instance := created.DBInstance
		assert.True(t, aws.BoolValue(instance.AutoMinorVersionUpgrade))
		assert.True(t, aws.BoolValue(instance.PerformanceInsightsEnabled))
		assert.Equal(t, int64(60), aws.Int64Value(instance.MonitoringInterval))
		assert.True(t, aws.BoolValue(instance.CopyTagsToSnapshot))

		_, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier:      aws.String(probeID),
			EnablePerformanceInsights: aws.Bool(false),
			MonitoringInterval:        aws.Int64(0),
			CopyTagsToSnapshot:        aws.Bool(false),
		})
		require.NoError(t, err)
		instance, err = harness.DescribeDBInstance(f.AWS, probeID)
		require.NoError(t, err)
		assert.False(t, aws.BoolValue(instance.PerformanceInsightsEnabled))
		assert.Zero(t, aws.Int64Value(instance.MonitoringInterval))
		assert.False(t, aws.BoolValue(instance.CopyTagsToSnapshot))
	})

	t.Run("DuplicateIdentifierIsRefused", func(t *testing.T) {
		expectCreateRefused(t, f, f.AWS, "DBInstanceAlreadyExists", validCreateInput(probeID))
	})

	// Grow-only, and refused before the instance leaves its current state: a
	// rejected resize must never stop a running database.
	t.Run("StorageShrinkIsRefused", func(t *testing.T) {
		harness.ExpectError(t, "InvalidParameterCombination", func() error {
			_, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
				DBInstanceIdentifier: aws.String(probeID),
				AllocatedStorage:     aws.Int64(dbStorageGiB + 5),
				ApplyImmediately:     aws.Bool(true),
			})
			return err
		})
	})
}
