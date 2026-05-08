/*
Copyright © 2026 Mulga Defense Corporation

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
)

var topCmd = &cobra.Command{
	Use:   "top",
	Short: "Display cluster resource usage",
	Long:  `Display resource usage (CPU, memory) for cluster nodes.`,
}

var topNodesCmd = &cobra.Command{
	Use:   "nodes",
	Short: "Display resource usage per node",
	Long:  `Display CPU and memory usage per node, plus a summary of available instance types across the cluster.`,
	Run:   runTopNodes,
}

func init() {
	rootCmd.AddCommand(topCmd)
	topCmd.AddCommand(topNodesCmd)

	topCmd.PersistentFlags().Duration("timeout", 3*time.Second, "Timeout for collecting responses from nodes")
}

func runTopNodes(cmd *cobra.Command, args []string) {
	cfg, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	timeout, _ := cmd.Flags().GetDuration("timeout")
	responses, err := collectResponses(nc, "spinifex.node.status", timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	respondedNodes := make(map[string]types.NodeStatusResponse)
	for _, data := range responses {
		var resp types.NodeStatusResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		respondedNodes[resp.Node] = resp
	}

	// Node resource table: union of config-known nodes + NATS responders
	nodeSet := make(map[string]struct{})
	for name := range cfg.Nodes {
		nodeSet[name] = struct{}{}
	}
	for name := range respondedNodes {
		nodeSet[name] = struct{}{}
	}
	nodeNames := make([]string, 0, len(nodeSet))
	for name := range nodeSet {
		nodeNames = append(nodeNames, name)
	}
	sort.Strings(nodeNames)

	nodeTable := pterm.TableData{
		{"NAME", "CPU (used/total)", "MEM (used/total)", "GPU (used/total)", "VMs"},
	}

	// Aggregate instance type capacity across all nodes
	capacityMap := make(map[string]*aggregatedCap)

	for _, name := range nodeNames {
		if resp, ok := respondedNodes[name]; ok {
			gpuCol := "-"
			if resp.GPUPassthrough {
				gpuCol = fmt.Sprintf("%d/%d", resp.AllocGPUs, resp.TotalGPUs)
			} else if resp.GPUCapable {
				gpuCol = fmt.Sprintf("0/%d*", len(resp.GPUModels))
			}
			nodeTable = append(nodeTable, []string{
				resp.Node,
				fmt.Sprintf("%d/%d", resp.AllocVCPU, resp.TotalVCPU),
				fmt.Sprintf("%s/%s", formatMemGB(resp.AllocMemGB), formatMemGB(resp.TotalMemGB)),
				gpuCol,
				strconv.Itoa(resp.VMCount),
			})

			for _, cap := range resp.InstanceTypes {
				if agg, ok := capacityMap[cap.Name]; ok {
					agg.Available += cap.Available
				} else {
					capacityMap[cap.Name] = &aggregatedCap{
						VCPU:      cap.VCPU,
						MemoryGB:  cap.MemoryGB,
						Available: cap.Available,
					}
				}
			}
		} else {
			nodeTable = append(nodeTable, []string{
				name,
				"-",
				"-",
				"-",
				"-",
			})
		}
	}

	pterm.DefaultTable.WithHasHeader().WithLeftAlignment().WithData(nodeTable).Render()

	// Instance type capacity summary
	if len(capacityMap) == 0 {
		return
	}

	fmt.Println()

	capNames := make([]string, 0, len(capacityMap))
	for name := range capacityMap {
		capNames = append(capNames, name)
	}
	sort.Strings(capNames)

	capTable := pterm.TableData{
		{"INSTANCE TYPE", "AVAILABLE", "VCPU", "MEMORY"},
	}

	for _, name := range capNames {
		agg := capacityMap[name]
		capTable = append(capTable, []string{
			name,
			strconv.Itoa(agg.Available),
			strconv.Itoa(agg.VCPU),
			formatMemGB(agg.MemoryGB),
		})
	}

	pterm.DefaultTable.WithHasHeader().WithLeftAlignment().WithData(capTable).Render()
}

type aggregatedCap struct {
	VCPU      int
	MemoryGB  float64
	Available int
}
