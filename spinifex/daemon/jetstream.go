package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

const (
	// InstanceStateBucket is the name of the KV bucket for storing instance state
	InstanceStateBucket = "spinifex-instance-state"
	// ClusterStateBucket is the name of the KV bucket for cluster state (heartbeats, shutdown markers, service maps)
	ClusterStateBucket = "spinifex-cluster-state"
	// InstanceStatePrefix is the key prefix for per-node instance state entries
	InstanceStatePrefix = "node."
	// StoppedInstancePrefix is the key prefix for stopped instances in shared KV
	StoppedInstancePrefix = "instance."
	// TerminatedInstanceBucket is the name of the KV bucket for terminated instances (auto-expiry via TTL)
	TerminatedInstanceBucket = "spinifex-terminated-instances"
	// TerminatedInstancePrefix is the key prefix for terminated instances
	TerminatedInstancePrefix = "terminated."

	// Schema versions for daemon KV buckets
	InstanceStateBucketVersion      = 1
	ClusterStateBucketVersion       = 1
	TerminatedInstanceBucketVersion = 1
)

// KVSyncObserver receives best-effort KV sync outcomes from
// WriteStateBytesBestEffort. Implementations must be safe for concurrent use
// and must not block — callbacks run in the same goroutine that performed the
// Put. nil observer is allowed.
type KVSyncObserver interface {
	RecordKVSyncSuccess(bucket string)
	RecordKVSyncFailure(bucket string, err error)
}

// JetStreamManager manages JetStream KV store operations for instance state
type JetStreamManager struct {
	js           nats.JetStreamContext
	kv           nats.KeyValue // spinifex-instance-state
	clusterKV    nats.KeyValue // spinifex-cluster-state
	terminatedKV nats.KeyValue // spinifex-terminated-instances
	replicas     int
	kvMu         sync.Mutex // protects kv during recovery
	obs          KVSyncObserver
}

// SetSyncObserver registers obs to receive best-effort KV sync outcomes. Pass
// nil to clear. Safe to call before or after Init*Bucket.
func (m *JetStreamManager) SetSyncObserver(obs KVSyncObserver) {
	m.obs = obs
}

// NewJetStreamManager creates a new JetStreamManager from a NATS connection.
// replicas specifies the number of replicas for the KV bucket (typically matches cluster node count).
func NewJetStreamManager(nc *nats.Conn, replicas int) (*JetStreamManager, error) {
	js, err := nc.JetStream()
	if err != nil {
		return nil, err
	}

	return &JetStreamManager{
		js:       js,
		replicas: replicas,
	}, nil
}

// InitKVBucket initializes the KV bucket, creating it if it doesn't exist
func (m *JetStreamManager) InitKVBucket() error {
	// Try to get the existing bucket first
	kv, err := m.js.KeyValue(InstanceStateBucket)
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			// Bucket doesn't exist, create it
			slog.Debug("Creating JetStream KV bucket", "bucket", InstanceStateBucket, "replicas", m.replicas)
			kv, err = m.js.CreateKeyValue(&nats.KeyValueConfig{
				Bucket:      InstanceStateBucket,
				Description: "Spinifex instance state storage",
				History:     1,          // Only keep latest value
				Replicas:    m.replicas, // Replication across cluster nodes
			})
			if err != nil {
				return err
			}
		} else {
			return err
		}
	} else {
		slog.Debug("Connected to existing JetStream KV bucket", "bucket", InstanceStateBucket)
	}

	m.kv = kv
	if err := migrate.DefaultRegistry.RunKV(InstanceStateBucket, kv, InstanceStateBucketVersion); err != nil {
		return fmt.Errorf("migrate %s: %w", InstanceStateBucket, err)
	}
	return nil
}

// InitClusterStateBucket initializes the cluster-state KV bucket, creating it if it doesn't exist.
func (m *JetStreamManager) InitClusterStateBucket() error {
	kv, err := m.js.KeyValue(ClusterStateBucket)
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			slog.Debug("Creating JetStream KV bucket", "bucket", ClusterStateBucket, "replicas", m.replicas)
			kv, err = m.js.CreateKeyValue(&nats.KeyValueConfig{
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
	if err := migrate.DefaultRegistry.RunKV(ClusterStateBucket, kv, ClusterStateBucketVersion); err != nil {
		return fmt.Errorf("migrate %s: %w", ClusterStateBucket, err)
	}
	return nil
}

// InitTerminatedInstanceBucket initializes the terminated-instances KV bucket with a 1-hour TTL.
// JetStream automatically purges keys after 1 hour, matching AWS behavior for terminated instances.
func (m *JetStreamManager) InitTerminatedInstanceBucket() error {
	kv, err := m.js.KeyValue(TerminatedInstanceBucket)
	if err != nil {
		if errors.Is(err, nats.ErrBucketNotFound) {
			slog.Debug("Creating JetStream KV bucket", "bucket", TerminatedInstanceBucket, "replicas", m.replicas)
			kv, err = m.js.CreateKeyValue(&nats.KeyValueConfig{
				Bucket:      TerminatedInstanceBucket,
				Description: "Terminated instances (auto-expire after 1 hour)",
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
		slog.Debug("Connected to existing JetStream KV bucket", "bucket", TerminatedInstanceBucket)
	}

	m.terminatedKV = kv
	if err := migrate.DefaultRegistry.RunKV(TerminatedInstanceBucket, kv, TerminatedInstanceBucketVersion); err != nil {
		return fmt.Errorf("migrate %s: %w", TerminatedInstanceBucket, err)
	}
	return nil
}

// isStreamUnavailable checks if an error indicates the underlying JetStream stream
// was lost or is unreachable. This can happen during NATS cluster formation when
// streams created with low replication are disrupted by node join/catchup operations.
// Different KV operations surface different errors when the stream is gone:
//   - Get/Keys → ErrNoResponders ("no responders available for request")
//   - Put/Delete → ErrNoStreamResponse ("no response from stream")
//   - Direct stream queries → ErrStreamNotFound ("stream not found")
func isStreamUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, nats.ErrStreamNotFound) ||
		errors.Is(err, nats.ErrNoStreamResponse) ||
		errors.Is(err, nats.ErrNoResponders) {
		return true
	}
	return strings.Contains(err.Error(), "stream not found")
}

// recoverBucket attempts to reconnect to or re-create a KV bucket after the
// underlying JetStream stream was lost during cluster formation.
// Returns the recovered KV handle directly so callers avoid a racy re-read.
// When a bucket is recreated, the schema version is re-stamped.
func (m *JetStreamManager) recoverBucket(cfg *nats.KeyValueConfig, field *nats.KeyValue, version int) (nats.KeyValue, error) {
	m.kvMu.Lock()
	defer m.kvMu.Unlock()

	// Try to reconnect to existing bucket first (another goroutine may have recovered it)
	kv, err := m.js.KeyValue(cfg.Bucket)
	if err == nil {
		*field = kv
		slog.Info("Reconnected to KV bucket", "bucket", cfg.Bucket)
		return kv, nil
	}

	if !errors.Is(err, nats.ErrBucketNotFound) && !isStreamUnavailable(err) {
		return nil, err
	}

	// Bucket truly doesn't exist — recreate it
	slog.Warn("KV bucket stream lost, recreating", "bucket", cfg.Bucket, "replicas", m.replicas)
	cfg.History = 1
	cfg.Replicas = m.replicas
	kv, err = m.js.CreateKeyValue(cfg)
	if err != nil {
		slog.Error("Failed to recreate KV bucket", "bucket", cfg.Bucket, "err", err)
		return nil, err
	}

	if err := migrate.DefaultRegistry.RunKV(cfg.Bucket, kv, version); err != nil {
		slog.Error("Failed to run migrations on recreated bucket", "bucket", cfg.Bucket, "err", err)
		return nil, fmt.Errorf("migrate recreated bucket %s: %w", cfg.Bucket, err)
	}

	*field = kv
	slog.Info("KV bucket recreated successfully", "bucket", cfg.Bucket)
	return kv, nil
}

func (m *JetStreamManager) recoverKVBucket() (nats.KeyValue, error) {
	return m.recoverBucket(&nats.KeyValueConfig{
		Bucket:      InstanceStateBucket,
		Description: "Spinifex instance state storage",
	}, &m.kv, InstanceStateBucketVersion)
}

func (m *JetStreamManager) recoverTerminatedKVBucket() (nats.KeyValue, error) {
	return m.recoverBucket(&nats.KeyValueConfig{
		Bucket:      TerminatedInstanceBucket,
		Description: "Terminated instances (auto-expire after 1 hour)",
		TTL:         1 * time.Hour,
	}, &m.terminatedKV, TerminatedInstanceBucketVersion)
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
	_, err = m.clusterKV.Put("heartbeat."+h.Node, data)
	return err
}

// ReadHeartbeat reads the heartbeat entry for the given node from the cluster-state KV.
func (m *JetStreamManager) ReadHeartbeat(nodeID string) (*Heartbeat, error) {
	if m.clusterKV == nil {
		return nil, errors.New("cluster state KV not initialized")
	}
	entry, err := m.clusterKV.Get("heartbeat." + nodeID)
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

// WriteClusterShutdown writes the cluster shutdown state to KV.
func (m *JetStreamManager) WriteClusterShutdown(state *ClusterShutdownState) error {
	if m.clusterKV == nil {
		return errors.New("cluster state KV not initialized")
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = m.clusterKV.Put("cluster.shutdown", data)
	return err
}

// ReadClusterShutdown reads the cluster shutdown state from KV.
func (m *JetStreamManager) ReadClusterShutdown() (*ClusterShutdownState, error) {
	if m.clusterKV == nil {
		return nil, errors.New("cluster state KV not initialized")
	}
	entry, err := m.clusterKV.Get("cluster.shutdown")
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
	err := m.clusterKV.Delete("cluster.shutdown")
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
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
	_, err = m.clusterKV.Put("shutdown."+nodeID, data)
	return err
}

// ReadShutdownMarker checks if a clean shutdown marker exists for the given node.
func (m *JetStreamManager) ReadShutdownMarker(nodeID string) (bool, error) {
	if m.clusterKV == nil {
		return false, errors.New("cluster state KV not initialized")
	}
	_, err := m.clusterKV.Get("shutdown." + nodeID)
	if errors.Is(err, nats.ErrKeyNotFound) {
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
	err := m.clusterKV.Delete("shutdown." + nodeID)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
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
	_, err = m.clusterKV.Put("node."+nodeID+".services", data)
	return err
}

// WriteState writes the instance state to the KV store for the given node.
// vms must be a snapshot owned by the caller — JetStreamManager does not lock.
func (m *JetStreamManager) WriteState(nodeID string, vms map[string]*vm.VM) error {
	if m.kv == nil {
		return errors.New("KV bucket not initialized")
	}

	jsonData, err := marshalInstanceState(vms)
	if err != nil {
		return err
	}

	key := InstanceStatePrefix + nodeID
	_, err = m.kv.Put(key, jsonData)
	if err != nil {
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "WriteState", "key", key, "err", err)
			kv, recoverErr := m.recoverKVBucket()
			if recoverErr != nil {
				return err
			}
			if _, retryErr := kv.Put(key, jsonData); retryErr != nil {
				return retryErr
			}
			slog.Debug("Wrote state to JetStream KV (after recovery)", "key", key, "instances", len(vms))
			return nil
		}
		return err
	}

	slog.Debug("Wrote state to JetStream KV", "key", key, "instances", len(vms))
	return nil
}

// WriteStateBestEffort attempts to push instance state to KV with a deadline.
// On timeout or error, it logs a warning and returns — never blocks the caller
// past `timeout` and never returns an error. Used when the local state file is
// the source of truth and KV is a best-effort cache. vms must be a snapshot
// owned by the caller (e.g. from vm.Manager.SnapshotMap).
//
// Note: kv.Put has no context API. On timeout, the in-flight Put goroutine
// continues and completes (or fails) on its own. This leaks at most one
// goroutine per write; under sustained partition the leak is bounded by
// WriteState call cadence (per-state-transition, not a tight loop).
func (m *JetStreamManager) WriteStateBestEffort(nodeID string, vms map[string]*vm.VM, timeout time.Duration) {
	if m.kv == nil {
		slog.Debug("KV bucket not initialized, skipping cluster sync", "node", nodeID)
		return
	}

	jsonData, err := marshalInstanceState(vms)
	if err != nil {
		slog.Warn("KV sync skipped: marshal failed", "node", nodeID, "err", err)
		return
	}

	m.WriteStateBytesBestEffort(nodeID, jsonData, timeout)
}

// WriteStateBytesBestEffort behaves like WriteStateBestEffort but accepts
// pre-marshalled JSON. Used by hot paths that marshal under a short-lived lock
// and commit lock-free.
func (m *JetStreamManager) WriteStateBytesBestEffort(nodeID string, jsonData []byte, timeout time.Duration) {
	if m.kv == nil {
		slog.Debug("KV bucket not initialized, skipping cluster sync", "node", nodeID)
		return
	}

	key := InstanceStatePrefix + nodeID
	done := make(chan error, 1)
	go func() {
		_, putErr := m.kv.Put(key, jsonData)
		done <- putErr
	}()

	select {
	case putErr := <-done:
		if putErr != nil {
			if m.obs != nil {
				m.obs.RecordKVSyncFailure(InstanceStateBucket, putErr)
			}
			slog.Warn("KV sync failed (best-effort)", "key", key, "err", putErr)
			return
		}
		if m.obs != nil {
			m.obs.RecordKVSyncSuccess(InstanceStateBucket)
		}
		slog.Debug("Wrote state to KV (best-effort)", "key", key, "bytes", len(jsonData))
	case <-time.After(timeout):
		if m.obs != nil {
			m.obs.RecordKVSyncFailure(InstanceStateBucket, fmt.Errorf("kv sync timeout after %s", timeout))
		}
		slog.Warn("KV sync timed out (best-effort)", "key", key, "timeout", timeout)
	}
}

// marshalInstanceState produces the JSON wire form of vms.
func marshalInstanceState(vms map[string]*vm.VM) ([]byte, error) {
	state := struct {
		VMS map[string]*vm.VM `json:"vms"`
	}{
		VMS: vms,
	}
	return json.Marshal(state)
}

// LoadState loads the instance state from the KV store for the given node.
// Returns an empty (non-nil) map when no state exists for the node.
func (m *JetStreamManager) LoadState(nodeID string) (map[string]*vm.VM, error) {
	if m.kv == nil {
		return nil, errors.New("KV bucket not initialized")
	}

	key := InstanceStatePrefix + nodeID
	entry, err := m.kv.Get(key)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			slog.Debug("No existing state in JetStream KV, returning empty state", "key", key)
			return make(map[string]*vm.VM), nil
		}
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "LoadState", "key", key, "err", err)
			kv, recoverErr := m.recoverKVBucket()
			if recoverErr != nil {
				return nil, err
			}
			// Retry the read — if we reconnected, data may still exist
			entry, err = kv.Get(key)
			if err != nil {
				if errors.Is(err, nats.ErrKeyNotFound) {
					slog.Warn("No state found after KV recovery", "key", key)
					return make(map[string]*vm.VM), nil
				}
				return nil, err
			}
			// Fall through to unmarshal below
		} else {
			return nil, err
		}
	}

	var state struct {
		VMS map[string]*vm.VM `json:"vms"`
	}
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		return nil, err
	}
	if state.VMS == nil {
		state.VMS = make(map[string]*vm.VM)
	}

	slog.Debug("Loaded state from JetStream KV", "key", key, "instances", len(state.VMS))
	return state.VMS, nil
}

// DeleteState removes the instance state from the KV store for the given node
func (m *JetStreamManager) DeleteState(nodeID string) error {
	if m.kv == nil {
		return errors.New("KV bucket not initialized")
	}

	key := InstanceStatePrefix + nodeID
	err := m.kv.Delete(key)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "DeleteState", "key", key, "err", err)
			kv, recoverErr := m.recoverKVBucket()
			if recoverErr != nil {
				return err
			}
			// Retry — if we reconnected the key may still exist
			if retryErr := kv.Delete(key); retryErr != nil && !errors.Is(retryErr, nats.ErrKeyNotFound) {
				return retryErr
			}
			return nil
		}
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

	// Iterate all streams and update any KV-backed stream (prefixed "KV_")
	updated := 0
	for name := range m.js.StreamNames() {
		if !strings.HasPrefix(name, "KV_") {
			continue
		}

		info, err := m.js.StreamInfo(name)
		if err != nil {
			slog.Warn("Failed to get stream info", "stream", name, "error", err)
			continue
		}

		if info.Config.Replicas >= newReplicas {
			continue
		}

		oldReplicas := info.Config.Replicas
		info.Config.Replicas = newReplicas
		if _, err := m.js.UpdateStream(&info.Config); err != nil {
			slog.Warn("Failed to update KV bucket replicas", "stream", name, "error", err)
			continue
		}

		bucket := strings.TrimPrefix(name, "KV_")
		slog.Info("Updated KV bucket replicas", "bucket", bucket, "oldReplicas", oldReplicas, "newReplicas", newReplicas)
		updated++
	}

	if updated > 0 {
		slog.Info("KV replication update complete", "bucketsUpdated", updated, "replicas", newReplicas)
	}

	return nil
}

// WriteStoppedInstance writes a stopped instance to the shared KV store.
func (m *JetStreamManager) WriteStoppedInstance(instanceID string, instance *vm.VM) error {
	if m.kv == nil {
		return errors.New("KV bucket not initialized")
	}

	jsonData, err := json.Marshal(instance)
	if err != nil {
		return err
	}

	key := StoppedInstancePrefix + instanceID
	_, err = m.kv.Put(key, jsonData)
	if err != nil {
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "WriteStoppedInstance", "key", key, "err", err)
			kv, recoverErr := m.recoverKVBucket()
			if recoverErr != nil {
				return err
			}
			if _, retryErr := kv.Put(key, jsonData); retryErr != nil {
				return retryErr
			}
			slog.Debug("Wrote stopped instance to JetStream KV (after recovery)", "key", key, "instanceId", instanceID)
			return nil
		}
		return err
	}

	slog.Debug("Wrote stopped instance to JetStream KV", "key", key, "instanceId", instanceID)
	return nil
}

// LoadStoppedInstance loads a stopped instance from the shared KV store.
// Returns nil, nil if the key does not exist.
func (m *JetStreamManager) LoadStoppedInstance(instanceID string) (*vm.VM, error) {
	if m.kv == nil {
		return nil, errors.New("KV bucket not initialized")
	}

	key := StoppedInstancePrefix + instanceID
	entry, err := m.kv.Get(key)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "LoadStoppedInstance", "key", key, "err", err)
			kv, recoverErr := m.recoverKVBucket()
			if recoverErr != nil {
				return nil, err
			}
			// Retry — if we reconnected, data may still exist
			entry, err = kv.Get(key)
			if err != nil {
				if errors.Is(err, nats.ErrKeyNotFound) {
					return nil, nil
				}
				return nil, err
			}
			// Fall through to unmarshal below
		} else {
			return nil, err
		}
	}

	var instance vm.VM
	if err := json.Unmarshal(entry.Value(), &instance); err != nil {
		return nil, err
	}

	return &instance, nil
}

// DeleteStoppedInstance removes a stopped instance from the shared KV store.
// It is idempotent — deleting a non-existent key is not an error.
func (m *JetStreamManager) DeleteStoppedInstance(instanceID string) error {
	if m.kv == nil {
		return errors.New("KV bucket not initialized")
	}

	key := StoppedInstancePrefix + instanceID
	err := m.kv.Delete(key)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "DeleteStoppedInstance", "key", key, "err", err)
			kv, recoverErr := m.recoverKVBucket()
			if recoverErr != nil {
				return err
			}
			// Retry — if we reconnected the key may still exist
			if retryErr := kv.Delete(key); retryErr != nil && !errors.Is(retryErr, nats.ErrKeyNotFound) {
				return retryErr
			}
			return nil
		}
		return err
	}

	slog.Debug("Deleted stopped instance from JetStream KV", "key", key)
	return nil
}

// ListStoppedInstances returns all stopped instances from the shared KV store.
func (m *JetStreamManager) ListStoppedInstances() ([]*vm.VM, error) {
	if m.kv == nil {
		return nil, errors.New("KV bucket not initialized")
	}

	keys, err := m.kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "ListStoppedInstances", "err", err)
			kv, recoverErr := m.recoverKVBucket()
			if recoverErr != nil {
				return nil, err
			}
			// Retry — if we reconnected, data may still exist
			keys, err = kv.Keys()
			if err != nil {
				if errors.Is(err, nats.ErrNoKeysFound) {
					return nil, nil
				}
				return nil, err
			}
			// Fall through to iterate keys below
		} else {
			return nil, err
		}
	}

	var instances []*vm.VM
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, StoppedInstancePrefix) {
			continue
		}

		entry, err := m.kv.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			return nil, err
		}

		var instance vm.VM
		if err := json.Unmarshal(entry.Value(), &instance); err != nil {
			slog.Error("Failed to unmarshal stopped instance", "key", key, "err", err)
			continue
		}

		instances = append(instances, &instance)
	}

	return instances, nil
}

// WriteTerminatedInstance writes a terminated instance to the terminated KV bucket.
// The entry will auto-expire after the bucket's TTL (1 hour).
func (m *JetStreamManager) WriteTerminatedInstance(instanceID string, instance *vm.VM) error {
	if m.terminatedKV == nil {
		return errors.New("terminated instance KV bucket not initialized")
	}

	jsonData, err := json.Marshal(instance)
	if err != nil {
		return err
	}

	key := TerminatedInstancePrefix + instanceID
	_, err = m.terminatedKV.Put(key, jsonData)
	if err != nil {
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "WriteTerminatedInstance", "key", key, "err", err)
			kv, recoverErr := m.recoverTerminatedKVBucket()
			if recoverErr != nil {
				return err
			}
			if _, retryErr := kv.Put(key, jsonData); retryErr != nil {
				return retryErr
			}
			slog.Debug("Wrote terminated instance to JetStream KV (after recovery)", "key", key, "instanceId", instanceID)
			return nil
		}
		return err
	}

	slog.Debug("Wrote terminated instance to JetStream KV", "key", key, "instanceId", instanceID)
	return nil
}

// ListTerminatedInstances returns all terminated instances from the terminated KV bucket.
func (m *JetStreamManager) ListTerminatedInstances() ([]*vm.VM, error) {
	if m.terminatedKV == nil {
		return nil, errors.New("terminated instance KV bucket not initialized")
	}

	keys, err := m.terminatedKV.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "ListTerminatedInstances", "err", err)
			kv, recoverErr := m.recoverTerminatedKVBucket()
			if recoverErr != nil {
				return nil, err
			}
			keys, err = kv.Keys()
			if err != nil {
				if errors.Is(err, nats.ErrNoKeysFound) {
					return nil, nil
				}
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	var instances []*vm.VM
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		if !strings.HasPrefix(key, TerminatedInstancePrefix) {
			continue
		}

		entry, err := m.terminatedKV.Get(key)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			return nil, err
		}

		var instance vm.VM
		if err := json.Unmarshal(entry.Value(), &instance); err != nil {
			slog.Error("Failed to unmarshal terminated instance", "key", key, "err", err)
			continue
		}

		instances = append(instances, &instance)
	}

	return instances, nil
}

// DeleteTerminatedInstance removes a terminated instance from the terminated KV bucket.
func (m *JetStreamManager) DeleteTerminatedInstance(instanceID string) error {
	if m.terminatedKV == nil {
		return errors.New("terminated instance KV bucket not initialized")
	}

	key := TerminatedInstancePrefix + instanceID
	err := m.terminatedKV.Delete(key)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "DeleteTerminatedInstance", "key", key, "err", err)
			kv, recoverErr := m.recoverTerminatedKVBucket()
			if recoverErr != nil {
				return err
			}
			if retryErr := kv.Delete(key); retryErr != nil && !errors.Is(retryErr, nats.ErrKeyNotFound) {
				return retryErr
			}
			return nil
		}
		return err
	}

	slog.Debug("Deleted terminated instance from JetStream KV", "key", key)
	return nil
}

// LoadTerminatedInstance loads a single terminated instance from the terminated KV bucket.
// Returns nil, nil if the key does not exist.
func (m *JetStreamManager) LoadTerminatedInstance(instanceID string) (*vm.VM, error) {
	if m.terminatedKV == nil {
		return nil, errors.New("terminated instance KV bucket not initialized")
	}

	key := TerminatedInstancePrefix + instanceID
	entry, err := m.terminatedKV.Get(key)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, nil
		}
		if isStreamUnavailable(err) {
			slog.Warn("KV stream unavailable, attempting recovery", "operation", "LoadTerminatedInstance", "key", key, "err", err)
			kv, recoverErr := m.recoverTerminatedKVBucket()
			if recoverErr != nil {
				return nil, err
			}
			entry, err = kv.Get(key)
			if err != nil {
				if errors.Is(err, nats.ErrKeyNotFound) {
					return nil, nil
				}
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	var instance vm.VM
	if err := json.Unmarshal(entry.Value(), &instance); err != nil {
		return nil, err
	}

	return &instance, nil
}
