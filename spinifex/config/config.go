package config

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"

	"github.com/spf13/viper"
)

type ClusterConfig struct {
	Epoch     uint64            `mapstructure:"epoch"`     // bump when leader commits changes
	Node      string            `mapstructure:"node"`      // my node name
	Version   string            `mapstructure:"version"`   // spinifex version
	Network   NetworkConfig     `mapstructure:"network"`   // cluster-wide external network settings
	Bootstrap BootstrapConfig   `mapstructure:"bootstrap"` // default VPC IDs for OVN reconciliation
	AWS       AWSConfig         `mapstructure:"aws"`       // cluster-wide AWS-parity settings (region, endpoint suffix)
	Nodes     map[string]Config `mapstructure:"nodes"`     // full config for every node
}

// Defaults for cluster-wide AWS-parity settings.
const (
	DefaultAWSRegion         = "us-east-1"
	DefaultAWSInternalSuffix = "spinifex.internal"
)

// AWSConfig holds cluster-wide AWS-parity settings shared across services.
// Region scopes the default AWS region; InternalSuffix is the internal DNS
// suffix used to build service endpoints (e.g. ecr.{region}.{suffix}).
type AWSConfig struct {
	Region         string `mapstructure:"region"`
	InternalSuffix string `mapstructure:"internal_suffix"`
}

// ExternalPool defines a range of routable IPs that Spinifex manages for public subnets.
type ExternalPool struct {
	Name       string   `mapstructure:"name"`        // Pool identifier (e.g., "wan", "dc1-primary")
	Source     string   `mapstructure:"source"`      // IP source: "static" (default) or "dhcp"
	BindBridge string   `mapstructure:"bind_bridge"` // Linux bridge for DHCP DORA (source=dhcp only)
	DHCPMAC    string   `mapstructure:"dhcp_mac"`    // DHCP client MAC strategy: "derived" (default) or "interface" (source=dhcp only)
	RangeStart string   `mapstructure:"range_start"` // First IP in range (static source only)
	RangeEnd   string   `mapstructure:"range_end"`   // Last IP in range (static source only)
	Gateway    string   `mapstructure:"gateway"`     // WAN default gateway (next hop for 0.0.0.0/0)
	GatewayIP  string   `mapstructure:"gateway_ip"`  // OVN router external IP (override; defaults to first IP in range)
	PrefixLen  int      `mapstructure:"prefix_len"`  // Subnet mask (default 24)
	DNSServers []string `mapstructure:"dns_servers"` // DNS servers for VM DHCP (auto-detected from host; fallback: 8.8.8.8, 1.1.1.1)
	Region     string   `mapstructure:"region"`      // Scope to region (optional — empty means any region)
	AZ         string   `mapstructure:"az"`          // Scope to AZ (optional — empty means any AZ in region)
	// GwLrpRangeStart/End reserve a sub-range for OVN gateway LRP IPs in centralized NAT mode.
	// Must NOT overlap [RangeStart, RangeEnd] — link-local 169.254/16 is rejected by upstream routers.
	GwLrpRangeStart string `mapstructure:"gw_lrp_range_start"`
	GwLrpRangeEnd   string `mapstructure:"gw_lrp_range_end"`
}

// NetworkConfig holds cluster-wide external network settings.
type NetworkConfig struct {
	ExternalMode  string         `mapstructure:"external_mode"`  // "pool" or "" (disabled)
	ExternalPools []ExternalPool `mapstructure:"external_pools"` // One or more IP pools
	// IPSecEnabled toggles OVN native IPsec (AES-256-GCM) on every node. Default true; disable only for trusted lab topologies.
	IPSecEnabled bool `mapstructure:"ipsec_enabled"`
	// NATExemptCIDRs are extra destinations that skip routed-mode SNAT (added
	// to the transit /24 in the spinifex_nat_exempt set). nat mode only.
	NATExemptCIDRs []string `mapstructure:"nat_exempt_cidrs"`
}

// BootstrapConfig holds the default VPC infrastructure IDs written by admin init.
// vpcd reads this on startup to ensure OVN topology exists for the bootstrap VPC.
type BootstrapConfig struct {
	AccountID  string `mapstructure:"account_id"`
	VpcId      string `mapstructure:"vpc_id"`
	SubnetId   string `mapstructure:"subnet_id"`
	IgwId      string `mapstructure:"igw_id"`
	Cidr       string `mapstructure:"cidr"`
	SubnetCidr string `mapstructure:"subnet_cidr"`
}

// Config holds all configuration for the application
type Config struct {
	// Node config
	Node string `json:"Node" mapstructure:"node"`
	Host string `json:"Host" mapstructure:"host"` // Unique hostname or IP of this node
	// AdvertiseIP is the off-host dial target. Empty falls back to Host for backward compat.
	AdvertiseIP string   `json:"AdvertiseIP" mapstructure:"advertise"`
	Region      string   `json:"Region" mapstructure:"region"`
	AZ          string   `json:"AZ" mapstructure:"az"`
	DataDir     string   `json:"DataDir" mapstructure:"data_dir"`
	Services    []string `json:"Services" mapstructure:"services"` // Which services this node runs locally

	Daemon     DaemonConfig     `json:"Daemon" mapstructure:"daemon"`
	NATS       NATSConfig       `json:"NATS" mapstructure:"nats"`
	Predastore PredastoreConfig `json:"Predastore" mapstructure:"predastore"`
	Viperblock ViperblockConfig `json:"Viperblock" mapstructure:"viperblock"`
	AWSGW      AWSGWConfig      `json:"AWSGW" mapstructure:"awsgw"`
	VPCD       VPCDConfig       `json:"VPCD" mapstructure:"vpcd"`
	Northstar  NorthstarConfig  `json:"Northstar" mapstructure:"northstar"`

	BaseDir string `json:"BaseDir" mapstructure:"base_dir"`
	WalDir  string `json:"WalDir" mapstructure:"wal_dir"`
}

type AWSGWConfig struct {
	Host    string `json:"Host" mapstructure:"host"`
	TLSKey  string `json:"TLSKey" mapstructure:"tlskey"`
	TLSCert string `json:"TLSCert" mapstructure:"tlscert"`
	Config  string `json:"Config" mapstructure:"config"`

	Debug         bool `json:"Debug" mapstructure:"debug"`
	ExpectedNodes int  `json:"ExpectedNodes" mapstructure:"expected_nodes"` // TODO: Replace with root cluster config
}

type ViperblockConfig struct {
	ShardWAL *bool `json:"ShardWAL" mapstructure:"shardwal"` // Enable sharded WAL (default false when nil)

	// EncryptionKeyFile is the path to the shared 32-byte AES-256 master key for viperblock at-rest encryption.
	// Empty means cleartext. When set, all VB instances must load it via masterkey.LoadShared.
	EncryptionKeyFile string `json:"EncryptionKeyFile" mapstructure:"encryption_key_file"`
}

// VPCDConfig holds the VPC daemon (vpcd) configuration.
type VPCDConfig struct {
	OVNNBAddr         string `json:"OVNNBAddr" mapstructure:"ovn_nb_addr"`                // OVN Northbound DB address; comma-separated list for a RAFT cluster (e.g., "tcp:127.0.0.1:6641" or "tcp:ip1:6641,tcp:ip2:6641,tcp:ip3:6641")
	OVNSBAddr         string `json:"OVNSBAddr" mapstructure:"ovn_sb_addr"`                // OVN Southbound DB address; comma-separated list for a RAFT cluster (e.g., "tcp:127.0.0.1:6642" or "tcp:ip1:6642,tcp:ip2:6642,tcp:ip3:6642")
	ExternalInterface string `json:"ExternalInterface" mapstructure:"external_interface"` // WAN NIC name (e.g., "eth1", "enp0s3") — the physical NIC on the WAN bridge
	BridgeMode        string `json:"BridgeMode" mapstructure:"bridge_mode"`               // "direct" or "veth" (auto-detected if empty)
}

// NorthstarConfig holds the per-node northstar DNS service configuration.
type NorthstarConfig struct {
	// ConfigPath is the path to northstar.toml written by `spx admin init`.
	ConfigPath string `json:"ConfigPath" mapstructure:"config_path"`
	// DefaultDomain and InternalDomain mirror the northstar zone domains as
	// non-secret values so producers (daemon, vpcd) can resolve DNS names
	// without reading the credential-bearing northstar.toml.
	DefaultDomain  string `json:"DefaultDomain" mapstructure:"default_domain"`
	InternalDomain string `json:"InternalDomain" mapstructure:"internal_domain"`
}

// ParseEndpoints splits a comma-separated OVSDB endpoint list (NB/SB RAFT
// cluster) into individual endpoints, trimming whitespace and dropping empties.
// A single endpoint yields a one-element slice; empty input yields nil. Both the
// libovsdb NB client (one WithEndpoint each) and ovn-sbctl --db= (which also
// accepts the raw comma list) consume the cluster form.
func ParseEndpoints(addr string) []string {
	var out []string
	for p := range strings.SplitSeq(addr, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

type PredastoreConfig struct {
	Host      string `json:"Host" mapstructure:"host"`
	Bucket    string `json:"Bucket" mapstructure:"bucket"`
	Region    string `json:"Region" mapstructure:"region"`
	AccessKey string `json:"AccessKey" mapstructure:"accesskey"`
	SecretKey string `json:"SecretKey" mapstructure:"secretkey"`
	BaseDir   string `json:"BaseDir" mapstructure:"base_dir"`
	NodeID    int    `json:"NodeID" mapstructure:"node_id"`
}

// GPUModelOverride maps a PCI vendor/device ID to a GPU instance family for
// dev or test nodes that carry consumer GPUs not in the production model list.
// Add entries under [[nodes.<node>.daemon.gpu_model_overrides]] in spinifex.toml.
type GPUModelOverride struct {
	VendorID     string `json:"VendorID" mapstructure:"vendor_id"`
	DeviceID     string `json:"DeviceID" mapstructure:"device_id"`
	Family       string `json:"Family" mapstructure:"family"`
	Manufacturer string `json:"Manufacturer" mapstructure:"manufacturer"`
	Name         string `json:"Name" mapstructure:"name"`
	MemoryMiB    int64  `json:"MemoryMiB" mapstructure:"memory_mib"`
	// XVGAOff forces x-vga=off in QEMU passthrough, overriding the per-GPU default.
	XVGAOff bool `json:"XVGAOff" mapstructure:"xvga_off"`
	// MIGProfile overrides the daemon-level MIGProfile for this GPU (same format, e.g. "1g.10gb").
	MIGProfile string `json:"MIGProfile" mapstructure:"mig_profile"`
}

// DaemonConfig holds the daemon configuration
type DaemonConfig struct {
	Host              string             `json:"Host" mapstructure:"host"`
	TLSKey            string             `json:"TLSKey" mapstructure:"tlskey"`
	TLSCert           string             `json:"TLSCert" mapstructure:"tlscert"`
	DevNetworking     bool               `json:"DevNetworking" mapstructure:"dev_networking"`          // VPC instances get both TAP + hostfwd for SSH dev access
	MgmtBridge        string             `json:"MgmtBridge" mapstructure:"mgmt_bridge"`                // Linux bridge for system instance control plane (default "br-mgmt")
	GPUPassthrough    bool               `json:"GPUPassthrough" mapstructure:"gpu_passthrough"`        // Enable VFIO GPU passthrough for g5.* instance types
	GPUModelOverrides []GPUModelOverride `json:"GPUModelOverrides" mapstructure:"gpu_model_overrides"` // Dev/test GPU mappings not in the production model list
	// MIGProfile enables NVIDIA MIG on all eligible GPUs (e.g. "1g.10gb"); empty disables. Per-GPU override via GPUModelOverrides[].MIGProfile.
	MIGProfile string `json:"MIGProfile" mapstructure:"mig_profile"`
}

// NATSConfig holds the NATS configuration
type NATSConfig struct {
	Host   string  `json:"Host" mapstructure:"host"`
	CACert string  `json:"CACert" mapstructure:"cacert"`
	ACL    NATSACL `json:"ACL" mapstructure:"acl"`
	Sub    NATSSub `json:"Sub" mapstructure:"sub"`
}

// NATSACL holds the NATS ACL configuration
type NATSACL struct {
	Token string `json:"Token" mapstructure:"token"`
}

// NATSSub holds the NATS subscription configuration
type NATSSub struct {
	Subject string `json:"Subject" mapstructure:"subject"`
}

// NodeBaseDir returns the BaseDir for the current node, or "" if config is nil, node is unset, or not found.
func (cc *ClusterConfig) NodeBaseDir() string {
	if cc == nil || cc.Node == "" {
		slog.Warn("NodeBaseDir: no config or node name set, using global PID path")
		return ""
	}
	node, ok := cc.Nodes[cc.Node]
	if !ok {
		slog.Error("NodeBaseDir: node not found in config", "node", cc.Node)
		return ""
	}
	if node.BaseDir == "" {
		slog.Warn("NodeBaseDir: BaseDir is empty for node, using global PID path", "node", cc.Node)
	}
	return node.BaseDir
}

// AllServices is the default service list when Services is empty (backward compat).
var AllServices = []string{"nats", "predastore", "viperblock", "daemon", "awsgw", "vpcd", "ui"}

// HasService reports whether the node runs the named service (empty list means all services).
func (c Config) HasService(name string) bool {
	services := c.Services
	if len(services) == 0 {
		services = AllServices
	}
	return slices.Contains(services, name)
}

// GetServices returns the configured service list, defaulting to AllServices.
func (c Config) GetServices() []string {
	if len(c.Services) == 0 {
		return AllServices
	}
	return c.Services
}

// LoadConfig loads the configuration from file and environment variables
func LoadConfig(configPath string) (*ClusterConfig, error) {
	// Set environment variable prefix
	viper.SetEnvPrefix("SPINIFEX")
	viper.AutomaticEnv()

	// Default ipsec_enabled to true; operators must explicitly set false to disable.
	viper.SetDefault("network.ipsec_enabled", true)

	// Cluster-wide AWS-parity defaults so existing deployments keep working.
	viper.SetDefault("aws.region", DefaultAWSRegion)
	viper.SetDefault("aws.internal_suffix", DefaultAWSInternalSuffix)

	// Try to load config file if it exists
	if configPath != "" {
		// Check if file exists
		if _, err := os.Stat(configPath); err == nil {
			viper.SetConfigFile(configPath)
			viper.SetConfigType("toml")

			if err := viper.ReadInConfig(); err != nil {
				return nil, fmt.Errorf("error reading config file: %w", err)
			}
			//fmt.Fprintf(os.Stderr, "Using config file: %s\n", viper.ConfigFileUsed())
		} else {
			fmt.Fprintf(os.Stderr, "Config file not found: %s, using environment variables and defaults\n", configPath)
		}
	}

	// Create config struct
	var config ClusterConfig
	if err := viper.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	// Backfill AWS-parity defaults when keys are unset so callers never see empties.
	if config.AWS.Region == "" {
		config.AWS.Region = DefaultAWSRegion
	}
	if config.AWS.InternalSuffix == "" {
		config.AWS.InternalSuffix = DefaultAWSInternalSuffix
	}

	// Rewrite 0.0.0.0 in Predastore.Host to 127.0.0.1 for the local node only (not a valid connect address).
	if local, ok := config.Nodes[config.Node]; ok {
		if strings.HasPrefix(local.Predastore.Host, "0.0.0.0") {
			local.Predastore.Host = strings.Replace(local.Predastore.Host, "0.0.0.0", "127.0.0.1", 1)
			config.Nodes[config.Node] = local
		}
	}

	if err := validateClusterConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// validateClusterConfig rejects legacy DHCP config keys and validates external pool ranges.
func validateClusterConfig(cc *ClusterConfig) error {
	if viper.IsSet("network.external_dhcp") {
		return fmt.Errorf("config: [network] external_dhcp is no longer supported; remove the key (static WAN-pool allocation only)")
	}
	for nodeName := range cc.Nodes {
		if viper.IsSet("nodes." + nodeName + ".vpcd.dhcp_bind_bridge") {
			return fmt.Errorf("config: [nodes.%s.vpcd] dhcp_bind_bridge is no longer supported; remove the key (vpcd no longer runs a DHCP client)", nodeName)
		}
	}

	if len(cc.Network.NATExemptCIDRs) > 0 && cc.Network.ExternalMode != "nat" {
		return fmt.Errorf("config: [network] nat_exempt_cidrs requires external_mode = \"nat\"")
	}
	for _, c := range cc.Network.NATExemptCIDRs {
		if _, err := netip.ParsePrefix(c); err != nil {
			return fmt.Errorf("config: [network] nat_exempt_cidrs entry %q: %w", c, err)
		}
	}

	type poolRange struct {
		name  string
		start netip.Addr
		end   netip.Addr
	}
	var ranges []poolRange
	for _, p := range cc.Network.ExternalPools {
		switch p.DHCPMAC {
		case "", "derived", "interface":
		default:
			return fmt.Errorf("config: [[network.external_pools]] %q: dhcp_mac=%q unsupported; use \"derived\" or \"interface\"", p.Name, p.DHCPMAC)
		}
		switch p.Source {
		case "", "static":
			if p.BindBridge != "" {
				return fmt.Errorf("config: [[network.external_pools]] %q: bind_bridge is only valid with source=\"dhcp\"", p.Name)
			}
			if p.DHCPMAC != "" {
				return fmt.Errorf("config: [[network.external_pools]] %q: dhcp_mac is only valid with source=\"dhcp\"", p.Name)
			}
		case "dhcp":
			if p.BindBridge == "" {
				return fmt.Errorf("config: [[network.external_pools]] %q: source=\"dhcp\" requires bind_bridge (Linux bridge for DHCP DORA)", p.Name)
			}
			if p.RangeStart != "" || p.RangeEnd != "" {
				return fmt.Errorf("config: [[network.external_pools]] %q: range_start/range_end not allowed with source=\"dhcp\" (addresses come from upstream)", p.Name)
			}
			if p.GwLrpRangeStart != "" || p.GwLrpRangeEnd != "" {
				return fmt.Errorf("config: [[network.external_pools]] %q: gw_lrp_range_start/gw_lrp_range_end not allowed with source=\"dhcp\" (gateway LRP IP is DORA'd per VPC)", p.Name)
			}
			continue
		default:
			return fmt.Errorf("config: [[network.external_pools]] %q: source=%q unsupported; use \"static\" or \"dhcp\"", p.Name, p.Source)
		}
		if p.RangeStart == "" || p.RangeEnd == "" {
			continue
		}
		start, err := netip.ParseAddr(p.RangeStart)
		if err != nil {
			return fmt.Errorf("config: pool %q range_start %q: %w", p.Name, p.RangeStart, err)
		}
		end, err := netip.ParseAddr(p.RangeEnd)
		if err != nil {
			return fmt.Errorf("config: pool %q range_end %q: %w", p.Name, p.RangeEnd, err)
		}
		if start.Compare(end) > 0 {
			return fmt.Errorf("config: pool %q range_start %s > range_end %s", p.Name, start, end)
		}
		if p.Gateway != "" && p.PrefixLen > 0 {
			gw, err := netip.ParseAddr(p.Gateway)
			if err != nil {
				return fmt.Errorf("config: pool %q gateway %q: %w", p.Name, p.Gateway, err)
			}
			cidr := netip.PrefixFrom(gw, p.PrefixLen).Masked()
			if !cidr.Contains(start) || !cidr.Contains(end) {
				return fmt.Errorf("config: pool %q range [%s, %s] not inside %s", p.Name, start, end, cidr)
			}
		}
		for _, prior := range ranges {
			if start.Compare(prior.end) <= 0 && prior.start.Compare(end) <= 0 {
				return fmt.Errorf("config: pool %q range [%s, %s] overlaps pool %q [%s, %s]", p.Name, start, end, prior.name, prior.start, prior.end)
			}
		}
		ranges = append(ranges, poolRange{name: p.Name, start: start, end: end})
	}
	return nil
}
