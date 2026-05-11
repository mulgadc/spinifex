package daemon

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestJetStreamManager_WriteAndLoadState tests round-trip write and load of instance state
func TestJetStreamManager_WriteAndLoadState(t *testing.T) {
	natsURL := sharedJSNATSURL

	// Connect to NATS
	nc, err := nats.Connect(natsURL)
	require.NoError(t, err, "Failed to connect to NATS")
	defer nc.Close()

	// Create JetStreamManager
	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err, "Failed to create JetStreamManager")

	// Initialize the KV bucket
	err = jsm.InitKVBucket()
	require.NoError(t, err, "Failed to init KV bucket")

	// Create test instances
	testNodeID := "test-node-1"
	testInstances := map[string]*vm.VM{
		"i-test-001": {
			ID:           "i-test-001",
			Status:       vm.StateRunning,
			InstanceType: "t3.micro",
		},
		"i-test-002": {
			ID:           "i-test-002",
			Status:       vm.StateStopped,
			InstanceType: "t3.small",
		},
	}

	// Write state
	err = jsm.WriteState(testNodeID, testInstances)
	require.NoError(t, err, "Failed to write state")

	// Load state
	loadedInstances, err := jsm.LoadState(testNodeID)
	require.NoError(t, err, "Failed to load state")
	require.NotNil(t, loadedInstances, "Loaded instances should not be nil")

	// Verify the loaded state matches
	assert.Len(t, loadedInstances, 2, "Should have 2 instances")
	assert.NotNil(t, loadedInstances["i-test-001"], "Should have i-test-001")
	assert.NotNil(t, loadedInstances["i-test-002"], "Should have i-test-002")
	assert.Equal(t, vm.StateRunning, loadedInstances["i-test-001"].Status)
	assert.Equal(t, vm.StateStopped, loadedInstances["i-test-002"].Status)
	assert.Equal(t, "t3.micro", loadedInstances["i-test-001"].InstanceType)
	assert.Equal(t, "t3.small", loadedInstances["i-test-002"].InstanceType)
}

// TestJetStreamManager_LoadState_KeyNotFound tests that LoadState returns empty state when key doesn't exist
func TestJetStreamManager_LoadState_KeyNotFound(t *testing.T) {
	natsURL := sharedJSNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)

	err = jsm.InitKVBucket()
	require.NoError(t, err)

	// Load state for a non-existent node
	instances, err := jsm.LoadState("non-existent-node")
	require.NoError(t, err, "Should not error when key not found")
	require.NotNil(t, instances, "Should return non-nil instances")
	assert.Empty(t, instances, "Should return empty VMS map")
}

// TestJetStreamManager_BucketCreation tests that InitKVBucket creates the bucket when it doesn't exist
func TestJetStreamManager_BucketCreation(t *testing.T) {
	natsURL := sharedJSNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)

	// InitKVBucket should create the bucket
	err = jsm.InitKVBucket()
	require.NoError(t, err, "Should create bucket without error")

	// Verify the bucket exists by checking jsm.kv is set
	assert.NotNil(t, jsm.kv, "KV bucket should be initialized")
}

// TestJetStreamManager_BucketReconnection tests that InitKVBucket connects to existing bucket
func TestJetStreamManager_BucketReconnection(t *testing.T) {
	natsURL := sharedJSNATSURL

	// First connection - create the bucket
	nc1, err := nats.Connect(natsURL)
	require.NoError(t, err)

	jsm1, err := NewJetStreamManager(nc1, 1)
	require.NoError(t, err)

	err = jsm1.InitKVBucket()
	require.NoError(t, err, "First InitKVBucket should succeed")

	// Write some test data
	testInstances := map[string]*vm.VM{
		"i-persist": {
			ID:     "i-persist",
			Status: vm.StateRunning,
		},
	}
	err = jsm1.WriteState("persist-node", testInstances)
	require.NoError(t, err)

	nc1.Close()

	// Second connection - should connect to existing bucket
	nc2, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc2.Close()

	jsm2, err := NewJetStreamManager(nc2, 1)
	require.NoError(t, err)

	err = jsm2.InitKVBucket()
	require.NoError(t, err, "Second InitKVBucket should succeed (reconnect)")

	// Verify data persisted
	loadedInstances, err := jsm2.LoadState("persist-node")
	require.NoError(t, err)
	assert.NotEmpty(t, loadedInstances, "Should have persisted instances")
	assert.NotNil(t, loadedInstances["i-persist"], "Should have i-persist")
	assert.Equal(t, vm.StateRunning, loadedInstances["i-persist"].Status)
}

// TestJetStreamManager_DeleteState tests deleting state from the KV store
func TestJetStreamManager_DeleteState(t *testing.T) {
	natsURL := sharedJSNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)

	err = jsm.InitKVBucket()
	require.NoError(t, err)

	// Write state
	testNodeID := "delete-test-node"
	testInstances := map[string]*vm.VM{
		"i-delete-me": {
			ID:     "i-delete-me",
			Status: vm.StateRunning,
		},
	}
	err = jsm.WriteState(testNodeID, testInstances)
	require.NoError(t, err)

	// Verify state exists
	loadedInstances, err := jsm.LoadState(testNodeID)
	require.NoError(t, err)
	assert.NotEmpty(t, loadedInstances)

	// Delete state
	err = jsm.DeleteState(testNodeID)
	require.NoError(t, err, "Should delete state without error")

	// Verify state is gone (should return empty state)
	loadedInstances, err = jsm.LoadState(testNodeID)
	require.NoError(t, err)
	assert.Empty(t, loadedInstances, "Should return empty state after deletion")
}

// TestJetStreamManager_DeleteState_NonExistent tests deleting state that doesn't exist
func TestJetStreamManager_DeleteState_NonExistent(t *testing.T) {
	natsURL := sharedJSNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)

	err = jsm.InitKVBucket()
	require.NoError(t, err)

	// Delete non-existent state should not error
	err = jsm.DeleteState("non-existent-node")
	require.NoError(t, err, "Deleting non-existent state should not error")
}

// TestJetStreamManager_WriteState_UpdateExisting tests that writing state updates existing entry
func TestJetStreamManager_WriteState_UpdateExisting(t *testing.T) {
	natsURL := sharedJSNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)

	err = jsm.InitKVBucket()
	require.NoError(t, err)

	testNodeID := "update-test-node"

	// Write initial state
	initialInstances := map[string]*vm.VM{
		"i-initial": {
			ID:     "i-initial",
			Status: vm.StateRunning,
		},
	}
	err = jsm.WriteState(testNodeID, initialInstances)
	require.NoError(t, err)

	// Update state with different instances
	updatedInstances := map[string]*vm.VM{
		"i-initial": {
			ID:     "i-initial",
			Status: vm.StateStopped, // Changed status
		},
		"i-new": { // Added new instance
			ID:     "i-new",
			Status: vm.StateRunning,
		},
	}
	err = jsm.WriteState(testNodeID, updatedInstances)
	require.NoError(t, err)

	// Load and verify updated state
	loadedInstances, err := jsm.LoadState(testNodeID)
	require.NoError(t, err)
	assert.Len(t, loadedInstances, 2, "Should have 2 instances")
	assert.Equal(t, vm.StateStopped, loadedInstances["i-initial"].Status, "Status should be updated")
	assert.NotNil(t, loadedInstances["i-new"], "Should have new instance")
}

// TestJetStreamManager_MultipleNodes tests storing state for multiple nodes
func TestJetStreamManager_MultipleNodes(t *testing.T) {
	natsURL := sharedJSNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)

	err = jsm.InitKVBucket()
	require.NoError(t, err)

	// Write state for node-1
	node1Instances := map[string]*vm.VM{
		"i-node1-001": {ID: "i-node1-001", Status: vm.StateRunning},
	}
	err = jsm.WriteState("node-1", node1Instances)
	require.NoError(t, err)

	// Write state for node-2
	node2Instances := map[string]*vm.VM{
		"i-node2-001": {ID: "i-node2-001", Status: vm.StateStopped},
		"i-node2-002": {ID: "i-node2-002", Status: vm.StateRunning},
	}
	err = jsm.WriteState("node-2", node2Instances)
	require.NoError(t, err)

	// Load and verify node-1 state
	loadedNode1, err := jsm.LoadState("node-1")
	require.NoError(t, err)
	assert.Len(t, loadedNode1, 1)
	assert.NotNil(t, loadedNode1["i-node1-001"])

	// Load and verify node-2 state
	loadedNode2, err := jsm.LoadState("node-2")
	require.NoError(t, err)
	assert.Len(t, loadedNode2, 2)
	assert.NotNil(t, loadedNode2["i-node2-001"])
	assert.NotNil(t, loadedNode2["i-node2-002"])

	// Verify node isolation - node-1 doesn't have node-2's instances
	_, exists := loadedNode1["i-node2-001"]
	assert.False(t, exists, "Node-1 should not have node-2's instances")
}

// TestJetStreamManager_KVNotInitialized tests error handling when KV is not initialized
func TestJetStreamManager_KVNotInitialized(t *testing.T) {
	natsURL := sharedJSNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	// Create JetStreamManager but don't call InitKVBucket
	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)

	testInstances := make(map[string]*vm.VM)
	err = jsm.WriteState("test-node", testInstances)
	assert.Error(t, err, "WriteState should error when KV not initialized")

	_, err = jsm.LoadState("test-node")
	assert.Error(t, err, "LoadState should error when KV not initialized")

	err = jsm.DeleteState("test-node")
	assert.Error(t, err, "DeleteState should error when KV not initialized")
}

// TestJetStreamManager_UpdateReplicas tests updating replica count for the KV bucket
func TestJetStreamManager_UpdateReplicas(t *testing.T) {
	natsURL := sharedJSNATSURL

	nc, err := nats.Connect(natsURL)
	require.NoError(t, err)
	defer nc.Close()

	// Create with 1 replica (typical for single node startup)
	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)

	err = jsm.InitKVBucket()
	require.NoError(t, err)

	// Verify initial replica count
	js, _ := nc.JetStream()
	streamInfo, err := js.StreamInfo("KV_" + InstanceStateBucket)
	require.NoError(t, err)
	assert.Equal(t, 1, streamInfo.Config.Replicas, "Should start with 1 replica")

	// Try to update to same replica count (should be a no-op)
	err = jsm.UpdateReplicas(1)
	assert.NoError(t, err, "Updating to same replica count should succeed")

	// Note: Increasing replicas beyond 1 requires additional NATS servers in the cluster,
	// which we don't have in the test environment. In a single-node test server,
	// attempting to increase replicas will fail with "insufficient resources" error.
	// This test verifies the basic functionality works.
}

// TestJetStreamManager_UpdateReplicas_NoInit tests UpdateReplicas when JS not initialized
func TestJetStreamManager_UpdateReplicas_NoInit(t *testing.T) {
	// Test with nil JetStream context
	jsm := &JetStreamManager{
		js:       nil,
		replicas: 1,
	}

	err := jsm.UpdateReplicas(3)
	assert.Error(t, err, "UpdateReplicas should error when JetStream not initialized")
}

// --- Stopped instance KV tests ---

// TestJetStreamManager_WriteAndLoadStoppedInstance tests round-trip write and load of a stopped instance
func TestJetStreamManager_WriteAndLoadStoppedInstance(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	testVM := &vm.VM{
		ID:           "i-stopped-001",
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		LastNode:     "node-1",
	}

	// Write stopped instance
	err = jsm.WriteStoppedInstance(testVM.ID, testVM)
	require.NoError(t, err)

	// Load stopped instance
	loaded, err := jsm.LoadStoppedInstance(testVM.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, testVM.ID, loaded.ID)
	assert.Equal(t, vm.StateStopped, loaded.Status)
	assert.Equal(t, "t3.micro", loaded.InstanceType)
	assert.Equal(t, "node-1", loaded.LastNode)

	// Cleanup
	_ = jsm.DeleteStoppedInstance(testVM.ID)
}

// TestJetStreamManager_LoadStoppedInstance_NotFound tests that LoadStoppedInstance returns nil for missing key
func TestJetStreamManager_LoadStoppedInstance_NotFound(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	loaded, err := jsm.LoadStoppedInstance("i-nonexistent")
	require.NoError(t, err)
	assert.Nil(t, loaded)
}

// TestJetStreamManager_DeleteStoppedInstance tests deleting a stopped instance (including non-existent)
func TestJetStreamManager_DeleteStoppedInstance(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	testVM := &vm.VM{
		ID:     "i-stopped-del",
		Status: vm.StateStopped,
	}

	// Write then delete
	err = jsm.WriteStoppedInstance(testVM.ID, testVM)
	require.NoError(t, err)

	err = jsm.DeleteStoppedInstance(testVM.ID)
	require.NoError(t, err)

	// Verify it's gone
	loaded, err := jsm.LoadStoppedInstance(testVM.ID)
	require.NoError(t, err)
	assert.Nil(t, loaded)

	// Delete non-existent should not error
	err = jsm.DeleteStoppedInstance("i-does-not-exist")
	require.NoError(t, err)
}

// TestJetStreamManager_ListStoppedInstances tests listing multiple stopped instances
func TestJetStreamManager_ListStoppedInstances(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	// Write multiple stopped instances
	vms := []*vm.VM{
		{ID: "i-list-001", Status: vm.StateStopped, InstanceType: "t3.micro"},
		{ID: "i-list-002", Status: vm.StateStopped, InstanceType: "t3.small"},
		{ID: "i-list-003", Status: vm.StateStopped, InstanceType: "t3.medium"},
	}

	for _, v := range vms {
		err = jsm.WriteStoppedInstance(v.ID, v)
		require.NoError(t, err)
	}

	// List stopped instances
	instances, err := jsm.ListStoppedInstances()
	require.NoError(t, err)

	// Should contain at least the 3 we wrote (may also contain instances from other tests)
	instanceIDs := make(map[string]bool)
	for _, inst := range instances {
		instanceIDs[inst.ID] = true
	}
	assert.True(t, instanceIDs["i-list-001"])
	assert.True(t, instanceIDs["i-list-002"])
	assert.True(t, instanceIDs["i-list-003"])

	// Cleanup
	for _, v := range vms {
		_ = jsm.DeleteStoppedInstance(v.ID)
	}
}

// TestJetStreamManager_StoppedInstances_NoInterference tests that stopped instances don't interfere with per-node state
func TestJetStreamManager_StoppedInstances_NoInterference(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	// Write per-node state
	nodeInstances := map[string]*vm.VM{
		"i-running-001": {ID: "i-running-001", Status: vm.StateRunning},
	}
	err = jsm.WriteState("interference-test-node", nodeInstances)
	require.NoError(t, err)

	// Write stopped instance
	stoppedVM := &vm.VM{ID: "i-stopped-interf", Status: vm.StateStopped}
	err = jsm.WriteStoppedInstance(stoppedVM.ID, stoppedVM)
	require.NoError(t, err)

	// Verify per-node state is unaffected
	loaded, err := jsm.LoadState("interference-test-node")
	require.NoError(t, err)
	assert.Len(t, loaded, 1)
	assert.NotNil(t, loaded["i-running-001"])

	// Verify stopped instance loads independently
	loadedStopped, err := jsm.LoadStoppedInstance(stoppedVM.ID)
	require.NoError(t, err)
	require.NotNil(t, loadedStopped)
	assert.Equal(t, "i-stopped-interf", loadedStopped.ID)

	// Cleanup
	_ = jsm.DeleteStoppedInstance(stoppedVM.ID)
	_ = jsm.DeleteState("interference-test-node")
}

// TestJetStreamManager_StoppedInstance_KVNotInitialized tests error handling when KV is not initialized
func TestJetStreamManager_StoppedInstance_KVNotInitialized(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	// Don't call InitKVBucket

	err = jsm.WriteStoppedInstance("i-test", &vm.VM{})
	assert.Error(t, err)

	_, err = jsm.LoadStoppedInstance("i-test")
	assert.Error(t, err)

	err = jsm.DeleteStoppedInstance("i-test")
	assert.Error(t, err)

	_, err = jsm.ListStoppedInstances()
	assert.Error(t, err)
}

// --- WriteServiceManifest KV tests ---

// TestJetStreamManager_WriteServiceManifest tests writing a service manifest to the cluster-state KV.
func TestJetStreamManager_WriteServiceManifest(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitClusterStateBucket()
	require.NoError(t, err)

	services := []string{"daemon", "nats", "predastore"}
	err = jsm.WriteServiceManifest("test-node-svc", services, "10.0.0.1:4222", "10.0.0.1:8443")
	require.NoError(t, err)

	// Read the KV entry directly and verify JSON contents
	entry, err := jsm.clusterKV.Get("node.test-node-svc.services")
	require.NoError(t, err)

	var manifest map[string]any
	err = json.Unmarshal(entry.Value(), &manifest)
	require.NoError(t, err)

	assert.Equal(t, "test-node-svc", manifest["node"])
	assert.Equal(t, "10.0.0.1:4222", manifest["nats_host"])
	assert.Equal(t, "10.0.0.1:8443", manifest["predastore_host"])
	assert.NotEmpty(t, manifest["timestamp"])

	// Verify services list
	svcList, ok := manifest["services"].([]any)
	require.True(t, ok, "services should be a JSON array")
	assert.Len(t, svcList, 3)
	assert.Equal(t, "daemon", svcList[0])
	assert.Equal(t, "nats", svcList[1])
	assert.Equal(t, "predastore", svcList[2])
}

// TestJetStreamManager_WriteServiceManifest_EmptyServices tests writing a manifest with no services.
func TestJetStreamManager_WriteServiceManifest_EmptyServices(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitClusterStateBucket()
	require.NoError(t, err)

	err = jsm.WriteServiceManifest("empty-svc-node", []string{}, "10.0.0.2:4222", "10.0.0.2:8443")
	require.NoError(t, err)

	entry, err := jsm.clusterKV.Get("node.empty-svc-node.services")
	require.NoError(t, err)

	var manifest map[string]any
	err = json.Unmarshal(entry.Value(), &manifest)
	require.NoError(t, err)

	assert.Equal(t, "empty-svc-node", manifest["node"])
	svcList, ok := manifest["services"].([]any)
	require.True(t, ok, "services should be a JSON array")
	assert.Empty(t, svcList)
}

// TestJetStreamManager_WriteServiceManifest_ClusterKVNotInitialized tests error when clusterKV is nil.
func TestJetStreamManager_WriteServiceManifest_ClusterKVNotInitialized(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	// Don't call InitClusterStateBucket

	err = jsm.WriteServiceManifest("test-node", []string{"daemon"}, "10.0.0.1:4222", "10.0.0.1:8443")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cluster state KV not initialized")
}

// --- KV bucket recovery tests ---
// These tests simulate the stream being deleted after initialization (which happens
// during NATS cluster formation when catchup resets corrupt a stream) and verify
// that operations recover by re-creating the bucket.

// deleteInstanceStateBucket deletes the underlying JetStream stream for the instance-state KV bucket.
func deleteInstanceStateBucket(t *testing.T, nc *nats.Conn) {
	t.Helper()
	js, err := nc.JetStream()
	require.NoError(t, err)
	err = js.DeleteStream("KV_" + InstanceStateBucket)
	require.NoError(t, err)
}

func TestJetStreamManager_WriteState_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	// Delete the underlying stream to simulate cluster formation loss
	deleteInstanceStateBucket(t, nc)

	// WriteState should recover by recreating the bucket
	testInstances := map[string]*vm.VM{
		"i-recover-001": {ID: "i-recover-001", Status: vm.StateRunning},
	}
	err = jsm.WriteState("recovery-node", testInstances)
	require.NoError(t, err, "WriteState should recover after stream loss")

	// Verify data was written
	loaded, err := jsm.LoadState("recovery-node")
	require.NoError(t, err)
	assert.Len(t, loaded, 1)
	assert.NotNil(t, loaded["i-recover-001"])
}

func TestJetStreamManager_LoadState_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)

	// LoadState should recover and return empty state
	loaded, err := jsm.LoadState("any-node")
	require.NoError(t, err, "LoadState should recover after stream loss")
	assert.Empty(t, loaded)
}

func TestJetStreamManager_DeleteState_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)

	// DeleteState should recover (nothing to delete in fresh bucket)
	err = jsm.DeleteState("any-node")
	require.NoError(t, err, "DeleteState should recover after stream loss")
}

func TestJetStreamManager_WriteStoppedInstance_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)

	// WriteStoppedInstance should recover
	testVM := &vm.VM{ID: "i-stopped-recover", Status: vm.StateStopped, InstanceType: "t3.micro"}
	err = jsm.WriteStoppedInstance(testVM.ID, testVM)
	require.NoError(t, err, "WriteStoppedInstance should recover after stream loss")

	// Verify data was written
	loaded, err := jsm.LoadStoppedInstance(testVM.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "i-stopped-recover", loaded.ID)

	_ = jsm.DeleteStoppedInstance(testVM.ID)
}

func TestJetStreamManager_LoadStoppedInstance_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)

	// LoadStoppedInstance should recover and return nil (not found)
	loaded, err := jsm.LoadStoppedInstance("i-nonexistent")
	require.NoError(t, err, "LoadStoppedInstance should recover after stream loss")
	assert.Nil(t, loaded)
}

func TestJetStreamManager_DeleteStoppedInstance_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)

	err = jsm.DeleteStoppedInstance("i-any")
	require.NoError(t, err, "DeleteStoppedInstance should recover after stream loss")
}

func TestJetStreamManager_ListStoppedInstances_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)

	// ListStoppedInstances should recover and return empty
	instances, err := jsm.ListStoppedInstances()
	require.NoError(t, err, "ListStoppedInstances should recover after stream loss")
	assert.Empty(t, instances)
}

// --- KV bucket recovery failure tests ---
// These tests verify that when recovery itself fails (e.g., NATS is unreachable),
// errors are properly propagated rather than silently returning empty state.

// swapToNonJSContext replaces the JetStreamManager's JS context with one from the
// non-JetStream NATS server. The original kv handle still targets the JS server
// (where the stream was deleted), so operations fail with stream-unavailable errors.
// Recovery then also fails because the non-JS server has no JetStream support.
func swapToNonJSContext(t *testing.T, jsm *JetStreamManager) {
	t.Helper()
	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(func() { nc.Close() })
	js, err := nc.JetStream()
	require.NoError(t, err)
	jsm.js = js
}

func TestJetStreamManager_WriteState_RecoveryFailure(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)
	swapToNonJSContext(t, jsm)

	testInstances := map[string]*vm.VM{"i-fail": {ID: "i-fail"}}
	err = jsm.WriteState("fail-node", testInstances)
	assert.Error(t, err, "WriteState should return error when recovery fails")
}

func TestJetStreamManager_LoadState_RecoveryFailure(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)
	swapToNonJSContext(t, jsm)

	loaded, err := jsm.LoadState("fail-node")
	assert.Error(t, err, "LoadState should return error when recovery fails")
	assert.Nil(t, loaded)
}

func TestJetStreamManager_DeleteState_RecoveryFailure(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)
	swapToNonJSContext(t, jsm)

	err = jsm.DeleteState("fail-node")
	assert.Error(t, err, "DeleteState should return error when recovery fails")
}

func TestJetStreamManager_WriteStoppedInstance_RecoveryFailure(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)
	swapToNonJSContext(t, jsm)

	testVM := &vm.VM{ID: "i-fail", Status: vm.StateStopped}
	err = jsm.WriteStoppedInstance(testVM.ID, testVM)
	assert.Error(t, err, "WriteStoppedInstance should return error when recovery fails")
}

func TestJetStreamManager_LoadStoppedInstance_RecoveryFailure(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)
	swapToNonJSContext(t, jsm)

	loaded, err := jsm.LoadStoppedInstance("i-fail")
	assert.Error(t, err, "LoadStoppedInstance should return error when recovery fails")
	assert.Nil(t, loaded)
}

func TestJetStreamManager_DeleteStoppedInstance_RecoveryFailure(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)
	swapToNonJSContext(t, jsm)

	err = jsm.DeleteStoppedInstance("i-fail")
	assert.Error(t, err, "DeleteStoppedInstance should return error when recovery fails")
}

func TestJetStreamManager_ListStoppedInstances_RecoveryFailure(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	err = jsm.InitKVBucket()
	require.NoError(t, err)

	deleteInstanceStateBucket(t, nc)
	swapToNonJSContext(t, jsm)

	instances, err := jsm.ListStoppedInstances()
	assert.Error(t, err, "ListStoppedInstances should return error when recovery fails")
	assert.Nil(t, instances)
}

func TestIsStreamUnavailable(t *testing.T) {
	assert.False(t, isStreamUnavailable(nil))
	assert.True(t, isStreamUnavailable(nats.ErrStreamNotFound))
	assert.True(t, isStreamUnavailable(nats.ErrNoStreamResponse))
	assert.True(t, isStreamUnavailable(nats.ErrNoResponders))
	assert.True(t, isStreamUnavailable(errors.New("nats: stream not found")))
	assert.False(t, isStreamUnavailable(errors.New("some other error")))
	assert.False(t, isStreamUnavailable(nats.ErrKeyNotFound))
}

// --- Terminated instance KV tests ---

func TestJetStreamManager_WriteAndLoadTerminatedInstance(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	testVM := &vm.VM{
		ID:           "i-term-001",
		Status:       vm.StateTerminated,
		InstanceType: "t3.micro",
		LastNode:     "node-1",
	}

	err = jsm.WriteTerminatedInstance(testVM.ID, testVM)
	require.NoError(t, err)

	loaded, err := jsm.LoadTerminatedInstance(testVM.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, testVM.ID, loaded.ID)
	assert.Equal(t, vm.StateTerminated, loaded.Status)
	assert.Equal(t, "t3.micro", loaded.InstanceType)
	assert.Equal(t, "node-1", loaded.LastNode)

	_ = jsm.DeleteTerminatedInstance(testVM.ID)
}

func TestJetStreamManager_LoadTerminatedInstance_NotFound(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	loaded, err := jsm.LoadTerminatedInstance("i-nonexistent")
	require.NoError(t, err)
	assert.Nil(t, loaded)
}

func TestJetStreamManager_DeleteTerminatedInstance(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	testVM := &vm.VM{
		ID:     "i-term-del",
		Status: vm.StateTerminated,
	}

	err = jsm.WriteTerminatedInstance(testVM.ID, testVM)
	require.NoError(t, err)

	err = jsm.DeleteTerminatedInstance(testVM.ID)
	require.NoError(t, err)

	loaded, err := jsm.LoadTerminatedInstance(testVM.ID)
	require.NoError(t, err)
	assert.Nil(t, loaded)

	// Delete non-existent should not error
	err = jsm.DeleteTerminatedInstance("i-does-not-exist")
	require.NoError(t, err)
}

func TestJetStreamManager_ListTerminatedInstances(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	vms := []*vm.VM{
		{ID: "i-tlist-001", Status: vm.StateTerminated, InstanceType: "t3.micro"},
		{ID: "i-tlist-002", Status: vm.StateTerminated, InstanceType: "t3.small"},
	}

	for _, v := range vms {
		require.NoError(t, jsm.WriteTerminatedInstance(v.ID, v))
	}
	defer func() {
		for _, v := range vms {
			_ = jsm.DeleteTerminatedInstance(v.ID)
		}
	}()

	instances, err := jsm.ListTerminatedInstances()
	require.NoError(t, err)
	// Use >= because other tests may leave entries in the shared bucket
	assert.GreaterOrEqual(t, len(instances), 2)

	idSet := map[string]bool{}
	for _, inst := range instances {
		idSet[inst.ID] = true
	}
	assert.True(t, idSet["i-tlist-001"])
	assert.True(t, idSet["i-tlist-002"])
}

func TestJetStreamManager_WriteLoadDeleteTerminatedInstance_RoundTrip(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	testVM := &vm.VM{
		ID:     "i-trt-001",
		Status: vm.StateTerminated,
	}

	// Write
	require.NoError(t, jsm.WriteTerminatedInstance(testVM.ID, testVM))

	// Load — should find it
	loaded, err := jsm.LoadTerminatedInstance(testVM.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, vm.StateTerminated, loaded.Status)

	// Delete
	require.NoError(t, jsm.DeleteTerminatedInstance(testVM.ID))

	// Load — should be gone
	loaded, err = jsm.LoadTerminatedInstance(testVM.ID)
	require.NoError(t, err)
	assert.Nil(t, loaded)
}

func TestJetStreamManager_TerminatedInstance_KVNotInitialized(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	// Don't call InitTerminatedInstanceBucket

	err = jsm.WriteTerminatedInstance("i-test", &vm.VM{})
	assert.Error(t, err)

	_, err = jsm.LoadTerminatedInstance("i-test")
	assert.Error(t, err)

	err = jsm.DeleteTerminatedInstance("i-test")
	assert.Error(t, err)

	_, err = jsm.ListTerminatedInstances()
	assert.Error(t, err)
}

// --- Terminated KV bucket recovery tests ---

// deleteTerminatedInstanceBucket deletes the underlying JetStream stream for the terminated-instances KV bucket.
func deleteTerminatedInstanceBucket(t *testing.T, nc *nats.Conn) {
	t.Helper()
	js, err := nc.JetStream()
	require.NoError(t, err)
	err = js.DeleteStream("KV_" + TerminatedInstanceBucket)
	require.NoError(t, err)
}

func TestJetStreamManager_WriteTerminatedInstance_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	deleteTerminatedInstanceBucket(t, nc)

	testVM := &vm.VM{ID: "i-term-recover", Status: vm.StateTerminated, InstanceType: "t3.micro"}
	err = jsm.WriteTerminatedInstance(testVM.ID, testVM)
	require.NoError(t, err, "WriteTerminatedInstance should recover after stream loss")

	loaded, err := jsm.LoadTerminatedInstance(testVM.ID)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "i-term-recover", loaded.ID)

	_ = jsm.DeleteTerminatedInstance(testVM.ID)
}

func TestJetStreamManager_LoadTerminatedInstance_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	deleteTerminatedInstanceBucket(t, nc)

	loaded, err := jsm.LoadTerminatedInstance("i-nonexistent")
	require.NoError(t, err, "LoadTerminatedInstance should recover after stream loss")
	assert.Nil(t, loaded)
}

func TestJetStreamManager_DeleteTerminatedInstance_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	deleteTerminatedInstanceBucket(t, nc)

	err = jsm.DeleteTerminatedInstance("i-any")
	require.NoError(t, err, "DeleteTerminatedInstance should recover after stream loss")
}

func TestJetStreamManager_ListTerminatedInstances_RecoverAfterStreamLost(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	deleteTerminatedInstanceBucket(t, nc)

	instances, err := jsm.ListTerminatedInstances()
	require.NoError(t, err, "ListTerminatedInstances should recover after stream loss")
	assert.Empty(t, instances)
}

func TestJetStreamManager_InitBuckets_WritesVersion(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)

	require.NoError(t, jsm.InitKVBucket())
	require.NoError(t, jsm.InitClusterStateBucket())
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	v, err := utils.ReadVersion(jsm.kv)
	require.NoError(t, err)
	assert.Equal(t, InstanceStateBucketVersion, v)

	v, err = utils.ReadVersion(jsm.clusterKV)
	require.NoError(t, err)
	assert.Equal(t, ClusterStateBucketVersion, v)

	v, err = utils.ReadVersion(jsm.terminatedKV)
	require.NoError(t, err)
	assert.Equal(t, TerminatedInstanceBucketVersion, v)
}

func TestJetStreamManager_ListStoppedInstances_SkipsVersionKey(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitKVBucket())

	// _version key is written by InitKVBucket; listing should not include it
	instances, err := jsm.ListStoppedInstances()
	require.NoError(t, err)
	assert.Empty(t, instances)
}

func TestJetStreamManager_ListTerminatedInstances_SkipsVersionKey(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitTerminatedInstanceBucket())

	// _version key is written by InitTerminatedInstanceBucket; listing should not include it
	instances, err := jsm.ListTerminatedInstances()
	require.NoError(t, err)
	assert.Empty(t, instances)
}

// fakeKVObserver records observer callbacks for tests.
type fakeKVObserver struct {
	mu        sync.Mutex
	successes []string
	failures  []fakeKVObserverFailure
}

type fakeKVObserverFailure struct {
	bucket string
	err    error
}

func (f *fakeKVObserver) RecordKVSyncSuccess(bucket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successes = append(f.successes, bucket)
}

func (f *fakeKVObserver) RecordKVSyncFailure(bucket string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, fakeKVObserverFailure{bucket: bucket, err: err})
}

func (f *fakeKVObserver) snapshot() ([]string, []fakeKVObserverFailure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	successes := append([]string(nil), f.successes...)
	failures := append([]fakeKVObserverFailure(nil), f.failures...)
	return successes, failures
}

func TestJetStreamManager_BestEffort_Success_NotifiesObserver(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitKVBucket())

	obs := &fakeKVObserver{}
	jsm.SetSyncObserver(obs)

	jsm.WriteStateBytesBestEffort("obs-success", []byte(`{"vms":{}}`), 5*time.Second)

	successes, failures := obs.snapshot()
	require.Len(t, successes, 1)
	assert.Equal(t, InstanceStateBucket, successes[0])
	assert.Empty(t, failures)
}

func TestJetStreamManager_BestEffort_PutError_NotifiesObserver(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitKVBucket())

	obs := &fakeKVObserver{}
	jsm.SetSyncObserver(obs)

	// Closing the connection forces the inflight Put to fail without timing out.
	nc.Close()

	jsm.WriteStateBytesBestEffort("obs-fail", []byte(`{"vms":{}}`), 5*time.Second)

	successes, failures := obs.snapshot()
	assert.Empty(t, successes)
	require.Len(t, failures, 1)
	assert.Equal(t, InstanceStateBucket, failures[0].bucket)
	assert.Error(t, failures[0].err)
}

func TestJetStreamManager_BestEffort_NilObserver_NoPanic(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitKVBucket())

	// No observer set — must not panic on success or failure paths.
	jsm.WriteStateBytesBestEffort("obs-nil", []byte(`{"vms":{}}`), 5*time.Second)
}
