package instance

import (
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// Filter names, as AWS spells them. Each Describe call recognises its own subset.
const (
	filterDBClusterID            = "db-cluster-id"
	filterDBInstanceID           = "db-instance-id"
	filterDBParameterGroupFamily = "db-parameter-group-family"
	filterDBSnapshotID           = "db-snapshot-id"
	filterDataType               = "data-type"
	filterDbiResourceID          = "dbi-resource-id"
	filterDomain                 = "domain"
	filterEngine                 = "engine"
	filterEventCategory          = "event-category"
	filterParameterName          = "parameter-name"
	filterResourceID             = "resource-id"
	filterSnapshotType           = "snapshot-type"
	filterStatus                 = "status"
)

// Filter is one validated Filters entry.
type Filter struct {
	Name   string
	Values []string
}

// AWS refuses an empty value list on these names, each in its own words, and
// accepts one on any other name without narrowing by it.
var emptyValuesRefusals = map[string]string{
	filterDBInstanceID:  "The list of DB Instance Identifiers must not be empty.",
	filterParameterName: "The list of ParameterNames must not be empty.",
	filterSnapshotType:  "Invalid value '' for filter snapshot-type",
}

// ReadFilters validates Filters against the names one Describe call recognises,
// in AWS's order: an unknown name, compared case-sensitively, before an absent
// or empty value list.
func ReadFilters(filters []*rds.Filter, recognised ...string) ([]Filter, error) {
	out := make([]Filter, 0, len(filters))
	for _, entry := range filters {
		name := "null"
		if entry != nil && entry.Name != nil {
			name = *entry.Name
		}
		if entry == nil || entry.Name == nil || !slices.Contains(recognised, name) {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Unrecognized filter name: %s", name)
		}
		if entry.Values == nil {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterCombination,
				"The values list cannot be null for the filter %s.", name)
		}
		if len(entry.Values) == 0 {
			if msg, refused := emptyValuesRefusals[name]; refused {
				return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterCombination, "%s", msg)
			}
			continue
		}
		out = append(out, Filter{Name: name, Values: aws.StringValueSlice(entry.Values)})
	}
	return out, nil
}

// matchesFilters is AWS's rule: a row must match every filter, and matches one
// by carrying any of its values. field returns the row's value for a filter
// name; a name the row has no field for matches nothing.
func matchesFilters(filters []Filter, field func(name string) (string, bool)) bool {
	for _, filter := range filters {
		value, ok := field(filter.Name)
		if !ok || !slices.Contains(filter.Values, value) {
			return false
		}
	}
	return true
}
