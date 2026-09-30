package clusterv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeStatusResponseJSONContract(t *testing.T) {
	data, err := json.Marshal(NodeStatusResponse{
		Node:           "node-a",
		Status:         "Ready",
		Host:           "10.0.0.1",
		Region:         "ap-southeast-2",
		AZ:             "ap-southeast-2a",
		Uptime:         42,
		Services:       []string{"nats", "spinifex"},
		TotalVCPU:      16,
		TotalMemGB:     64,
		ReservedVCPU:   2,
		ReservedMemGB:  4,
		AllocVCPU:      4,
		AllocMemGB:     8,
		TotalGPUs:      1,
		AllocGPUs:      1,
		GPUCapable:     true,
		GPUPassthrough: true,
		GPUModels:      []string{"NVIDIA A10"},
		GPUs: []GPUInfo{{
			PCIAddress: "0000:01:00.0",
			Model:      "NVIDIA A10",
			VRAMMiB:    23028,
			MIGEnabled: true,
			MIGProfile: "1g.6gb",
			InstanceID: "i-abc123",
			Slices: []GPUSliceInfo{{
				GIID:       1,
				Profile:    "1g.6gb",
				VRAMMiB:    6144,
				MdevPath:   "/sys/bus/mdev/devices/example",
				InstanceID: "i-abc123",
			}},
		}},
		VMCount: 1,
		InstanceTypes: []InstanceTypeCap{{
			Name: "g5.xlarge", VCPU: 4, MemoryGB: 16, Available: 2,
		}},
		NATSRole:       "leader",
		PredastoreRole: "follower",
		OVNNBRole:      "leader",
		OVNSBRole:      "follower",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"node":"node-a", "status":"Ready", "host":"10.0.0.1",
		"region":"ap-southeast-2", "az":"ap-southeast-2a", "uptime":42,
		"services":["nats","spinifex"],
		"total_vcpu":16, "total_mem_gb":64,
		"reserved_vcpu":2, "reserved_mem_gb":4,
		"alloc_vcpu":4, "alloc_mem_gb":8,
		"total_gpus":1, "alloc_gpus":1,
		"gpu_capable":true, "gpu_passthrough":true, "gpu_models":["NVIDIA A10"],
		"gpus":[{
			"pci_address":"0000:01:00.0", "model":"NVIDIA A10", "vram_mib":23028,
			"mig_enabled":true, "mig_profile":"1g.6gb", "instance_id":"i-abc123",
			"slices":[{
				"gi_id":1, "profile":"1g.6gb", "vram_mib":6144,
				"mdev_path":"/sys/bus/mdev/devices/example", "instance_id":"i-abc123"
			}]
		}],
		"vm_count":1,
		"instance_types":[{"name":"g5.xlarge", "vcpu":4, "memory_gb":16, "available":2}],
		"nats_role":"leader", "predastore_role":"follower",
		"ovn_nb_role":"leader", "ovn_sb_role":"follower"
	}`, string(data))
}

func TestNodeStatusSubject(t *testing.T) {
	require.Equal(t, "spinifex.node.status", NodeStatusSubject)
}
