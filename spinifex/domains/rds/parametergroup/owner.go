package parametergroup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// Dependants reports the sorted identifiers of the DB instances in the account that name the group,
// current or pending.
type Dependants interface {
	InstancesUsing(ctx context.Context, accountID, name string) ([]string, error)
}

// Bucket opens the account's RDS bucket, which holds the group's records.
type Bucket func(ctx context.Context, accountID string) (*kvstore.Bucket, error)

// Owner is the sole writer of DB parameter group records and their overrides.
type Owner struct {
	dependants Dependants
	bucket     Bucket
}

// New returns an Owner.
func New(dependants Dependants, bucket Bucket) *Owner {
	return &Owner{dependants: dependants, bucket: bucket}
}

// Spec is a validated create request. It starts with no overrides: every value is a catalog default
// until SetOverrides stores one, so a fresh group and the default group resolve to the same effective set.
type Spec struct {
	Name        string
	Family      string
	Description string
	Tags        map[string]string
}

// Create stores a new group, or fails with DBParameterGroupAlreadyExists when the name is taken.
func (o *Owner) Create(ctx context.Context, accountID string, spec Spec) (*Record, error) {
	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := &Record{
		Name:        spec.Name,
		AccountID:   accountID,
		Family:      spec.Family,
		Description: spec.Description,
		Tags:        spec.Tags,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// Create rather than Set, so two concurrent creates of one name have exactly
	// one winner instead of both reporting success over each other's family.
	if _, err := kvstore.On[Record](kv).Create(ctx, MetaKey(spec.Name), rec); err != nil {
		if errors.Is(err, kvstore.ErrExists) {
			return nil, awserrors.Errorf(awserrors.ErrorDBParameterGroupAlreadyExists,
				"DB parameter group %s already exists", spec.Name)
		}
		return nil, err
	}

	slog.InfoContext(ctx, "rds: DB parameter group created",
		"dbParameterGroup", spec.Name, "accountId", accountID, "family", spec.Family)
	return rec, nil
}

// Get returns the group, including the implicit default group synthesised for an engine that has
// never been created, or DBParameterGroupNotFound.
func (o *Owner) Get(ctx context.Context, accountID, name string) (*Record, error) {
	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	rec, _, err := get(ctx, kv, accountID, name)
	return rec, err
}

// List returns every group in the account, including the implicit default group of every registered
// engine, sorted by name and deduplicated: a customer group that happens to carry a default's name
// (which Create refuses) would otherwise be reported twice.
func (o *Owner) List(ctx context.Context, accountID string) ([]Record, error) {
	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	names, err := storedNames(ctx, kv)
	if err != nil {
		return nil, err
	}
	for _, engineName := range rdsengine.SupportedEngines() {
		engine, err := rdsengine.LookupEngine(engineName)
		if err != nil {
			return nil, err
		}
		names = append(names, engine.DefaultParameterGroupName())
	}
	slices.Sort(names)
	names = slices.Compact(names)

	recs := make([]Record, 0, len(names))
	for _, name := range names {
		rec, _, err := get(ctx, kv, accountID, name)
		if err != nil {
			// Deleted between the listing and this read; the same answer a describe
			// one tick later would give.
			if awserrors.IsErrorCode(err, awserrors.ErrorDBParameterGroupNotFound) {
				continue
			}
			return nil, err
		}
		recs = append(recs, *rec)
	}
	return recs, nil
}

// Overrides returns the stored overrides of one parameter group, keyed by parameter name.
func (o *Owner) Overrides(ctx context.Context, accountID, name string) (map[string]Override, error) {
	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return overrides(ctx, kv, name)
}

// SetOverrides stores each override at its own key, one write per parameter, so a modify touching one
// setting cannot clobber a concurrent change to another. It refuses a default group, which has no
// stored values of its own, and an unknown group, so no override is written without its group.
func (o *Owner) SetOverrides(ctx context.Context, accountID, name string, values []Override) error {
	if _, ok := rdsengine.EngineForDefaultParameterGroup(name); ok {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"DB parameter group %s is a default group and cannot be modified", name)
	}
	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	if _, _, err := get(ctx, kv, accountID, name); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, value := range values {
		value.UpdatedAt = now
		if err := kvstore.On[Override](kv).Set(ctx, ParamKey(name, value.Name), &value); err != nil {
			return err
		}
	}
	return nil
}

// Delete is refused for a default group, and while any instance still names the group, including one
// that is only deleting or only pending it: releasing it early would let a teardown lose the record of
// its configuration, and would make destroy ordering ambiguous. The values are removed before the
// group's own record, so a crash between the two leaves orphaned parameter keys rather than a group a
// later create could inherit silently.
func (o *Owner) Delete(ctx context.Context, accountID, name string) error {
	if _, ok := rdsengine.EngineForDefaultParameterGroup(name); ok {
		return awserrors.Errorf(awserrors.ErrorDBParameterGroupInvalidState,
			"DB parameter group %s is a default group and cannot be deleted", name)
	}
	kv, err := o.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	if _, _, err := get(ctx, kv, accountID, name); err != nil {
		return err
	}
	users, err := o.dependants.InstancesUsing(ctx, accountID, name)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return awserrors.Errorf(awserrors.ErrorDBParameterGroupInvalidState,
			"DB parameter group %s is still used by %s", name, strings.Join(users, ", "))
	}

	values, err := overrides(ctx, kv, name)
	if err != nil {
		return err
	}
	for _, param := range slices.Sorted(maps.Keys(values)) {
		if err := kv.Delete(ctx, ParamKey(name, param)); err != nil {
			return fmt.Errorf("rds: delete parameter %s of group %s: %w", param, name, err)
		}
	}
	if err := kv.Delete(ctx, MetaKey(name)); err != nil {
		return fmt.Errorf("rds: delete DB parameter group %s: %w", name, err)
	}

	slog.InfoContext(ctx, "rds: DB parameter group deleted", "dbParameterGroup", name, "accountId", accountID)
	return nil
}

// The stored record, or the lazily materialised default group. A default group is synthesised rather
// than written on read: the record carries nothing a write would preserve, and materialising it on a
// read would make a read path a writer for no gain. The record plus its revision, so a caller doing a
// CAS update is not forced into a second read.
func get(ctx context.Context, kv *kvstore.Bucket, accountID, name string) (*Record, uint64, error) {
	rec, rev, err := kvstore.On[Record](kv).Get(ctx, MetaKey(name))
	if err == nil {
		return rec, rev, nil
	}
	if !errors.Is(err, kvstore.ErrNotFound) {
		return nil, 0, err
	}
	if engine, ok := rdsengine.EngineForDefaultParameterGroup(name); ok {
		return defaultRecord(engine, accountID), 0, nil
	}
	return nil, 0, awserrors.Errorf(awserrors.ErrorDBParameterGroupNotFound, "DB parameter group %s not found", name)
}

// The implicit group, identical for every account. It carries no tags and no stored values, so it
// resolves to the catalog defaults alone.
func defaultRecord(engine rdsengine.Engine, accountID string) *Record {
	return &Record{
		Name:        engine.DefaultParameterGroupName(),
		AccountID:   accountID,
		Family:      engine.ParameterGroupFamily(),
		Description: fmt.Sprintf("Default parameter group for %s", engine.ParameterGroupFamily()),
	}
}

// overrides returns the stored overrides of one group, keyed by parameter name, against an
// already-opened bucket.
func overrides(ctx context.Context, kv *kvstore.Bucket, name string) (map[string]Override, error) {
	paramPrefix := ParamsPrefix(name)
	names, err := paramNames(ctx, kv, paramPrefix)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Override, len(names))
	for _, param := range names {
		v, _, err := kvstore.On[Override](kv).Get(ctx, paramPrefix+param)
		if errors.Is(err, kvstore.ErrNotFound) {
			// Reset between the listing and this read; the same answer a resolve one
			// tick later would give.
			continue
		}
		if err != nil {
			return nil, err
		}
		out[param] = *v
	}
	return out, nil
}

// storedNames walks the .../meta keys, which is what makes a group's own record findable among the
// per-parameter keys hanging off the same prefix.
func storedNames(ctx context.Context, kv *kvstore.Bucket) ([]string, error) {
	keys, err := bucketKeys(ctx, kv)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		rest, prefixed := strings.CutPrefix(key, prefix)
		name, suffixed := strings.CutSuffix(rest, "/meta")
		if !prefixed || !suffixed {
			continue
		}
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// paramNames returns the leaf names directly under paramPrefix.
func paramNames(ctx context.Context, kv *kvstore.Bucket, paramPrefix string) ([]string, error) {
	keys, err := bucketKeys(ctx, kv)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		name, ok := strings.CutPrefix(key, paramPrefix)
		if !ok {
			continue
		}
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// An empty bucket yields no keys rather than an error, so a first-ever list answers with an empty
// result instead of failing.
func bucketKeys(ctx context.Context, b *kvstore.Bucket) ([]string, error) {
	kv, err := b.KV(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("rds: list keys: %w", err)
	}
	return keys, nil
}
