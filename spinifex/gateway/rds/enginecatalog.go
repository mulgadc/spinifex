package gateway_rds

import (
	"context"
	"errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"log/slog"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/nats-io/nats.go"
)

// The filter names each action recognises, compared case-sensitively as AWS
// does. The two actions take disjoint typed parameters, so each keeps its own list.
const (
	filterNameEngine               = "engine"
	filterNameEngineVersion        = "engine-version"
	filterNameParameterGroupFamily = "db-parameter-group-family"
	filterNameStatus               = "status"
	filterNameDBInstanceClass      = "db-instance-class"
	filterNameLicenseModel         = "license-model"
	filterNameVpc                  = "vpc"
)

// rdsSizing is this gateway's composition of the engine package's class-to-
// memory projection, built once from the platform's instance-type table.
var rdsSizing = handlers_rds.InstanceSizing()

var (
	engineVersionFilterNames = []string{
		filterNameEngine, filterNameEngineVersion, filterNameParameterGroupFamily, filterNameStatus,
	}
	orderableFilterNames = []string{
		filterNameEngine, filterNameEngineVersion, filterNameDBInstanceClass, filterNameLicenseModel, filterNameVpc,
	}
)

// DescribeDBEngineVersions reads the static engine catalog with no I/O: Engine is a filter here
// rather than a required parameter, and an unknown one is an empty list rather than the rejection
// create-db-instance gives it.
func DescribeDBEngineVersions(ctx context.Context, input *rds.DescribeDBEngineVersionsInput, _ *nats.Conn, _ Caller) (any, error) {
	filter := rdsengine.EngineVersionFilter{}
	filter.Engine.AddParam(aws.StringValue(input.Engine))
	filter.EngineVersion.AddParam(aws.StringValue(input.EngineVersion))
	filter.ParameterGroupFamily.AddParam(aws.StringValue(input.DBParameterGroupFamily))

	entries, err := handlers_rds.ReadFilters(input.Filters, engineVersionFilterNames...)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		switch entry.Name {
		case filterNameEngine:
			filter.Engine.AddFilter(entry.Values)
		case filterNameEngineVersion:
			filter.EngineVersion.AddFilter(entry.Values)
		case filterNameParameterGroupFamily:
			filter.ParameterGroupFamily.AddFilter(entry.Values)
		case filterNameStatus:
			filter.Status.AddFilter(entry.Values)
		}
	}

	// DefaultOnly, IncludeAll, ListSupportedCharacterSets and ListSupportedTimezones
	// are accepted and not read: each is an identity on a catalog of one available
	// version per engine with no character-set or timezone list to populate.
	versions, marker, err := handlers_rds.Page(handlers_rds.EngineVersions(filter),
		handlers_rds.EngineVersionPageKey, input.MaxRecords, input.Marker)
	if err != nil {
		return nil, err
	}
	return &rds.DescribeDBEngineVersionsOutput{DBEngineVersions: versions, Marker: marker}, nil
}

// DescribeOrderableDBInstanceOptions requires Engine, so an absent one is MissingParameter and an
// unknown one is the InvalidParameterValue LookupEngine already words. Everything else narrows.
func DescribeOrderableDBInstanceOptions(ctx context.Context, input *rds.DescribeOrderableDBInstanceOptionsInput, nc *nats.Conn, _ Caller, env Env) (any, error) {
	if aws.StringValue(input.AvailabilityZoneGroup) != "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"AvailabilityZoneGroup is not supported: this platform exposes a single zone and names none")
	}
	if strings.TrimSpace(aws.StringValue(input.Engine)) == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "Engine is required")
	}
	engine, err := rdsengine.LookupEngine(aws.StringValue(input.Engine))
	if err != nil {
		return nil, err
	}

	filter := rdsengine.OrderableFilter{}
	filter.Engine.AddParam(engine.Name)
	filter.EngineVersion.AddParam(aws.StringValue(input.EngineVersion))
	filter.DBInstanceClass.AddParam(aws.StringValue(input.DBInstanceClass))
	filter.LicenseModel.AddParam(aws.StringValue(input.LicenseModel))
	if input.Vpc != nil {
		filter.Vpc.AddParam(strconv.FormatBool(aws.BoolValue(input.Vpc)))
	}

	entries, err := handlers_rds.ReadFilters(input.Filters, orderableFilterNames...)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		switch entry.Name {
		case filterNameEngine:
			filter.Engine.AddFilter(entry.Values)
		case filterNameEngineVersion:
			filter.EngineVersion.AddFilter(entry.Values)
		case filterNameDBInstanceClass:
			filter.DBInstanceClass.AddFilter(entry.Values)
		case filterNameLicenseModel:
			filter.LicenseModel.AddFilter(entry.Values)
		case filterNameVpc:
			parsed, perr := boolFilterValues(entry.Name, entry.Values)
			if perr != nil {
				return nil, perr
			}
			filter.Vpc.AddFilter(parsed)
		}
	}

	runnable, err := clusterRunnableTypes(ctx, nc, env)
	if err != nil {
		return nil, err
	}
	options, marker, err := handlers_rds.Page(handlers_rds.OrderableOptions(filter, rdsSizing, runnable),
		handlers_rds.OrderableOptionPageKey, input.MaxRecords, input.Marker)
	if err != nil {
		return nil, err
	}
	return &rds.DescribeOrderableDBInstanceOptionsOutput{OrderableDBInstanceOptions: options, Marker: marker}, nil
}

// Which EC2 instance types the cluster's nodes report they can run, as a
// membership test. Capacity is deliberately not consulted: an exhausted cluster
// still offers the class, and a create that cannot be placed is a create-time
// failure rather than a class the region does not have.
//
// The probe names no instance type, because the union is what tells a cluster
// that answered nothing apart from one whose nodes run none of the db.* classes:
// DescribeInstanceTypes filters its reply by the requested types and reports a
// timed-out gather as an empty list with no error, so asking for the six would
// collapse both onto the same answer.
func clusterRunnableTypes(ctx context.Context, nc *nats.Conn, env Env) (func(string) bool, error) {
	out, err := ec2instanceapi.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{},
		nc, env.ExpectedNodes, nil, awsidentifiers.GlobalAccountID)
	if err != nil {
		slog.ErrorContext(ctx, "RDS: instance-type capability probe failed", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	supported := make(map[string]bool, len(out.InstanceTypes))
	for _, info := range out.InstanceTypes {
		if info != nil && info.InstanceType != nil {
			supported[*info.InstanceType] = true
		}
	}
	// No node answered. Falling back to the full class list here would offer
	// classes that cannot launch, which is the failure this probe exists to stop.
	if len(supported) == 0 {
		slog.ErrorContext(ctx, "RDS: no node reported any instance type")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return func(instanceType string) bool { return supported[instanceType] }, nil
}

// A bool-shaped filter is parsed rather than compared as text, so it accepts
// every spelling the typed parameter does. Matching only "true" and "false"
// would answer "1" with an empty catalog the caller cannot tell from a real one.
func boolFilterValues(name string, values []string) ([]string, error) {
	parsed := make([]string, 0, len(values))
	for _, value := range values {
		b, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"filter %q takes a boolean, not %q", name, value)
		}
		parsed = append(parsed, strconv.FormatBool(b))
	}
	return parsed, nil
}
