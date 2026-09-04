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

// DefaultMgmtBridgeIP is the canonical br-mgmt host address the control plane
// advertises: the EKS gateway URL, predastore endpoint, and node-group userdata
// all target it. Kept in sync with setup-ovn.sh MGMT_CIDR. Server certs must
// always carry this as a SAN so publish succeeds even when br-mgmt happens to be
// down at cert-generation time (interface enumeration would otherwise miss it).
const DefaultMgmtBridgeIP = "10.15.8.1"

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

// DefaultUnderlayMTU is the standard Ethernet payload, and the assumption a
// cluster runs under until an operator raises the fabric and says so.
const DefaultUnderlayMTU = 1500

// NetworkConfig holds cluster-wide external network settings.
type NetworkConfig struct {
	ExternalMode  string         `mapstructure:"external_mode"`  // "pool" or "" (disabled)
	ExternalPools []ExternalPool `mapstructure:"external_pools"` // One or more IP pools
	// IPSecEnabled toggles OVN native IPsec (AES-256-GCM) on every node. Default true; disable only for trusted lab topologies.
	IPSecEnabled bool `mapstructure:"ipsec_enabled"`
	// UnderlayMTU is the MTU the fabric between nodes can actually carry, and the
	// figure the advertised guest MTU is derived from. Cluster-wide because every
	// OVN network rides the same tunnels. Raise it only after verifying the switch
	// end to end; overstating it blackholes large frames silently.
	UnderlayMTU int `mapstructure:"underlay_mtu"`
	// FirewallEnabled toggles the host firewall that scopes cluster ports to cluster
	// members. Three-state on purpose: nil means the install path decides, via the
	// mode file setup.sh writes. An explicit false tears down an existing policy.
	FirewallEnabled *bool `mapstructure:"firewall_enabled"`
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

// Config holds all configuration for the application.
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

	Daemon      DaemonConfig      `json:"Daemon" mapstructure:"daemon"`
	NATS        NATSConfig        `json:"NATS" mapstructure:"nats"`
	Predastore  PredastoreConfig  `json:"Predastore" mapstructure:"predastore"`
	Viperblock  ViperblockConfig  `json:"Viperblock" mapstructure:"viperblock"`
	EBS         EBSConfig         `json:"EBS" mapstructure:"ebs"`
	AWSGW       AWSGWConfig       `json:"AWSGW" mapstructure:"awsgw"`
	VPCD        VPCDConfig        `json:"VPCD" mapstructure:"vpcd"`
	Northstar   NorthstarConfig   `json:"Northstar" mapstructure:"northstar"`
	RDS         RDSConfig         `json:"RDS" mapstructure:"rds"`
	ACM         ACMConfig         `json:"ACM" mapstructure:"acm"`
	Bedrock     BedrockConfig     `json:"Bedrock" mapstructure:"bedrock"`
	OchreVector OchreVectorConfig `json:"OchreVector" mapstructure:"ochre_vector"`

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

	// DescribeSource selects what DescribeInstances answers from: "fanout"
	// (default, today's node fan-out plus KV buckets), "shadow" (fan-out
	// serves, cache answers alongside for comparison), or "cache" (the
	// instance cache serves directly). Read once at startup like every other
	// gateway setting — there is no reload path. An empty or unrecognised
	// value resolves to "fanout" rather than failing the gateway to start.
	DescribeSource string `json:"DescribeSource" mapstructure:"describe_source"`
}

type ViperblockConfig struct {
	ShardWAL *bool `json:"ShardWAL" mapstructure:"shardwal"` // Enable sharded WAL (default false when nil)

	// EncryptionKeyFile is the path to the shared 32-byte AES-256 master key for viperblock at-rest encryption.
	// Empty means cleartext. When set, all VB instances must load it via masterkey.LoadShared.
	EncryptionKeyFile string `json:"EncryptionKeyFile" mapstructure:"encryption_key_file"`

	// GCEnabled turns on viperblock chunk garbage collection (mark-and-sweep,
	// snapshot-ancestry gated) on every VB this node constructs: the nbdkit
	// plugin backing a mounted volume, and the short-lived VBs used by the
	// volume service. Default false when nil so existing deployments keep
	// today's behavior until explicitly opted in.
	GCEnabled *bool `json:"GCEnabled" mapstructure:"gc_enabled"`
}

// EBS provider selectors. Viperblockd routes EBS calls to the viperblockd
// daemon and qemunbd to the qemunbdd daemon, both over the versioned
// ebs.provider.v1.* NATS contract. Embedded named the removed in-process
// engine and survives solely so a config that still selects it can be
// rejected by name.
const (
	EBSProviderEmbedded    = "embedded"
	EBSProviderViperblockd = "viperblockd"
	EBSProviderQEMUNBD     = "qemunbd"
)

// Per-volume export tunables. Both are applied per nbdkit process, and
// spinifex runs one of those per volume, so the host-level cost of each is
// the value multiplied by the volume count.
const (
	// DefaultNBDKitThreads matches nbdkit's own default for a
	// thread_model=parallel plugin. Passing it explicitly rather than
	// omitting -t keeps the effective value visible in the process argv.
	DefaultNBDKitThreads = 16

	// MaxNBDKitThreads is a sanity ceiling, not a tuned limit.
	MaxNBDKitThreads = 256

	// DefaultCacheSizeMB is the per-volume plaintext read cache.
	DefaultCacheSizeMB = 128
)

// EBSConfig selects which provider backs EBS and carries the per-volume
// export tunables. Not nested under ViperblockConfig: it names the provider
// boundary, not one provider's settings, so a second provider never needs a
// rename.
type EBSConfig struct {
	// Provider is "viperblockd" or "qemunbd" and may be left unset. Volumes
	// are persisted in ebsmetadata.
	Provider string `json:"Provider" mapstructure:"provider"`

	// DefaultThreads is nbdkit's -t: worker threads per NBD connection, which
	// bounds how many requests are in flight inside viperblock for one volume.
	// QEMU opens a single connection per volume, so this is the whole
	// per-volume concurrency ceiling. 0 uses DefaultNBDKitThreads.
	DefaultThreads int `json:"DefaultThreads" mapstructure:"default_threads"`

	// CacheSizeMB is the per-volume plaintext read cache in MiB. A pointer so
	// an explicit 0 (cache disabled) stays distinguishable from an unset key
	// (DefaultCacheSizeMB), the same way ViperblockConfig treats its toggles.
	// Auxiliary -efi volumes are always uncached regardless of this value.
	CacheSizeMB *int `json:"CacheSizeMB" mapstructure:"cache_size_mb"`
}

// ResolvedProvider normalizes an empty Provider to EBSProviderViperblockd,
// the default provider.
func (c EBSConfig) ResolvedProvider() string {
	if c.Provider == "" {
		return EBSProviderViperblockd
	}
	return c.Provider
}

// ResolvedThreads normalizes an unset DefaultThreads to DefaultNBDKitThreads.
func (c EBSConfig) ResolvedThreads() int {
	if c.DefaultThreads == 0 {
		return DefaultNBDKitThreads
	}
	return c.DefaultThreads
}

// ResolvedCacheSizeMB normalizes an unset CacheSizeMB to DefaultCacheSizeMB.
// An explicit 0 is honoured and disables the cache.
func (c EBSConfig) ResolvedCacheSizeMB() int {
	if c.CacheSizeMB == nil {
		return DefaultCacheSizeMB
	}
	return *c.CacheSizeMB
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

// Every DB VM's primary NIC lives in the shared system VPC, which gives the
// in-guest agent management egress while the customer ENI stays ingress-only.
type RDSConfig struct {
	// The IPv4 /14 the system VPC's /22 is carved from. It must not overlap the
	// EKS control-plane supernet or any customer VPC CIDR.
	SystemVPCSupernet string `json:"SystemVPCSupernet" mapstructure:"system_vpc_supernet"`

	// Clamped to 1..3. Zero defaults to one, which is all a single-AZ platform
	// can place across.
	SystemVPCPrivateSubnets int `json:"SystemVPCPrivateSubnets" mapstructure:"system_vpc_private_subnets"`

	// How long a creating DB instance may go without a healthy agent heartbeat
	// before the reconciler marks it failed. Zero takes the built-in default,
	// which covers a cold boot plus initdb on the smallest instance class.
	BootstrapTimeoutSeconds int `json:"BootstrapTimeoutSeconds" mapstructure:"bootstrap_timeout_seconds"`

	// How long an available DB instance may be observed with its VM down and its
	// agent silent before the reconciler reports it failed. Zero takes the
	// built-in default of one heartbeat interval, which requires two reconciler
	// passes to agree; raise it to give EC2's own VM auto-restart more room
	// before a customer sees the instance reported as failed.
	FailureGraceSeconds int `json:"FailureGraceSeconds" mapstructure:"failure_grace_seconds"`

	// The upper bound on a DB instance's BackupRetentionPeriod, and what a create
	// that names none gets. Zero takes the built-in 7 for both. Retention length
	// does not change the physical footprint of a backed-up volume — any snapshot
	// latches viperblock chunk GC off for the life of the volume — so a short
	// retention buys nothing but a smaller restore surface.
	BackupRetentionCapDays int `json:"BackupRetentionCapDays" mapstructure:"backup_retention_cap_days"`
	BackupRetentionDays    int `json:"BackupRetentionDays" mapstructure:"backup_retention_days"`

	// The daily UTC blocks an unnamed backup or maintenance window is assigned
	// inside, as hh24:mi-hh24:mi. They must not overlap: the windows derived from
	// them must not either. Empty takes the built-in 03:00-11:00 and 11:00-19:00.
	BackupWindowBlock      string `json:"BackupWindowBlock" mapstructure:"backup_window_block"`
	MaintenanceWindowBlock string `json:"MaintenanceWindowBlock" mapstructure:"maintenance_window_block"`

	// How many automated snapshots one retention pass may delete. Zero takes the
	// built-in bound; a pass that under-collects is corrected two minutes later.
	BackupSweepDeleteLimit int `json:"BackupSweepDeleteLimit" mapstructure:"backup_sweep_delete_limit"`
}

// RDSDefaultSystemVPCSupernet anchors the RDS system VPC address space at
// 10.248.0.0/14, immediately below and disjoint from the EKS control-plane
// supernet, so a name-hash collision can never place an RDS subnet in EKS space.
const RDSDefaultSystemVPCSupernet = "10.248.0.0/14"

// Every self-hosted vLLM serving VM's primary NIC lives in the shared Bedrock
// system VPC, mirroring RDS's DB-VM VPC: one shared VPC per region rather
// than one per endpoint, since a serving VM has no customer ENI to isolate.
type BedrockConfig struct {
	// The IPv4 /14 the system VPC's /22 is carved from. It must not overlap the
	// RDS or EKS control-plane supernets or any customer VPC CIDR.
	SystemVPCSupernet string `json:"SystemVPCSupernet" mapstructure:"system_vpc_supernet"`

	// Clamped to 1..3. Zero defaults to one, which is all a single-AZ platform
	// can place across.
	SystemVPCPrivateSubnets int `json:"SystemVPCPrivateSubnets" mapstructure:"system_vpc_private_subnets"`
}

// BedrockDefaultSystemVPCSupernet anchors the Bedrock system VPC address
// space at 10.244.0.0/14, immediately below and disjoint from both the RDS
// (10.248.0.0/14) and EKS control-plane (10.252.0.0/14) supernets.
const BedrockDefaultSystemVPCSupernet = "10.244.0.0/14"

// OchreVectorConfig gates the Ochre vector store's platform Postgres
// appliance and VectorService NATS surface. Disabled by default: an unset
// section constructs nothing and registers no ochre.vector.* subjects, so an
// existing deployment is unaffected until an operator opts in.
type OchreVectorConfig struct {
	Enabled bool `json:"Enabled" mapstructure:"enabled"`
	// EmbeddingModel documents this deployment's served embedding model id;
	// each index still pins its own model id at CreateIndex time. Any
	// self-host model id resolves via handlers_bedrock.DynamicEndpointResolver.
	EmbeddingModel string `json:"EmbeddingModel" mapstructure:"embedding_model"`
	// RerankModel is the served cross-encoder rerank model id, resolved the
	// same way EmbeddingModel is. Empty disables reranking entirely: Query
	// falls back to plain KNN top-k rather than resolving a default.
	RerankModel string `json:"RerankModel" mapstructure:"rerank_model"`
	// PostgresImage names the appliance's Postgres image. Currently
	// informational only: RDS's CreateDBInstanceInput has no image-selection
	// field, so this is not yet threaded into the appliance launch.
	PostgresImage string `json:"PostgresImage" mapstructure:"postgres_image"`
}

// ACMConfig holds the operator-level ACM certificate-issuance configuration.
// Deliberately small: four keys, nothing deployment-specific and nothing
// derivable. In particular there is no allowed-domains list (public modes are
// authorized by the ACME CA's own validation, PRIVATE_CA by the CA's own name
// constraints), no default validation mode (derived from DNSProvider and
// whether northstar hosts the zone, never configured), no renewal thresholds
// (proportional, hence constants) and no private CA paths (folded into the
// existing CA path handling in admin.ConfigSettings).
type ACMConfig struct {
	Enabled      bool   `json:"Enabled" mapstructure:"enabled"`
	DirectoryURL string `json:"DirectoryURL" mapstructure:"directory_url"`
	ContactEmail string `json:"ContactEmail" mapstructure:"contact_email"`
	// DNSProvider is the lego DNS provider id used for PROVIDER_API DNS-01
	// challenges. Its credentials come from environment variables per lego's
	// own convention, never from this file — a provider token in a
	// configuration file is a token in every backup. A non-empty value here is
	// what selects PROVIDER_API at RequestCertificate time.
	DNSProvider string `json:"DNSProvider" mapstructure:"dns_provider"`
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
	// HostID is which [[host]] of the predastore topology this node runs;
	// unset means the node runs the whole topology in one process.
	HostID int `json:"HostID" mapstructure:"host_id"`
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

// DaemonConfig holds the daemon configuration.
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

// NATSConfig holds the NATS configuration.
type NATSConfig struct {
	Host   string  `json:"Host" mapstructure:"host"`
	CACert string  `json:"CACert" mapstructure:"cacert"`
	ACL    NATSACL `json:"ACL" mapstructure:"acl"`
	Sub    NATSSub `json:"Sub" mapstructure:"sub"`
}

// NATSACL holds the NATS ACL configuration.
type NATSACL struct {
	Token string `json:"Token" mapstructure:"token"`
}

// NATSSub holds the NATS subscription configuration.
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

// LoadConfig loads the configuration from file and environment variables.
func LoadConfig(configPath string) (*ClusterConfig, error) {
	// Set environment variable prefix
	viper.SetEnvPrefix("SPINIFEX")
	viper.AutomaticEnv()

	// Default ipsec_enabled to true; operators must explicitly set false to disable.
	viper.SetDefault("network.ipsec_enabled", true)
	viper.SetDefault("network.underlay_mtu", DefaultUnderlayMTU)

	// No default for network.firewall_enabled on purpose. Setting one here would
	// make the key always present and hide the difference between "unset" and
	// "explicitly false"; the install path's mode file resolves the unset case.

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
	for nodeName, nodeCfg := range cc.Nodes {
		if viper.IsSet("nodes." + nodeName + ".vpcd.dhcp_bind_bridge") {
			return fmt.Errorf("config: [nodes.%s.vpcd] dhcp_bind_bridge is no longer supported; remove the key (vpcd no longer runs a DHCP client)", nodeName)
		}
		switch nodeCfg.EBS.Provider {
		case "", EBSProviderViperblockd, EBSProviderQEMUNBD:
		case EBSProviderEmbedded:
			// Refuse rather than silently upgrade: the embedded engine is gone,
			// and starting anyway would migrate this node's volumes to
			// ebsmetadata one-way without the operator ever asking for it.
			return fmt.Errorf("config: [nodes.%s.ebs] provider=%q has been removed; set provider = %q or remove the key (this is a one-way switch: volumes move to ebsmetadata)", nodeName, EBSProviderEmbedded, EBSProviderViperblockd)
		default:
			return fmt.Errorf("config: [nodes.%s.ebs] provider=%q unsupported; use %q or %q, or remove the key", nodeName, nodeCfg.EBS.Provider, EBSProviderViperblockd, EBSProviderQEMUNBD)
		}
		// Range checks only. Both settings are deliberately unbounded above by
		// anything host-aware: they are operator tunables, and the host cost is
		// the value times the volume count for the operator to weigh.
		if t := nodeCfg.EBS.DefaultThreads; t < 0 || t > MaxNBDKitThreads {
			return fmt.Errorf("config: [nodes.%s.ebs] default_threads=%d out of range; use 1-%d, or remove the key for the default of %d", nodeName, t, MaxNBDKitThreads, DefaultNBDKitThreads)
		}
		if c := nodeCfg.EBS.CacheSizeMB; c != nil && *c < 0 {
			return fmt.Errorf("config: [nodes.%s.ebs] cache_size_mb=%d must not be negative; use 0 to disable the cache, or remove the key for the default of %d", nodeName, *c, DefaultCacheSizeMB)
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
