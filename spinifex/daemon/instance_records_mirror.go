package daemon

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/vm"
)

// mirrorState tracks what this node last put on the per-resource key space, so
// a change to one instance writes one record.
//
// Without it every state change would rewrite the whole running set one key at
// a time, which is worse than the single blob it replaces rather than better:
// splitting the blob exists to stop two instances on a node serialising against
// each other.
type mirrorState struct {
	mu     sync.Mutex
	seeded bool
	digest map[string]uint64
}

// MirrorRunningSet reconciles this node's running instances onto the
// per-resource key space, after the blob it mirrors has been written.
//
// Best-effort, like that blob write: the local state file is the source of
// truth, the blob still decides which instances are running here, and a read
// that finds no mirror answers from the blob member. So a failure degrades a
// read rather than breaking one, and is logged rather than returned.
func (m *JetStreamManager) MirrorRunningSet(nodeID string, vms map[string]*vm.VM) {
	if m.records == nil || m.stopped == nil {
		return
	}

	m.mirror.mu.Lock()
	defer m.mirror.mu.Unlock()

	if !m.mirror.seeded {
		if err := m.seedRunningMirror(nodeID); err != nil {
			slog.Warn("Could not read the existing instance records; the next state write retries",
				"node", nodeID, "err", err)
			return
		}
	}

	for id, instance := range vms {
		if instance == nil {
			continue
		}
		sum, err := recordDigest(instance.Record())
		if err != nil {
			slog.Error("Could not encode an instance record", "instanceId", id, "err", err)
			continue
		}
		if current, ok := m.mirror.digest[id]; ok && current == sum {
			continue
		}

		// A failed mirror forgets its digest as well as its record, so the next
		// state write repeats the attempt rather than reading the failure as
		// already mirrored.
		if err := mirrorRecord(m.records, id, instance); err != nil {
			slog.Error("Could not mirror an instance record", "instanceId", id, "err", err)
			delete(m.mirror.digest, id)
			continue
		}
		m.mirror.digest[id] = sum
	}

	for id := range m.mirror.digest {
		if _, ok := vms[id]; ok {
			continue
		}
		if m.retireMirror(id) {
			delete(m.mirror.digest, id)
		}
	}
}

// retireMirror drops the record of an instance that has left this node's
// running set, reporting whether the question is settled.
//
// It is not settled by deleting unconditionally. The stopped key space shares
// the record key, and an instance that stops leaves the running set as
// WriteStoppedInstance mirrors it there, so an unconditional delete would race
// a live write — and would keep deleting, on every state change, the records of
// instances stopped on this node. Where the stopped key holds the instance the
// record is handed over rather than retired, which is also what it becomes at
// the cutover, when the two key spaces are one.
func (m *JetStreamManager) retireMirror(instanceID string) bool {
	stopped, err := loadRecord(m.stopped, StoppedInstancePrefix+instanceID)
	if err != nil {
		slog.Warn("Could not tell whether a departed instance is stopped; leaving its record in place",
			"instanceId", instanceID, "err", err)
		return false
	}
	if stopped != nil {
		return true
	}

	if err := m.records.Delete(context.Background(), instanceRecordKey(instanceID)); err != nil {
		slog.Error("Could not remove the record of an instance that left this node",
			"instanceId", instanceID, "err", err)
		return false
	}
	return true
}

// seedRunningMirror primes the digest map from the records already on the key
// space for this node, so the first state write after boot rewrites only what
// actually changed while the node was down — and so a record left behind by a
// previous process is still a candidate for retirement.
//
// Ownership is read from status.LastNode, the same field the GC uses to decide
// whose records a node may touch.
func (m *JetStreamManager) seedRunningMirror(nodeID string) error {
	existing, err := listRecords(m.records, InstanceRecordPrefix)
	if err != nil {
		return err
	}

	digest := make(map[string]uint64, len(existing))
	for _, record := range existing {
		if record.Status.LastNode != nodeID {
			continue
		}
		sum, err := recordDigest(record)
		if err != nil {
			return err
		}
		digest[record.Metadata.Name] = sum
	}

	m.mirror.digest = digest
	m.mirror.seeded = true
	slog.Debug("Seeded the instance record mirror", "node", nodeID, "records", len(digest))
	return nil
}

// recordDigest identifies a record by its wire form, so a state change that
// leaves an instance untouched does not become a KV write. encoding/json sorts
// map keys, so the same record always encodes to the same bytes.
func recordDigest(record *vm.InstanceRecord) (uint64, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return 0, err
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return h.Sum64(), nil
}

// answerFromMirrors replaces each member of a node's running set with its
// mirror where there is one. The blob still decides membership — see
// loadPreferringRecord for why a mirror cannot be trusted to — so a record with
// no member behind it is ignored here.
func (m *JetStreamManager) answerFromMirrors(vms map[string]*vm.VM) error {
	if m.records == nil || len(vms) == 0 {
		return nil
	}

	mirrored, err := listRecords(m.records, InstanceRecordPrefix)
	if err != nil {
		return err
	}
	for _, record := range mirrored {
		if _, ok := vms[record.Metadata.Name]; !ok {
			continue
		}
		vms[record.Metadata.Name] = vm.VMFromRecord(record)
	}
	return nil
}
