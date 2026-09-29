package handlers_rds

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func filter(name string, values ...string) []*rds.Filter {
	return []*rds.Filter{{Name: aws.String(name), Values: aws.StringSlice(values)}}
}

func TestReadFilters_AnswersAsAWSDoes(t *testing.T) {
	cases := map[string]struct {
		filters []*rds.Filter
		code    string
		msg     string
	}{
		"unknown name":  {filter("bogus", "x"), awserrors.ErrorInvalidParameterValue, "Unrecognized filter name: bogus"},
		"name case":     {filter("Engine", "x"), awserrors.ErrorInvalidParameterValue, "Unrecognized filter name: Engine"},
		"missing name":  {[]*rds.Filter{{Values: aws.StringSlice([]string{"x"})}}, awserrors.ErrorInvalidParameterValue, "Unrecognized filter name: null"},
		"nil entry":     {[]*rds.Filter{nil}, awserrors.ErrorInvalidParameterValue, "Unrecognized filter name: null"},
		"unknown first": {[]*rds.Filter{{Name: aws.String("bogus")}}, awserrors.ErrorInvalidParameterValue, "Unrecognized filter name: bogus"},
		"no values": {[]*rds.Filter{{Name: aws.String("engine")}}, awserrors.ErrorInvalidParameterCombination,
			"The values list cannot be null for the filter engine."},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ReadFilters(tc.filters, filterEngine)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.code)
			assert.Contains(t, err.Error(), tc.msg)
		})
	}

	got, err := ReadFilters(append(filter(filterEngine, "postgres"), filter(filterEngine, "mariadb")...), filterEngine)
	require.NoError(t, err)
	assert.Equal(t, []Filter{{filterEngine, []string{"postgres"}}, {filterEngine, []string{"mariadb"}}}, got)
}

// AWS applies parameter-name, matching names in any case, and accepts data-type
// without narrowing by it.
func TestDescribeDBParameters_FiltersOnParameterName(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(testParameterGroup), testAccountID)
	require.NoError(t, err)
	describe := func(filters []*rds.Filter) (*rds.DescribeDBParametersOutput, error) {
		return h.svc.DescribeDBParameters(t.Context(), &rds.DescribeDBParametersInput{
			DBParameterGroupName: aws.String(testParameterGroup),
			Filters:              filters,
			MaxRecords:           aws.Int64(100),
		}, testAccountID)
	}

	byName, err := describe(filter(filterParameterName, "nope", "WORK_MEM"))
	require.NoError(t, err)
	require.Len(t, byName.Parameters, 1)
	assert.Equal(t, "work_mem", aws.StringValue(byName.Parameters[0].ParameterName))

	all, err := describe(nil)
	require.NoError(t, err)
	byType, err := describe(filter(filterDataType, "nope"))
	require.NoError(t, err)
	assert.Len(t, byType.Parameters, len(all.Parameters))

	_, err = describe(filter("source", "user"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unrecognized filter name: source")
}

// On these calls AWS refuses a name it does not recognise but narrows by none
// of the names it does.
func TestRecognisedFiltersThatDoNotNarrow(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	input := validCreateInput()
	input.Tags = awsTags("env", "prod")
	_, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)
	h.svc.RecordEvent(t.Context(), testAccountID, EventSourceTypeDBInstance, testDBInstanceID,
		"DB instance stopped.", EventCategoryAvailability)
	arn := FormatARN(ResourceKindDBInstance, testRegion, testAccountID, testDBInstanceID)

	groups := func(filters []*rds.Filter) (int, error) {
		out, err := h.svc.DescribeDBParameterGroups(t.Context(), &rds.DescribeDBParameterGroupsInput{Filters: filters}, testAccountID)
		if err != nil {
			return 0, err
		}
		return len(out.DBParameterGroups), nil
	}
	tags := func(filters []*rds.Filter) (int, error) {
		out, err := h.svc.ListTagsForResource(t.Context(), &rds.ListTagsForResourceInput{ResourceName: aws.String(arn), Filters: filters}, testAccountID)
		if err != nil {
			return 0, err
		}
		return len(out.TagList), nil
	}
	events := func(filters []*rds.Filter) (int, error) {
		out, err := h.svc.DescribeEvents(t.Context(), &rds.DescribeEventsInput{Filters: filters}, testAccountID)
		if err != nil {
			return 0, err
		}
		return len(out.Events), nil
	}

	for name, tc := range map[string]struct {
		call       func([]*rds.Filter) (int, error)
		recognised [][]*rds.Filter
		unknown    string
	}{
		"DescribeDBParameterGroups": {groups,
			[][]*rds.Filter{filter(filterDBParameterGroupFamily, "mysql8.0"), filter(filterEngine, "mysql")}, "db-parameter-group-name"},
		"ListTagsForResource": {tags, [][]*rds.Filter{filter(filterResourceID, "nope")}, "tag-key"},
		"DescribeEvents": {events,
			[][]*rds.Filter{filter(filterEventCategory, "backup"), filter(filterDBInstanceID, "other-db")}, "source-id"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, err := tc.call(nil)
			require.NoError(t, err)
			require.NotZero(t, want)
			for _, filters := range tc.recognised {
				got, err := tc.call(filters)
				require.NoError(t, err)
				assert.Equal(t, want, got, aws.StringValue(filters[0].Name))
			}
			_, err = tc.call(filter(tc.unknown, "x"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "Unrecognized filter name: "+tc.unknown)
		})
	}
}

// AWS ignores Filters here entirely, even an unknown or malformed entry.
func TestDescribeDBSubnetGroups_IgnoresFilters(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	_, err := h.svc.DescribeDBSubnetGroups(t.Context(), &rds.DescribeDBSubnetGroupsInput{
		Filters: []*rds.Filter{{Name: aws.String("bogus")}, {Values: aws.StringSlice([]string{"x"})}},
	}, testAccountID)
	require.NoError(t, err)
}
