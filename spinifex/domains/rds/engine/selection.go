package engine

import (
	"slices"
	"strconv"
	"strings"
)

// The status every offered version carries. One version per engine and it is
// the one the AMI ships, so nothing is ever deprecated, pending or beta.
//
// Duplicated against handlers_rds's own copy deliberately: this one backs the
// selection filter here, that one backs the AWS SDK projection there.
const engineVersionStatusAvailable = "available"

// Every endpoint is a private VPC address, so a vpc=false filter matches
// nothing. Duplicated against handlers_rds's own copy for the same reason as
// engineVersionStatusAvailable above.
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
// The result is engine-owned; a caller that needs an AWS SDK projection builds
// it from these rather than this package depending on the SDK.
func EngineVersions(filter EngineVersionFilter) []Engine {
	out := make([]Engine, 0, len(engines))
	for _, name := range SupportedEngines() {
		e := engines[name]
		if !filter.matches(e) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// OrderableOption is one orderable engine/class combination that passed every
// filter and whose EC2 instance type runnable accepted, ready for a caller to
// project into its own output shape.
type OrderableOption struct {
	Engine          Engine
	DBInstanceClass string
}

// OrderableOptions is the cross product of the engines, their pinned version and
// the db.* classes, minus every class whose EC2 instance type runnable rejects.
// runnable is the cluster's own answer to "can a node run this", which is the
// difference between a class that validates and one that can actually launch.
func OrderableOptions(filter OrderableFilter, sizing Sizing, runnable func(instanceType string) bool) []OrderableOption {
	out := make([]OrderableOption, 0, len(engines)*len(dbInstanceClasses))
	for _, name := range SupportedEngines() {
		e := engines[name]
		if !filter.matchesEngine(e) {
			continue
		}
		for _, class := range SupportedInstanceClasses() {
			if !accepts(filter.DBInstanceClass, class) {
				continue
			}
			instanceType, err := sizing.InstanceTypeForClass(class)
			if err != nil || !runnable(instanceType) {
				continue
			}
			out = append(out, OrderableOption{Engine: e, DBInstanceClass: class})
		}
	}
	return out
}
