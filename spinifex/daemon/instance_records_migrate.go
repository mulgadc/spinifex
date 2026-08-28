package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// The copy-forward onto the per-resource key space. Running it as a migration
// rather than lazily on read is what lets a listing merge the two key spaces
// cheaply instead of falling back per key: after this, the only records missing
// from it are the ones a node that predates it wrote afterwards.
//
// The node.<id> blobs are a separate step, because each holds a whole node's
// running set in one record and moving one is a split rather than a copy.
//
// The last step of each bucket re-keys "i/<id>" to "i.<id>". The earlier steps
// write whatever the prefix currently is, so it only has to run where a build
// that wrote the slash already has.
func init() {
	migrate.DefaultRegistry.RegisterKV(InstanceStateBucket, migrate.KVMigration{
		FromVersion: 1,
		ToVersion:   2,
		Description: "copy stopped instances forward to the per-resource key space",
		Run: func(ctx context.Context, kvc migrate.KVContext) error {
			return copyInstancesForward(ctx, kvc, StoppedInstancePrefix)
		},
	})
	migrate.DefaultRegistry.RegisterKV(InstanceStateBucket, migrate.KVMigration{
		FromVersion: 2,
		ToVersion:   3,
		Description: "copy each node's running instances forward to the per-resource key space",
		Run:         copyRunningSetsForward,
	})
	migrate.DefaultRegistry.RegisterKV(InstanceStateBucket, migrate.KVMigration{
		FromVersion: 3,
		ToVersion:   4,
		Description: "re-key instance records from i/<id> to i.<id>",
		Run:         rekeyRecordSeparator,
	})
	migrate.DefaultRegistry.RegisterKV(InstanceStateBucket, migrate.KVMigration{
		FromVersion: 4,
		ToVersion:   5,
		Description: "seed a presence marker for every node that has a running-set blob",
		Run:         seedNodePresence,
	})
	migrate.DefaultRegistry.RegisterKV(TerminatedInstanceBucket, migrate.KVMigration{
		FromVersion: 1,
		ToVersion:   2,
		Description: "copy terminated instances forward to the per-resource key space",
		Run: func(ctx context.Context, kvc migrate.KVContext) error {
			return copyInstancesForward(ctx, kvc, TerminatedInstancePrefix)
		},
	})
	migrate.DefaultRegistry.RegisterKV(TerminatedInstanceBucket, migrate.KVMigration{
		FromVersion: 2,
		ToVersion:   3,
		Description: "re-key instance records from i/<id> to i.<id>",
		Run:         rekeyRecordSeparator,
	})
}

// slashRecordPrefix is the separator the per-resource key space first shipped
// with. It is here only so the re-key can find what that build wrote; nothing
// else may use it.
const slashRecordPrefix = "i/"

// rekeyRecordSeparator moves every record from the slash-separated key space to
// the dot-separated one, so a watch can filter it by subject token.
//
// The old key is deleted rather than left as an orphan. A node still writing
// slashes reads them too, so what a delete costs it is a fallback to the key
// the record mirrors — the same degradation as a mirror that was never written,
// which the shadow rule already makes safe.
func rekeyRecordSeparator(ctx context.Context, kvc migrate.KVContext) error {
	keys, err := kvc.KV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list keys: %w", err)
	}

	moved := 0
	for _, key := range keys {
		if !strings.HasPrefix(key, slashRecordPrefix) {
			continue
		}

		entry, err := kvc.KV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return fmt.Errorf("read %s: %w", key, err)
		}

		dest := instanceRecordKey(strings.TrimPrefix(key, slashRecordPrefix))
		if _, err := kvc.KV.Create(ctx, dest, entry.Value()); err != nil {
			if !errors.Is(err, jetstream.ErrKeyExists) {
				return fmt.Errorf("write %s: %w", dest, err)
			}
		}
		if err := kvc.KV.Delete(ctx, key); err != nil {
			return fmt.Errorf("delete %s: %w", key, err)
		}
		moved++
	}

	kvc.Logger.Info("Re-keyed instance records onto the dot separator", "moved", moved)
	return nil
}

// seedNodePresence gives every node that had a running-set blob the presence
// marker that replaces it.
//
// Without this a node reads no marker until its own first state write, and a
// node that boots before it writes would be told the cluster has no record of
// it. That is the safe direction — restore declines to adopt rather than
// adopting an empty set — but it also means a node cannot recover its instances
// from KV across the crossing, which is the one time it most needs to.
//
// The blob is read, not consumed. It is what a node rolled back to the previous
// release reads, and this is the step that stops being reversible if it moves.
func seedNodePresence(ctx context.Context, kvc migrate.KVContext) error {
	keys, err := kvc.KV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list keys: %w", err)
	}

	marker, err := json.Marshal(LocalState{SchemaVersion: LocalStateSchemaVersion})
	if err != nil {
		return fmt.Errorf("encode presence marker: %w", err)
	}

	seeded := 0
	for _, key := range keys {
		if !strings.HasPrefix(key, InstanceStatePrefix) {
			continue
		}

		dest := NodePresencePrefix + strings.TrimPrefix(key, InstanceStatePrefix)
		if _, err := kvc.KV.Create(ctx, dest, marker); err != nil {
			// A node that has already upgraded wrote its own marker; it is the
			// same value, and the newer write is not overwritten with this one.
			if errors.Is(err, jetstream.ErrKeyExists) {
				continue
			}
			return fmt.Errorf("write %s: %w", dest, err)
		}
		seeded++
	}

	kvc.Logger.Info("Seeded node presence markers", "seeded", seeded)
	return nil
}

// copyInstancesForward writes a record for every key under prefix.
//
// Two nodes can reach this at once — the version stamp is a read-then-write,
// not a CAS — and a node that has already upgraded can be writing the
// destination while the scan runs. Both are handled by creating rather than
// putting: a destination that already exists holds either the same copy or a
// fresher one, and neither may be overwritten with what this scan read.
func copyInstancesForward(ctx context.Context, kvc migrate.KVContext, prefix string) error {
	keys, err := kvc.KV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list keys: %w", err)
	}

	copied := 0
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}

		entry, err := kvc.KV.Get(ctx, key)
		if err != nil {
			// Deleted between the listing and the read: there is nothing left to
			// copy, and the delete is the newer fact.
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return fmt.Errorf("read %s: %w", key, err)
		}

		var instance vm.VM
		if err := json.Unmarshal(entry.Value(), &instance); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}

		record, err := json.Marshal(instance.Record())
		if err != nil {
			return fmt.Errorf("encode record for %s: %w", key, err)
		}

		dest := instanceRecordKey(strings.TrimPrefix(key, prefix))
		if _, err := kvc.KV.Create(ctx, dest, record); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				continue
			}
			return fmt.Errorf("write %s: %w", dest, err)
		}
		copied++
	}

	kvc.Logger.Info("Copied instances onto the per-resource key space",
		"prefix", prefix, "copied", copied)
	return nil
}

// copyRunningSetsForward writes a record for every instance inside
// every node's blob, splitting each of those records into as many as it holds.
//
// It copies every node's set, not just the one running it. The key space is
// shared, the migration runs once per bucket rather than once per node, and a
// node that is down would otherwise have no record of its instances until it
// came back — which is exactly when a reader most needs one.
func copyRunningSetsForward(ctx context.Context, kvc migrate.KVContext) error {
	keys, err := kvc.KV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list keys: %w", err)
	}

	copied := 0
	for _, key := range keys {
		if !strings.HasPrefix(key, InstanceStatePrefix) {
			continue
		}

		entry, err := kvc.KV.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return fmt.Errorf("read %s: %w", key, err)
		}

		var state LocalState
		if err := json.Unmarshal(entry.Value(), &state); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		if err := checkNodeStateVersion(key, state.SchemaVersion); err != nil {
			return err
		}

		for id, instance := range state.VMS {
			if instance == nil {
				continue
			}
			record, err := json.Marshal(instance.Record())
			if err != nil {
				return fmt.Errorf("encode record for %s in %s: %w", id, key, err)
			}

			dest := instanceRecordKey(id)
			if _, err := kvc.KV.Create(ctx, dest, record); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					continue
				}
				return fmt.Errorf("write %s: %w", dest, err)
			}
			copied++
		}
	}

	kvc.Logger.Info("Copied running instances onto the per-resource key space",
		"prefix", InstanceStatePrefix, "copied", copied)
	return nil
}
