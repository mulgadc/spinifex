package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go/jetstream"
)

// AWS's cap on one ModifyDBParameterGroup call.
const maxParametersPerModify = 20

// The prefix AWS reserves for the groups the service itself owns. A group named
// with it is reported and resolvable but never modifiable or deletable.
const defaultParameterGroupPrefix = "default."

// Creates a customer-owned parameter group. It starts empty: every value is a
// catalog default until the customer overrides one, so a fresh group and the
// default group resolve to the same effective set.
func (s *Service) CreateDBParameterGroup(ctx context.Context, input *rds.CreateDBParameterGroupInput, accountID string) (*rds.CreateDBParameterGroupOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	name := aws.StringValue(input.DBParameterGroupName)
	if err := validateDBGroupName("DBParameterGroupName", name); err != nil {
		return nil, err
	}
	// Rejected rather than accepted-and-shadowed: a customer group under the
	// reserved prefix would be indistinguishable from the implicit one.
	if isDefaultParameterGroupName(name) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DBParameterGroupName may not begin with %q, which the service reserves", defaultParameterGroupPrefix)
	}
	description := aws.StringValue(input.Description)
	if err := validateDBGroupDescription("Description", description); err != nil {
		return nil, err
	}
	family, err := validateParameterGroupFamily(aws.StringValue(input.DBParameterGroupFamily))
	if err != nil {
		return nil, err
	}
	tags, err := validateTags(input.Tags)
	if err != nil {
		return nil, err
	}

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := DBParameterGroupRecord{
		Name:        name,
		AccountID:   accountID,
		Family:      family,
		Description: description,
		Tags:        tags,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := createJSON(ctx, kv, DBParameterGroupMetaKey(name), &rec); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, awserrors.Errorf(awserrors.ErrorDBParameterGroupAlreadyExists,
				"DB parameter group %s already exists", name)
		}
		return nil, err
	}

	slog.InfoContext(ctx, "rds: DB parameter group created",
		"dbParameterGroup", name, "accountId", accountID, "family", family)
	return &rds.CreateDBParameterGroupOutput{DBParameterGroup: s.projectParameterGroupRecord(&rec)}, nil
}

// The implicit default group is reported alongside the customer's own, whether
// or not anything has materialised it yet — a client that lists groups before
// creating its first instance must still see the one it can name.
func (s *Service) DescribeDBParameterGroups(ctx context.Context, input *rds.DescribeDBParameterGroupsInput, accountID string) (*rds.DescribeDBParameterGroupsOutput, error) {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if input != nil {
		if name := aws.StringValue(input.DBParameterGroupName); name != "" {
			rec, _, err := getDBParameterGroup(ctx, kv, accountID, name)
			if err != nil {
				return nil, err
			}
			return &rds.DescribeDBParameterGroupsOutput{
				DBParameterGroups: []*rds.DBParameterGroup{s.projectParameterGroupRecord(rec)},
			}, nil
		}
	}

	names, err := ListDBParameterGroupNames(ctx, kv)
	if err != nil {
		return nil, err
	}
	// The default group is synthesised rather than read, so it appears exactly
	// once whether or not a prior create persisted it.
	for _, engine := range engines {
		names = append(names, engine.DefaultParameterGroupName())
	}
	slices.Sort(names)
	names = slices.Compact(names)

	groups := make([]*rds.DBParameterGroup, 0, len(names))
	for _, name := range names {
		rec, _, err := getDBParameterGroup(ctx, kv, accountID, name)
		if err != nil {
			// Deleted between the listing and this read; the same answer a describe
			// one tick later would give.
			if strings.HasPrefix(err.Error(), awserrors.ErrorDBParameterGroupNotFound) {
				continue
			}
			return nil, err
		}
		groups = append(groups, s.projectParameterGroupRecord(rec))
	}
	return &rds.DescribeDBParameterGroupsOutput{DBParameterGroups: groups}, nil
}

// Stores validated overrides, one KV key per parameter, so a modify touching one
// setting cannot clobber a concurrent change to another. The whole request
// is validated before anything is written: a batch with one bad value must leave
// the group exactly as it was rather than half-applied.
func (s *Service) ModifyDBParameterGroup(ctx context.Context, input *rds.ModifyDBParameterGroupInput, accountID string) (*rds.DBParameterGroupNameMessage, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	name := aws.StringValue(input.DBParameterGroupName)
	if name == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBParameterGroupName is required")
	}
	if isDefaultParameterGroupName(name) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DB parameter group %s is a default group and cannot be modified", name)
	}
	if len(input.Parameters) == 0 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Parameters must name at least one parameter")
	}
	if len(input.Parameters) > maxParametersPerModify {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"at most %d parameters may be modified in one request, got %d", maxParametersPerModify, len(input.Parameters))
	}

	updates, err := validateParameterUpdates(input.Parameters)
	if err != nil {
		return nil, err
	}

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if _, _, err := getDBParameterGroup(ctx, kv, accountID, name); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	for _, update := range updates {
		if err := putJSON(ctx, kv, DBParameterGroupParamKey(name, update.Name), &DBParameterRecord{
			Name:        update.Name,
			Value:       update.Value,
			ApplyMethod: update.ApplyMethod,
			UpdatedAt:   now,
		}); err != nil {
			return nil, err
		}
	}

	slog.InfoContext(ctx, "rds: DB parameter group modified",
		"dbParameterGroup", name, "accountId", accountID, "parameters", len(updates))
	return &rds.DBParameterGroupNameMessage{DBParameterGroupName: aws.String(name)}, nil
}

// Merges catalog defaults with the group's stored overrides. The defaults are
// evaluated at the smallest supported class, because a parameter group is not
// bound to an instance: a customer reading a group's values before creating
// anything has to see literals, and formulas stay off this surface.
func (s *Service) DescribeDBParameters(ctx context.Context, input *rds.DescribeDBParametersInput, accountID string) (*rds.DescribeDBParametersOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	name := aws.StringValue(input.DBParameterGroupName)
	if name == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBParameterGroupName is required")
	}
	source := strings.ToLower(strings.TrimSpace(aws.StringValue(input.Source)))
	switch source {
	case "", ParameterSourceUser, ParameterSourceEngineDefault:
	default:
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Source %q is not one of %s or %s", source, ParameterSourceUser, ParameterSourceEngineDefault)
	}

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if _, _, err := getDBParameterGroup(ctx, kv, accountID, name); err != nil {
		return nil, err
	}
	overrides, err := ListDBParameterOverrides(ctx, kv, name)
	if err != nil {
		return nil, err
	}
	memoryMiB, err := classMemoryMiB(smallestInstanceClass())
	if err != nil {
		return nil, err
	}

	out := &rds.DescribeDBParametersOutput{}
	for _, param := range CatalogParameterNames() {
		spec, _ := LookupParameter(param)
		override, isOverride := overrides[param]
		value := override.Value
		if !isOverride {
			value = spec.DefaultAt(memoryMiB)
		}
		if source != "" && source != parameterSource(isOverride) {
			continue
		}
		out.Parameters = append(out.Parameters, projectParameter(spec, value, override.ApplyMethod, isOverride))
	}
	return out, nil
}

// Refused for a default group, and while any instance still references it —
// including one that is only deleting, so a destroy that races the teardown
// fails cleanly rather than leaving a live engine's configuration unreadable.
func (s *Service) DeleteDBParameterGroup(ctx context.Context, input *rds.DeleteDBParameterGroupInput, accountID string) (*rds.DeleteDBParameterGroupOutput, error) {
	if input == nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "empty request")
	}
	name := aws.StringValue(input.DBParameterGroupName)
	if name == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "DBParameterGroupName is required")
	}
	if isDefaultParameterGroupName(name) {
		return nil, awserrors.Errorf(awserrors.ErrorDBParameterGroupInvalidState,
			"DB parameter group %s is a default group and cannot be deleted", name)
	}

	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if _, _, err := getDBParameterGroup(ctx, kv, accountID, name); err != nil {
		return nil, err
	}
	users, err := instancesUsingGroup(ctx, kv, func(rec *DBInstanceRecord) bool {
		return rec.DBParameterGroupName == name ||
			(rec.PendingModifiedValues != nil && rec.PendingModifiedValues.DBParameterGroupName == name)
	})
	if err != nil {
		return nil, err
	}
	if len(users) > 0 {
		return nil, awserrors.Errorf(awserrors.ErrorDBParameterGroupInvalidState,
			"DB parameter group %s is still used by %s", name, strings.Join(users, ", "))
	}

	// The values go first: a crash between the two leaves orphaned parameter keys
	// under a name a later create would then inherit silently.
	overrides, err := ListDBParameterOverrides(ctx, kv, name)
	if err != nil {
		return nil, err
	}
	for _, param := range slices.Sorted(maps.Keys(overrides)) {
		if err := kv.Delete(ctx, DBParameterGroupParamKey(name, param)); err != nil {
			return nil, fmt.Errorf("rds: delete parameter %s of group %s: %w", param, name, err)
		}
	}
	if err := kv.Delete(ctx, DBParameterGroupMetaKey(name)); err != nil {
		return nil, fmt.Errorf("rds: delete DB parameter group %s: %w", name, err)
	}

	slog.InfoContext(ctx, "rds: DB parameter group deleted", "dbParameterGroup", name, "accountId", accountID)
	return &rds.DeleteDBParameterGroupOutput{}, nil
}

// One validated override from a ModifyDBParameterGroup request.
type parameterUpdate struct {
	Name        string
	Value       string
	ApplyMethod string
}

// Every entry is checked before any is written. A name repeated in one request
// keeps its last value, as AWS does.
func validateParameterUpdates(params []*rds.Parameter) ([]parameterUpdate, error) {
	byName := make(map[string]parameterUpdate, len(params))
	for _, param := range params {
		if param == nil {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "a parameter entry is empty")
		}
		spec, err := validateParameterValue(aws.StringValue(param.ParameterName), aws.StringValue(param.ParameterValue))
		if err != nil {
			return nil, err
		}
		method, err := resolveApplyMethod(spec, aws.StringValue(param.ApplyMethod))
		if err != nil {
			return nil, err
		}
		byName[spec.Name] = parameterUpdate{
			Name:        spec.Name,
			Value:       strings.TrimSpace(aws.StringValue(param.ParameterValue)),
			ApplyMethod: method,
		}
	}

	updates := make([]parameterUpdate, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		updates = append(updates, byName[name])
	}
	return updates, nil
}

// A static parameter cannot be applied immediately, which AWS rejects rather
// than silently downgrading — accepting it would tell the customer the change is
// live when the engine has not adopted it.
func resolveApplyMethod(spec ParameterSpec, requested string) (string, error) {
	method := strings.ToLower(strings.TrimSpace(requested))
	switch method {
	case "":
		if spec.ApplyType == ApplyTypeStatic {
			return ApplyMethodPendingReboot, nil
		}
		return ApplyMethodImmediate, nil
	case ApplyMethodPendingReboot:
		return ApplyMethodPendingReboot, nil
	case ApplyMethodImmediate:
		if spec.ApplyType == ApplyTypeStatic {
			return "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
				"parameter %s is static, so ApplyMethod must be %s", spec.Name, ApplyMethodPendingReboot)
		}
		return ApplyMethodImmediate, nil
	default:
		return "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"ApplyMethod %q is not one of %s or %s", requested, ApplyMethodImmediate, ApplyMethodPendingReboot)
	}
}

// The stored record, or the lazily materialised default group. A default group
// is synthesised rather than written on read: the record carries nothing a write
// would preserve, and materialising it on a describe would make a read path a
// writer for no gain.
func getDBParameterGroup(ctx context.Context, kv jetstream.KeyValue, accountID, name string) (*DBParameterGroupRecord, uint64, error) {
	var rec DBParameterGroupRecord
	rev, found, err := getJSONRevision(ctx, kv, DBParameterGroupMetaKey(name), &rec)
	if err != nil {
		return nil, 0, err
	}
	if found {
		return &rec, rev, nil
	}
	if engine, ok := engineForDefaultParameterGroup(name); ok {
		return defaultParameterGroupRecord(engine, accountID), 0, nil
	}
	return nil, 0, awserrors.Errorf(awserrors.ErrorDBParameterGroupNotFound, "DB parameter group %s not found", name)
}

// The implicit group, identical for every account. It carries no tags and no
// stored values, so it resolves to the catalog defaults alone.
func defaultParameterGroupRecord(engine Engine, accountID string) *DBParameterGroupRecord {
	return &DBParameterGroupRecord{
		Name:        engine.DefaultParameterGroupName(),
		AccountID:   accountID,
		Family:      engine.ParameterGroupFamily(),
		Description: fmt.Sprintf("Default parameter group for %s", engine.ParameterGroupFamily()),
	}
}

func isDefaultParameterGroupName(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), defaultParameterGroupPrefix)
}

// The engine whose implicit default group carries this name, so an unrecognised
// default.* name is a not-found rather than a group that resolves to nothing.
func engineForDefaultParameterGroup(name string) (Engine, bool) {
	for _, engine := range engines {
		if engine.DefaultParameterGroupName() == name {
			return engine, true
		}
	}
	return Engine{}, false
}

// v1 pins one engine major, so the family is the pinned one or nothing. An empty
// family takes the pin rather than failing, since a client that omits it can
// only mean the one family this platform offers.
func validateParameterGroupFamily(family string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(family))
	if trimmed == "" {
		return enginePostgres.ParameterGroupFamily(), nil
	}
	for _, engine := range engines {
		if trimmed == engine.ParameterGroupFamily() {
			return engine.ParameterGroupFamily(), nil
		}
	}
	return "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"DBParameterGroupFamily %q is not offered; %s is the only supported family", family, enginePostgres.ParameterGroupFamily())
}

func parameterSource(isOverride bool) string {
	if isOverride {
		return ParameterSourceUser
	}
	return ParameterSourceEngineDefault
}

// A computed default is reported as engine-default with its literal value, never
// as the formula that produced it.
func projectParameter(spec ParameterSpec, value, storedApplyMethod string, isOverride bool) *rds.Parameter {
	out := &rds.Parameter{
		ParameterName:  aws.String(spec.Name),
		ParameterValue: aws.String(value),
		Description:    aws.String(spec.Description),
		Source:         aws.String(parameterSource(isOverride)),
		ApplyType:      aws.String(spec.ApplyType),
		DataType:       aws.String(spec.DataType),
		IsModifiable:   aws.Bool(spec.IsModifiable),
	}
	if allowed := spec.AllowedValues(); allowed != "" {
		out.AllowedValues = aws.String(allowed)
	}
	// An override reports the method the customer stored. The fallback preserves
	// defaults and records written before ApplyMethod was persisted.
	if storedApplyMethod != "" {
		out.ApplyMethod = aws.String(storedApplyMethod)
	} else if spec.ApplyType == ApplyTypeStatic {
		out.ApplyMethod = aws.String(ApplyMethodPendingReboot)
	} else {
		out.ApplyMethod = aws.String(ApplyMethodImmediate)
	}
	return out
}

func (s *Service) projectParameterGroupRecord(rec *DBParameterGroupRecord) *rds.DBParameterGroup {
	if rec == nil {
		return nil
	}
	return &rds.DBParameterGroup{
		DBParameterGroupName:   aws.String(rec.Name),
		DBParameterGroupFamily: aws.String(rec.Family),
		DBParameterGroupArn:    aws.String(DBParameterGroupARN(s.region, rec.AccountID, rec.Name)),
		Description:            aws.String(rec.Description),
	}
}

// The effective set an instance of this class runs with under this group: catalog
// defaults evaluated at the class, overlaid with the group's stored overrides.
// A group that does not exist fails here rather than silently resolving to the
// bare defaults, which would run a database on settings nobody asked for.
func (s *Service) resolveGroupParameters(ctx context.Context, kv jetstream.KeyValue, accountID, group, instanceClass string) ([]Parameter, error) {
	if _, _, err := getDBParameterGroup(ctx, kv, accountID, group); err != nil {
		return nil, err
	}
	overrides, err := ListDBParameterOverrides(ctx, kv, group)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(overrides))
	for name, override := range overrides {
		values[name] = override.Value
	}
	return ResolveEffectiveParameters(instanceClass, values)
}
