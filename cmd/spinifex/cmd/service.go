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
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/mulgadc/predastore/s3"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/service"
	"github.com/mulgadc/spinifex/spinifex/services/awsgw"
	"github.com/mulgadc/spinifex/spinifex/services/nats"
	"github.com/mulgadc/spinifex/spinifex/services/predastore"
	"github.com/mulgadc/spinifex/spinifex/services/spinifexui"
	"github.com/mulgadc/spinifex/spinifex/services/viperblockd"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage Spinifex services",
}

var predastoreCmd = &cobra.Command{
	Use:   "predastore",
	Short: "Manage the predastore service",
}

var viperblockCmd = &cobra.Command{
	Use:   "viperblock",
	Short: "Manage the viperblock service",
}

var natsCmd = &cobra.Command{
	Use:   "nats",
	Short: "Manage the nats service",
}

var spinifexCmd = &cobra.Command{
	Use:   "spinifex",
	Short: "Manage the spinifex service",
}

var awsgwCmd = &cobra.Command{
	Use:   "awsgw",
	Short: "Manage the awsgw (AWS gateway) service",
}

var vpcdCmd = &cobra.Command{
	Use:   "vpcd",
	Short: "Manage the vpcd (VPC daemon) service",
}

var spinifexUICmd = &cobra.Command{
	Use:     "spinifex-ui",
	Aliases: []string{"ui", "spinifexui"},
	Short:   "Manage the spinifex-ui service",
}

var predastoreStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the predastore service",
	Run: func(cmd *cobra.Command, args []string) {
		// Add your start logic here
		fmt.Println("Starting predastore service...")

		// Get the port from the flags
		port := viper.GetInt("port")
		host := viper.GetString("host")
		basePath := viper.GetString("base-path")
		debug := viper.GetBool("debug")

		// Required, no default
		if basePath == "" {
			fmt.Println("Base path is not set")
			return
		}

		configPath := viper.GetString("config-path")

		if configPath == "" {
			fmt.Println("Config path is not set")
			return
		}

		tlsCert := viper.GetString("tls-cert")

		if tlsCert == "" {
			fmt.Println("TLS cert is not set")
			return
		}

		tlsKey := viper.GetString("tls-key")

		if tlsKey == "" {
			fmt.Println("TLS key is not set")
			return
		}

		backendType := viper.GetString("backend")
		var backend s3.BackendType

		if backendType == "distributed" {
			backend = s3.BackendDistributed
		} else {
			backend = s3.BackendFilesystem
		}

		nodeID := viper.GetInt("node-id")
		pprofEnabled := viper.GetBool("pprof")
		pprofOutput := viper.GetString("pprof-output")

		service, err := service.New("predastore", &predastore.Config{
			Port:       port,
			Host:       host,
			BasePath:   basePath,
			ConfigPath: configPath,
			Debug:      debug,
			TlsCert:    tlsCert,
			TlsKey:     tlsKey,

			Backend: backend,
			NodeID:  nodeID,

			PprofEnabled:    pprofEnabled,
			PprofOutputPath: pprofOutput,
		})

		if err != nil {
			fmt.Println("Error starting predastore service:", err)
			return
		}

		if _, err := service.Start(); err != nil {
			fmt.Println("Error starting predastore service:", err)
			os.Exit(1)
		}

		fmt.Println("Predastore service started", service)
	},
}

var predastoreStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the predastore service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Stopping predastore service...")

		service, err := service.New("predastore", &predastore.Config{})

		if err != nil {
			fmt.Println("Error stopping predastore service:", err)
			return
		}

		if err = service.Stop(); err != nil {
			fmt.Println("Error stopping predastore service:", err)
			os.Exit(1)
		}

		fmt.Println("Predastore service stopped")

	},
}

var predastoreStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Get status of the predastore service",
	Run: func(cmd *cobra.Command, args []string) {
		// Add your status logic here
		fmt.Println("Predastore service status: ...")
	},
}

// Repeat for viperblock
var viperblockStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the viperblock service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Starting viperblock service...")

		cfgFile := viper.GetString("config")

		if cfgFile == "" {
			fmt.Println("Config file is not set")
			return
		}

		fmt.Println("Loading config from:", cfgFile)

		// TODO: Support ENV vars, CLI, otherwise revert to config.LoadConfig()
		clusterConfig, err := config.LoadConfig(cfgFile)
		if err != nil {
			fmt.Println("Error loading config file:", err)
			return
		}
		nodeConfig := clusterConfig.Nodes[clusterConfig.Node]

		natsHost := viper.GetString("nats-host")

		if natsHost != "" {
			fmt.Println("Overwriting natsHost:", natsHost)
			nodeConfig.NATS.Host = natsHost
		}

		s3Host := viper.GetString("s3-host")

		if s3Host != "" {
			fmt.Println("Overwriting s3host:", s3Host)
			nodeConfig.Predastore.Host = s3Host
		}

		s3Bucket := viper.GetString("s3-bucket")

		if s3Bucket != "" {
			fmt.Println("Overwriting s3bucket:", s3Bucket)
			nodeConfig.Predastore.Bucket = s3Bucket
		}

		s3Region := viper.GetString("s3-region")

		if s3Region != "" {
			fmt.Println("Overwriting s3Region:", s3Region)
			nodeConfig.Predastore.Region = s3Region
		}

		accessKey := viper.GetString("access-key")
		if accessKey != "" {
			fmt.Println("Overwriting access-key: ****")
			nodeConfig.Predastore.AccessKey = accessKey
		}

		secretKey := viper.GetString("secret-key")
		if secretKey != "" {
			fmt.Println("Overwriting secret-key: ****")
			nodeConfig.Predastore.SecretKey = secretKey
		}

		baseDir := viper.GetString("base-dir")
		if baseDir != "" {
			fmt.Println("Overwriting base-dir:", baseDir)
			nodeConfig.Predastore.BaseDir = baseDir
		}

		// Apply changes back to cluster config
		clusterConfig.Nodes[clusterConfig.Node] = nodeConfig

		pluginPath := viper.GetString("plugin-path")

		if pluginPath == "" {
			err := fmt.Errorf("plugin-path must be defined")
			slog.Error(err.Error())
			os.Exit(1)
		}

		// Check plugin path exists
		if _, err := os.Stat(pluginPath); os.IsNotExist(err) {
			err := fmt.Errorf("plugin-path does not exist: %s", pluginPath)
			slog.Error(err.Error())
			os.Exit(1)
		}

		// Resolve sharded WAL setting: default false unless explicitly set to true
		shardWAL := false
		if nodeConfig.Viperblock.ShardWAL != nil {
			shardWAL = *nodeConfig.Viperblock.ShardWAL
		}

		service, err := service.New("viperblock", &viperblockd.Config{
			NatsHost:   nodeConfig.NATS.Host,
			NatsToken:  nodeConfig.NATS.ACL.Token,
			NatsCACert: nodeConfig.NATS.CACert,
			PluginPath: pluginPath,
			S3Host:     nodeConfig.Predastore.Host,
			Bucket:     nodeConfig.Predastore.Bucket,
			Region:     nodeConfig.Predastore.Region,
			AccessKey:  nodeConfig.Predastore.AccessKey,
			SecretKey:  nodeConfig.Predastore.SecretKey,
			BaseDir:    nodeConfig.Predastore.BaseDir,
			NodeName:   clusterConfig.Node,
			ShardWAL:   shardWAL,
		})

		if err != nil {
			fmt.Println("Error starting viperblock service:", err)
			return
		}

		_, err = service.Start()

		if err != nil {
			fmt.Println("Error starting viperblock service:", err)
			return
		}

		fmt.Println("Viperblock service started", service)
	},
}

var viperblockStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the viperblock service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Stopping viperblock service...")

		service, err := service.New("viperblock", &viperblockd.Config{})

		if err != nil {
			fmt.Println("Error stopping viperblock service:", err)
			return
		}

		if err = service.Stop(); err != nil {
			fmt.Println("Error stopping viperblock service:", err)
			os.Exit(1)
		}

		fmt.Println("Viperblock service stopped")

	},
}

var viperblockStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Get status of the viperblock service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Viperblock service status: ...")
	},
}

// Repeat for nats
var natsStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the nats service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Starting nats service...")

		port := viper.GetInt("port")
		host := viper.GetString("host")
		debug := viper.GetBool("debug")
		dataDir := viper.GetString("data-dir")
		jetStream := viper.GetBool("jetstream")

		cfgFile := viper.GetString("config")

		service, err := service.New("nats", &nats.Config{
			ConfigFile: cfgFile,
			Port:       port,
			Host:       host,
			Debug:      debug,
			DataDir:    dataDir,
			JetStream:  jetStream,
		})

		if err != nil {
			fmt.Println("Error starting nats service:", err)
			return
		}

		if _, err = service.Start(); err != nil {
			fmt.Println("Error starting nats service:", err)
			os.Exit(1)
		}
		fmt.Println("NATS service started")
	},
}

var natsStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the nats service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Stopping nats service...")

		service, err := service.New("nats", &nats.Config{})

		if err != nil {
			fmt.Println("Error stopping nats service:", err)
			return
		}

		if err = service.Stop(); err != nil {
			fmt.Println("Error stopping nats service:", err)
			os.Exit(1)
		}

		fmt.Println("Nats service stopped")
	},
}

var natsStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Get status of the nats service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Nats service status: ...")
	},
}

// Repeat for spinifex
var spinifexStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the spinifex service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Starting spinifex service...")

		cfgFile := viper.GetString("config")

		if cfgFile == "" {
			fmt.Println("Config file is not set")
			return
		}

		// TODO: Support ENV vars, CLI, otherwise revert to config.LoadConfig()
		clusterConfig, err := config.LoadConfig(cfgFile)
		if err != nil {
			fmt.Println("Error loading config file:", err)
			return
		}
		nodeConfig := clusterConfig.Nodes[clusterConfig.Node]

		// Overwrite defaults (CLI first, config second, env third)
		baseDir := viper.GetString("base-dir")

		if baseDir != "" {
			fmt.Println("Overwriting base-dir to:", baseDir)
			nodeConfig.BaseDir = baseDir
		}

		// Overwrite defaults (CLI first, config second, env third)
		walDir := viper.GetString("wal-dir")

		if walDir != "" {
			fmt.Println("Overwriting wal-dir to:", walDir)
			nodeConfig.WalDir = walDir
		}

		// Apply changes back to cluster config
		clusterConfig.Nodes[clusterConfig.Node] = nodeConfig

		svc, err := service.New("spinifex", clusterConfig)

		if err != nil {
			fmt.Println("Error starting spinifex service:", err)
			return
		}

		// Set config path for cluster manager
		if spxSvc, ok := svc.(interface{ SetConfigPath(string) }); ok {
			spxSvc.SetConfigPath(cfgFile)
		}

		if _, err = svc.Start(); err != nil {
			fmt.Println("Error starting spinifex service:", err)
			os.Exit(1)
		}
		fmt.Println("Spinifex service started")
	},
}

var spinifexStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the spinifex service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Stopping spinifex service...")

		service, err := service.New("spinifex", &config.ClusterConfig{})

		if err != nil {
			fmt.Println("Error stopping spinifex service:", err)
			return
		}

		if err = service.Stop(); err != nil {
			fmt.Println("Error stopping spinifex service:", err)
			os.Exit(1)
		}

		fmt.Println("Spinifex service stopped")
	},
}

var spinifexStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Get status of the spinifex service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Spinifex service status: ...")
	},
}

// AWS GW

var awsgwStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the awsgw service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Starting awsgw service...")

		cfgFile := viper.GetString("config")

		if cfgFile == "" {
			fmt.Println("Config file is not set")
			return
		}

		fmt.Println("Loading config from:", cfgFile)

		// TODO: Support ENV vars, CLI, otherwise revert to config.LoadConfig()
		clusterConfig, err := config.LoadConfig(cfgFile)
		if err != nil {
			fmt.Println("Error loading config file:", err)
			return
		}
		nodeConfig := clusterConfig.Nodes[clusterConfig.Node]

		// Overwrite defaults (CLI first, config second, env third)
		awsgwHost := viper.GetString("host")
		if awsgwHost != "" {
			fmt.Println("Overwriting awsgw host to:", awsgwHost)
			//nodeConfig.AWSGW.Host = awsgwHost
		}

		awsgwTlsCert := viper.GetString("tls-cert")
		if awsgwTlsCert != "" {
			fmt.Println("Overwriting awsgw tls-cert to:", awsgwTlsCert)
			nodeConfig.AWSGW.TLSCert = awsgwTlsCert
		}

		awsgwTlsKey := viper.GetString("tls-key")

		if awsgwTlsKey != "" {
			fmt.Println("Overwriting awsgw tls-key to:", awsgwTlsKey)
			nodeConfig.AWSGW.TLSKey = awsgwTlsKey
		}

		baseDir := viper.GetString("base-dir")

		if baseDir != "" {
			fmt.Println("Overwriting awsgw base-dir to:", baseDir)
			nodeConfig.BaseDir = baseDir
		}

		// Apply changes back to cluster config
		clusterConfig.Nodes[clusterConfig.Node] = nodeConfig

		awsgw.SetBuildInfo(Version, Commit)
		service, err := service.New("awsgw", clusterConfig)

		if err != nil {
			fmt.Println("Error starting awsgw service:", err)
			return
		}

		if _, err = service.Start(); err != nil {
			fmt.Println("Error starting awsgw service:", err)
			os.Exit(1)
		}
		fmt.Println("AWSGW service started")
	},
}

var awsgwStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the awsgw service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Stopping awsgw service...")

		service, err := service.New("awsgw", &config.ClusterConfig{})

		if err != nil {
			fmt.Println("Error stopping awsgw service:", err)
			return
		}

		if err = service.Stop(); err != nil {
			fmt.Println("Error stopping awsgw service:", err)
			os.Exit(1)
		}

		fmt.Println("AWSGW service stopped")
	},
}

var awsgwStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Get status of the awsgw service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("AWSGW service status: ...")
	},
}

var spinifexUIStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the spinifex-ui service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Starting spinifex-ui service...")

		port := viper.GetInt("spinifex-ui-port")
		host := viper.GetString("spinifex-ui-host")
		tlsCert := viper.GetString("spinifex-ui-tls-cert")
		tlsKey := viper.GetString("spinifex-ui-tls-key")

		svc, err := service.New("spinifex-ui", &spinifexui.Config{
			Port:    port,
			Host:    host,
			TLSCert: tlsCert,
			TLSKey:  tlsKey,
		})

		if err != nil {
			fmt.Println("Error starting spinifex-ui service:", err)
			return
		}

		if _, err = svc.Start(); err != nil {
			fmt.Println("Error starting spinifex-ui service:", err)
			os.Exit(1)
		}
		fmt.Println("spinifex-ui service started")
	},
}

var spinifexUIStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the spinifex-ui service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Stopping spinifex-ui service...")

		svc, err := service.New("spinifex-ui", &spinifexui.Config{})

		if err != nil {
			fmt.Println("Error stopping spinifex-ui service:", err)
			return
		}

		if err = svc.Stop(); err != nil {
			fmt.Println("Error stopping spinifex-ui service:", err)
			os.Exit(1)
		}
		fmt.Println("spinifex-ui service stopped")
	},
}

var spinifexUIStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Get status of the spinifex-ui service",
	Run: func(cmd *cobra.Command, args []string) {
		svc, err := service.New("spinifex-ui", &spinifexui.Config{})

		if err != nil {
			fmt.Println("Error getting spinifex-ui service status:", err)
			return
		}

		status, err := svc.Status()
		if err != nil {
			fmt.Println("Error getting spinifex-ui service status:", err)
			return
		}

		fmt.Println("spinifex-ui service status:", status)
	},
}

// checkLegacyWanBridgeKey fails vpcd startup if the deprecated `wan_bridge`
// TOML key or `SPINIFEX_VPCD_WAN_BRIDGE` env-var is present. The key was
// renamed to `dhcp_bind_bridge` (see mulga-998.a) because the old name misled
// operators into pointing at the OVN-side bridge ("br-ext"), which never sees
// LAN DHCP traffic. Per D3: no silent alias, no auto-rewrite — operator must
// rename the key before vpcd will start.
func checkLegacyWanBridgeKey(node, cfgFile string) error {
	legacyInTOML := viper.IsSet("nodes." + node + ".vpcd.wan_bridge")
	legacyInEnv := os.Getenv("SPINIFEX_VPCD_WAN_BRIDGE") != ""
	if !legacyInTOML && !legacyInEnv {
		return nil
	}
	source := cfgFile
	if legacyInEnv {
		source = "env SPINIFEX_VPCD_WAN_BRIDGE"
	}
	return fmt.Errorf(
		"vpcd: deprecated 'wan_bridge' key found in %s. "+
			"Rename 'wan_bridge' to 'dhcp_bind_bridge'. The value may also be wrong — verify it is "+
			"the Linux bridge holding your WAN NIC (veth mode) or the OVS bridge holding your WAN NIC (direct mode); "+
			"never 'br-ext'. Typical value on a consumer-router LAN: 'br-wan'. "+
			"Then: sudo systemctl restart spinifex-vpcd",
		source,
	)
}

var vpcdStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the vpcd service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Starting vpcd service...")

		cfgFile := viper.GetString("config")
		if cfgFile == "" {
			fmt.Println("Config file is not set")
			return
		}

		clusterConfig, err := config.LoadConfig(cfgFile)
		if err != nil {
			fmt.Println("Error loading config file:", err)
			return
		}

		if err := checkLegacyWanBridgeKey(clusterConfig.Node, cfgFile); err != nil {
			slog.Error(err.Error())
			os.Exit(1)
		}

		nodeConfig := clusterConfig.Nodes[clusterConfig.Node]

		// Map cluster-wide external pools to vpcd config
		var extPools []vpcd.ExternalPoolConfig
		for _, p := range clusterConfig.Network.ExternalPools {
			extPools = append(extPools, vpcd.ExternalPoolConfig{
				Name:       p.Name,
				RangeStart: p.RangeStart,
				RangeEnd:   p.RangeEnd,
				Gateway:    p.Gateway,
				GatewayIP:  p.GatewayIP,
				PrefixLen:  p.PrefixLen,
				DNSServers: p.DNSServers,
				Region:     p.Region,
				AZ:         p.AZ,
			})
		}

		// Fallback chassis names derived from config node names. vpcd prefers
		// discovering chassis from the OVN Southbound DB at startup; these are
		// only used if that discovery fails.
		var chassisNames []string
		for nodeName := range clusterConfig.Nodes {
			chassisNames = append(chassisNames, "chassis-"+nodeName)
		}

		var bootstrap *vpcd.BootstrapVPC
		if clusterConfig.Bootstrap.VpcId != "" {
			bootstrap = &vpcd.BootstrapVPC{
				AccountID:  clusterConfig.Bootstrap.AccountID,
				VpcId:      clusterConfig.Bootstrap.VpcId,
				SubnetId:   clusterConfig.Bootstrap.SubnetId,
				IgwId:      clusterConfig.Bootstrap.IgwId,
				Cidr:       clusterConfig.Bootstrap.Cidr,
				SubnetCidr: clusterConfig.Bootstrap.SubnetCidr,
			}
		}

		baseDir := viper.GetString("base-dir")
		if baseDir != "" {
			fmt.Println("Overwriting vpcd base-dir to:", baseDir)
			nodeConfig.BaseDir = baseDir
		}

		svc, err := service.New("vpcd", &vpcd.Config{
			NatsHost:          nodeConfig.NATS.Host,
			NatsToken:         nodeConfig.NATS.ACL.Token,
			NatsCACert:        nodeConfig.NATS.CACert,
			OVNNBAddr:         nodeConfig.VPCD.OVNNBAddr,
			OVNSBAddr:         nodeConfig.VPCD.OVNSBAddr,
			BaseDir:           nodeConfig.BaseDir,
			Debug:             false,
			ExternalMode:      clusterConfig.Network.ExternalMode,
			ExternalPools:     extPools,
			ChassisNames:      chassisNames,
			Bootstrap:         bootstrap,
			ExternalInterface: nodeConfig.VPCD.ExternalInterface,
			DhcpBindBridge:    nodeConfig.VPCD.DhcpBindBridge,
			BridgeMode:        nodeConfig.VPCD.BridgeMode,
		})
		if err != nil {
			fmt.Println("Error starting vpcd service:", err)
			return
		}

		if _, err = svc.Start(); err != nil {
			fmt.Println("Error starting vpcd service:", err)
			os.Exit(1)
		}
		fmt.Println("vpcd service started")
	},
}

var vpcdStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the vpcd service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Stopping vpcd service...")

		svc, err := service.New("vpcd", &vpcd.Config{})
		if err != nil {
			fmt.Println("Error stopping vpcd service:", err)
			return
		}

		if err = svc.Stop(); err != nil {
			fmt.Println("Error stopping vpcd service:", err)
			os.Exit(1)
		}
		fmt.Println("vpcd service stopped")
	},
}

var vpcdStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Get status of the vpcd service",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("vpcd service status: ...")
	},
}

func init() {
	viper.SetEnvPrefix("SPINIFEX") // Prefix for environment variables
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	viper.AutomaticEnv() // Read environment variables automatically

	rootCmd.AddCommand(serviceCmd)

	serviceCmd.AddCommand(predastoreCmd)

	// Predastore Port
	predastoreCmd.PersistentFlags().Int("port", 8443, "Predastore (S3) port")
	viper.BindEnv("port", "SPINIFEX_PREDASTORE_PORT")
	viper.BindPFlag("port", predastoreCmd.PersistentFlags().Lookup("port"))

	// Predastore Host
	predastoreCmd.PersistentFlags().String("host", "0.0.0.0", "Predastore (S3) host")
	viper.BindEnv("host", "SPINIFEX_PREDASTORE_HOST")
	viper.BindPFlag("host", predastoreCmd.PersistentFlags().Lookup("host"))

	// Base path
	predastoreCmd.PersistentFlags().String("base-path", "", "Predastore (S3) base path")
	viper.BindEnv("base-path", "SPINIFEX_PREDASTORE_BASE_PATH")
	viper.BindPFlag("base-path", predastoreCmd.PersistentFlags().Lookup("base-path"))

	// Predastore Config Path
	predastoreCmd.PersistentFlags().String("config-path", "", "Predastore (S3) config path")
	viper.BindEnv("config-path", "SPINIFEX_PREDASTORE_CONFIG_PATH")
	viper.BindPFlag("config-path", predastoreCmd.PersistentFlags().Lookup("config-path"))

	// Predastore Debug
	predastoreCmd.PersistentFlags().Bool("debug", false, "Predastore (S3) debug")
	viper.BindEnv("debug", "SPINIFEX_PREDASTORE_DEBUG")
	viper.BindPFlag("debug", predastoreCmd.PersistentFlags().Lookup("debug"))

	// Predastore TLS Cert
	predastoreCmd.PersistentFlags().String("tls-cert", "", "Predastore (S3) TLS certificate")
	viper.BindEnv("tls-cert", "SPINIFEX_PREDASTORE_TLS_CERT")
	viper.BindPFlag("tls-cert", predastoreCmd.PersistentFlags().Lookup("tls-cert"))

	// Predastore TLS Key
	predastoreCmd.PersistentFlags().String("tls-key", "", "Predastore (S3) TLS key")
	viper.BindEnv("tls-key", "SPINIFEX_PREDASTORE_TLS_KEY")
	viper.BindPFlag("tls-key", predastoreCmd.PersistentFlags().Lookup("tls-key"))

	// Predastore Backend
	predastoreCmd.PersistentFlags().String("backend", "distributed", "Predastore (S3) backend")
	viper.BindEnv("backend", "SPINIFEX_PREDASTORE_BACKEND")
	viper.BindPFlag("backend", predastoreCmd.PersistentFlags().Lookup("backend"))

	// Predastore Node ID
	predastoreCmd.PersistentFlags().Int("node-id", 0, "Predastore (S3) node ID")
	viper.BindEnv("node-id", "SPINIFEX_PREDASTORE_NODE_ID")
	viper.BindPFlag("node-id", predastoreCmd.PersistentFlags().Lookup("node-id"))

	// Predastore CPU Profiling
	predastoreCmd.PersistentFlags().Bool("pprof", false, "Enable CPU profiling (also via PPROF_ENABLED=1)")
	viper.BindEnv("pprof", "PPROF_ENABLED")
	viper.BindPFlag("pprof", predastoreCmd.PersistentFlags().Lookup("pprof"))

	// Predastore CPU Profile Output Path
	predastoreCmd.PersistentFlags().String("pprof-output", "/tmp/predastore-cpu.prof", "CPU profile output path")
	viper.BindEnv("pprof-output", "PPROF_OUTPUT")
	viper.BindPFlag("pprof-output", predastoreCmd.PersistentFlags().Lookup("pprof-output"))

	predastoreCmd.AddCommand(predastoreStartCmd)
	predastoreCmd.AddCommand(predastoreStopCmd)
	predastoreCmd.AddCommand(predastoreStatusCmd)

	serviceCmd.AddCommand(viperblockCmd)

	viperblockCmd.PersistentFlags().String("s3-host", "", "Predastore (S3) host URI")
	viper.BindEnv("s3-host", "SPINIFEX_VIPERBLOCK_S3_HOST")
	viper.BindPFlag("s3-host", predastoreCmd.PersistentFlags().Lookup("s3-host"))

	viperblockCmd.PersistentFlags().String("s3-bucket", "predastore", "Predastore (S3) bucket")
	viper.BindEnv("s3-bucket", "SPINIFEX_VIPERBLOCK_S3_BUCKET")
	viper.BindPFlag("s3-bucket", predastoreCmd.PersistentFlags().Lookup("s3-bucket"))

	viperblockCmd.PersistentFlags().String("s3-region", "ap-southeast-2", "Predastore (S3) region")
	viper.BindEnv("s3-region", "SPINIFEX_VIPERBLOCK_S3_REGION")
	viper.BindPFlag("s3-region", predastoreCmd.PersistentFlags().Lookup("s3-region"))

	viperblockCmd.PersistentFlags().String("plugin-path", "/opt/spinifex/lib/nbdkit-viperblock-plugin.so", "Pathname to the nbdkit viperblockplugin")
	viper.BindEnv("plugin-path", "SPINIFEX_VIPERBLOCK_PLUGIN_PATH")
	viper.BindPFlag("plugin-path", predastoreCmd.PersistentFlags().Lookup("plugin-path"))

	viperblockCmd.AddCommand(viperblockStartCmd)
	viperblockCmd.AddCommand(viperblockStopCmd)
	viperblockCmd.AddCommand(viperblockStatusCmd)

	// Nats
	serviceCmd.AddCommand(natsCmd)

	natsCmd.AddCommand(natsStartCmd)
	natsCmd.AddCommand(natsStopCmd)
	natsCmd.AddCommand(natsStatusCmd)

	// Add NATS flags
	natsCmd.PersistentFlags().Int("port", 4222, "NATS server port")
	viper.BindEnv("port", "SPINIFEX_NATS_PORT")
	viper.BindPFlag("port", natsCmd.PersistentFlags().Lookup("port"))

	natsCmd.PersistentFlags().String("host", "0.0.0.0", "NATS server host")
	viper.BindEnv("host", "SPINIFEX_NATS_HOST")
	viper.BindPFlag("host", natsCmd.PersistentFlags().Lookup("host"))

	natsCmd.PersistentFlags().Bool("debug", false, "Enable debug logging")
	viper.BindEnv("debug", "SPINIFEX_NATS_DEBUG")
	viper.BindPFlag("debug", natsCmd.PersistentFlags().Lookup("debug"))

	natsCmd.PersistentFlags().String("data-dir", "", "NATS data directory")
	viper.BindEnv("data-dir", "SPINIFEX_NATS_DATA_DIR")
	viper.BindPFlag("data-dir", natsCmd.PersistentFlags().Lookup("data-dir"))

	natsCmd.PersistentFlags().Bool("jetstream", false, "Enable JetStream")
	viper.BindEnv("jetstream", "SPINIFEX_NATS_JETSTREAM")
	viper.BindPFlag("jetstream", natsCmd.PersistentFlags().Lookup("jetstream"))

	// Spinifex
	serviceCmd.AddCommand(spinifexCmd)

	spinifexCmd.AddCommand(spinifexStartCmd)
	spinifexCmd.AddCommand(spinifexStopCmd)
	spinifexCmd.AddCommand(spinifexStatusCmd)

	spinifexCmd.PersistentFlags().String("wal-dir", "", "Write-ahead log (WAL) directory. Place on high-speed NVMe disk, or tmpfs for development.")
	viper.BindEnv("wal-dir", "SPINIFEX_WAL_DIR")
	viper.BindPFlag("wal-dir", spinifexCmd.PersistentFlags().Lookup("wal-dir"))

	// AWS GW
	serviceCmd.AddCommand(awsgwCmd)

	awsgwCmd.PersistentFlags().String("host", "0.0.0.0:9999", "AWS Gateway server host")
	viper.BindEnv("host", "SPINIFEX_AWSGW_HOST")
	viper.BindPFlag("host", awsgwCmd.PersistentFlags().Lookup("host"))

	// AWS GW TLS Cert
	awsgwCmd.PersistentFlags().String("tls-cert", "", "AWS Gateway TLS certificate")
	viper.BindEnv("tls-cert", "SPINIFEX_AWSGW_TLS_CERT")
	viper.BindPFlag("tls-cert", awsgwCmd.PersistentFlags().Lookup("tls-cert"))

	// AWS GW TLS Key
	awsgwCmd.PersistentFlags().String("tls-key", "", "AWS Gateway TLS key")
	viper.BindEnv("tls-key", "SPINIFEX_AWSGW_TLS_KEY")
	viper.BindPFlag("tls-key", awsgwCmd.PersistentFlags().Lookup("tls-key"))

	awsgwCmd.PersistentFlags().Bool("debug", false, "AWS Gateway Debug")
	viper.BindEnv("debug", "SPINIFEX_AWSGW_DEBUG")
	viper.BindPFlag("debug", awsgwCmd.PersistentFlags().Lookup("debug"))

	awsgwCmd.AddCommand(awsgwStartCmd)
	awsgwCmd.AddCommand(awsgwStopCmd)
	awsgwCmd.AddCommand(awsgwStatusCmd)

	// spinifex-ui
	serviceCmd.AddCommand(spinifexUICmd)

	spinifexUICmd.PersistentFlags().Int("port", 3000, "spinifex-ui server port")
	viper.BindEnv("spinifex-ui-port", "SPINIFEX_UI_PORT")
	viper.BindPFlag("spinifex-ui-port", spinifexUICmd.PersistentFlags().Lookup("port"))

	spinifexUICmd.PersistentFlags().String("host", "0.0.0.0", "spinifex-ui server host")
	viper.BindEnv("spinifex-ui-host", "SPINIFEX_UI_HOST")
	viper.BindPFlag("spinifex-ui-host", spinifexUICmd.PersistentFlags().Lookup("host"))

	spinifexUICmd.PersistentFlags().String("tls-cert", "", "TLS certificate path")
	viper.BindEnv("spinifex-ui-tls-cert", "SPINIFEX_UI_TLS_CERT")
	viper.BindPFlag("spinifex-ui-tls-cert", spinifexUICmd.PersistentFlags().Lookup("tls-cert"))

	spinifexUICmd.PersistentFlags().String("tls-key", "", "TLS key path")
	viper.BindEnv("spinifex-ui-tls-key", "SPINIFEX_UI_TLS_KEY")
	viper.BindPFlag("spinifex-ui-tls-key", spinifexUICmd.PersistentFlags().Lookup("tls-key"))

	spinifexUICmd.AddCommand(spinifexUIStartCmd)
	spinifexUICmd.AddCommand(spinifexUIStopCmd)
	spinifexUICmd.AddCommand(spinifexUIStatusCmd)

	// vpcd
	serviceCmd.AddCommand(vpcdCmd)

	vpcdCmd.AddCommand(vpcdStartCmd)
	vpcdCmd.AddCommand(vpcdStopCmd)
	vpcdCmd.AddCommand(vpcdStatusCmd)
}
