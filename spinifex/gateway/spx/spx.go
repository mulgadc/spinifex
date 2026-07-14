// Package spx implements spinifex platform-level gateway handlers (version,
// node status, VM inventory) that are distinct from the AWS-compatible APIs.
package spx

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// VersionOutput is the response for GetVersion.
type VersionOutput struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	License string `json:"license"`
}

// GetVersion returns build-time version metadata.
func GetVersion(version, commit string) (*VersionOutput, error) {
	license := "open-source"
	if strings.Contains(version, "[commercial]") {
		license = "commercial"
	}
	return &VersionOutput{
		Version: version,
		Commit:  commit,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		License: license,
	}, nil
}

// GetNodesOutput is the response for GetNodes.
type GetNodesOutput struct {
	Nodes       []types.NodeStatusResponse `json:"nodes"`
	ClusterMode string                     `json:"cluster_mode"`
}

// GetNodes queries all daemon nodes via NATS fan-out and returns their status.
func GetNodes(ctx context.Context, nc *nats.Conn, expectedNodes int) (*GetNodesOutput, error) {
	frames, _, err := utils.Gather(ctx, nc, "spinifex.node.status", []byte("{}"),
		utils.GatherOpts{Timeout: 3 * time.Second, ExpectedNodes: expectedNodes})
	if err != nil {
		return nil, err
	}

	nodes := make([]types.NodeStatusResponse, 0, len(frames))
	for _, frame := range frames {
		var node types.NodeStatusResponse
		if json.Unmarshal(frame, &node) == nil {
			nodes = append(nodes, node)
		}
	}

	clusterMode := "single-node"
	if len(nodes) > 1 {
		clusterMode = "multi-node"
	}

	return &GetNodesOutput{
		Nodes:       nodes,
		ClusterMode: clusterMode,
	}, nil
}

// VMInfoWithNode extends the daemon's VMInfo with node attribution.
type VMInfoWithNode struct {
	types.VMInfo

	Node string `json:"node"`
}

// GetVMsOutput is the response for GetVMs.
type GetVMsOutput struct {
	VMs []VMInfoWithNode `json:"vms"`
}

// GetVMs queries all daemon nodes via NATS fan-out and returns their VMs.
func GetVMs(ctx context.Context, nc *nats.Conn, expectedNodes int) (*GetVMsOutput, error) {
	frames, _, err := utils.Gather(ctx, nc, "spinifex.node.vms", []byte("{}"),
		utils.GatherOpts{Timeout: 3 * time.Second, ExpectedNodes: expectedNodes})
	if err != nil {
		return nil, err
	}

	allVMs := make([]VMInfoWithNode, 0)
	for _, frame := range frames {
		var nodeResp types.NodeVMsResponse
		if json.Unmarshal(frame, &nodeResp) != nil {
			continue
		}
		for _, vm := range nodeResp.VMs {
			allVMs = append(allVMs, VMInfoWithNode{
				VMInfo: vm,
				Node:   nodeResp.Node,
			})
		}
	}

	return &GetVMsOutput{
		VMs: allVMs,
	}, nil
}
