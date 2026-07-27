package migrate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// KVMigration represents a versioned transformation of KV bucket data.
type KVMigration struct {
	FromVersion int
	ToVersion   int
	Description string
	Run         func(ctx context.Context, kvc KVContext) error
}

// KVContext provides KV migration functions access to the bucket being migrated.
// JetStream is non-nil only when the caller used RunKVWithJetStream — used by
// migrations that need to read sibling buckets (e.g. owner-attribution lookups).
type KVContext struct {
	KV        jetstream.KeyValue
	JetStream jetstream.JetStream
	Logger    *slog.Logger
}

// ConfigMigration represents a versioned transformation of on-disk config files.
type ConfigMigration struct {
	FromVersion int
	ToVersion   int
	Description string
	Run         func(ctx ConfigContext) error
}

// ConfigContext provides config migration functions access to the filesystem.
type ConfigContext struct {
	ConfigDir string // path to /etc/spinifex or equivalent
	DataDir   string // path to /var/lib/spinifex or equivalent
	Logger    *slog.Logger
}

// PendingMigration describes a migration that has not yet been applied.
type PendingMigration struct {
	Target      string
	FromVersion int
	ToVersion   int
	Description string
}

// configTarget bundles the relative path and version reader for a config file.
type configTarget struct {
	path   string // relative to configDir (e.g. "nats/nats.conf")
	reader ConfigVersionReader
}

// Registry holds all registered migrations, keyed by target name.
type Registry struct {
	kvMigrations     map[string][]KVMigration
	configMigrations map[string][]ConfigMigration
	configTargets    map[string]configTarget
}

// DefaultRegistry is the global migration registry. Migrations self-register
// via init() functions so they are available before any service starts.
var DefaultRegistry = NewRegistry()

// NewRegistry creates an empty migration registry.
func NewRegistry() *Registry {
	return &Registry{
		kvMigrations:     make(map[string][]KVMigration),
		configMigrations: make(map[string][]ConfigMigration),
		configTargets:    make(map[string]configTarget),
	}
}

// RegisterKV adds a KV bucket migration. Migrations are kept sorted by FromVersion.
func (r *Registry) RegisterKV(bucket string, m KVMigration) {
	r.kvMigrations[bucket] = append(r.kvMigrations[bucket], m)
	sort.Slice(r.kvMigrations[bucket], func(i, j int) bool {
		return r.kvMigrations[bucket][i].FromVersion < r.kvMigrations[bucket][j].FromVersion
	})
}

// RegisterConfigTarget registers a config file target with its relative path
// and version reader. Must be called before RegisterConfig for the same target.
func (r *Registry) RegisterConfigTarget(name, relPath string, reader ConfigVersionReader) {
	r.configTargets[name] = configTarget{path: relPath, reader: reader}
}

// RegisterConfig adds a config file migration. Migrations are kept sorted by FromVersion.
func (r *Registry) RegisterConfig(target string, m ConfigMigration) {
	r.configMigrations[target] = append(r.configMigrations[target], m)
	sort.Slice(r.configMigrations[target], func(i, j int) bool {
		return r.configMigrations[target][i].FromVersion < r.configMigrations[target][j].FromVersion
	})
}

// RunKVWithJetStream is RunKV with a JetStream handle attached to each
// migration's KVContext, enabling cross-bucket reads (e.g. owner-attribution
// during a backfill). Prefer plain RunKV when the migration is self-contained.
func (r *Registry) RunKVWithJetStream(ctx context.Context, bucket string, kv jetstream.KeyValue, js jetstream.JetStream, targetVersion int) error {
	return r.runKV(ctx, bucket, kv, js, targetVersion)
}

// RunKV applies pending KV migrations up to targetVersion. Stamps directly when
// no migrations are registered (fresh bucket). Errors if the chain is incomplete.
func (r *Registry) RunKV(ctx context.Context, bucket string, kv jetstream.KeyValue, targetVersion int) error {
	return r.runKV(ctx, bucket, kv, nil, targetVersion)
}

func (r *Registry) runKV(ctx context.Context, bucket string, kv jetstream.KeyValue, js jetstream.JetStream, targetVersion int) error {
	current, err := kvutil.ReadVersion(ctx, kv)
	if err != nil {
		return fmt.Errorf("read version for %s: %w", bucket, err)
	}

	if current >= targetVersion {
		return nil
	}

	all := r.kvMigrations[bucket]

	// Fresh bucket, no migrations: stamp directly (common first-init path).
	if current == 0 && len(all) == 0 {
		return kvutil.WriteVersion(ctx, kv, targetVersion)
	}

	// Fresh bucket with migrations: no v0 schema by convention; start at chain bottom.
	if current == 0 {
		current = all[0].FromVersion // sorted ascending by FromVersion
	}

	// Require a complete chain from current to target.
	var pending []KVMigration
	for _, m := range all {
		if m.FromVersion >= current && m.ToVersion <= targetVersion {
			pending = append(pending, m)
		}
	}

	if len(pending) == 0 {
		return fmt.Errorf("no migrations registered for %s from version %d to %d", bucket, current, targetVersion)
	}

	// Validate contiguous chain.
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

// RunConfig executes all pending config migrations for a target.
// Before each migration step, a timestamped backup is created.
// Returns an error on failure (setup.sh should abort).
func (r *Registry) RunConfig(target string, configDir, dataDir string) error {
	t, ok := r.configTargets[target]
	if !ok {
		return fmt.Errorf("unknown config target: %s", target)
	}

	migrations := r.configMigrations[target]
	if len(migrations) == 0 {
		return nil
	}

	fullPath := filepath.Join(configDir, t.path)

	// If the config file doesn't exist (fresh install), skip migrations.
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat config file %s: %w", fullPath, err)
	}

	current, err := t.reader.ReadVersion(fullPath)
	if err != nil {
		return fmt.Errorf("read version for %s: %w", target, err)
	}

	var pending []ConfigMigration
	for _, m := range migrations {
		if m.FromVersion >= current {
			pending = append(pending, m)
		}
	}

	if len(pending) == 0 {
		return nil
	}

	// Validate contiguous chain (same check as RunKV).
	expected := current
	for _, m := range pending {
		if m.FromVersion != expected {
			return fmt.Errorf("config migration chain gap for %s: expected from %d, got from %d", target, expected, m.FromVersion)
		}
		expected = m.ToVersion
	}

	logger := slog.Default()
	for _, m := range pending {
		// Create backup before migration.
		backupPath, err := BackupConfig(fullPath, m.FromVersion, m.ToVersion)
		if err != nil {
			return fmt.Errorf("backup %s before migration %d→%d: %w", target, m.FromVersion, m.ToVersion, err)
		}
		logger.Info("Created config backup", "target", target, "backup", backupPath)

		logger.Info("Running config migration", "target", target, "from", m.FromVersion, "to", m.ToVersion, "description", m.Description)
		ctx := ConfigContext{ConfigDir: configDir, DataDir: dataDir, Logger: logger}
		if err := m.Run(ctx); err != nil {
			return fmt.Errorf("config migration %s %d→%d failed: %w", target, m.FromVersion, m.ToVersion, err)
		}

		// Stamp new version after successful migration.
		if err := t.reader.WriteVersion(fullPath, m.ToVersion); err != nil {
			return fmt.Errorf("stamp version %d on %s: %w", m.ToVersion, target, err)
		}
	}

	return nil
}

// RunAllConfig executes pending config migrations for all registered targets.
// Used by `spx admin upgrade`.
func (r *Registry) RunAllConfig(configDir, dataDir string) error {
	for target := range r.configTargets {
		if err := r.RunConfig(target, configDir, dataDir); err != nil {
			return err
		}
	}
	return nil
}

// PendingConfig returns all pending config migrations across all targets.
func (r *Registry) PendingConfig(configDir string) ([]PendingMigration, error) {
	var result []PendingMigration
	for name, t := range r.configTargets {
		fullPath := filepath.Join(configDir, t.path)

		if _, err := os.Stat(fullPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat config file %s: %w", fullPath, err)
		}

		current, err := t.reader.ReadVersion(fullPath)
		if err != nil {
			return nil, fmt.Errorf("read version for %s: %w", name, err)
		}

		for _, m := range r.configMigrations[name] {
			if m.FromVersion >= current {
				result = append(result, PendingMigration{
					Target:      name,
					FromVersion: m.FromVersion,
					ToVersion:   m.ToVersion,
					Description: m.Description,
				})
			}
		}
	}
	return result, nil
}

// ConfigVersions returns current versions for all registered config targets.
// Targets whose file does not exist are omitted.
func (r *Registry) ConfigVersions(configDir string) (map[string]int, error) {
	versions := make(map[string]int)
	for name, t := range r.configTargets {
		fullPath := filepath.Join(configDir, t.path)
		if _, err := os.Stat(fullPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat config file %s: %w", fullPath, err)
		}
		v, err := t.reader.ReadVersion(fullPath)
		if err != nil {
			return nil, fmt.Errorf("read version for %s: %w", name, err)
		}
		versions[name] = v
	}
	return versions, nil
}

// BackupConfig creates a timestamped backup of a config file before migration.
// The backup is stored alongside the original file.
func BackupConfig(path string, fromVersion, toVersion int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read file for backup: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat file for backup: %w", err)
	}

	backupPath := fmt.Sprintf("%s.pre-migrate-%dto%d.%d", path, fromVersion, toVersion, time.Now().Unix())
	if err := os.WriteFile(backupPath, data, info.Mode()); err != nil {
		return "", fmt.Errorf("write backup: %w", err)
	}

	return backupPath, nil
}
