package handlers_rds

import (
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
)

// The status every offered version carries. One version per engine and it is
// the one the AMI ships, so nothing is ever deprecated, pending or beta.
const engineVersionStatusAvailable = "available"

// The only network type offered. Advertising DUAL would promise an address
// family no subnet hands out.
const networkTypeIPv4 = "IPV4"

// Every endpoint is a private VPC address, so a vpc=false filter matches
// nothing. Named once so the filter and the reported field cannot drift apart.
const orderableVpc = true

// ValueSets is a conjunction of accepted-value sets, one per source: the typed
// parameter and each Filters entry that names the same field. A row must satisfy
// every set, so naming two different engines matches nothing rather than both.
type ValueSets [][]string

// AddParam constrains by a typed parameter. An omitted one narrows nothing,
// which is why an empty value is not recorded as a set that matches nothing.
func (v *ValueSets) AddParam(value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	*v = append(*v, []string{normaliseFilterValue(value)})
}

// AddFilter constrains by one Filters entry. Unlike AddParam an empty or
// unmatchable value is kept, because the caller wrote the filter deliberately.
func (v *ValueSets) AddFilter(values []string) {
	set := make([]string, 0, len(values))
	for _, value := range values {
		set = append(set, normaliseFilterValue(value))
	}
	*v = append(*v, set)
}

// A free function rather than a method, so ValueSets keeps the pointer receivers
// its two mutators need without mixing the two receiver kinds on one type.
func accepts(sets ValueSets, value string) bool {
	value = normaliseFilterValue(value)
	for _, set := range sets {
		if !slices.Contains(set, value) {
			return false
		}
	}
	return true
}

// Every value either side of the comparison is a lowercase identifier already,
// so folding here only makes a shouted filter work rather than widening a match.
func normaliseFilterValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// EngineVersionFilter narrows the engine-version catalog. Every field is
// optional; a zero filter returns every row.
type EngineVersionFilter struct {
	Engine               ValueSets
	EngineVersion        ValueSets
	ParameterGroupFamily ValueSets
	Status               ValueSets
}

func (f EngineVersionFilter) matches(e Engine) bool {
	return accepts(f.Engine, e.Name) &&
		accepts(f.EngineVersion, e.EngineVersion()) &&
		accepts(f.ParameterGroupFamily, e.ParameterGroupFamily()) &&
		accepts(f.Status, engineVersionStatusAvailable)
}

// OrderableFilter narrows the orderable-option catalog. Vpc is a value set like
// the rest, holding "true" or "false", so an unset filter stays distinct from
// one asking for non-VPC options.
type OrderableFilter struct {
	Engine          ValueSets
	EngineVersion   ValueSets
	DBInstanceClass ValueSets
	LicenseModel    ValueSets
	Vpc             ValueSets
}

func (f OrderableFilter) matchesEngine(e Engine) bool {
	return accepts(f.Engine, e.Name) &&
		accepts(f.EngineVersion, e.EngineVersion()) &&
		accepts(f.LicenseModel, e.licenseModel) &&
		accepts(f.Vpc, strconv.FormatBool(orderableVpc))
}

// EngineVersions is the engine half of the catalog: one row per engine, since
// v1 pins a single major each and that pin is the only version an AMI serves.
func EngineVersions(filter EngineVersionFilter) []*rds.DBEngineVersion {
	out := make([]*rds.DBEngineVersion, 0, len(engines))
	for _, name := range SupportedEngines() {
		engine := engines[name]
		if !filter.matches(engine) {
			continue
		}
		out = append(out, engine.describeVersion())
	}
	return out
}

// OrderableOptions is the cross product of the engines, their pinned version and
// the db.* classes, minus every class whose EC2 instance type runnable rejects.
// runnable is the cluster's own answer to "can a node run this", which is the
// difference between a class that validates and one that can actually launch.
func OrderableOptions(filter OrderableFilter, runnable func(instanceType string) bool) []*rds.OrderableDBInstanceOption {
	out := make([]*rds.OrderableDBInstanceOption, 0, len(engines)*len(dbInstanceClasses))
	for _, name := range SupportedEngines() {
		engine := engines[name]
		if !filter.matchesEngine(engine) {
			continue
		}
		for _, class := range SupportedInstanceClasses() {
			if !accepts(filter.DBInstanceClass, class) {
				continue
			}
			instanceType, err := InstanceTypeForClass(class)
			if err != nil || !runnable(instanceType) {
				continue
			}
			out = append(out, engine.orderableOption(class))
		}
	}
	return out
}

// Everything the platform has a truth for. Fields the SDK struct carries that
// nothing here answers — the custom-engine manifest, the CA identifiers, the
// installation-file locations — are left nil rather than guessed at.
func (e Engine) describeVersion() *rds.DBEngineVersion {
	return &rds.DBEngineVersion{
		Engine:                     aws.String(e.Name),
		EngineVersion:              aws.String(e.EngineVersion()),
		MajorEngineVersion:         aws.String(e.MajorVersion),
		DBParameterGroupFamily:     aws.String(e.ParameterGroupFamily()),
		DBEngineDescription:        aws.String(e.description),
		DBEngineVersionDescription: aws.String(e.description + " " + e.MajorVersion),
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
func (e Engine) orderableOption(class string) *rds.OrderableDBInstanceOption {
	return &rds.OrderableDBInstanceOption{
		Engine:          aws.String(e.Name),
		EngineVersion:   aws.String(e.EngineVersion()),
		DBInstanceClass: aws.String(class),
		LicenseModel:    aws.String(e.licenseModel),

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
