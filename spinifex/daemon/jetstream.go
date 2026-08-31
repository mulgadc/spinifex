package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// InstanceStateBucket is the name of the KV bucket for storing instance state.
	InstanceStateBucket = "spinifex-instance-state"
	// ClusterStateBucket is the name of the KV bucket for cluster state (heartbeats, shutdown markers, service maps).
	ClusterStateBucket = "spinifex-cluster-state"
	// InstanceStatePrefix is the key prefix for the per-node running-set blobs
	// the record space replaces. Nothing writes it after the cutover; the
	// migration reads it and a rolled-back node still finds what it left.
	InstanceStatePrefix = "node."
	// NodePresencePrefix is the key prefix for a node's presence marker.
	//
	// Deliberately not InstanceStatePrefix. The marker holds no instances, so
	// writing it there would empty the blob it shares a key with, and a node
	// rolled back to the release before the cutover reads that blob to find out
	// what it was running. Frozen means not written, not written empty.
	NodePresencePrefix = "nodepresence."
	// StoppedInstancePrefix is the key prefix for stopped instances in shared KV.
	StoppedInstancePrefix = "instance."
	// TerminatedInstanceBucket is the name of the KV bucket for terminated instances (auto-expiry via TTL).
	TerminatedInstanceBucket = "spinifex-terminated-instances"
	// TerminatedInstancePrefix is the key prefix for terminated instances.
	TerminatedInstancePrefix = "terminated."

	// Schema versions for daemon KV buckets. Both instance buckets copied their
	// per-instance keys onto the record space at 2; instance-state took the node
	// blobs at 3; both re-keyed that space from "i/" to "i." next. See
	// instance_records_migrate.go.
	//
	// instance-state 5 is the cutover: the record space became the only copy.
	// The bump is also the compatibility stamp — a build that predates it stops
	// on a bucket stamped 5 rather than reading the keys it no longer owns.
	InstanceStateBucketVersion      = 5
	ClusterStateBucketVersion       = 1
	TerminatedInstanceBucketVersion = 3
)

// KVSyncObserver receives best-effort KV sync outcomes from
// WriteStateBytesBestEffort. Implementations must be safe for concurrent use
// and must not block — callbacks run in the same goroutine that performed the
// Put. nil observer is allowed.
type KVSyncObserver interface {
	RecordKVSyncSuccess(bucket string)
	RecordKVSyncFailure(bucket string, err error)
}

// JetStreamManager manages JetStream KV store operations for instance state.
//
// The instance-state bucket carries several record types, so it is one kvstore
// bucket with a typed view per type: they share its handle, and a recovery
// driven through any of them repairs all of them. A node's running set is
// stored as LocalState, the same envelope the local state file uses.
type JetStreamManager struct {
	js        jetstream.JetStream
	stateB    *kvstore.Bucket            // spinifex-instance-state
	nodeState *kvstore.Store[LocalState] // nodepresence.<id> markers
	stopped   *kvstore.Store[vm.VM]      // instance.<id>, frozen: drained, never written
	term      *kvstore.Store[vm.VM]      // spinifex-terminated-instances
	// The per-resource key space the three views above are moving onto. Each
	// is a third view over a bucket one of them already holds, so the two
	// spaces share a handle and a recovery driven through either repairs both.
	records     *kvstore.Store[vm.InstanceRecord] // i.<id>, instance-state bucket
	termRecords *kvstore.Store[vm.InstanceRecord] // i.<id>, terminated bucket
	clusterKV   jetstream.KeyValue                // spinifex-cluster-state
	replicas    int
	obs         KVSyncObserver
	running     runningSetState
}

// checkNodeStateVersion reports whether a node record read from KV is one this
// binary can parse. Version 0 predates record versioning and reads as current;
// a version above current was written by a newer node and is not guessed at.
func checkNodeStateVersion(key string, version int) error {
	if version > LocalStateSchemaVersion {
		return fmt.Errorf("instance state %s: record schema_version %d is newer than this node understands (%d)",
			key, version, LocalStateSchemaVersion)
	}
	return nil
}

// SetSyncObserver registers obs to receive best-effort KV sync outcomes. Pass
// nil to clear. Safe to call before or after Init*Bucket.
func (m *JetStreamManager) SetSyncObserver(obs KVSyncObserver) {
	m.obs = obs
}

// KVHealthy reports whether JetStream is reachable and quorate via a cheap
// AccountInfo round-trip. The GC backstop gates every sweep on this so it never
// reaps against a desired-state it cannot trust (ADR-0003 §3).
func (m *JetStreamManager) KVHealthy() bool {
	if m.js == nil {
		return false
	}
	_, err := m.js.AccountInfo(context.Background())
	return err == nil
}

// NewJetStreamManager creates a new JetStreamManager from a NATS connection.
// replicas specifies the number of replicas for the KV bucket (typically matches cluster node count).
func NewJetStreamManager(nc *nats.Conn, replicas int) (*JetStreamManager, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, err
	}

	return &JetStreamManager{
		js:       js,
		replicas: replicas,
	}, nil
}

// instanceStateConfig describes the instance-state bucket. Extracted so the
// bucket can be rebuilt against a different JetStream client without the
// description drifting from the one InitKVBucket creates.
func instanceStateConfig(replicas int) kvstore.Config {
	return kvstore.Config{
		Name:        InstanceStateBucket,
		Description: "Spinifex instance state storage",
		History:     1,
		Replicas:    replicas,
		// The owning node republishes its record on its next write, so an
		// emptied bucket costs a sync rather than the records themselves.
		RecreateIfMissing: true,
		OnOpen: func(ctx context.Context, kv jetstream.KeyValue) error {
			return migrate.DefaultRegistry.RunKV(ctx, InstanceStateBucket, kv, InstanceStateBucketVersion)
		},
		Missing: "KV bucket not initialized",
	}
}

// terminatedInstanceConfig describes the terminated-instances bucket.
func terminatedInstanceConfig(replicas int) kvstore.Config {
	return kvstore.Config{
		Name:              TerminatedInstanceBucket,
		Description:       "Terminated instances (auto-expire after 1 hour)",
		History:           1,
		Replicas:          replicas,
		TTL:               1 * time.Hour,
		RecreateIfMissing: true,
		OnOpen: func(ctx context.Context, kv jetstream.KeyValue) error {
			return migrate.DefaultRegistry.RunKV(ctx, TerminatedInstanceBucket, kv, TerminatedInstanceBucketVersion)
		},
		Missing: "terminated instance KV bucket not initialized",
	}
}

// setInstanceStateBucket points the typed views at b. They share its handle,
// so a recovery driven through any of them repairs all of them.
func (m *JetStreamManager) setInstanceStateBucket(b *kvstore.Bucket) {
	m.stateB = b
	m.nodeState = kvstore.On[LocalState](b)
	m.stopped = kvstore.On[vm.VM](b)
	m.records = kvstore.On[vm.InstanceRecord](b)
}

// InitKVBucket initializes the KV bucket, creating it if it doesn't exist.
func (m *JetStreamManager) InitKVBucket() error {
	m.setInstanceStateBucket(kvstore.NewBucket(m.js, instanceStateConfig(m.replicas)))

	// Opened eagerly: a bucket that cannot be created must fail startup here,
	// not on the first write, and Tier 1 boot must not reach cluster KV at all.
	_, err := m.stateB.KV(context.Background())
	return err
}

// InitClusterStateBucket initializes the cluster-state KV bucket, creating it if it doesn't exist.
func (m *JetStreamManager) InitClusterStateBucket() error {
	ctx := context.Background()
	kv, err := m.js.KeyValue(ctx, ClusterStateBucket)
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			slog.Debug("Creating JetStream KV bucket", "bucket", ClusterStateBucket, "replicas", m.replicas)
			kv, err = m.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
				Bucket:      ClusterStateBucket,
				Description: "Spinifex cluster state (heartbeats, shutdown markers, service maps)",
				History:     1,
				Replicas:    m.replicas,
				TTL:         1 * time.Hour,
			})
			if err != nil {
				return err
			}
		} else {
			return err
		}
	} else {
		slog.Debug("Connected to existing JetStream KV bucket", "bucket", ClusterStateBucket)
	}

	m.clusterKV = kv
	if err := migrate.DefaultRegistry.RunKV(ctx, ClusterStateBucket, kv, ClusterStateBucketVersion); err != nil {
		return fmt.Errorf("migrate %s: %w", ClusterStateBucket, err)
	}
	return nil
}

// InitTerminatedInstanceBucket initializes the terminated-instances KV bucket with a 1-hour TTL.
// JetStream automatically purges keys after 1 hour, matching AWS behavior for terminated instances.
func (m *JetStreamManager) InitTerminatedInstanceBucket() error {
	m.term = kvstore.New[vm.VM](m.js, terminatedInstanceConfig(m.replicas))
	// A second view over the same handle, for the same reason the instance-state
	// bucket carries several.
	m.termRecords = kvstore.On[vm.InstanceRecord](m.term.Bucket)
	_, err := m.term.KV(context.Background())
	return err
}

// Heartbeat represents a daemon's periodic health status published to cluster KV.
//
// AvailableVCPU / AvailableMem are observability-only (host - allocated,
// raw). Scheduling routing happens at the local daemon via admission
// control, which already accounts for the reserve; ReservedVCPU /
// ReservedMem are exposed purely for operator dashboards and capacity
// reporting.
type Heartbeat struct {
	Node          string   `json:"node"`
	Epoch         uint64   `json:"epoch"`
	Timestamp     string   `json:"timestamp"`
	Services      []string `json:"services"`
	VMCount       int      `json:"vm_count"`
	AllocatedVCPU int      `json:"allocated_vcpu"`
	AvailableVCPU int      `json:"available_vcpu"`
	AllocatedMem  float64  `json:"allocated_mem_gb"`
	AvailableMem  float64  `json:"available_mem_gb"`
	ReservedVCPU  int      `json:"reserved_vcpu"`
	ReservedMem   float64  `json:"reserved_mem_gb"`
}

// WriteHeartbeat writes a heartbeat entry for the given node to the cluster-state KV.
func (m *JetStreamManager) WriteHeartbeat(h *Heartbeat) error {
	if m.clusterKV == nil {
		return errors.New("cluster state KV not initialized")
	}
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = m.clusterKV.Put(context.Background(), "heartbeat."+h.Node, data)
	return err
}

// ReadHeartbeat reads the heartbeat entry for the given node from the cluster-state KV.
func (m *JetStreamManager) ReadHeartbeat(nodeID string) (*Heartbeat, error) {
	if m.clusterKV == nil {
		return nil, errors.New("cluster state KV not initialized")
	}
	entry, err := m.clusterKV.Get(context.Background(), "heartbeat."+nodeID)
	if err != nil {
		return nil, err
	}
	var h Heartbeat
	if err := json.Unmarshal(entry.Value(), &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// ClusterShutdownState tracks the coordinated cluster shutdown progress in KV.
type ClusterShutdownState struct {
	Initiator  string            `json:"initiator"`
	Phase      string            `json:"phase"`
	Started    string            `json:"started"`
	Timeout    string            `json:"timeout"`
	Force      bool              `json:"force"`
	NodesTotal int               `json:"nodes_total"`
	NodesAcked map[string]string `json:"nodes_acked"`
}

// WriteClusterShutdown writes the cluster shutdown state to KV, replacing
// any existing value. Uses CAS internally (bounded retry) so a write racing
// a concurrent update is detected and retried rather than silently applied
// out of order.
func (m *JetStreamManager) WriteClusterShutdown(state *ClusterShutdownState) error {
	if m.clusterKV == nil {
		return errors.New("cluster state KV not initialized")
	}
	_, err := kvutil.Update(context.Background(), m.clusterKV, "cluster.shutdown",
		kvutil.CASConfig{CreateIfAbsent: true},
		func(s *ClusterShutdownState) (bool, error) { *s = *state; return true, nil })
	return err
}

// UpdateClusterShutdown atomically applies mutate to the current cluster
// shutdown state (e.g. merging a node's ack into NodesAcked) and writes it
// back with optimistic concurrency, retrying on a concurrent writer's
// revision conflict. Returns jetstream.ErrKeyNotFound if no shutdown is in progress.
func (m *JetStreamManager) UpdateClusterShutdown(mutate func(*ClusterShutdownState)) (*ClusterShutdownState, error) {
	if m.clusterKV == nil {
		return nil, errors.New("cluster state KV not initialized")
	}
	return kvutil.Update(context.Background(), m.clusterKV, "cluster.shutdown", kvutil.CASConfig{},
		func(s *ClusterShutdownState) (bool, error) { mutate(s); return true, nil })
}

// UpdateMgmtIPAM atomically applies mutate to the mgmt-ipam record for
// subnet and writes it back with optimistic concurrency, retrying on a
// concurrent writer's revision conflict. createIfAbsent lets the first
// allocation for a subnet create its record; releases pass false so a
// release of an instance nobody holds an address for does not conjure an
// empty record into existence. Every node sharing the management bridge's
// L2 segment reads and writes the same record, keyed by subnet, which is
// what makes mgmt IP allocation safe across the whole cluster rather than
// just this process.
func (m *JetStreamManager) UpdateMgmtIPAM(subnet string, mutate func(*MgmtIPRecord), createIfAbsent bool) (*MgmtIPRecord, error) {
	if m.clusterKV == nil {
		return nil, errors.New("cluster state KV not initialized")
	}
	return kvutil.Update(context.Background(), m.clusterKV, mgmtIPAMKeyPrefix+subnet,
		kvutil.CASConfig{CreateIfAbsent: createIfAbsent},
		func(r *MgmtIPRecord) (bool, error) { mutate(r); return true, nil })
}

// ReadClusterShutdown reads the cluster shutdown state from KV.
func (m *JetStreamManager) ReadClusterShutdown() (*ClusterShutdownState, error) {
	if m.clusterKV == nil {
		return nil, errors.New("cluster state KV not initialized")
	}
	entry, err := m.clusterKV.Get(context.Background(), "cluster.shutdown")
	if err != nil {
		return nil, err
	}
	var state ClusterShutdownState
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// DeleteClusterShutdown removes the cluster shutdown state from KV.
func (m *JetStreamManager) DeleteClusterShutdown() error {
	if m.clusterKV == nil {
		return errors.New("cluster state KV not initialized")
	}
	err := m.clusterKV.Delete(context.Background(), "cluster.shutdown")
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// WriteShutdownMarker writes a shutdown marker for the given node to the cluster-state KV.
func (m *JetStreamManager) WriteShutdownMarker(nodeID string) error {
	if m.clusterKV == nil {
		return errors.New("cluster state KV not initialized")
	}
	data, err := json.Marshal(map[string]any{
		"node":      nodeID,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal shutdown marker: %w", err)
	}
	_, err = m.clusterKV.Put(context.Background(), "shutdown."+nodeID, data)
	return err
}

// ReadShutdownMarker checks if a clean shutdown marker exists for the given node.
func (m *JetStreamManager) ReadShutdownMarker(nodeID string) (bool, error) {
	if m.clusterKV == nil {
		return false, errors.New("cluster state KV not initialized")
	}
	_, err := m.clusterKV.Get(context.Background(), "shutdown."+nodeID)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// DeleteShutdownMarker removes the shutdown marker for the given node.
func (m *JetStreamManager) DeleteShutdownMarker(nodeID string) error {
	if m.clusterKV == nil {
		return errors.New("cluster state KV not initialized")
	}
	err := m.clusterKV.Delete(context.Background(), "shutdown."+nodeID)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// WriteServiceManifest writes the service manifest for the given node to the cluster-state KV.
func (m *JetStreamManager) WriteServiceManifest(nodeID string, services []string, natsHost, predastoreHost string) error {
	if m.clusterKV == nil {
		return errors.New("cluster state KV not initialized")
	}
	data, err := json.Marshal(map[string]any{
		"node":            nodeID,
		"services":        services,
		"nats_host":       natsHost,
		"predastore_host": predastoreHost,
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal service manifest: %w", err)
	}
	_, err = m.clusterKV.Put(context.Background(), "node."+nodeID+".services", data)
	return err
}

// WriteNodeMarker records that this node has cluster state at all, without
// recording what it is.
//
// Reads need it because "this node has no instances" and "there is no cluster
// record of this node" are different answers and only one of them may replace a
// node's local state. Scanning the records cannot tell them apart — both scan
// empty — and restore adopts the cluster's set wholesale when it believes there
// is one, so conflating them drops every instance on a node whose records are
// briefly unreadable.
//
// The marker carries no instances, so a state change no longer rewrites the
// node's whole set. That is the cost the split exists to remove; keeping an
// empty envelope keeps the answer and not the cost.
func (m *JetStreamManager) WriteNodeMarker(nodeID string) error {
	if m.nodeState == nil {
		return errors.New("KV bucket not initialized")
	}

	key := NodePresencePrefix + nodeID
	marker := LocalState{SchemaVersion: LocalStateSchemaVersion}
	return m.nodeState.Set(context.Background(), key, &marker)
}

// WriteNodeMarkerBestEffort writes this node's presence marker with a deadline.
// On timeout or error it logs and returns — never blocks the caller past
// timeout and never returns an error. The local state file is the source of
// truth; KV is the cluster's view of it, and the next state change retries.
func (m *JetStreamManager) WriteNodeMarkerBestEffort(nodeID string, timeout time.Duration) {
	if m.nodeState == nil {
		slog.Debug("KV bucket not initialized, skipping cluster sync", "node", nodeID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	key := NodePresencePrefix + nodeID
	marker := LocalState{SchemaVersion: LocalStateSchemaVersion}
	if err := m.nodeState.Set(ctx, key, &marker); err != nil {
		if m.obs != nil {
			m.obs.RecordKVSyncFailure(InstanceStateBucket, err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("KV sync timed out (best-effort)", "key", key, "timeout_ms", otelsetup.Millis(timeout))
			return
		}
		slog.Warn("KV sync failed (best-effort)", "key", key, "err", err)
		return
	}
	if m.obs != nil {
		m.obs.RecordKVSyncSuccess(InstanceStateBucket)
	}
	slog.Debug("Wrote node marker to KV (best-effort)", "key", key)
}

// LoadState loads the instances the given node owns, from one record each.
//
// The bool reports whether the cluster has a record of this node at all, and it
// is the marker's answer rather than the records'. A node with no instances and
// a node whose records could not be read both scan empty, and only the first of
// them may replace what the node has locally.
func (m *JetStreamManager) LoadState(nodeID string) (map[string]*vm.VM, bool, error) {
	if m.nodeState == nil {
		return nil, false, errors.New("KV bucket not initialized")
	}

	key := NodePresencePrefix + nodeID
	marker, _, err := m.nodeState.Get(context.Background(), key)
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			slog.Debug("No existing state in JetStream KV", "key", key)
			return make(map[string]*vm.VM), false, nil
		}
		return nil, false, err
	}
	if err := checkNodeStateVersion(key, marker.SchemaVersion); err != nil {
		return nil, false, err
	}

	vms, err := m.nodeRunningRecords(nodeID)
	if err != nil {
		return nil, false, err
	}

	slog.Debug("Loaded state from JetStream KV", "node", nodeID, "instances", len(vms))
	return vms, true, nil
}

// DeleteState removes the instance state from the KV store for the given node.
func (m *JetStreamManager) DeleteState(nodeID string) error {
	if m.nodeState == nil {
		return errors.New("KV bucket not initialized")
	}

	key := NodePresencePrefix + nodeID
	if err := m.nodeState.Delete(context.Background(), key); err != nil {
		return err
	}

	slog.Debug("Deleted state from JetStream KV", "key", key)
	return nil
}

// UpdateReplicas updates the replica count for ALL JetStream KV buckets.
// It iterates over every KV_* stream and bumps replicas to match the cluster size.
// This ensures service buckets (IAM, VPC, IGW, etc.) are replicated alongside daemon buckets.
// This should be called when new nodes join the cluster.
func (m *JetStreamManager) UpdateReplicas(newReplicas int) error {
	if m.js == nil {
		return errors.New("JetStream context not initialized")
	}

	m.replicas = newReplicas

	ctx := context.Background()
	// Iterate all streams and update any KV-backed stream (prefixed "KV_")
	updated := 0
	lister := m.js.StreamNames(ctx)
	for name := range lister.Name() {
		if !strings.HasPrefix(name, "KV_") {
			continue
		}

		stream, err := m.js.Stream(ctx, name)
		if err != nil {
			slog.Warn("Failed to open stream", "stream", name, "error", err)
			continue
		}
		info, err := stream.Info(ctx)
		if err != nil {
			slog.Warn("Failed to get stream info", "stream", name, "error", err)
			continue
		}

		if info.Config.Replicas >= newReplicas {
			continue
		}

		oldReplicas := info.Config.Replicas
		info.Config.Replicas = newReplicas
		if _, err := m.js.UpdateStream(ctx, info.Config); err != nil {
			slog.Warn("Failed to update KV bucket replicas", "stream", name, "error", err)
			continue
		}

		bucket := strings.TrimPrefix(name, "KV_")
		slog.Info("Updated KV bucket replicas", "bucket", bucket, "oldReplicas", oldReplicas, "newReplicas", newReplicas)
		updated++
	}
	if err := lister.Err(); err != nil {
		return fmt.Errorf("list JetStream streams: %w", err)
	}

	if updated > 0 {
		slog.Info("KV replication update complete", "bucketsUpdated", updated, "replicas", newReplicas)
	}

	return nil
}

// WriteStoppedInstance writes a stopped instance to the shared KV store,
// replacing any existing record for instanceID. Uses CAS internally (bounded
// retry) so a write racing a concurrent update is detected and retried
// rather than silently overwriting it out of order. This is the substrate
// callers that read-modify-write a stopped instance (e.g. tag/attribute
// mutations) build on.
//
// The instance keeps the one key it has held since it was launched. Stopping
// it rewrites that key rather than moving it, so nothing can observe the
// instance at neither key or at both.
func (m *JetStreamManager) WriteStoppedInstance(instanceID string, instance *vm.VM) error {
	if m.records == nil {
		return errors.New("KV bucket not initialized")
	}

	// The write stamps what it means rather than trusting the caller to have.
	// Membership is a predicate over the record now, so an instance written
	// here without both fields set would be stored and then not found — and the
	// caller that forgot is not the one that discovers it.
	record := instance.Record()
	record.Status.Status = vm.StateStopped
	record.Spec.DesiredState = vm.DesiredStopped
	if err := m.records.Replace(context.Background(), instanceRecordKey(instanceID), record); err != nil {
		return err
	}

	slog.Debug("Wrote stopped instance to JetStream KV", "instanceId", instanceID)
	return nil
}

// LoadStoppedInstance loads a stopped instance from the shared KV store.
// Returns nil, nil when no record exists, and also when one does but holds an
// instance that is not stopped: the key is shared with this node's running set,
// so the record has to be asked what it is.
func (m *JetStreamManager) LoadStoppedInstance(instanceID string) (*vm.VM, error) {
	if m.records == nil {
		return nil, errors.New("KV bucket not initialized")
	}
	return loadInstance(m.records, instanceID, operatorStopped)
}

// DeleteStoppedInstance removes a stopped instance from the shared KV store. It
// is idempotent — deleting a non-existent key is not an error.
func (m *JetStreamManager) DeleteStoppedInstance(instanceID string) error {
	if m.records == nil {
		return errors.New("KV bucket not initialized")
	}

	if err := m.records.Delete(context.Background(), instanceRecordKey(instanceID)); err != nil {
		return err
	}
	m.drainFrozenKey(m.stopped, StoppedInstancePrefix+instanceID)

	slog.Debug("Deleted stopped instance from JetStream KV", "instanceId", instanceID)
	return nil
}

// ClaimStoppedInstance atomically removes instanceID's record from the
// shared KV store and returns the VM it held, so that at most one caller
// across the cluster can ever win a race to (re)launch the same stopped
// instance. It is the first step of StartStoppedInstance, replacing the
// unguarded Load+Delete pair that previously let two callers both observe
// the instance as claimable. A losing racer (this node's local fallback
// racing a forwarded call, two nodes racing the same forwarded call, or a
// retry after the instance was already claimed) gets vm.ErrStoppedInstanceClaimed
// instead of a VM and must not proceed to allocate resources or launch qemu.
//
// Exclusivity is a compare-and-set on the record rather than a delete of it.
// A delete was safe while the key held nothing but a stopped instance; the key
// holds the instance for its whole life now, so deleting to claim it is
// deleting the instance, and a claimant that fails after winning takes the
// instance with it.
//
// The winner clears DesiredStopped, which is what makes the record no longer
// claimable. A loser's CAS fails on the revision, re-reads, finds an instance
// nobody asked to be stopped, and reports the claim lost — so exactly one
// caller can win, which is the property the delete had.
//
// LastNode is left alone. The claimant has not run the instance yet, and the
// node that last did is the one that should recover it if this launch never
// happens.
func (m *JetStreamManager) ClaimStoppedInstance(instanceID string) (*vm.VM, error) {
	if m.records == nil {
		return nil, errors.New("KV bucket not initialized")
	}

	key := instanceRecordKey(instanceID)
	record, rev, err := m.records.Get(context.Background(), key)
	if err != nil {
		if errors.Is(err, kvstore.ErrNotFound) {
			return nil, vm.ErrStoppedInstanceClaimed
		}
		return nil, err
	}
	if !operatorStopped(record) {
		return nil, vm.ErrStoppedInstanceClaimed
	}

	claimed := *record
	claimed.Spec.DesiredState = vm.DesiredRunning
	if err := m.records.CompareAndSet(context.Background(), key, &claimed, rev); err != nil {
		if errors.Is(err, kvstore.ErrConflict) {
			return nil, vm.ErrStoppedInstanceClaimed
		}
		return nil, err
	}
	m.drainFrozenKey(m.stopped, StoppedInstancePrefix+instanceID)

	slog.Debug("Claimed stopped instance from JetStream KV", "instanceId", instanceID)
	return vm.VMFromRecord(&claimed), nil
}

// UpdateStoppedInstance atomically applies mutate to the current KV-stored
// stopped record for instanceID and writes it back using optimistic
// concurrency (CAS), retrying on a concurrent writer's revision conflict.
// createIfAbsent is false: if a winning ClaimStoppedInstance deletes the
// record mid-flight, the CAS write fails with jetstream.ErrKeyNotFound instead of
// resurrecting it. Used by tag/attribute mutations that read-modify-write a
// stopped instance so they cannot race a claim into recreating a stale
// record. Returns jetstream.ErrKeyNotFound if no record exists.
//
// The mutation runs against the record, on the one key the instance has, so
// there is a single CAS anchor rather than two that can diverge.
func (m *JetStreamManager) UpdateStoppedInstance(instanceID string, mutate func(*vm.VM)) (*vm.VM, error) {
	if m.records == nil {
		return nil, errors.New("KV bucket not initialized")
	}
	return mutateInstance(m.records, instanceID, operatorStopped, mutate)
}

// mutateInstance applies mutate to the instance a record holds, under the
// store's CAS loop, and returns what committed. The callers speak vm.VM and the
// store speaks records, so the conversion happens inside the loop rather than
// around it — a retry has to re-read and re-convert, not replay a stale copy.
//
// want is checked inside the loop, and a record it rejects is reported absent.
// The key outlives the set it is in now, so "still stopped" has to be re-tested
// on the value each attempt reads: a claim that landed between a caller's own
// load and this write leaves the key there, and without the test the caller's
// mutation would land on an instance somebody else has already started.
func mutateInstance(store *kvstore.Store[vm.InstanceRecord], instanceID string,
	want func(*vm.InstanceRecord) bool, mutate func(*vm.VM)) (*vm.VM, error) {
	var updated *vm.VM
	err := store.Mutate(context.Background(), instanceRecordKey(instanceID),
		func(record *vm.InstanceRecord) (bool, error) {
			if want != nil && !want(record) {
				return false, kvstore.ErrNotFound
			}
			instance := vm.VMFromRecord(record)
			mutate(instance)
			*record = *instance.Record()
			updated = instance
			return true, nil
		})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// drainFrozenKey removes the key the record replaced. The old key spaces are
// frozen rather than deleted at the cutover, so the crossing can be rolled
// back — but an instance that is gone should not be recoverable, and draining
// them on the delete path is what stops the frozen space growing forever.
func (m *JetStreamManager) drainFrozenKey(store *kvstore.Store[vm.VM], key string) {
	if store == nil {
		return
	}
	if err := store.Delete(context.Background(), key); err != nil {
		slog.Debug("Could not drain a frozen key", "key", key, "err", err)
	}
}

// mutateRecord applies mutate under the store's CAS loop and returns the
// committed record. An absent key surfaces as kvstore.ErrNotFound, which is what
// both StateStore interfaces are written against.
func mutateRecord[T any](store *kvstore.Store[T], key string, mutate func(*T)) (*T, error) {
	var updated *T
	err := store.Mutate(context.Background(), key, func(v *T) (bool, error) {
		mutate(v)
		updated = v
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// ListStoppedInstances returns the stopped instances in the shared KV store.
// The key space also holds every running instance, so membership is the
// record's answer rather than the prefix's.
func (m *JetStreamManager) ListStoppedInstances() ([]*vm.VM, error) {
	if m.records == nil {
		return nil, errors.New("KV bucket not initialized")
	}
	return listInstances(m.records, operatorStopped)
}

// listRecords returns every record under prefix. Unlike the raw scan it
// replaces it fails on an undecodable record rather than skipping it: these
// listings feed DescribeInstances, where a silently dropped instance reads as
// terminated.
func listRecords[T any](store *kvstore.Store[T], prefix string) ([]*T, error) {
	records, err := store.List(context.Background(), prefix)
	if err != nil {
		return nil, err
	}
	out := make([]*T, 0, len(records))
	for i := range records {
		out = append(out, &records[i])
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// WriteTerminatedInstance writes a terminated instance to the terminated KV
// bucket, replacing any existing record for instanceID. The entry will
// auto-expire after the bucket's TTL (1 hour). Uses CAS internally (bounded
// retry) so a write racing a concurrent update (e.g. the teardown reaper
// advancing the Teardown map) is detected and retried rather than silently
// overwriting it out of order. Callers that need to merge into the current
// record instead of replacing it wholesale should use
// UpdateTerminatedInstance.
func (m *JetStreamManager) WriteTerminatedInstance(instanceID string, instance *vm.VM) error {
	if m.termRecords == nil {
		return errors.New("terminated instance KV bucket not initialized")
	}

	if err := writeRecord(m.termRecords, instanceID, instance); err != nil {
		return err
	}

	slog.Debug("Wrote terminated instance to JetStream KV", "instanceId", instanceID)
	return nil
}

// UpdateTerminatedInstance atomically applies mutate to the current
// KV-stored terminated record for instanceID and writes it back using
// optimistic concurrency (CAS), retrying on a concurrent writer's revision
// conflict. Used by the teardown reaper to merge per-dependent Teardown
// progress without clobbering marks written by a concurrent update to the
// same record. Returns jetstream.ErrKeyNotFound if no record exists yet.
func (m *JetStreamManager) UpdateTerminatedInstance(instanceID string, mutate func(*vm.VM)) (*vm.VM, error) {
	if m.termRecords == nil {
		return nil, errors.New("terminated instance KV bucket not initialized")
	}
	// No predicate: the terminated bucket is its own key space, so being in it
	// is the whole of the membership test.
	return mutateInstance(m.termRecords, instanceID, nil, mutate)
}

// ListTerminatedInstances returns all terminated instances from the terminated
// KV bucket. Its key space holds nothing else, so every record is one.
func (m *JetStreamManager) ListTerminatedInstances() ([]*vm.VM, error) {
	if m.termRecords == nil {
		return nil, errors.New("terminated instance KV bucket not initialized")
	}
	return listInstances(m.termRecords, nil)
}

// DeleteTerminatedInstance removes a terminated instance from the terminated KV
// bucket.
func (m *JetStreamManager) DeleteTerminatedInstance(instanceID string) error {
	if m.termRecords == nil {
		return errors.New("terminated instance KV bucket not initialized")
	}

	if err := m.termRecords.Delete(context.Background(), instanceRecordKey(instanceID)); err != nil {
		return err
	}
	m.drainFrozenKey(m.term, TerminatedInstancePrefix+instanceID)

	slog.Debug("Deleted terminated instance from JetStream KV", "instanceId", instanceID)
	return nil
}

// LoadTerminatedInstance loads a single terminated instance from the terminated
// KV bucket. Returns nil, nil if the instance does not exist.
func (m *JetStreamManager) LoadTerminatedInstance(instanceID string) (*vm.VM, error) {
	if m.termRecords == nil {
		return nil, errors.New("terminated instance KV bucket not initialized")
	}
	return loadInstance(m.termRecords, instanceID, nil)
}
