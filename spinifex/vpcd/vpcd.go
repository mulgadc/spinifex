package vpcd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/network/subscribers"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// Bridge mode constants for external connectivity. Bridge mode selects how
// the WAN NIC reaches the OVS integration bridge:
//   - Direct: WAN NIC is added directly to br-external as an OVS port.
//     Enables distributed NAT. Only safe when the WAN NIC is NOT the SSH/
//     management NIC.
//   - Veth: a veth pair links a Linux bridge (br-wan) to the OVS bridge
//     (br-ext). Requires centralized NAT — the Linux bridge intermediary
//     breaks distributed NAT hairpin routing.
const (
	BridgeModeDirect = "direct"
	BridgeModeVeth   = "veth"
	// OvnExternalBridge is the OVS bridge that ovn-bridge-mappings targets
	// for the "external" localnet. Owned by setup-ovn.sh and independent of
	// the WAN bridge (which is Linux-side in veth mode).
	OvnExternalBridge = "br-ext"
)

// waitForFlowsHV shells out to `ovn-nbctl --wait=hv sync`, bumping
// NB_Global.nb_cfg and blocking until every connected chassis has acknowledged
// the new sequence — i.e. ovn-northd has compiled NB→SB and ovn-controller
// has installed the resulting flows. Used as the NAT manager's flows barrier
// so newly-launched VMs aren't unreachable while their gateway chassis is
// still catching up (mulga-siv-105). Bounded by ovn-nbctl --timeout=30; on
// overrun we log a Warn and return nil so the caller continues.
//
// Declared as a var so tests can stub it.
var waitForFlowsHV = func() error {
	start := time.Now()
	cmd := sudoCommand("ovn-nbctl",
		"--no-leader-only",
		"--timeout=30",
		"--wait=hv",
		"sync",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("vpcd: OVN flows-ready barrier overran; continuing without confirmation",
			"elapsed", time.Since(start),
			"err", err,
			"output", strings.TrimSpace(string(out)),
		)
		return nil
	}
	slog.Debug("vpcd: OVN flows-ready barrier complete", "elapsed", time.Since(start))
	return nil
}

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
	// Bootstrap holds the default VPC config from spinifex.toml for first-boot reconciliation.
	Bootstrap *BootstrapVPC
	// ExternalInterface is the WAN NIC name (e.g., "enp0s3"). Used by
	// detectBridgeMode for veth/direct auto-detection.
	ExternalInterface string
	// BridgeMode is "direct" or "veth". Direct bridge adds the WAN NIC
	// directly to the OVS bridge; veth uses a veth pair to link a Linux bridge
	// to OVS. When empty, auto-detected at startup.
	BridgeMode string
	// AZ is the local availability zone identifier. The reconciler uses it
	// to scope its IntentState scan to local-AZ KV records; new VPC records
	// are stamped with this value at create time.
	AZ string
}

// ExternalPoolConfig mirrors config.ExternalPool for vpcd's internal use.
type ExternalPoolConfig struct {
	Name            string
	RangeStart      string
	RangeEnd        string
	Gateway         string
	GatewayIP       string
	PrefixLen       int
	DNSServers      []string
	Region          string
	AZ              string
	GwLrpRangeStart string // Sub-range for OVN gateway LRP IPs in centralized NAT mode (mulga-siv-36).
	GwLrpRangeEnd   string
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

// externalCIDRFromBridge returns the first IPv4 CIDR assigned to the named
// bridge interface. Used to discover the host's uplink network at startup
// (the OS assigns this via netplan static config or systemd-networkd DHCP
// before Spinifex starts).
//
// Injected as a var so tests can stub it without requiring a real interface.
var externalCIDRFromBridge = func(bridge string) (netip.Prefix, error) {
	iface, err := net.InterfaceByName(bridge)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("interface %q: %w", bridge, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("addrs %q: %w", bridge, err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		v4 := ipnet.IP.To4()
		if v4 == nil {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		addr, _ := netip.AddrFromSlice(v4)
		return netip.PrefixFrom(addr, ones), nil
	}
	return netip.Prefix{}, fmt.Errorf("no IPv4 address on %q", bridge)
}

// resolveExternalCIDR blocks until the WAN bridge has an IPv4 address or the
// timeout elapses. This guards the boot race where vpcd starts before
// systemd-networkd finishes DHCP on br-wan, or before netplan applies a
// static config. Returns the resolved CIDR for downstream consumers.
//
// Forward-compatible with the ADR-0006 L0 contract: this becomes the backing
// implementation of HostWiring.ExternalCIDR() once the network/host package
// lands. Until then the value is only used to fail-start on missing uplink.
func resolveExternalCIDR(ctx context.Context, bridge string, timeout time.Duration) (netip.Prefix, error) {
	const retryDelay = 500 * time.Millisecond
	deadline := time.Now().Add(timeout)
	attempt := 0
	for {
		attempt++
		cidr, err := externalCIDRFromBridge(bridge)
		if err == nil {
			if attempt > 1 {
				slog.Info("vpcd: external CIDR resolved", "bridge", bridge, "cidr", cidr.String(), "attempts", attempt)
			}
			return cidr, nil
		}
		if time.Now().After(deadline) {
			return netip.Prefix{}, fmt.Errorf("external CIDR not resolved on %q after %s (%d attempts): %w",
				bridge, timeout, attempt, err)
		}
		slog.Warn("vpcd: external CIDR not yet assigned, retrying",
			"bridge", bridge, "err", err, "attempt", attempt, "retry_in", retryDelay)
		select {
		case <-ctx.Done():
			return netip.Prefix{}, fmt.Errorf("external CIDR resolution cancelled: %w", ctx.Err())
		case <-time.After(retryDelay):
		}
	}
}

// ensureExternalCIDRReady blocks at startup until the WAN bridge has an IPv4
// address, or returns an error if resolution fails within the timeout. The OS
// (netplan static or systemd-networkd DHCP) assigns the uplink address before
// Spinifex starts in steady-state; a missing address at this point means a
// boot race or misconfiguration. Bounded retry so systemd's
// Restart=on-failure does not flap through transient gaps. No-op when
// externalMode is empty (overlay-only deployments).
func ensureExternalCIDRReady(ctx context.Context, externalMode, bridge string) error {
	if externalMode == "" {
		return nil
	}
	cidr, err := resolveExternalCIDR(ctx, bridge, 30*time.Second)
	if err != nil {
		slog.Error("vpcd: external CIDR resolution failed", "bridge", bridge, "err", err)
		return err
	}
	slog.Info("vpcd: external CIDR resolved at startup", "bridge", bridge, "cidr", cidr.String())
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
	nc, err := utils.ConnectNATSWithRetry(admin.DialTarget(cfg.NatsHost), cfg.NatsToken, cfg.NatsCACert)
	if err != nil {
		slog.Error("Failed to connect to NATS", "err", err)
		return err
	}
	defer nc.Close()

	// Connect to OVN NB DB (required — vpcd is useless without it)
	if cfg.OVNNBAddr == "" {
		return fmt.Errorf("OVN NB DB address not configured (ovn_nb_addr is empty)")
	}

	liveClient := ovn.NewLiveClient(cfg.OVNNBAddr)
	ctx := context.Background()
	if err := liveClient.Connect(ctx); err != nil {
		slog.Error("Failed to connect to OVN NB DB", "endpoint", cfg.OVNNBAddr, "err", err)
		return fmt.Errorf("connect OVN NB DB: %w", err)
	}
	defer liveClient.Close()
	slog.Info("Connected to OVN NB DB", "endpoint", cfg.OVNNBAddr)

	bridgeMode, wanBridge := resolveBridgeConfig(cfg.BridgeMode, cfg.ExternalInterface)
	slog.Info("External bridge mode", "mode", bridgeMode, "wan_bridge", wanBridge)
	if err := verifyBridgeMode(bridgeMode, cfg.ExternalInterface, wanBridge); err != nil {
		slog.Error("vpcd: bridge mode sanity check failed", "err", err)
		return err
	}

	// Resolve external CIDR from the WAN bridge before any reconcile. Skipped
	// when external networking is disabled (overlay-only deployments).
	if err := ensureExternalCIDRReady(ctx, cfg.ExternalMode, wanBridge); err != nil {
		return err
	}

	if cfg.ExternalMode != "" {
		slog.Info("External network enabled", "mode", cfg.ExternalMode, "pools", len(cfg.ExternalPools))
	}
	// Discover chassis from OVN SBDB. The OVS-managed system-id (persisted at
	// /etc/openvswitch/system-id.conf and re-applied on every boot) is the
	// canonical chassis identity; SBDB is the only source of truth. Fail-start
	// rather than guess — a missing chassis means ovn-controller hasn't
	// registered yet (boot race) and systemd's Restart=on-failure will retry.
	chassisNames, err := discoverChassis(cfg.OVNSBAddr)
	if err != nil {
		return fmt.Errorf("vpcd: discover OVN chassis: %w", err)
	}
	if len(chassisNames) == 0 {
		return fmt.Errorf("vpcd: no OVN chassis registered in SBDB — is ovn-controller running and connected?")
	}
	slog.Info("vpcd: gateway chassis discovered", "chassis", chassisNames)

	uplinkMode := host.UplinkModePhysical
	if bridgeMode == BridgeModeVeth {
		uplinkMode = host.UplinkModeVeth
	}
	natMode := policy.NATModeFromUplinkMode(uplinkMode)

	// Construct the network manager stack. The reconciler is the single
	// intent-driven pass that runs once at startup (leader-gated) and then
	// on a 5-minute drift ticker; together they replace the five separate
	// passes the old vpcd ran on boot (mulga-siv-125, Phase 2).
	var topoOpts []topology.Option
	if dns := pickDNSServer(cfg.ExternalPools); dns != "" {
		topoOpts = append(topoOpts, topology.WithDNSServer(func() string { return dns }))
	}
	topoMgr := topology.NewLiveManager(liveClient, topoOpts...)

	sgMgr := policy.NewSecurityGroupManager(liveClient)
	natMgr, err := policy.NewNATManager(liveClient, natMode, policy.WithFlowsBarrier(waitForFlowsHV))
	if err != nil {
		return fmt.Errorf("construct NAT manager: %w", err)
	}
	routeMgr := policy.NewRouteManager(liveClient)

	var igwPool *external.ExternalPoolConfig
	if cfg.ExternalMode != "" && len(cfg.ExternalPools) > 0 {
		p := externalPoolConfigToShared(cfg.ExternalPools[0])
		igwPool = &p
	}
	igwMgr, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN:          liveClient,
		Routes:       routeMgr,
		NAT:          natMgr,
		Pool:         igwPool,
		Allocator:    external.NewStaticRangeAllocator(liveClient),
		Chassis:      chassisNames,
		NATMode:      natMode,
		FlowsBarrier: waitForFlowsHV,
	})
	if err != nil {
		return fmt.Errorf("construct IGW manager: %w", err)
	}

	// Elect a single vpcd to run startup reconcile. Without this, N vpcds in
	// a multi-node cluster all hit Get-then-Create on Logical_Router with no
	// atomicity, producing duplicate rows that ovn-nbctl rejects with
	// "Multiple logical routers named '...'. Use a UUID." (mulga-siv-29).
	// Runtime VPC events still fan out via the vpcd-workers queue group, so
	// non-leaders remain functional after Subscribe below.
	holder, _ := os.Hostname()
	releaseLeader, isLeader := reconcile.AcquireLeader(nc, holder)

	subscriber, err := subscribers.New(subscribers.Config{
		Topology: topoMgr,
		SG:       sgMgr,
		NAT:      natMgr,
		IGW:      igwMgr,
	})
	if err != nil {
		return fmt.Errorf("construct subscriber: %w", err)
	}
	subs, err := subscriber.Subscribe(nc)
	if err != nil {
		slog.Error("Failed to subscribe to VPC topics", "err", err)
		return err
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	js, err := nc.JetStream()
	if err != nil {
		slog.Error("Failed to get JetStream context", "err", err)
		return err
	}

	rec, err := reconcile.New(reconcile.Config{
		OVN:          liveClient,
		SG:           sgMgr,
		NAT:          natMgr,
		Routes:       routeMgr,
		IGW:          igwMgr,
		Topology:     topoMgr,
		LocalAZ:      cfg.AZ,
		NodeHostname: holder,
		Chassis:      chassisNames,
	})
	if err != nil {
		return fmt.Errorf("construct reconciler: %w", err)
	}

	// Startup reconcile: leader-gated read of NATS KV intent state, applied
	// once against OVN NB DB. The drift loop below handles the recovery case
	// (KV not yet populated when this fires — daemon's EnsureDefaultVPC may
	// race with vpcd's startup). Per-event NATS subscribers route through
	// the same network/* managers, so the startup pass and the runtime fast
	// path share one implementation.
	//
	// ReconcileApplyOnly (not Reconcile) — orphan pruning is unsafe at
	// startup because intent may be stale: daemon EnsureDefaultVPC on a
	// peer node can be mid-flight (KV write committed but not yet visible
	// here) while a peer subscriber concurrently creates port groups from
	// its vpc.create-sg event. A startup prune then sweeps those port
	// groups as orphans, breaking every subsequent RunInstances until the
	// 5-minute drift tick recreates them. Drift uses full Reconcile.
	if isLeader {
		intent, intentErr := reconcile.LoadIntentFromKV(ctx, js, cfg.AZ)
		if intentErr != nil {
			slog.Warn("vpcd: startup intent load failed", "err", intentErr)
		} else if err := rec.ReconcileApplyOnly(ctx, intent); err != nil {
			slog.Warn("vpcd: startup reconcile failed", "err", err)
		}
		releaseLeader()
	}

	// Periodic drift detection. Leader-gated on the shared reconcile bucket
	// so only one vpcd in the cluster scans at a time; cancelled when the
	// parent ctx is.
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	loopDone := make(chan struct{})
	go func() {
		reconcile.DriftLoop(loopCtx, rec, nc, cfg.AZ, holder)
		close(loopDone)
	}()

	slog.Info("vpcd service started, waiting for VPC lifecycle events",
		"subscriptions", len(subs))

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	slog.Info("vpcd service shutting down")
	loopCancel()
	<-loopDone
	return nil
}

// pickDNSServer returns the OVN dhcp_options dns_server string ("{a, b}")
// from the first unscoped (no region, no AZ) pool with DNS servers set.
// Empty string falls back to topology.NewLiveManager's default
// ("{8.8.8.8, 1.1.1.1}").
func pickDNSServer(pools []ExternalPoolConfig) string {
	for _, p := range pools {
		if p.Region == "" && p.AZ == "" && len(p.DNSServers) > 0 {
			return "{" + strings.Join(p.DNSServers, ", ") + "}"
		}
	}
	return ""
}

// externalPoolConfigToShared translates the vpcd-local ExternalPoolConfig
// into the network/external package's shared shape. The two will merge
// once topology.go is split (bead mulga-siv-125.3.4 — see external.go's
// type doc); until then this is the seam.
func externalPoolConfigToShared(p ExternalPoolConfig) external.ExternalPoolConfig {
	return external.ExternalPoolConfig{
		Name:            p.Name,
		RangeStart:      p.RangeStart,
		RangeEnd:        p.RangeEnd,
		Gateway:         p.Gateway,
		GatewayIP:       p.GatewayIP,
		PrefixLen:       p.PrefixLen,
		DNSServers:      p.DNSServers,
		Region:          p.Region,
		AZ:              p.AZ,
		GwLrpRangeStart: p.GwLrpRangeStart,
		GwLrpRangeEnd:   p.GwLrpRangeEnd,
	}
}

// resolveBridgeConfig picks the bridge mode and WAN bridge name to use,
// auto-detecting mode when unset. Empty mode stays empty — verifyBridgeMode
// rejects it with a list of supported values (D12). The WAN bridge name is
// always "br-wan" (the consumer-router convention) since vpcd no longer
// accepts a per-node override.
func resolveBridgeConfig(cfgBridgeMode, externalIface string) (string, string) {
	bridgeMode := cfgBridgeMode
	if bridgeMode == "" && externalIface != "" {
		bridgeMode = detectBridgeMode(externalIface)
	}
	return bridgeMode, "br-wan"
}

// ifaceExists returns true when the kernel reports the named link.
var ifaceExists = func(name string) bool {
	return exec.Command("ip", "link", "show", name).Run() == nil
}

// detectBridgeMode checks how the WAN bridge is wired:
//   - veth: veth-wan-ovs interface exists (Linux bridge linked to OVS via veth pair)
//   - direct: physical NIC is added directly to the OVS bridge
//
// Each decision point logs at Info so `journalctl -u spinifex-vpcd | grep
// bridge` surfaces the full trail. The fall-through case logs at Warn — the
// silent Debug fall-through is what let the veth-persistence bug hide for
// weeks (mulga-998.b Fix 2).
func detectBridgeMode(externalIface string) string {
	if ifaceExists("veth-wan-ovs") {
		slog.Info("vpcd: detected veth pair linking Linux bridge to OVS", "mode", BridgeModeVeth)
		return BridgeModeVeth
	}
	slog.Warn("vpcd: no veth interface found, assuming direct bridge mode",
		"external_iface", externalIface, "checked_veth", "veth-wan-ovs",
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
//   - direct: ExternalInterface must be an OVS port on the WAN bridge. That
//     is the whole contract of direct mode.
//   - veth: (a) veth-wan-ovs must be an OVS port on OvnExternalBridge — the
//     OVN side, owned by setup-ovn.sh's ovn-bridge-mappings. (b) veth-wan-br
//     must be enslaved to the WAN bridge — the Linux side.
//   - empty / unknown: fail with the list of supported values.
//
// Fail-start, not soft-degrade — the distributed-NAT-on-veth-host footgun is
// exactly what this plan set out to kill.
func verifyBridgeMode(mode, externalIface, wanBridge string) error {
	switch mode {
	case BridgeModeDirect:
		if externalIface == "" {
			return fmt.Errorf("vpcd: direct bridge mode requires external_interface (the WAN NIC name)")
		}
		if wanBridge == "" {
			return fmt.Errorf("vpcd: direct bridge mode requires the WAN bridge (the OVS bridge holding the WAN NIC)")
		}
		br, err := portToBr(externalIface)
		if err != nil {
			return fmt.Errorf("vpcd: direct bridge mode: %w", err)
		}
		if br != wanBridge {
			return fmt.Errorf("vpcd: direct bridge mode: %q is on OVS bridge %q, expected %q",
				externalIface, br, wanBridge)
		}
		return nil
	case BridgeModeVeth:
		if wanBridge == "" {
			return fmt.Errorf("vpcd: veth bridge mode requires wan_bridge (the Linux bridge holding the WAN NIC)")
		}
		br, err := portToBr("veth-wan-ovs")
		if err != nil {
			return fmt.Errorf("vpcd: veth bridge mode: veth-wan-ovs not on OVS — is spinifex-veth-wan.service running? %w", err)
		}
		if br != OvnExternalBridge {
			return fmt.Errorf("vpcd: veth bridge mode: veth-wan-ovs is on OVS bridge %q, expected %q",
				br, OvnExternalBridge)
		}
		master, err := readLinkMaster("veth-wan-br")
		if err != nil {
			return fmt.Errorf("vpcd: veth bridge mode: veth-wan-br missing or has no master — is spinifex-veth-wan.service running? %w", err)
		}
		if master != wanBridge {
			return fmt.Errorf("vpcd: veth bridge mode: veth-wan-br master is %q, expected %q (wan_bridge)",
				master, wanBridge)
		}
		return nil
	default:
		return fmt.Errorf("vpcd: unknown bridge_mode %q — supported values: %q, %q",
			mode, BridgeModeDirect, BridgeModeVeth)
	}
}
