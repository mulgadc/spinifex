package gateway_rds

//test:in-package — shares newStubbedNATS, testCaller and testEnv with
// handler_test.go, which is in-package for the same reason: the stubbed NATS
// scaffolding every dispatch test needs is not reachable from outside.

import (
	"bytes"
	"encoding/xml"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/private/protocol/xml/xmlutil"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func describeEngineVersions(t *testing.T, query map[string]string) []*rds.DBEngineVersion {
	t.Helper()
	query["Action"] = "DescribeDBEngineVersions"
	body, err := Dispatch(t.Context(), "DescribeDBEngineVersions", query, nil, testCaller, testEnv)
	require.NoError(t, err)

	var out rds.DescribeDBEngineVersionsOutput
	require.NoError(t, xmlutil.UnmarshalXML(&out, xml.NewDecoder(bytes.NewReader(body)),
		"DescribeDBEngineVersionsResult"))
	return out.DBEngineVersions
}

func describeOrderable(t *testing.T, nc *nats.Conn, query map[string]string) []*rds.OrderableDBInstanceOption {
	t.Helper()
	query["Action"] = "DescribeOrderableDBInstanceOptions"
	body, err := Dispatch(t.Context(), "DescribeOrderableDBInstanceOptions", query, nc, testCaller, testEnv)
	require.NoError(t, err)

	var out rds.DescribeOrderableDBInstanceOptionsOutput
	require.NoError(t, xmlutil.UnmarshalXML(&out, xml.NewDecoder(bytes.NewReader(body)),
		"DescribeOrderableDBInstanceOptionsResult"))
	return out.OrderableDBInstanceOptions
}

func engineNames(rows []*rds.DBEngineVersion) []string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, aws.StringValue(row.Engine))
	}
	return names
}

func classNames(options []*rds.OrderableDBInstanceOption) []string {
	names := make([]string, 0, len(options))
	for _, option := range options {
		names = append(names, aws.StringValue(option.DBInstanceClass))
	}
	return names
}

// AWS nests each row inside its list, and the SDK's own unmarshaler will not
// find one written any other way.
func TestDispatch_DescribeDBEngineVersionsRendersTheNestedList(t *testing.T) {
	rows := describeEngineVersions(t, map[string]string{})
	require.Len(t, rows, len(handlers_rds.SupportedEngines()))
	assert.Equal(t, handlers_rds.SupportedEngines(), engineNames(rows))

	for _, row := range rows {
		assert.NotEmpty(t, aws.StringValue(row.EngineVersion))
		assert.NotEmpty(t, aws.StringValue(row.DBParameterGroupFamily))
		assert.Equal(t, "available", aws.StringValue(row.Status))
	}
}

func TestDispatch_DescribeOrderableDBInstanceOptionsRendersTheNestedList(t *testing.T) {
	options := describeOrderable(t, newStubbedNATS(t), map[string]string{"Engine": "postgres"})
	require.Len(t, options, len(handlers_rds.SupportedInstanceClasses()))
	assert.Equal(t, handlers_rds.SupportedInstanceClasses(), classNames(options))

	for _, option := range options {
		assert.Equal(t, "postgres", aws.StringValue(option.Engine))
		assert.Equal(t, "gp3", aws.StringValue(option.StorageType))
		assert.True(t, aws.BoolValue(option.Vpc))
		assert.Equal(t, []string{"IPV4"}, aws.StringValueSlice(option.SupportedNetworkTypes))
	}
}

func TestDescribeDBEngineVersions_AppliesTypedParametersAndFilters(t *testing.T) {
	cases := []struct {
		name  string
		query map[string]string
		want  []string
	}{
		{"engine parameter", map[string]string{"Engine": "postgres"}, []string{"postgres"}},
		{"engine filter", map[string]string{
			"Filters.Filter.1.Name": "engine", "Filters.Filter.1.Values.Value.1": "mariadb",
		}, []string{"mariadb"}},
		{"engine filter with two values", map[string]string{
			"Filters.Filter.1.Name":           "engine",
			"Filters.Filter.1.Values.Value.1": "postgres",
			"Filters.Filter.1.Values.Value.2": "mariadb",
		}, handlers_rds.SupportedEngines()},
		{"engine version parameter", map[string]string{"EngineVersion": "18"}, []string{"postgres"}},
		{"parameter group family parameter",
			map[string]string{"DBParameterGroupFamily": "mariadb11.8"}, []string{"mariadb"}},
		{"parameter group family filter", map[string]string{
			"Filters.Filter.1.Name": "db-parameter-group-family", "Filters.Filter.1.Values.Value.1": "postgres18",
		}, []string{"postgres"}},
		{"status filter", map[string]string{
			"Filters.Filter.1.Name": "status", "Filters.Filter.1.Values.Value.1": "available",
		}, handlers_rds.SupportedEngines()},
		// A query for a row that does not exist is empty; only a request to build
		// something that cannot exist is refused.
		{"unpinned version", map[string]string{"EngineVersion": "17"}, []string{}},
		{"unknown engine", map[string]string{"Engine": "oracle-ee"}, []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, engineNames(describeEngineVersions(t, tc.query)))
		})
	}
}

func TestDescribeOrderableDBInstanceOptions_AppliesTypedParametersAndFilters(t *testing.T) {
	nc := newStubbedNATS(t)
	all := handlers_rds.SupportedInstanceClasses()

	cases := []struct {
		name  string
		query map[string]string
		want  []string
	}{
		{"class parameter", map[string]string{"DBInstanceClass": "db.t3.micro"}, []string{"db.t3.micro"}},
		{"class filter", map[string]string{
			"Filters.Filter.1.Name": "db-instance-class", "Filters.Filter.1.Values.Value.1": "db.m5.large",
		}, []string{"db.m5.large"}},
		{"engine version parameter", map[string]string{"EngineVersion": "18"}, all},
		{"license model parameter", map[string]string{"LicenseModel": "postgresql-license"}, all},
		{"license model filter", map[string]string{
			"Filters.Filter.1.Name": "license-model", "Filters.Filter.1.Values.Value.1": "general-public-license",
		}, []string{}},
		{"vpc true", map[string]string{"Vpc": "true"}, all},
		// Every endpoint is a private VPC address, so there is no non-VPC option.
		{"vpc false", map[string]string{"Vpc": "false"}, []string{}},
		{"vpc filter false", map[string]string{
			"Filters.Filter.1.Name": "vpc", "Filters.Filter.1.Values.Value.1": "false",
		}, []string{}},
		// The typed parameter takes every spelling ParseBool does, so the filter
		// has to agree with it: "1" narrowing to nothing would be indistinguishable
		// from a cluster that runs none of the classes.
		{"vpc numeric parameter", map[string]string{"Vpc": "1"}, all},
		{"vpc numeric filter", map[string]string{
			"Filters.Filter.1.Name": "vpc", "Filters.Filter.1.Values.Value.1": "1",
		}, all},
		{"vpc numeric filter false", map[string]string{
			"Filters.Filter.1.Name": "vpc", "Filters.Filter.1.Values.Value.1": "0",
		}, []string{}},
		{"vpc shouted filter", map[string]string{
			"Filters.Filter.1.Name": "vpc", "Filters.Filter.1.Values.Value.1": "TRUE",
		}, all},
		{"unpinned version", map[string]string{"EngineVersion": "17"}, []string{}},
		{"unknown class", map[string]string{"DBInstanceClass": "db.r5.24xlarge"}, []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.query["Engine"] = "postgres"
			assert.Equal(t, tc.want, classNames(describeOrderable(t, nc, tc.query)))
		})
	}
}

// The two actions take disjoint typed parameters, so each has its own closed
// filter vocabulary and a name valid on the other one is still rejected.
func TestDescribeCatalogs_RejectAnUnrecognisedFilterName(t *testing.T) {
	cases := []struct {
		action string
		filter string
	}{
		{"DescribeDBEngineVersions", "not-a-filter"},
		{"DescribeDBEngineVersions", "db-instance-class"},
		{"DescribeDBEngineVersions", "license-model"},
		{"DescribeDBEngineVersions", "vpc"},
		{"DescribeOrderableDBInstanceOptions", "not-a-filter"},
		{"DescribeOrderableDBInstanceOptions", "db-parameter-group-family"},
		{"DescribeOrderableDBInstanceOptions", "status"},
	}

	nc := newStubbedNATS(t)
	for _, tc := range cases {
		t.Run(tc.action+"/"+tc.filter, func(t *testing.T) {
			_, err := Dispatch(t.Context(), tc.action, map[string]string{
				"Action": tc.action, "Engine": "postgres",
				"Filters.Filter.1.Name": tc.filter, "Filters.Filter.1.Values.Value.1": "anything",
			}, nc, testCaller, testEnv)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
			assert.Contains(t, err.Error(), tc.filter)
		})
	}
}

// A value the typed parameter would have refused outright is refused here too,
// rather than narrowing the catalog to nothing and reporting that as the answer.
func TestDescribeOrderableDBInstanceOptions_RejectANonBooleanVpcFilter(t *testing.T) {
	for _, value := range []string{"yes", "", "2"} {
		t.Run(value, func(t *testing.T) {
			_, err := Dispatch(t.Context(), "DescribeOrderableDBInstanceOptions", map[string]string{
				"Action": "DescribeOrderableDBInstanceOptions", "Engine": "postgres",
				"Filters.Filter.1.Name": "vpc", "Filters.Filter.1.Values.Value.1": value,
			}, newStubbedNATS(t), testCaller, testEnv)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		})
	}
}

func TestDescribeCatalogs_RejectMalformedFilters(t *testing.T) {
	cases := []struct {
		name  string
		query map[string]string
	}{
		{"no values", map[string]string{"Filters.Filter.1.Name": "engine"}},
		{"no name", map[string]string{"Filters.Filter.1.Values.Value.1": "postgres"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.query["Action"] = "DescribeDBEngineVersions"
			_, err := Dispatch(t.Context(), "DescribeDBEngineVersions", tc.query, nil, testCaller, testEnv)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		})
	}
}

// Neither action ever issues a Marker, so one in a request was fabricated by the
// caller. This diverges from the other RDS describes, which parse and ignore it,
// because those may one day paginate and these two never will.
func TestDescribeCatalogs_RejectAMarker(t *testing.T) {
	nc := newStubbedNATS(t)
	for _, action := range []string{"DescribeDBEngineVersions", "DescribeOrderableDBInstanceOptions"} {
		t.Run(action, func(t *testing.T) {
			_, err := Dispatch(t.Context(), action, map[string]string{
				"Action": action, "Engine": "postgres", "Marker": "page-two",
			}, nc, testCaller, testEnv)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
			assert.Contains(t, err.Error(), "Marker")
		})
	}
}

func TestDescribeOrderableDBInstanceOptions_RequiresAKnownEngine(t *testing.T) {
	cases := []struct {
		name  string
		query map[string]string
		want  string
	}{
		{"absent", map[string]string{}, awserrors.ErrorMissingParameter},
		{"blank", map[string]string{"Engine": "  "}, awserrors.ErrorMissingParameter},
		{"unknown", map[string]string{"Engine": "oracle-ee"}, awserrors.ErrorInvalidParameterValue},
		{"mysql is not an alias for mariadb", map[string]string{"Engine": "mysql"},
			awserrors.ErrorInvalidParameterValue},
		{"availability zone group", map[string]string{"Engine": "postgres", "AvailabilityZoneGroup": "az-group-1"},
			awserrors.ErrorInvalidParameterValue},
	}

	nc := newStubbedNATS(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.query["Action"] = "DescribeOrderableDBInstanceOptions"
			_, err := Dispatch(t.Context(), "DescribeOrderableDBInstanceOptions", tc.query, nc, testCaller, testEnv)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// The four booleans are accepted as provable identities on this catalog rather
// than parsed and dropped, and this is what keeps the claim provable: adding a
// second version per engine, or populating either list, fails here and forces
// the parameter to be implemented rather than left inert.
func TestDescribeDBEngineVersions_TheFourBooleansAreProvableNoOps(t *testing.T) {
	// Compared as decoded responses rather than byte-wise: BuildXML emits sibling
	// elements in map order, so a row's own members reorder between two calls that
	// returned exactly the same rows.
	baseline := describeEngineVersions(t, map[string]string{})

	booleans := []string{"DefaultOnly", "IncludeAll", "ListSupportedCharacterSets", "ListSupportedTimezones"}
	for _, name := range booleans {
		for _, value := range []string{"true", "false"} {
			t.Run(name+"="+value, func(t *testing.T) {
				assert.Equal(t, baseline, describeEngineVersions(t, map[string]string{name: value}))
			})
		}
	}

	// The conditions that make each identity hold, so the assertion above cannot
	// stay true by accident once the catalog grows.
	rows := baseline
	byEngine := map[string]int{}
	for _, row := range rows {
		byEngine[aws.StringValue(row.Engine)]++
		assert.Equal(t, "available", aws.StringValue(row.Status),
			"IncludeAll cannot widen a set only while every version is available")
		assert.Empty(t, row.SupportedCharacterSets,
			"ListSupportedCharacterSets is an identity only while the list is empty")
		assert.Empty(t, row.SupportedTimezones,
			"ListSupportedTimezones is an identity only while the list is empty")
	}
	for engine, count := range byEngine {
		assert.Equal(t, 1, count,
			"DefaultOnly cannot narrow a set only while %s has exactly one version", engine)
	}
}

// A stubbed cluster that answers nothing must not fall back to the static class
// list: reporting classes that cannot launch is the failure this probe prevents.
func TestDescribeOrderableDBInstanceOptions_ASilentClusterIsServerInternal(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	_, err := Dispatch(t.Context(), "DescribeOrderableDBInstanceOptions", map[string]string{
		"Action": "DescribeOrderableDBInstanceOptions", "Engine": "postgres",
	}, nc, testCaller, testEnv)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// A cluster whose nodes answered but run none of the db.* types is an empty
// option list, which is the truthful answer and not the same case as above.
func TestDescribeOrderableDBInstanceOptions_AnArmOnlyClusterOffersNothing(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	respondWith(t, nc, "ec2.DescribeInstanceTypes", &ec2.DescribeInstanceTypesOutput{
		InstanceTypes: instanceTypeInfo("t4g.micro", "t4g.small", "m6g.large"),
	})

	assert.Empty(t, describeOrderable(t, nc, map[string]string{"Engine": "postgres"}))
}

// Only the classes the nodes report are offered, so a class that would fail at
// launch is never in the answer.
func TestDescribeOrderableDBInstanceOptions_OffersOnlyWhatTheNodesRun(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	respondWith(t, nc, "ec2.DescribeInstanceTypes", &ec2.DescribeInstanceTypesOutput{
		InstanceTypes: instanceTypeInfo("t3.micro", "t3.small", "t4g.micro", "m6g.large"),
	})

	assert.Equal(t, []string{"db.t3.micro", "db.t3.small"},
		classNames(describeOrderable(t, nc, map[string]string{"Engine": "postgres"})))
}
