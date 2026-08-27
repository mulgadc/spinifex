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

// The copy-forward onto i/<id>. Running it as a migration rather than lazily on
// read is what lets a listing merge the two key spaces cheaply instead of
// falling back per key: after this, the only records missing from i/<id> are
// the ones a node that predates it wrote afterwards.
//
// The node.<id> blob is deliberately not copied. It holds a whole node's
// running set in one record, so moving it is a split rather than a copy.
func init() {
	migrate.DefaultRegistry.RegisterKV(InstanceStateBucket, migrate.KVMigration{
		FromVersion: 1,
		ToVersion:   2,
		Description: "copy stopped instances forward to the i/<id> key space",
		Run: func(ctx context.Context, kvc migrate.KVContext) error {
			return copyInstancesForward(ctx, kvc, StoppedInstancePrefix)
		},
	})
	migrate.DefaultRegistry.RegisterKV(TerminatedInstanceBucket, migrate.KVMigration{
		FromVersion: 1,
		ToVersion:   2,
		Description: "copy terminated instances forward to the i/<id> key space",
		Run: func(ctx context.Context, kvc migrate.KVContext) error {
			return copyInstancesForward(ctx, kvc, TerminatedInstancePrefix)
		},
	})
}

// copyInstancesForward writes an i/<id> record for every key under prefix.
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
