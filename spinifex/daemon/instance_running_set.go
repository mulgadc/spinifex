package daemon

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/vm"
)

// runningSetState tracks what this node last wrote to the per-resource key
// space, so a change to one instance writes one record.
//
// Without it every state change would rewrite the whole running set one key at
// a time, which is worse than the single blob it replaces rather than better:
// splitting the blob exists to stop two instances on a node serialising against
// each other.
type runningSetState struct {
	mu sync.Mutex
	// seededFor is the node the digest describes, not a bool: the digest is
	// per-node state and carries no node in it, so a manager asked to write a
	// second node's set would otherwise measure it against the first node's and
	// retire every record the first one owns.
	seededFor string
	digest    map[string]uint64
}

// RunningSetResult counts what one reconcile actually did. A healthy node that
// has published everything scores zero across the board, which is what makes it
// usable as a repair signal rather than as traffic.
type RunningSetResult struct {
	Written int // records published, whether new, changed or retried after a failure
	Retired int // instances released from the digest, whether deleted or handed over
	Failed  int // records this pass could not publish or settle
}

// WriteRunningSet reconciles this node's running instances onto the
// per-resource key space.
//
// Best-effort, like the marker write it follows: the local state file is the
// source of truth for the node's own instances, so a failure here leaves the
// cluster's view stale rather than losing anything, and is logged rather than
// returned. Whatever did not land is repeated by the next state change or by
// the repair sweep, whichever comes first.
func (m *JetStreamManager) WriteRunningSet(nodeID, az string, vms map[string]*vm.VM) RunningSetResult {
	var result RunningSetResult
	if m.records == nil {
		return result
	}

	m.running.mu.Lock()
	defer m.running.mu.Unlock()

	if m.running.seededFor != nodeID {
		if err := m.seedRunningSet(nodeID); err != nil {
			slog.Warn("Could not read the existing instance records; the next state write retries",
				"node", nodeID, "err", err)
			result.Failed++
			return result
		}
	}

	for id, instance := range vms {
		if instance == nil {
			continue
		}
		// Ownership is stamped here rather than taken from the instance. This
		// is the only place that knows whose running set is being written, and
		// a record that reaches the key space without it belongs to no node —
		// so nothing lists it and nothing retires it.
		record := instance.Record()
		record.Status.LastNode = nodeID
		record.Status.AZ = az

		sum, err := recordDigest(record)
		if err != nil {
			slog.Error("Could not encode an instance record", "instanceId", id, "err", err)
			result.Failed++
			continue
		}
		if current, ok := m.running.digest[id]; ok && current == sum {
			continue
		}

		// A failed write forgets its digest as well as its record, so the next
		// state write repeats the attempt rather than reading the failure as
		// already written.
		if err := m.records.Replace(context.Background(), instanceRecordKey(id), record); err != nil {
			slog.Error("Could not write an instance record", "instanceId", id, "err", err)
			delete(m.running.digest, id)
			result.Failed++
			continue
		}
		m.running.digest[id] = sum
		result.Written++
	}

	for id := range m.running.digest {
		if _, ok := vms[id]; ok {
			continue
		}
		if m.retireRecord(nodeID, id) {
			delete(m.running.digest, id)
			result.Retired++
			continue
		}
		result.Failed++
	}
	return result
}

// retireRecord drops the record of an instance that has left nodeID's running
// set, reporting whether the question is settled.
//
// It is not settled by deleting unconditionally. One key now holds an instance
// for its whole life, so the record of a departed instance is the same record
// whoever took it over is now using. Two ways to leave are not deaths: an
// operator stop, where WriteStoppedInstance has just rewritten that key, and a
// move, where another node has claimed it. Both are handed over, not retired.
func (m *JetStreamManager) retireRecord(nodeID, instanceID string) bool {
	record, err := loadRecord(m.records, instanceRecordKey(instanceID))
	if err != nil {
		slog.Warn("Could not tell whether a departed instance is stopped; leaving its record in place",
			"instanceId", instanceID, "err", err)
		return false
	}
	if record == nil {
		return true
	}
	if operatorStopped(record) {
		return true
	}
	if record.Status.LastNode != nodeID {
		slog.Debug("An instance that left this node is owned elsewhere; leaving its record alone",
			"instanceId", instanceID, "owner", record.Status.LastNode)
		return true
	}

	if err := m.records.Delete(context.Background(), instanceRecordKey(instanceID)); err != nil {
		slog.Error("Could not remove the record of an instance that left this node",
			"instanceId", instanceID, "err", err)
		return false
	}
	return true
}

// seedRunningSet primes the digest map from the records already on the key
// space for this node, so the first state write after boot rewrites only what
// actually changed while the node was down — and so a record left behind by a
// previous process is still a candidate for retirement.
func (m *JetStreamManager) seedRunningSet(nodeID string) error {
	existing, err := listRecords(m.records, InstanceRecordPrefix)
	if err != nil {
		return err
	}

	digest := make(map[string]uint64, len(existing))
	for _, record := range existing {
		if !runsOn(record, nodeID) {
			continue
		}
		sum, err := recordDigest(record)
		if err != nil {
			return err
		}
		digest[record.Metadata.Name] = sum
	}

	m.running.digest = digest
	m.running.seededFor = nodeID
	slog.Debug("Seeded the instance record set", "node", nodeID, "records", len(digest))
	return nil
}

// recordDigest identifies a record by its wire form, so a state change that
// leaves an instance untouched does not become a KV write. encoding/json sorts
// map keys, so the same record always encodes to the same bytes.
//
// LastQMPSuccess is held out of it. It advances on every successful poll, so
// digesting it would make every running instance look changed once a minute and
// turn the repair sweep into permanent write traffic. Nothing reads it back —
// impairment is judged from QMPConsecutiveFailures and ImpairedSince, and node
// liveness from the heartbeat store — so holding it out costs no reader
// anything and keeps publication on the cadence it had before the sweep existed.
func recordDigest(record *vm.InstanceRecord) (uint64, error) {
	stable := *record
	stable.Status.Health.LastQMPSuccess = time.Time{}

	data, err := json.Marshal(&stable)
	if err != nil {
		return 0, err
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return h.Sum64(), nil
}

// nodeRunningRecords returns the records nodeID owns, as instances.
func (m *JetStreamManager) nodeRunningRecords(nodeID string) (map[string]*vm.VM, error) {
	records, err := listRecords(m.records, InstanceRecordPrefix)
	if err != nil {
		return nil, err
	}

	vms := make(map[string]*vm.VM, len(records))
	for _, record := range records {
		if !runsOn(record, nodeID) {
			continue
		}
		vms[record.Metadata.Name] = vm.VMFromRecord(record)
	}
	return vms, nil
}
