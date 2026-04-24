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
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/dhcp"
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
	// DhcpBindBridge is the bridge where the DHCP client binds its AF_PACKET
	// socket. In veth mode this is the Linux bridge that holds the WAN NIC
	// (e.g. "br-wan"); in direct mode this is the OVS bridge holding the
	// WAN NIC. Never the OVN-side "br-ext" — that never sees LAN DHCP.
	DhcpBindBridge string
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
//
// Uses Output() (stdout only) for the same reason as portToBr: vpcd.service's
// AmbientCapabilities trips sudo's PAM into emitting audit warnings on stderr,
// which CombinedOutput would merge into stdout and poison the system-id.
// A corrupted localID causes discoverChassis to skip the live chassis as
// "stale", leaving gateway_chassis pointing at a fallback name that no real
// chassis owns → cr-gw* chassisredirect ports stay unbound → no proxy-ARP
// for EIPs.
var localSystemID = func() (string, error) {
	out, err := sudoCommand("ovs-vsctl", "get", "open_vswitch", ".", "external-ids:system-id").Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("ovs-vsctl get system-id: %s: %w", stderr, err)
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
	// Output() not CombinedOutput(): sudo PAM audit noise on stderr would
	// otherwise be parsed as chassis name/hostname pairs.
	out, err := sudoCommand("ovn-sbctl", args...).Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("ovn-sbctl list Chassis: %s: %w", stderr, err)
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
	nc, err := utils.ConnectNATS(admin.DialTarget(cfg.NatsHost), cfg.NatsToken, cfg.NatsCACert)
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

	bridgeMode, dhcpBindBridge := resolveBridgeConfig(cfg.BridgeMode, cfg.ExternalInterface, cfg.DhcpBindBridge)
	slog.Info("External bridge mode", "mode", bridgeMode, "dhcp_bind_bridge", dhcpBindBridge)
	if err := verifyBridgeMode(bridgeMode, cfg.ExternalInterface, dhcpBindBridge); err != nil {
		slog.Error("vpcd: bridge mode sanity check failed", "err", err)
		return err
	}

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

	// DHCP manager: services vpc.dhcp.acquire/release requests from the
	// daemon-side ExternalIPAM handlers, runs a renewal goroutine per lease,
	// and persists leases in spinifex-dhcp-leases KV. Started unconditionally
	// — pools with source="static" never issue acquire requests, so the
	// Manager sits idle until something actually wants DHCP.
	js, err := nc.JetStream()
	if err != nil {
		slog.Error("Failed to get JetStream context for DHCP manager", "err", err)
		return err
	}
	dhcpManager, err := NewDHCPManager(nc, js, dhcp.NewNClient4(15*time.Second, 3))
	if err != nil {
		slog.Error("Failed to create DHCP manager", "err", err)
		return err
	}
	defer dhcpManager.Close()

	if err := dhcpManager.Bootstrap(ctx); err != nil {
		slog.Error("DHCP manager bootstrap failed", "err", err)
		return err
	}
	dhcpSubs, err := dhcpManager.Subscribe(nc)
	if err != nil {
		slog.Error("DHCP manager subscribe failed", "err", err)
		return err
	}
	defer func() {
		for _, s := range dhcpSubs {
			_ = s.Unsubscribe()
		}
	}()

	// Pass 3: Retrofit localnet options on every external switch. Walks OVN
	// directly so stale/missing KV records can't hide a stale nat-addresses
	// or cleared network_name. Idempotent; silent when all correct.
	topo.RetrofitAllExternalLocalnetOptions(ctx)

	slog.Info("vpcd service started, waiting for VPC lifecycle events",
		"subscriptions", len(subs), "dhcp_subscriptions", len(dhcpSubs))

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	slog.Info("vpcd service shutting down")
	return nil
}

// resolveBridgeConfig picks the bridge mode and DHCP-bind-bridge to use,
// auto-detecting mode when unset. Empty mode stays empty — verifyBridgeMode
// rejects it with a list of supported values (D12). Empty bind bridge
// defaults to "br-wan", the consumer-router convention.
func resolveBridgeConfig(cfgBridgeMode, externalIface, cfgDhcpBindBridge string) (string, string) {
	bridgeMode := cfgBridgeMode
	if bridgeMode == "" && externalIface != "" {
		bridgeMode = detectBridgeMode(externalIface)
	}
	dhcpBindBridge := cfgDhcpBindBridge
	if dhcpBindBridge == "" {
		dhcpBindBridge = "br-wan"
	}
	return bridgeMode, dhcpBindBridge
}

// ifaceIsMacvlan returns true when the given interface exists and is a
// macvlan sub-interface. Injected as a var so detectBridgeMode tests can stub
// it without launching ip(8).
var ifaceIsMacvlan = func(name string) bool {
	out, err := exec.Command("ip", "-d", "link", "show", name).CombinedOutput()
	return err == nil && strings.Contains(string(out), "macvlan")
}

// ifaceExists returns true when the kernel reports the named link.
var ifaceExists = func(name string) bool {
	return exec.Command("ip", "link", "show", name).Run() == nil
}

// detectBridgeMode checks how the WAN bridge is wired:
//   - macvlan: spx-ext-{iface} macvlan sub-interface exists
//   - veth: veth-wan-ovs interface exists (Linux bridge linked to OVS via veth pair)
//   - direct: physical NIC is added directly to the OVS bridge
//
// Each decision point logs at Info so `journalctl -u spinifex-vpcd | grep
// bridge` surfaces the full trail. The fall-through case logs at Warn — the
// silent Debug fall-through is what let the veth-persistence bug hide for
// weeks (mulga-998.b Fix 2).
func detectBridgeMode(externalIface string) string {
	macvlanName := "spx-ext-" + externalIface
	if ifaceIsMacvlan(macvlanName) {
		slog.Info("vpcd: detected macvlan interface on WAN bridge", "iface", macvlanName, "mode", BridgeModeMacvlan)
		return BridgeModeMacvlan
	}
	if ifaceExists("veth-wan-ovs") {
		slog.Info("vpcd: detected veth pair linking Linux bridge to OVS", "mode", BridgeModeVeth)
		return BridgeModeVeth
	}
	slog.Warn("vpcd: no macvlan or veth interface found, assuming direct bridge mode",
		"external_iface", externalIface, "checked_macvlan", macvlanName, "checked_veth", "veth-wan-ovs",
		"mode", BridgeModeDirect)
	return BridgeModeDirect
}

// portToBr returns the OVS bridge that owns `port`. Returns "" when the port
// is not in OVSDB. Used by the post-detect sanity checks.
//
// Uses Output() (stdout only) because vpcd.service runs with AmbientCapabilities
// set, which causes sudo's PAM to emit "sudo: unable to send audit message"
// warnings on stderr. CombinedOutput would merge those into stdout and poison
// the bridge-name compare.
var portToBr = func(port string) (string, error) {
	out, err := sudoCommand("ovs-vsctl", "port-to-br", port).Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("ovs-vsctl port-to-br %s: %s: %w", port, stderr, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// readLinkMaster returns the master of a kernel link by reading
// /sys/class/net/<iface>/master. Returns "" if the link has no master.
var readLinkMaster = func(iface string) (string, error) {
	target, err := os.Readlink(filepath.Join("/sys/class/net", iface, "master"))
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// verifyBridgeMode is the post-detect sanity check. It refuses to start vpcd
// when the chosen bridge mode does not match the host plumbing (D4+D18):
//
//   - direct: ExternalInterface must be an OVS port on DhcpBindBridge. That is
//     the whole contract of direct mode.
//   - veth: (a) veth-wan-ovs must be an OVS port on OvnExternalBridge — the
//     OVN side, owned by setup-ovn.sh's ovn-bridge-mappings. (b) veth-wan-br
//     must be enslaved to DhcpBindBridge — the Linux side, where the DHCP
//     client sees LAN frames.
//   - empty / unknown: fail with the list of supported values.
//
// Fail-start, not soft-degrade — the distributed-NAT-on-veth-host footgun is
// exactly what this plan set out to kill.
func verifyBridgeMode(mode, externalIface, dhcpBindBridge string) error {
	switch mode {
	case BridgeModeDirect:
		if externalIface == "" {
			return fmt.Errorf("vpcd: direct bridge mode requires external_interface (the WAN NIC name)")
		}
		if dhcpBindBridge == "" {
			return fmt.Errorf("vpcd: direct bridge mode requires dhcp_bind_bridge (the OVS bridge holding the WAN NIC)")
		}
		br, err := portToBr(externalIface)
		if err != nil {
			return fmt.Errorf("vpcd: direct bridge mode: %w", err)
		}
		if br != dhcpBindBridge {
			return fmt.Errorf("vpcd: direct bridge mode: %q is on OVS bridge %q, expected %q (dhcp_bind_bridge)",
				externalIface, br, dhcpBindBridge)
		}
		return nil
	case BridgeModeVeth:
		if dhcpBindBridge == "" {
			return fmt.Errorf("vpcd: veth bridge mode requires dhcp_bind_bridge (the Linux bridge holding the WAN NIC)")
		}
		br, err := portToBr("veth-wan-ovs")
		if err != nil {
			return fmt.Errorf("vpcd: veth bridge mode: veth-wan-ovs not on OVS — is setup-ovn.sh's veth branch installed and systemd-networkd up? %w", err)
		}
		if br != OvnExternalBridge {
			return fmt.Errorf("vpcd: veth bridge mode: veth-wan-ovs is on OVS bridge %q, expected %q",
				br, OvnExternalBridge)
		}
		master, err := readLinkMaster("veth-wan-br")
		if err != nil {
			return fmt.Errorf("vpcd: veth bridge mode: veth-wan-br missing or has no master — systemd-networkd drop-in not applied? %w", err)
		}
		if master != dhcpBindBridge {
			return fmt.Errorf("vpcd: veth bridge mode: veth-wan-br master is %q, expected %q (dhcp_bind_bridge)",
				master, dhcpBindBridge)
		}
		return nil
	default:
		return fmt.Errorf("vpcd: unknown bridge_mode %q — supported values: %q, %q",
			mode, BridgeModeDirect, BridgeModeVeth)
	}
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
