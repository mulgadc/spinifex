package clusterv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeVMsResponseJSONContract(t *testing.T) {
	data, err := json.Marshal(NodeVMsResponse{
		Node: "node-a",
		Host: "10.0.0.1",
		VMs: []VMInfo{{
			InstanceID:   "i-abc123",
			Status:       "running",
			InstanceType: "t3.small",
			VCPU:         2,
			MemoryGB:     2.0,
			LaunchTime:   1736935200,
			ManagedBy:    "elbv2",
			GPUs: []VMGPUInfo{{
				Model:      "NVIDIA A10",
				VRAMMiB:    23028,
				PCIAddress: "0000:01:00.0",
				Profile:    "MIG 1g.6gb",
				MdevPath:   "/sys/bus/mdev/devices/example",
			}},
			Health:     "ok",
			CrashCount: 2,
		}},
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"node":"node-a",
		"host":"10.0.0.1",
		"vms":[{
			"instance_id":"i-abc123",
			"status":"running",
			"instance_type":"t3.small",
			"vcpu":2,
			"memory_gb":2,
			"launch_time":1736935200,
			"managed_by":"elbv2",
			"gpus":[{
				"model":"NVIDIA A10",
				"vram_mib":23028,
				"pci_address":"0000:01:00.0",
				"profile":"MIG 1g.6gb",
				"mdev_path":"/sys/bus/mdev/devices/example"
			}],
			"health":"ok",
			"crash_count":2
		}]
	}`, string(data))
}

func TestNodeVMsSubject(t *testing.T) {
	require.Equal(t, "spinifex.node.vms", NodeVMsSubject)
}
