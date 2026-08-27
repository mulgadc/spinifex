package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// InstanceRecordPrefix is the key prefix for the per-resource instance record
// space. The stopped and terminated key spaces are mirrored onto it; the
// node.<id> blob is not, and still holds a node's whole running set.
//
// It cannot collide with the three prefixes it replaces. Those separate their
// prefix from the ID with ".", and a key beginning "i/" is neither "instance."
// nor "node." nor "terminated." — List and DeletePrefix match on a plain
// string prefix, not on NATS subject tokens, so the two spaces are disjoint
// while they share a bucket.
const InstanceRecordPrefix = "i/"

// instanceRecordKey is the only place the prefix and the ID are joined, so a
// reader and a writer cannot disagree about the key shape.
func instanceRecordKey(instanceID string) string {
	return InstanceRecordPrefix + instanceID
}

// WriteInstanceRecord writes an instance record to the shared KV store,
// replacing any existing record for instanceID. Replace rather than Set: the
// write is wholesale, and doing it under CAS is what stops a racing update
// landing out of order.
func (m *JetStreamManager) WriteInstanceRecord(instanceID string, record *vm.InstanceRecord) error {
	if m.records == nil {
		return errors.New("KV bucket not initialized")
	}

	key := instanceRecordKey(instanceID)
	if err := m.records.Replace(context.Background(), key, record); err != nil {
		return err
	}

	slog.Debug("Wrote instance record to JetStream KV", "key", key, "instanceId", instanceID)
	return nil
}

// LoadInstanceRecord loads one instance record from the shared KV store.
// Returns nil, nil if the key does not exist, matching the accessors it will
// replace.
func (m *JetStreamManager) LoadInstanceRecord(instanceID string) (*vm.InstanceRecord, error) {
	if m.records == nil {
		return nil, errors.New("KV bucket not initialized")
	}
	return loadRecord(m.records, instanceRecordKey(instanceID))
}

// UpdateInstanceRecord atomically applies mutate to the current record for
// instanceID and writes it back under CAS, retrying a concurrent writer's
// revision conflict. Returns kvstore.ErrNotFound if no record exists: a mutation
// racing a delete must not resurrect what it deleted.
func (m *JetStreamManager) UpdateInstanceRecord(instanceID string, mutate func(*vm.InstanceRecord)) (*vm.InstanceRecord, error) {
	if m.records == nil {
		return nil, errors.New("KV bucket not initialized")
	}
	return mutateRecord(m.records, instanceRecordKey(instanceID), mutate)
}

// DeleteInstanceRecord removes an instance record from the shared KV store.
// It is idempotent — deleting a non-existent key is not an error.
func (m *JetStreamManager) DeleteInstanceRecord(instanceID string) error {
	if m.records == nil {
		return errors.New("KV bucket not initialized")
	}

	key := instanceRecordKey(instanceID)
	if err := m.records.Delete(context.Background(), key); err != nil {
		return err
	}

	slog.Debug("Deleted instance record from JetStream KV", "key", key)
	return nil
}

// ListInstanceRecords returns every instance record in the shared KV store.
// This is the scan that replaces the single Get of a node's blob, so it must
// not skip a record it cannot decode: a dropped instance reads as terminated.
func (m *JetStreamManager) ListInstanceRecords() ([]*vm.InstanceRecord, error) {
	if m.records == nil {
		return nil, errors.New("KV bucket not initialized")
	}
	return listRecords(m.records, InstanceRecordPrefix)
}

// WriteTerminatedInstanceRecord writes a terminated instance record to the
// terminated KV bucket, replacing any existing record for instanceID. The entry
// expires with the bucket's TTL.
func (m *JetStreamManager) WriteTerminatedInstanceRecord(instanceID string, record *vm.InstanceRecord) error {
	if m.termRecords == nil {
		return errors.New("terminated instance KV bucket not initialized")
	}

	key := instanceRecordKey(instanceID)
	if err := m.termRecords.Replace(context.Background(), key, record); err != nil {
		return err
	}

	slog.Debug("Wrote terminated instance record to JetStream KV", "key", key, "instanceId", instanceID)
	return nil
}

// LoadTerminatedInstanceRecord loads one terminated instance record.
// Returns nil, nil if the key does not exist.
func (m *JetStreamManager) LoadTerminatedInstanceRecord(instanceID string) (*vm.InstanceRecord, error) {
	if m.termRecords == nil {
		return nil, errors.New("terminated instance KV bucket not initialized")
	}
	return loadRecord(m.termRecords, instanceRecordKey(instanceID))
}

// UpdateTerminatedInstanceRecord atomically applies mutate to the current
// terminated record for instanceID and writes it back under CAS. The teardown
// reaper merges per-dependent progress this way rather than replacing the
// record, so it cannot clobber a mark a concurrent update wrote.
func (m *JetStreamManager) UpdateTerminatedInstanceRecord(instanceID string, mutate func(*vm.InstanceRecord)) (*vm.InstanceRecord, error) {
	if m.termRecords == nil {
		return nil, errors.New("terminated instance KV bucket not initialized")
	}
	return mutateRecord(m.termRecords, instanceRecordKey(instanceID), mutate)
}

// DeleteTerminatedInstanceRecord removes a terminated instance record. It is
// idempotent, and the bucket's TTL removes anything this misses.
func (m *JetStreamManager) DeleteTerminatedInstanceRecord(instanceID string) error {
	if m.termRecords == nil {
		return errors.New("terminated instance KV bucket not initialized")
	}

	key := instanceRecordKey(instanceID)
	if err := m.termRecords.Delete(context.Background(), key); err != nil {
		return err
	}

	slog.Debug("Deleted terminated instance record from JetStream KV", "key", key)
	return nil
}

// ListTerminatedInstanceRecords returns every terminated instance record.
func (m *JetStreamManager) ListTerminatedInstanceRecords() ([]*vm.InstanceRecord, error) {
	if m.termRecords == nil {
		return nil, errors.New("terminated instance KV bucket not initialized")
	}
	return listRecords(m.termRecords, InstanceRecordPrefix)
}

// mirrorRecord writes instance to the per-resource key space, after the write
// it mirrors has already committed at the key it replaces.
//
// Reads prefer the per-resource key, so what they depend on is that a record
// present there is never older than the one at the key it mirrors. A mirror
// write that fails takes the new key with it, which restores that by making
// the reader fall back — a degraded write, not a failed one, so it is logged
// rather than returned. Only a failure to remove the stale mirror is returned:
// that is the one outcome leaving a read able to serve a record older than the
// one just committed.
func mirrorRecord(store *kvstore.Store[vm.InstanceRecord], instanceID string, instance *vm.VM) error {
	key := instanceRecordKey(instanceID)
	err := store.Replace(context.Background(), key, instance.Record())
	if err == nil {
		return nil
	}

	if delErr := store.Delete(context.Background(), key); delErr != nil {
		return fmt.Errorf("mirror %s failed and its stale record could not be removed: %w",
			key, errors.Join(err, delErr))
	}
	slog.Error("Instance record mirror failed; reads fall back to the record it mirrors",
		"key", key, "instanceId", instanceID, "err", err)
	return nil
}

// loadPreferringRecord reads the per-resource key and falls back to the key it
// replaces. Both are maintained during the transition, so an instance last
// written by a node that predates the per-resource space is still found. A
// decode failure at the new key is returned rather than fallen back on: it
// means the conversion is wrong, and hiding it behind the old key is how that
// reaches a cutover unnoticed.
func loadPreferringRecord(records *kvstore.Store[vm.InstanceRecord], legacy *kvstore.Store[vm.VM], legacyKey, instanceID string) (*vm.VM, error) {
	record, err := loadRecord(records, instanceRecordKey(instanceID))
	if err != nil {
		return nil, err
	}
	if record != nil {
		return vm.VMFromRecord(record), nil
	}

	instance, _, err := legacy.Get(context.Background(), legacyKey)
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return instance, nil
}

// listPreferringRecords merges the two key spaces, the per-resource key
// winning. Neither is a superset of the other: the migration copies what
// existed when it ran, and a node that predates the per-resource space still
// writes only the old key, so a listing that read either alone would drop
// instances.
func listPreferringRecords(records *kvstore.Store[vm.InstanceRecord], legacy *kvstore.Store[vm.VM], legacyPrefix string) ([]*vm.VM, error) {
	newer, err := listRecords(records, InstanceRecordPrefix)
	if err != nil {
		return nil, err
	}
	older, err := listRecords(legacy, legacyPrefix)
	if err != nil {
		return nil, err
	}

	out := make([]*vm.VM, 0, len(newer)+len(older))
	seen := make(map[string]struct{}, len(newer))
	for _, record := range newer {
		instance := vm.VMFromRecord(record)
		seen[instance.ID] = struct{}{}
		out = append(out, instance)
	}
	for _, instance := range older {
		if _, mirrored := seen[instance.ID]; mirrored {
			continue
		}
		out = append(out, instance)
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// deleteBothRecords removes an instance from both key spaces. Both deletes are
// idempotent, so a retry repairs a partial failure; until then the instance is
// still readable through whichever key survived, which is why the error is
// returned rather than logged.
func deleteBothRecords(records *kvstore.Store[vm.InstanceRecord], legacy *kvstore.Store[vm.VM], legacyKey, instanceID string) error {
	if err := records.Delete(context.Background(), instanceRecordKey(instanceID)); err != nil {
		return err
	}
	return legacy.Delete(context.Background(), legacyKey)
}

// loadRecord reads one record, reporting an absent key as nil rather than as an
// error. Every Load accessor on this manager answers that way, so a caller
// asking after an instance that is simply not there does not have to know which
// sentinel the store uses.
func loadRecord[T any](store *kvstore.Store[T], key string) (*T, error) {
	record, _, err := store.Get(context.Background(), key)
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return record, nil
}
