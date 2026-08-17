package gateway_rds

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// The filter names each action recognises. Rejecting an unknown name is only
// implementable against a closed vocabulary, and the two actions take disjoint
// typed parameters, so each keeps its own list rather than sharing one.
const (
	filterNameEngine               = "engine"
	filterNameEngineVersion        = "engine-version"
	filterNameParameterGroupFamily = "db-parameter-group-family"
	filterNameStatus               = "status"
	filterNameDBInstanceClass      = "db-instance-class"
	filterNameLicenseModel         = "license-model"
	filterNameVpc                  = "vpc"
)

var (
	engineVersionFilterNames = []string{
		filterNameEngine, filterNameEngineVersion, filterNameParameterGroupFamily, filterNameStatus,
	}
	orderableFilterNames = []string{
		filterNameEngine, filterNameEngineVersion, filterNameDBInstanceClass, filterNameLicenseModel, filterNameVpc,
	}
)

// The engine catalog is static, so this reads it with no I/O at all: Engine is a
// filter here rather than a required parameter, and an unknown one is an empty
// list rather than the rejection create-db-instance gives it.
func DescribeDBEngineVersions(ctx context.Context, input *rds.DescribeDBEngineVersionsInput, _ *nats.Conn, _ Caller) (any, error) {
	if err := rejectMarker(input.Marker, "DescribeDBEngineVersions"); err != nil {
		return nil, err
	}

	filter := handlers_rds.EngineVersionFilter{}
	filter.Engine.AddParam(aws.StringValue(input.Engine))
	filter.EngineVersion.AddParam(aws.StringValue(input.EngineVersion))
	filter.ParameterGroupFamily.AddParam(aws.StringValue(input.DBParameterGroupFamily))

	for _, entry := range input.Filters {
		name, values, err := filterEntry(entry)
		if err != nil {
			return nil, err
		}
		switch name {
		case filterNameEngine:
			filter.Engine.AddFilter(values)
		case filterNameEngineVersion:
			filter.EngineVersion.AddFilter(values)
		case filterNameParameterGroupFamily:
			filter.ParameterGroupFamily.AddFilter(values)
		case filterNameStatus:
			filter.Status.AddFilter(values)
		default:
			return nil, unknownFilterName(name, engineVersionFilterNames)
		}
	}

	// DefaultOnly, IncludeAll, ListSupportedCharacterSets and ListSupportedTimezones
	// are accepted and not read: each is an identity on a catalog of one available
	// version per engine with no character-set or timezone list to populate.
	return &rds.DescribeDBEngineVersionsOutput{DBEngineVersions: handlers_rds.EngineVersions(filter)}, nil
}

// Engine is required, so an absent one is MissingParameter and an unknown one is
// the InvalidParameterValue LookupEngine already words. Everything else narrows.
func DescribeOrderableDBInstanceOptions(ctx context.Context, input *rds.DescribeOrderableDBInstanceOptionsInput, nc *nats.Conn, _ Caller, env Env) (any, error) {
	if err := rejectMarker(input.Marker, "DescribeOrderableDBInstanceOptions"); err != nil {
		return nil, err
	}
	if aws.StringValue(input.AvailabilityZoneGroup) != "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"AvailabilityZoneGroup is not supported: this platform exposes a single zone and names none")
	}
	if strings.TrimSpace(aws.StringValue(input.Engine)) == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "Engine is required")
	}
	engine, err := handlers_rds.LookupEngine(aws.StringValue(input.Engine))
	if err != nil {
		return nil, err
	}

	filter := handlers_rds.OrderableFilter{}
	filter.Engine.AddParam(engine.Name)
	filter.EngineVersion.AddParam(aws.StringValue(input.EngineVersion))
	filter.DBInstanceClass.AddParam(aws.StringValue(input.DBInstanceClass))
	filter.LicenseModel.AddParam(aws.StringValue(input.LicenseModel))
	if input.Vpc != nil {
		filter.Vpc.AddParam(strconv.FormatBool(aws.BoolValue(input.Vpc)))
	}

	for _, entry := range input.Filters {
		name, values, err := filterEntry(entry)
		if err != nil {
			return nil, err
		}
		switch name {
		case filterNameEngine:
			filter.Engine.AddFilter(values)
		case filterNameEngineVersion:
			filter.EngineVersion.AddFilter(values)
		case filterNameDBInstanceClass:
			filter.DBInstanceClass.AddFilter(values)
		case filterNameLicenseModel:
			filter.LicenseModel.AddFilter(values)
		case filterNameVpc:
			filter.Vpc.AddFilter(values)
		default:
			return nil, unknownFilterName(name, orderableFilterNames)
		}
	}

	runnable, err := clusterRunnableTypes(ctx, nc, env)
	if err != nil {
		return nil, err
	}
	return &rds.DescribeOrderableDBInstanceOptionsOutput{
		OrderableDBInstanceOptions: handlers_rds.OrderableOptions(filter, runnable),
	}, nil
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
	out, err := gateway_ec2_instance.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{},
		nc, env.ExpectedNodes, utils.GlobalAccountID)
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

// Neither action ever issues a Marker, so one in a request can only have been
// fabricated. Answering it as page one would report the whole catalog as if it
// were a later page.
func rejectMarker(marker *string, action string) error {
	if aws.StringValue(marker) == "" {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"Marker is not supported: %s returns every row in a single page", action)
}

// Both members are required by the shape, and treating either as absent would
// silently widen the result rather than narrow it.
func filterEntry(entry *rds.Filter) (string, []string, error) {
	if entry == nil || strings.TrimSpace(aws.StringValue(entry.Name)) == "" {
		return "", nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "each filter must carry a name")
	}
	name := strings.ToLower(strings.TrimSpace(aws.StringValue(entry.Name)))
	if len(entry.Values) == 0 {
		return "", nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"filter %q must carry at least one value", name)
	}
	return name, aws.StringValueSlice(entry.Values), nil
}

// Rejected rather than ignored: these two actions exist only to be filtered, so
// a dropped filter returns rows the caller asked not to see and cannot detect.
func unknownFilterName(name string, recognised []string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"filter %q is not recognised; supported filters are %s", name, strings.Join(recognised, ", "))
}
