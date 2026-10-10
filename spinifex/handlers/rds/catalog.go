package handlers_rds

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
)

// The status every offered version carries. One version per engine and it is
// the one the AMI ships, so nothing is ever deprecated, pending or beta.
//
// Duplicated against the engine package's own copy deliberately: that one
// backs the selection filter there, this one backs the SDK projection here.
const engineVersionStatusAvailable = "available"

// The only network type offered. Advertising DUAL would promise an address
// family no subnet hands out.
const networkTypeIPv4 = "IPV4"

// Every endpoint is a private VPC address, so a vpc=false filter matches
// nothing. Duplicated against the engine package's own copy for the same
// reason as engineVersionStatusAvailable above.
const orderableVpc = true

// EngineVersions is the AWS SDK projection of the engine package's own
// selection: it builds the DBEngineVersion rows this handler reports, from the
// engine-owned result the selection itself does not depend on the SDK for.
func EngineVersions(filter rdsengine.EngineVersionFilter) []*rds.DBEngineVersion {
	engines := rdsengine.EngineVersions(filter)
	out := make([]*rds.DBEngineVersion, 0, len(engines))
	for _, e := range engines {
		out = append(out, describeVersion(e))
	}
	return out
}

// OrderableOptions is the AWS SDK projection of the engine package's own
// selection, for the same reason as EngineVersions above.
func OrderableOptions(filter rdsengine.OrderableFilter, sizing rdsengine.Sizing, runnable func(instanceType string) bool) []*rds.OrderableDBInstanceOption {
	options := rdsengine.OrderableOptions(filter, sizing, runnable)
	out := make([]*rds.OrderableDBInstanceOption, 0, len(options))
	for _, option := range options {
		out = append(out, orderableOption(option.Engine, option.DBInstanceClass))
	}
	return out
}

// Everything the platform has a truth for. Fields the SDK struct carries that
// nothing here answers — the custom-engine manifest, the CA identifiers, the
// installation-file locations — are left nil rather than guessed at.
func describeVersion(e rdsengine.Engine) *rds.DBEngineVersion {
	return &rds.DBEngineVersion{
		Engine:                     aws.String(e.Name),
		EngineVersion:              aws.String(e.EngineVersion()),
		MajorEngineVersion:         aws.String(e.MajorVersion),
		DBParameterGroupFamily:     aws.String(e.ParameterGroupFamily()),
		DBEngineDescription:        aws.String(e.Description()),
		DBEngineVersionDescription: aws.String(e.Description() + " " + e.MajorVersion),
		Status:                     aws.String(engineVersionStatusAvailable),

		// Empty rather than absent: there is no in-place upgrade to target, no log
		// type to export, no engine mode to select, and no engine-selectable
		// character set or timezone — the timezone knob lives in the parameter
		// catalog, keyed by family, not here.
		ValidUpgradeTarget:     []*rds.UpgradeTarget{},
		SupportedFeatureNames:  []*string{},
		ExportableLogTypes:     []*string{},
		SupportedEngineModes:   []*string{},
		SupportedCharacterSets: []*rds.CharacterSet{},
		SupportedTimezones:     []*rds.Timezone{},

		SupportsReadReplica:                       aws.Bool(false),
		SupportsGlobalDatabases:                   aws.Bool(false),
		SupportsLogExportsToCloudwatchLogs:        aws.Bool(false),
		SupportsParallelQuery:                     aws.Bool(false),
		SupportsBabelfish:                         aws.Bool(false),
		SupportsIntegrations:                      aws.Bool(false),
		SupportsLimitlessDatabase:                 aws.Bool(false),
		SupportsLocalWriteForwarding:              aws.Bool(false),
		SupportsCertificateRotationWithoutRestart: aws.Bool(false),
	}
}

// Every false below restates a line of rejectUnimplemented or of the
// accepted-but-inert set, so an option cannot advertise a capability the create
// path refuses.
func orderableOption(e rdsengine.Engine, class string) *rds.OrderableDBInstanceOption {
	return &rds.OrderableDBInstanceOption{
		Engine:          aws.String(e.Name),
		EngineVersion:   aws.String(e.EngineVersion()),
		DBInstanceClass: aws.String(class),
		LicenseModel:    aws.String(e.LicenseModel()),

		StorageType:               aws.String(storageTypeGP3),
		MinStorageSize:            aws.Int64(minAllocatedStorageGiB),
		MaxStorageSize:            aws.Int64(maxAllocatedStorageGiB),
		SupportsStorageEncryption: aws.Bool(true),

		Vpc:                   aws.Bool(orderableVpc),
		SupportedNetworkTypes: []*string{aws.String(networkTypeIPv4)},

		// Both are empty by design. AvailabilityZone is a rejected create
		// parameter, so naming a zone would invite a request that fails; and
		// ProcessorFeatures is accepted and ignored, so advertising one would
		// promise a knob that does nothing.
		AvailabilityZones:          []*rds.AvailabilityZone{},
		AvailableProcessorFeatures: []*rds.AvailableProcessorFeature{},

		MultiAZCapable:                    aws.Bool(false),
		ReadReplicaCapable:                aws.Bool(false),
		SupportsIops:                      aws.Bool(false),
		SupportsStorageThroughput:         aws.Bool(false),
		SupportsStorageAutoscaling:        aws.Bool(false),
		SupportsIAMDatabaseAuthentication: aws.Bool(false),
		SupportsEnhancedMonitoring:        aws.Bool(false),
		SupportsPerformanceInsights:       aws.Bool(false),
		SupportsKerberosAuthentication:    aws.Bool(false),
		SupportsGlobalDatabases:           aws.Bool(false),
		SupportsClusters:                  aws.Bool(false),
		OutpostCapable:                    aws.Bool(false),
		SupportsDedicatedLogVolume:        aws.Bool(false),
	}
}
