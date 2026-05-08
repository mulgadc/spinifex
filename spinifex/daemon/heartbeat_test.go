package daemon

import (
	"encoding/json"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildHeartbeat verifies that buildHeartbeat populates the struct from daemon state.
func TestBuildHeartbeat(t *testing.T) {
	rm, err := NewResourceManager(nil, nil)
	require.NoError(t, err)

	d := &Daemon{
		node: "test-node",
		clusterConfig: &config.ClusterConfig{
			Epoch: 5,
		},
		config: &config.Config{
			Services: []string{"daemon", "nats", "viperblock"},
		},
		resourceMgr: rm,
		vmMgr:       vm.NewManager(),
	}

	h := d.buildHeartbeat()

	assert.Equal(t, "test-node", h.Node)
	assert.Equal(t, uint64(5), h.Epoch)
	assert.NotEmpty(t, h.Timestamp)
	assert.Equal(t, []string{"daemon", "nats", "viperblock"}, h.Services)
	assert.Equal(t, 0, h.VMCount)
	assert.Equal(t, 0, h.AllocatedVCPU)
	assert.Greater(t, h.AvailableVCPU, 0)
	assert.Greater(t, h.AvailableMem, 0.0)
	assert.Equal(t, rm.reservedVCPU, h.ReservedVCPU, "ReservedVCPU must be populated from ResourceManager")
	assert.InDelta(t, rm.reservedMem, h.ReservedMem, 0.001, "ReservedMem must be populated from ResourceManager")
	assert.Greater(t, h.ReservedVCPU, 0, "default reserve is non-zero")
	assert.Greater(t, h.ReservedMem, 0.0, "default reserve is non-zero")
}

// TestHeartbeatReflectsAllocation verifies that allocating resources changes the heartbeat values.
func TestHeartbeatReflectsAllocation(t *testing.T) {
	rm, err := NewResourceManager(nil, nil)
	require.NoError(t, err)

	d := &Daemon{
		node: "alloc-node",
		clusterConfig: &config.ClusterConfig{
			Epoch: 1,
		},
		config: &config.Config{
			Services: []string{"daemon"},
		},
		resourceMgr: rm,
		vmMgr:       vm.NewManager(),
	}

	// Take a heartbeat before allocation
	before := d.buildHeartbeat()

	// Find an instance type we can allocate
	var allocType string
	for typeName := range d.resourceMgr.instanceTypes {
		if d.resourceMgr.canAllocate(d.resourceMgr.instanceTypes[typeName], 1) >= 1 {
			allocType = typeName
			break
		}
	}
	require.NotEmpty(t, allocType, "Should have at least one allocatable instance type")

	// Allocate resources
	err = d.resourceMgr.allocate(d.resourceMgr.instanceTypes[allocType])
	require.NoError(t, err)

	// Add a VM to the instance map
	d.vmMgr.Insert(&vm.VM{
		ID:           "i-test-001",
		Status:       vm.StateRunning,
		InstanceType: allocType,
	})

	// Take a heartbeat after allocation
	after := d.buildHeartbeat()

	assert.Equal(t, 1, after.VMCount, "Should reflect 1 VM")
	assert.Greater(t, after.AllocatedVCPU, before.AllocatedVCPU, "AllocatedVCPU should increase")
	assert.Less(t, after.AvailableVCPU, before.AvailableVCPU, "AvailableVCPU should decrease")
}

// TestHeartbeatKVContract pins the on-wire JSON shape and the KV key layout
// for WriteHeartbeat/ReadHeartbeat. A renamed struct tag or changed key
// prefix would break cross-version cluster reads — a pure round-trip would
// not catch either, so we assert against the raw KV bytes.
func TestHeartbeatKVContract(t *testing.T) {
	nc, err := nats.Connect(sharedJSNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	jsm, err := NewJetStreamManager(nc, 1)
	require.NoError(t, err)
	require.NoError(t, jsm.InitClusterStateBucket())

	h := &Heartbeat{
		Node:          "kv-contract-node",
		Epoch:         3,
		Timestamp:     "2025-01-01T00:00:00Z",
		Services:      []string{"daemon", "nats"},
		VMCount:       2,
		AllocatedVCPU: 4,
		AvailableVCPU: 12,
		AllocatedMem:  8.0,
		AvailableMem:  24.0,
		ReservedVCPU:  2,
		ReservedMem:   4.0,
	}

	require.NoError(t, jsm.WriteHeartbeat(h))

	// Verify the KV key prefix and the JSON tag names directly. Reading back
	// via ReadHeartbeat alone would also pass against a typo'd tag.
	entry, err := jsm.clusterKV.Get("heartbeat." + h.Node)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(entry.Value(), &raw))
	assert.Equal(t, "kv-contract-node", raw["node"])
	assert.Equal(t, float64(3), raw["epoch"])
	assert.Equal(t, "2025-01-01T00:00:00Z", raw["timestamp"])
	assert.Equal(t, []any{"daemon", "nats"}, raw["services"])
	assert.Equal(t, float64(2), raw["vm_count"])
	assert.Equal(t, float64(4), raw["allocated_vcpu"])
	assert.Equal(t, float64(12), raw["available_vcpu"])
	assert.Equal(t, 8.0, raw["allocated_mem_gb"])
	assert.Equal(t, 24.0, raw["available_mem_gb"])
	assert.Equal(t, float64(2), raw["reserved_vcpu"])
	assert.Equal(t, 4.0, raw["reserved_mem_gb"])

	// And ReadHeartbeat decodes the same value.
	loaded, err := jsm.ReadHeartbeat(h.Node)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, h, loaded)
}
