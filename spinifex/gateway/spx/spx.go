// Package spx implements spinifex platform-level gateway handlers (version,
// node status, VM inventory) that are distinct from the AWS-compatible APIs.
package spx

import (
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
func GetNodes(nc *nats.Conn, expectedNodes int) (*GetNodesOutput, error) {
	inbox := nats.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, err
	}
	defer sub.Unsubscribe()

	err = nc.PublishRequest("spinifex.node.status", inbox, []byte("{}"))
	if err != nil {
		return nil, err
	}

	timeout := 3 * time.Second
	deadline := time.Now().Add(timeout)
	var nodes []types.NodeStatusResponse
	responsesReceived := 0

	for time.Now().Before(deadline) {
		if expectedNodes > 0 && responsesReceived >= expectedNodes {
			break
		}
		remaining := time.Until(deadline)
		msg, err := sub.NextMsg(remaining)
		if err == nats.ErrTimeout {
			break
		}
		if err != nil {
			break
		}
		responsesReceived++

		if _, valErr := utils.ValidateErrorPayload(msg.Data); valErr != nil {
			continue
		}

		var node types.NodeStatusResponse
		if err := json.Unmarshal(msg.Data, &node); err != nil {
			continue
		}
		nodes = append(nodes, node)
	}

	if nodes == nil {
		nodes = []types.NodeStatusResponse{}
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
func GetVMs(nc *nats.Conn, expectedNodes int) (*GetVMsOutput, error) {
	inbox := nats.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, err
	}
	defer sub.Unsubscribe()

	err = nc.PublishRequest("spinifex.node.vms", inbox, []byte("{}"))
	if err != nil {
		return nil, err
	}

	timeout := 3 * time.Second
	deadline := time.Now().Add(timeout)
	var allVMs []VMInfoWithNode
	responsesReceived := 0

	for time.Now().Before(deadline) {
		if expectedNodes > 0 && responsesReceived >= expectedNodes {
			break
		}
		remaining := time.Until(deadline)
		msg, err := sub.NextMsg(remaining)
		if err == nats.ErrTimeout {
			break
		}
		if err != nil {
			break
		}
		responsesReceived++

		if _, valErr := utils.ValidateErrorPayload(msg.Data); valErr != nil {
			continue
		}

		var nodeResp types.NodeVMsResponse
		if err := json.Unmarshal(msg.Data, &nodeResp); err != nil {
			continue
		}
		for _, vm := range nodeResp.VMs {
			allVMs = append(allVMs, VMInfoWithNode{
				VMInfo: vm,
				Node:   nodeResp.Node,
			})
		}
	}

	if allVMs == nil {
		allVMs = []VMInfoWithNode{}
	}

	return &GetVMsOutput{
		VMs: allVMs,
	}, nil
}
