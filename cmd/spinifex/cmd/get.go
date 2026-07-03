package cmd

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "Display cluster resources",
	Long:  `Display cluster resources such as nodes and VMs.`,
}

var getNodesCmd = &cobra.Command{
	Use:   "nodes",
	Short: "Display cluster nodes",
	Long:  `Display all physical nodes in the cluster with status, IP, region, and services.`,
	Run:   runGetNodes,
}

var getVMsCmd = &cobra.Command{
	Use:     "vms",
	Aliases: []string{"instances"},
	Short:   "Display VMs across the cluster",
	Long:    `Display all VMs running across the cluster with instance type, resources, and placement.`,
	Run:     runGetVMs,
}

func init() {
	rootCmd.AddCommand(getCmd)
	getCmd.AddCommand(getNodesCmd)
	getCmd.AddCommand(getVMsCmd)

	getCmd.PersistentFlags().Duration("timeout", 3*time.Second, "Timeout for collecting responses from nodes")
}

// loadConfigAndConnect loads the cluster config and connects to NATS.
func loadConfigAndConnect() (*config.ClusterConfig, *nats.Conn, error) {
	cfgPath := viper.GetString("config")
	if cfgPath == "" {
		cfgPath = DefaultConfigFile()
	}

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Backfill BaseDir from config file path when not set in the config.
	if nodeConfig, ok := cfg.Nodes[cfg.Node]; ok && nodeConfig.BaseDir == "" {
		if isProductionLayout() {
			// Production: BaseDir is /var/lib/spinifex (data dir).
			// /var/lib/spinifex/config is a symlink to /etc/spinifex.
			nodeConfig.BaseDir = DefaultDataDir()
		} else {
			// Dev: config lives at <baseDir>/config/spinifex.toml, so baseDir is two levels up.
			nodeConfig.BaseDir = filepath.Dir(filepath.Dir(cfgPath))
		}
		cfg.Nodes[cfg.Node] = nodeConfig
	}

	nodeConfig := cfg.Nodes[cfg.Node]
	nc, err := utils.ConnectNATS(admin.DialTarget(nodeConfig.NATS.Host), nodeConfig.NATS.ACL.Token, nodeConfig.NATS.CACert)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	return cfg, nc, nil
}

// collectResponses publishes to a fan-out topic and collects all responses within the timeout.
func collectResponses(nc *nats.Conn, topic string, timeout time.Duration) ([][]byte, error) {
	inbox := nats.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to inbox: %w", err)
	}
	defer sub.Unsubscribe()

	if err := nc.PublishRequest(topic, inbox, nil); err != nil {
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}
	nc.Flush()

	var responses [][]byte
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			break // timeout or other error — done collecting
		}
		responses = append(responses, msg.Data)
	}
	return responses, nil
}

func formatUptime(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	d := time.Duration(seconds) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

func formatRoles(resp types.NodeStatusResponse) string {
	var roles []string
	if resp.NATSRole != "" {
		roles = append(roles, "nats:"+resp.NATSRole)
	}
	if resp.PredastoreRole != "" {
		roles = append(roles, "predastore:"+resp.PredastoreRole)
	}
	if len(roles) == 0 {
		return "-"
	}
	return strings.Join(roles, ",")
}

func formatMemGB(gb float64) string {
	if gb >= 1.0 {
		return fmt.Sprintf("%.1fGi", gb)
	}
	return fmt.Sprintf("%dMi", int(gb*1024))
}

func runGetNodes(cmd *cobra.Command, args []string) {
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

	// Parse responses into a map by node name
	respondedNodes := make(map[string]types.NodeStatusResponse)
	for _, data := range responses {
		var resp types.NodeStatusResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		respondedNodes[resp.Node] = resp
	}

	// Build table: union of config-known nodes + NATS responders
	tableData := pterm.TableData{
		{"NAME", "STATUS", "ROLES", "IP", "REGION", "AZ", "UPTIME", "VMs", "SERVICES"},
	}

	// Collect all node names: config + responded (union)
	nodeSet := make(map[string]struct{})
	for name := range cfg.Nodes {
		nodeSet[name] = struct{}{}
	}
	for name := range respondedNodes {
		nodeSet[name] = struct{}{}
	}
	nodeNames := slices.Sorted(maps.Keys(nodeSet))

	for _, name := range nodeNames {
		nodeCfg := cfg.Nodes[name]
		if resp, ok := respondedNodes[name]; ok {
			tableData = append(tableData, []string{
				resp.Node,
				resp.Status,
				formatRoles(resp),
				resp.Host,
				resp.Region,
				resp.AZ,
				formatUptime(resp.Uptime),
				strconv.Itoa(resp.VMCount),
				strings.Join(resp.Services, ","),
			})
		} else {
			tableData = append(tableData, []string{
				name,
				"NotReady",
				"-",
				nodeCfg.Host,
				nodeCfg.Region,
				nodeCfg.AZ,
				"-",
				"-",
				strings.Join(nodeCfg.GetServices(), ","),
			})
		}
	}

	pterm.DefaultTable.WithHasHeader().WithLeftAlignment().WithData(tableData).Render()
}

func runGetVMs(cmd *cobra.Command, args []string) {
	_, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	timeout, _ := cmd.Flags().GetDuration("timeout")
	responses, err := collectResponses(nc, "spinifex.node.vms", timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	type vmRow struct {
		types.VMInfo

		Node string
		Host string
	}

	var allVMs []vmRow
	for _, data := range responses {
		var resp types.NodeVMsResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		for _, v := range resp.VMs {
			allVMs = append(allVMs, vmRow{VMInfo: v, Node: resp.Node, Host: resp.Host})
		}
	}

	if len(allVMs) == 0 {
		fmt.Println("No VMs found.")
		return
	}

	// Sort by node then instance ID
	sort.Slice(allVMs, func(i, j int) bool {
		if allVMs[i].Node != allVMs[j].Node {
			return allVMs[i].Node < allVMs[j].Node
		}
		return allVMs[i].InstanceID < allVMs[j].InstanceID
	})

	tableData := pterm.TableData{
		{"INSTANCE", "STATUS", "HEALTH", "CRASHES", "TYPE", "VCPU", "MEM", "NODE", "IP", "AGE"},
	}

	for _, v := range allVMs {
		age := "-"
		if v.LaunchTime > 0 {
			age = formatUptime(time.Now().Unix() - v.LaunchTime)
		}
		health := v.Health
		if health == "" {
			health = "-"
		}
		tableData = append(tableData, []string{
			v.InstanceID,
			v.Status,
			health,
			strconv.Itoa(v.CrashCount),
			v.InstanceType,
			strconv.Itoa(v.VCPU),
			formatMemGB(v.MemoryGB),
			v.Node,
			v.Host,
			age,
		})
	}

	pterm.DefaultTable.WithHasHeader().WithLeftAlignment().WithData(tableData).Render()
}
