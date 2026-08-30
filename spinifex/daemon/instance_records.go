package daemon

import (
	"context"
	"errors"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// InstanceRecordPrefix is the key prefix for the per-resource instance record
// space. All three key spaces it replaces are mirrored onto it.
//
// It cannot collide with those three. List and DeletePrefix match on a plain
// string prefix rather than on NATS subject tokens, and "instance.", "node."
// and "terminated." do not begin "i." — so the spaces stay disjoint while they
// share a bucket.
//
// The separator is a dot rather than a slash because a watch filter is a NATS
// subject filter, where "*" matches one dot-delimited token. "i/<id>" is a
// single token, so "i/*" matches nothing and a watch over this space would
// have to take the whole bucket. "i.<id>" is two, so "i.*" is the filter the
// DNS reconcile narrows to at the cutover.
const InstanceRecordPrefix = "i."

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

// writeRecord writes instance to the per-resource key space.
//
// Replace rather than Set: the write is wholesale, and doing it under CAS is
// what stops a racing update landing out of order.
func writeRecord(store *kvstore.Store[vm.InstanceRecord], instanceID string, instance *vm.VM) error {
	return store.Replace(context.Background(), instanceRecordKey(instanceID), instance.Record())
}

// loadInstance reads one instance from the per-resource key space, reporting an
// absent key as nil. want filters what counts as present: a key holds an
// instance for its whole life now, so "is there a record" and "is it the kind
// of instance I asked for" are different questions.
func loadInstance(store *kvstore.Store[vm.InstanceRecord], instanceID string,
	want func(*vm.InstanceRecord) bool) (*vm.VM, error) {
	record, err := loadRecord(store, instanceRecordKey(instanceID))
	if err != nil || record == nil {
		return nil, err
	}
	if want != nil && !want(record) {
		return nil, nil
	}
	return vm.VMFromRecord(record), nil
}

// listInstances returns every instance in the key space that want accepts.
func listInstances(store *kvstore.Store[vm.InstanceRecord],
	want func(*vm.InstanceRecord) bool) ([]*vm.VM, error) {
	records, err := listRecords(store, InstanceRecordPrefix)
	if err != nil {
		return nil, err
	}

	out := make([]*vm.VM, 0, len(records))
	for _, record := range records {
		if want != nil && !want(record) {
			continue
		}
		out = append(out, vm.VMFromRecord(record))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
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
