// Package migrate applies versioned transformations to shared KV state.
//
// Individual domains register their migrations here; this package owns the
// chain validation and version-stamping mechanics, not any domain record.
package migrate

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// KVMigration represents a versioned transformation of KV bucket data.
type KVMigration struct {
	FromVersion int
	ToVersion   int
	Description string
	Run         func(ctx context.Context, kvc KVContext) error
}

// KVContext provides a migration with its target bucket. JetStream is non-nil
// only for RunKVWithJetStream, for migrations that must read sibling buckets.
type KVContext struct {
	KV        jetstream.KeyValue
	JetStream jetstream.JetStream
	Logger    *slog.Logger
}

// Registry holds migrations keyed by KV bucket name.
type Registry struct {
	kvMigrations map[string][]KVMigration
}

// DefaultRegistry is the process-wide KV migration registry. Domains register
// their versioned state transitions during package initialization.
var DefaultRegistry = NewRegistry()

// NewRegistry creates an empty KV migration registry.
func NewRegistry() *Registry {
	return &Registry{kvMigrations: make(map[string][]KVMigration)}
}

// RegisterKV adds a bucket migration. Migrations execute by FromVersion order.
func (r *Registry) RegisterKV(bucket string, m KVMigration) {
	r.kvMigrations[bucket] = append(r.kvMigrations[bucket], m)
	sort.Slice(r.kvMigrations[bucket], func(i, j int) bool {
		return r.kvMigrations[bucket][i].FromVersion < r.kvMigrations[bucket][j].FromVersion
	})
}

// SchemaAheadError reports a bucket migrated past what this build understands.
// A node must stop rather than write an incomplete view of newer state.
type SchemaAheadError struct {
	Bucket     string
	Found      int
	Understood int
}

func (e SchemaAheadError) Error() string {
	return fmt.Sprintf(
		"%s is at schema version %d but this build understands %d: another node is running a newer release of Spinifex; upgrade this node to match",
		e.Bucket, e.Found, e.Understood)
}

// RunKVWithJetStream is RunKV with a JetStream handle attached to each
// migration context, for migrations that need sibling-bucket reads.
func (r *Registry) RunKVWithJetStream(ctx context.Context, bucket string, kv jetstream.KeyValue, js jetstream.JetStream, targetVersion int) error {
	return r.runKV(ctx, bucket, kv, js, targetVersion)
}

// RunKV applies pending migrations up to targetVersion. A fresh bucket with no
// registered migrations is stamped directly; an incomplete chain is refused.
func (r *Registry) RunKV(ctx context.Context, bucket string, kv jetstream.KeyValue, targetVersion int) error {
	return r.runKV(ctx, bucket, kv, nil, targetVersion)
}

func (r *Registry) runKV(ctx context.Context, bucket string, kv jetstream.KeyValue, js jetstream.JetStream, targetVersion int) error {
	current, err := kvutil.ReadVersion(ctx, kv)
	if err != nil {
		return fmt.Errorf("read version for %s: %w", bucket, err)
	}

	if current > targetVersion {
		return SchemaAheadError{Bucket: bucket, Found: current, Understood: targetVersion}
	}
	if current == targetVersion {
		return nil
	}

	all := r.kvMigrations[bucket]
	if current == 0 && len(all) == 0 {
		return kvutil.WriteVersion(ctx, kv, targetVersion)
	}

	// A fresh bucket has no v0 schema; begin at the lowest registered step.
	if current == 0 {
		current = all[0].FromVersion
	}

	var pending []KVMigration
	for _, m := range all {
		if m.FromVersion >= current && m.ToVersion <= targetVersion {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return fmt.Errorf("no migrations registered for %s from version %d to %d", bucket, current, targetVersion)
	}

	expected := current
	for _, m := range pending {
		if m.FromVersion != expected {
			return fmt.Errorf("migration chain gap for %s: expected from %d, got from %d", bucket, expected, m.FromVersion)
		}
		expected = m.ToVersion
	}
	if expected != targetVersion {
		return fmt.Errorf("migration chain for %s ends at version %d, target is %d", bucket, expected, targetVersion)
	}

	logger := slog.Default()
	for _, m := range pending {
		logger.Info("Running KV migration", "bucket", bucket, "from", m.FromVersion, "to", m.ToVersion, "description", m.Description)
		kvc := KVContext{KV: kv, JetStream: js, Logger: logger}
		if err := m.Run(ctx, kvc); err != nil {
			return fmt.Errorf("KV migration %s %d→%d failed: %w", bucket, m.FromVersion, m.ToVersion, err)
		}
		if err := kvutil.WriteVersion(ctx, kv, m.ToVersion); err != nil {
			return fmt.Errorf("stamp version %d on %s: %w", m.ToVersion, bucket, err)
		}
	}
	return nil
}
