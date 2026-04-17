package vpcd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// sudoCommand wraps exec.Command with sudo when running as non-root.
// OVS/OVN commands require elevated privileges; when running as root
// (Docker, production) no wrapper is needed.
func sudoCommand(name string, args ...string) *exec.Cmd {
	if os.Getuid() == 0 {
		return exec.Command(name, args...)
	}
	return exec.Command("sudo", append([]string{name}, args...)...)
}

var serviceName = "vpcd"

// BootstrapVPC holds the default VPC infrastructure IDs from spinifex.toml.
// vpcd uses this to ensure OVN topology exists on first boot (before NATS KV
// has any data) and on subsequent boots where OVN state may have been lost.
type BootstrapVPC struct {
	AccountID  string
	VpcId      string
	SubnetId   string
	IgwId      string
	Cidr       string
	SubnetCidr string
}

// Config holds the vpcd service configuration.
type Config struct {
	// NatsHost is the NATS server address (host:port).
	NatsHost string
	// NatsToken is the NATS authentication token.
	NatsToken string
	// NatsCACert is the path to the CA certificate for NATS TLS.
	NatsCACert string
	// OVNNBAddr is the OVN Northbound DB address (e.g., "tcp:127.0.0.1:6641").
	OVNNBAddr string
	// OVNSBAddr is the OVN Southbound DB address (e.g., "tcp:127.0.0.1:6642"), used for monitoring.
	OVNSBAddr string
	// BaseDir is the base directory for PID files and state.
	BaseDir string
	// Debug enables debug logging.
	Debug bool
	// ExternalMode is "pool" or "" (disabled).
	ExternalMode string
	// ExternalPools holds the cluster-wide external IP pool configs.
	ExternalPools []ExternalPoolConfig
	// ChassisNames are fallback OVN chassis identifiers for gateway HA scheduling.
	// Normally discovered from the OVN Southbound DB at startup. Only used if
	// SBDB discovery fails.
	ChassisNames []string
	// Bootstrap holds the default VPC config from spinifex.toml for first-boot reconciliation.
	Bootstrap *BootstrapVPC
	// ExternalInterface is the WAN NIC name (e.g., "enp0s3"). Used to align
	// the macvlan MAC with the OVN gateway MAC for inbound traffic.
	ExternalInterface string
	// WanBridge is the OVS bridge name for WAN traffic.
	// Maps to OVN logical network "external" via ovn-bridge-mappings.
	// Typically "br-ext" (veth mode linking Linux bridge to OVS) or the
	// bridge name itself when the default route is already on an OVS bridge.
	WanBridge string
	// BridgeMode is "direct", "macvlan", or "veth". Direct bridge adds the WAN
	// NIC directly to the OVS bridge; macvlan creates a sub-interface; veth uses
	// a veth pair to link a Linux bridge to OVS. When empty, auto-detected at
	// startup.
	BridgeMode string
}

// ExternalPoolConfig mirrors config.ExternalPool for vpcd's internal use.
type ExternalPoolConfig struct {
	Name       string
	RangeStart string
	RangeEnd   string
	Gateway    string
	GatewayIP  string
	PrefixLen  int
	DNSServers []string
	Region     string
	AZ         string
}

// Service implements the Spinifex service interface for vpcd.
type Service struct {
	Config *Config
}

// New creates a new vpcd Service.
func New(config any) (*Service, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type for vpcd service")
	}
	return &Service{
		Config: cfg,
	}, nil
}

// Start starts the vpcd service.
func (svc *Service) Start() (int, error) {
	if err := utils.WritePidFileTo(svc.Config.BaseDir, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}

	err := launchService(svc.Config)
	if err != nil {
		slog.Error("Failed to launch vpcd service", "err", err)
		return 0, err
	}

	return os.Getpid(), nil
}

// Stop stops the vpcd service.
func (svc *Service) Stop() error {
	return utils.StopProcessAt(svc.Config.BaseDir, serviceName)
}

// Status returns the vpcd service status.
func (svc *Service) Status() (string, error) {
	return utils.ServiceStatus(svc.Config.BaseDir, serviceName)
}

// Shutdown gracefully shuts down the vpcd service.
func (svc *Service) Shutdown() error {
	return svc.Stop()
}

// Reload reloads the vpcd service configuration.
func (svc *Service) Reload() error {
	return nil
}

// checkBrInt verifies the OVS integration bridge (br-int) exists.
// This is the bridge that all VM TAP devices connect to.
var checkBrInt = func() error {
	cmd := sudoCommand("ovs-vsctl", "br-exists", "br-int")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("br-int does not exist (%w): run ./scripts/setup-ovn.sh --management", err)
	}
	return nil
}

// checkOVNController verifies that ovn-controller is running on this host.
// OVN 22.03+ moved the control socket from /var/run/openvswitch/ to /var/run/ovn/,
// so the short-form "ovs-appctl -t ovn-controller" fails on newer packages.
// We try the legacy path first, then the new path, then fall back to systemctl.
var checkOVNController = func() error {
	// Legacy path: works when socket is in /var/run/openvswitch/
	if sudoCommand("ovs-appctl", "-t", "ovn-controller", "version").Run() == nil {
		return nil
	}

	// OVN 22.03+: socket moved to /var/run/ovn/
	if matches, _ := filepath.Glob("/var/run/ovn/ovn-controller.*.ctl"); len(matches) > 0 {
		if sudoCommand("ovs-appctl", "-t", matches[0], "version").Run() == nil {
			return nil
		}
	}

	// Fallback: check if the service is active via systemctl
	if sudoCommand("systemctl", "is-active", "--quiet", "ovn-controller").Run() == nil {
		return nil
	}

	return fmt.Errorf("ovn-controller is not running: run ./scripts/setup-ovn.sh --management")
}

// localSystemID returns the OVS external-ids:system-id, which is the chassis
// name that the local ovn-controller registers in the Southbound DB.
var localSystemID = func() (string, error) {
	out, err := sudoCommand("ovs-vsctl", "get", "open_vswitch", ".", "external-ids:system-id").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ovs-vsctl get system-id: %s: %w", strings.TrimSpace(string(out)), err)
	}
	// ovs-vsctl wraps the value in quotes
	return strings.Trim(strings.TrimSpace(string(out)), "\""), nil
}

// discoverChassis queries the OVN Southbound DB for registered chassis names.
// It cross-references with the local OVS system-id to filter out stale chassis
// entries on this host. When the system-id is changed (e.g. setup-ovn.sh re-run),
// the old Chassis row persists in the SBDB — using it for gateway scheduling
// causes the gateway port to bind to a chassis that no ovn-controller owns.
var discoverChassis = func(sbAddr string) ([]string, error) {
	localID, err := localSystemID()
	if err != nil {
		return nil, fmt.Errorf("discover chassis: %w", err)
	}
	localHostname, _ := os.Hostname()

	args := []string{"--no-leader-only"}
	if sbAddr != "" {
		args = append(args, "--db="+sbAddr)
	}
	// OVN 25.03+ removed the "list-chassis" convenience command.
	// Use "--columns=name,hostname list Chassis" which works on all versions.
	args = append(args, "--bare", "--columns=name,hostname", "list", "Chassis")
	out, err := sudoCommand("ovn-sbctl", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("ovn-sbctl list Chassis: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return parseChassisList(string(out), localID, localHostname), nil
}

// parseChassisList parses ovn-sbctl --bare --columns=name,hostname output and
// filters out stale chassis on the local host. The output format is pairs of
// name/hostname lines separated by blank lines.
func parseChassisList(raw, localID, localHostname string) []string {
	var names []string
	var pair []string
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			// Blank line = row separator; process the accumulated pair
			if len(pair) == 2 {
				names = appendIfActive(names, pair[0], pair[1], localID, localHostname)
			}
			pair = pair[:0]
			continue
		}
		pair = append(pair, line)
	}
	// Handle last row (no trailing blank line)
	if len(pair) == 2 {
		names = appendIfActive(names, pair[0], pair[1], localID, localHostname)
	}
	return names
}

func appendIfActive(names []string, name, hostname, localID, localHostname string) []string {
	if hostname == localHostname && name != localID {
		slog.Info("discoverChassis: skipping stale local chassis", "name", name, "hostname", hostname, "localID", localID)
		return names
	}
	return append(names, name)
}

// preflightOVN runs all OVN preflight checks and returns the first failure.
func preflightOVN() error {
	if err := checkBrInt(); err != nil {
		return fmt.Errorf("OVN preflight failed: %w", err)
	}
	if err := checkOVNController(); err != nil {
		return fmt.Errorf("OVN preflight failed: %w", err)
	}
	return nil
}

func launchService(cfg *Config) error {
	slog.Info("Starting vpcd service",
		"ovn_nb_addr", cfg.OVNNBAddr,
		"nats_host", cfg.NatsHost,
	)

	// OVN preflight: verify br-int and ovn-controller before proceeding
	if err := preflightOVN(); err != nil {
		slog.Error("OVN preflight check failed — vpcd cannot start without OVN", "err", err)
		return err
	}
	slog.Info("OVN preflight passed (br-int exists, ovn-controller running)")

	// Connect to NATS
	nc, err := utils.ConnectNATS(cfg.NatsHost, cfg.NatsToken, cfg.NatsCACert)
	if err != nil {
		slog.Error("Failed to connect to NATS", "err", err)
		return err
	}
	defer nc.Close()

	// Connect to OVN NB DB (required — vpcd is useless without it)
	if cfg.OVNNBAddr == "" {
		return fmt.Errorf("OVN NB DB address not configured (ovn_nb_addr is empty)")
	}

	liveClient := NewLiveOVNClient(cfg.OVNNBAddr)
	ctx := context.Background()
	if err := liveClient.Connect(ctx); err != nil {
		slog.Error("Failed to connect to OVN NB DB", "endpoint", cfg.OVNNBAddr, "err", err)
		return fmt.Errorf("connect OVN NB DB: %w", err)
	}
	defer liveClient.Close()
	slog.Info("Connected to OVN NB DB", "endpoint", cfg.OVNNBAddr)

	// Detect bridge mode: if not explicitly configured, auto-detect by checking
	// whether the WAN bridge has a macvlan port or a physical NIC.
	bridgeMode := cfg.BridgeMode
	if bridgeMode == "" && cfg.ExternalInterface != "" {
		bridgeMode = detectBridgeMode(cfg.ExternalInterface)
	}
	if bridgeMode == "" {
		bridgeMode = BridgeModeMacvlan // default for backward compatibility
	}
	wanBridge := cfg.WanBridge
	if wanBridge == "" {
		wanBridge = "br-wan"
	}
	slog.Info("External bridge mode", "mode", bridgeMode, "wan_bridge", wanBridge)

	// Reconcile OVN topology from bootstrap config before subscribing.
	// This ensures the default VPC topology exists even if admin init ran
	// before services were started (first-install case).
	var topoOpts []TopologyOption
	if cfg.ExternalMode != "" {
		topoOpts = append(topoOpts, WithExternalNetwork(cfg.ExternalMode, cfg.ExternalPools))
		slog.Info("External network enabled", "mode", cfg.ExternalMode, "pools", len(cfg.ExternalPools))
	}
	// Discover chassis from OVN SBDB (source of truth). Fall back to config
	// if SBDB query fails (e.g., SBDB address not configured).
	chassisNames, err := discoverChassis(cfg.OVNSBAddr)
	if err != nil {
		slog.Warn("Failed to discover chassis from OVN SBDB, falling back to config", "err", err)
		chassisNames = cfg.ChassisNames
	}
	if len(chassisNames) > 0 {
		topoOpts = append(topoOpts, WithChassisNames(chassisNames))
		slog.Info("Gateway chassis configured", "source", "sbdb", "chassis", chassisNames)
	}
	topoOpts = append(topoOpts, WithBridgeMode(bridgeMode))
	topo := NewTopologyHandler(liveClient, topoOpts...)

	// Run reconciliation before subscribing
	Reconcile(ctx, topo, cfg.Bootstrap)

	// Macvlan mode only: align macvlan MAC with the OVN gateway router MAC.
	// The macvlan only delivers inbound unicast matching its own MAC. With
	// centralized NAT, OVN uses the router MAC for all external ARP replies.
	// Setting the macvlan MAC to the router MAC ensures inbound traffic reaches OVS.
	// Direct bridge mode doesn't need this — OVS sees all traffic on the wire.
	if bridgeMode == BridgeModeMacvlan && cfg.Bootstrap != nil && cfg.Bootstrap.VpcId != "" && cfg.ExternalInterface != "" {
		gwMAC := generateMAC("gw-" + cfg.Bootstrap.VpcId)
		macvlanName := "spx-ext-" + cfg.ExternalInterface
		if err := setMacvlanMAC(macvlanName, gwMAC); err != nil {
			slog.Warn("vpcd: failed to set macvlan MAC (inbound traffic may not work)", "iface", macvlanName, "mac", gwMAC, "err", err)
		} else {
			slog.Info("vpcd: macvlan MAC aligned with gateway", "iface", macvlanName, "mac", gwMAC)
		}
	}

	subs, err := topo.Subscribe(nc)
	if err != nil {
		slog.Error("Failed to subscribe to VPC topics", "err", err)
		return err
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pass 2: Reconcile from NATS KV (handles reboots, OVN DB loss, missed events).
	// Runs after subscribing so new events are not missed during reconciliation.
	ReconcileFromKV(ctx, nc, topo)

	slog.Info("vpcd service started, waiting for VPC lifecycle events", "subscriptions", len(subs))

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	slog.Info("vpcd service shutting down")
	return nil
}

// detectBridgeMode checks how the WAN bridge is wired:
//   - macvlan: spx-ext-{iface} macvlan sub-interface exists
//   - veth: veth-wan-ovs interface exists (Linux bridge linked to OVS via veth pair)
//   - direct: physical NIC is added directly to the OVS bridge
func detectBridgeMode(externalIface string) string {
	macvlanName := "spx-ext-" + externalIface
	out, err := exec.Command("ip", "-d", "link", "show", macvlanName).CombinedOutput()
	if err == nil && strings.Contains(string(out), "macvlan") {
		slog.Debug("vpcd: detected macvlan interface on WAN bridge", "iface", macvlanName)
		return BridgeModeMacvlan
	}
	// Check for veth pair linking a Linux bridge to OVS (setup-ovn.sh creates
	// veth-wan-br ↔ veth-wan-ovs when the default route is on a Linux bridge).
	if _, vethErr := exec.Command("ip", "link", "show", "veth-wan-ovs").CombinedOutput(); vethErr == nil {
		slog.Debug("vpcd: detected veth pair linking Linux bridge to OVS")
		return BridgeModeVeth
	}
	slog.Debug("vpcd: no macvlan or veth found, assuming direct bridge mode", "checked", macvlanName)
	return BridgeModeDirect
}

// setMacvlanMAC sets the MAC address on a macvlan interface. The interface is
// brought down, MAC changed, and brought back up. Requires sudo/NET_ADMIN.
func setMacvlanMAC(iface, mac string) error {
	cmds := [][]string{
		{"sudo", "ip", "link", "set", iface, "down"},
		{"sudo", "ip", "link", "set", iface, "address", mac},
		{"sudo", "ip", "link", "set", iface, "up"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w (%s)", args[3], err, string(out))
		}
	}
	return nil
}
