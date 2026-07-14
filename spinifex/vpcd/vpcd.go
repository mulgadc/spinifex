package vpcd

import (
	"context"
	"errors"
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
	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/network/subscribers"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Bridge mode selects how the WAN NIC reaches the OVS bridge.
// Direct: WAN NIC added to OVS (distributed NAT, not safe on mgmt NIC).
// Veth: veth pair links a Linux bridge to OVS (requires centralized NAT).
// NAT: no WAN NIC bridged; transit veth + host masquerade (routed NAT).
const (
	BridgeModeDirect = "direct"
	BridgeModeVeth   = "veth"
	BridgeModeNAT    = "nat"
	// OvnExternalBridge is the OVS bridge targeted by ovn-bridge-mappings for the "external" localnet.
	OvnExternalBridge = "br-ext"
)

// waitForFlowsHV runs `ovn-nbctl --wait=hv sync`, blocking until all chassis acknowledge the new NB sequence.
// Bounded at 30 s; overruns log a Warn and return nil. Declared as a var so tests can stub it.
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

// sudoCommand wraps exec.Command with sudo when not root; OVS/OVN commands require elevated privileges.
func sudoCommand(name string, args ...string) *exec.Cmd {
	if os.Getuid() == 0 {
		return exec.Command(name, args...)
	}
	return exec.Command("sudo", append([]string{name}, args...)...)
}

var serviceName = "vpcd"

// Compile-time check: host.GatewayClaimProber implements reconcile.GatewayClaimVerifier.
var _ reconcile.GatewayClaimVerifier = (*host.GatewayClaimProber)(nil)

// Compile-time check: host.GatewayClaimProber implements subscribers.MACBindingFlusher.
var _ subscribers.MACBindingFlusher = (*host.GatewayClaimProber)(nil)

// BootstrapVPC holds the default VPC IDs from spinifex.toml used to seed OVN topology on first boot.
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
	// ExternalMode is "pool", "nat" (routed, outbound-only), or "" (disabled).
	ExternalMode string
	// ExternalPools holds the cluster-wide external IP pool configs.
	ExternalPools []external.ExternalPoolConfig
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
	// NorthstarBaseDomain is the cluster's authoritative base domain (northstar
	// default_domain, e.g. "spx3.net"). IMDS uses it to serve public-hostname.
	// Empty disables the public-hostname metadata key.
	NorthstarBaseDomain string
	// NorthstarInternalDomain is the cluster's private AWS-parity domain (northstar
	// internal_domain, default "compute.internal"). IMDS serves it as local-hostname
	// so the guest's own name matches the record the DNS writer publishes.
	NorthstarInternalDomain string
	// ResolverNameservers are the WAN IPs of cluster nodes running northstar,
	// used as the per-tap DNS shim's forward targets. When set, DHCP advertises
	// the link-local VPC DNS address (169.254.169.253) instead of the upstream
	// pool DNS.
	ResolverNameservers []string
	// NATExemptCIDRs are extra destinations that skip routed-mode SNAT,
	// appended to the transit /24 in the spinifex_nat_exempt set. nat mode only.
	NATExemptCIDRs []string
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

// checkOVNController verifies ovn-controller is running. Tries legacy socket path, then OVN 22.03+ path, then systemctl.
var checkOVNController = func() error {
	if sudoCommand("ovs-appctl", "-t", "ovn-controller", "version").Run() == nil {
		return nil
	}
	if matches, _ := filepath.Glob("/var/run/ovn/ovn-controller.*.ctl"); len(matches) > 0 {
		if sudoCommand("ovs-appctl", "-t", matches[0], "version").Run() == nil {
			return nil
		}
	}
	if sudoCommand("systemctl", "is-active", "--quiet", "ovn-controller").Run() == nil {
		return nil
	}

	return fmt.Errorf("ovn-controller is not running: run ./scripts/setup-ovn.sh --management")
}

// localSystemID returns the OVS external-ids:system-id (the chassis name in the Southbound DB).
// Uses Output() not CombinedOutput(): AmbientCapabilities causes sudo's PAM to emit stderr noise
// that would corrupt the system-id and cause discoverChassis to misidentify the local chassis.
var localSystemID = func() (string, error) {
	out, err := sudoCommand("ovs-vsctl", "get", "open_vswitch", ".", "external-ids:system-id").Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("ovs-vsctl get system-id: %s: %w", stderr, err)
	}
	return strings.Trim(strings.TrimSpace(string(out)), "\""), nil
}

// discoverChassis queries the OVN Southbound DB for chassis names, filtering out stale local entries.
// Stale rows (from a system-id change) must be excluded to prevent gateway ports binding to phantom chassis.
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
	// OVN 25.03+ removed "list-chassis"; use "--columns=name,hostname list Chassis" instead.
	// Output() not CombinedOutput(): sudo PAM stderr would corrupt name/hostname parsing.
	args = append(args, "--bare", "--columns=name,hostname", "list", "Chassis")
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

// parseChassisList parses ovn-sbctl --bare --columns=name,hostname output (name/hostname pairs separated by blank lines)
// and filters out stale chassis on the local host.
func parseChassisList(raw, localID, localHostname string) []string {
	var names []string
	var pair []string
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if len(pair) == 2 {
				names = appendIfActive(names, pair[0], pair[1], localID, localHostname)
			}
			pair = pair[:0]
			continue
		}
		pair = append(pair, line)
	}
	if len(pair) == 2 { // last row may have no trailing blank line
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

// externalCIDRFromBridge returns the first IPv4 CIDR on the named bridge.
// Injected as a var so tests can stub it without a real interface.
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

// externalCIDRRetryDelay is the poll interval between resolve attempts; tests shorten it.
var externalCIDRRetryDelay = 500 * time.Millisecond

// externalCIDRResolveTimeout bounds startup resolution of the WAN bridge CIDR; tests shorten it.
var externalCIDRResolveTimeout = 30 * time.Second

// resolveExternalCIDR blocks until the WAN bridge has an IPv4 address or timeout elapses.
// Guards the boot race where vpcd starts before systemd-networkd or netplan assigns the uplink address.
func resolveExternalCIDR(ctx context.Context, bridge string, timeout time.Duration) (netip.Prefix, error) {
	retryDelay := externalCIDRRetryDelay
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

// ensureExternalCIDRReady blocks until the WAN bridge has an IPv4 address within the timeout.
// No-op when externalMode is empty (overlay-only). Missing address indicates boot race or misconfiguration.
func ensureExternalCIDRReady(ctx context.Context, externalMode, bridge string) error {
	if externalMode == "" {
		return nil
	}
	cidr, err := resolveExternalCIDR(ctx, bridge, externalCIDRResolveTimeout)
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

	if err := preflightOVN(); err != nil {
		slog.Error("OVN preflight check failed — vpcd cannot start without OVN", "err", err)
		return err
	}
	slog.Info("OVN preflight passed (br-int exists, ovn-controller running)")

	nc, err := utils.ConnectNATSWithRetry(admin.DialTarget(cfg.NatsHost), cfg.NatsToken, cfg.NatsCACert)
	if err != nil {
		slog.Error("Failed to connect to NATS", "err", err)
		return err
	}
	defer nc.Close()

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

	if bridgeMode == BridgeModeNAT {
		// Re-ensure kernel egress rules on every start so they survive reboots
		// and firewall flushes without iptables-persistent.
		if err := host.EnsureNATEgressRules(ctx, host.NewExecRunner()); err != nil {
			slog.Error("vpcd: NAT egress rule install failed", "err", err)
			return err
		}
		if cfg.ExternalMode == "nat" && len(cfg.ExternalPools) == 0 {
			slog.Warn("vpcd: nat mode with no external pool; synthesizing default transit pool",
				"name", host.NATTransitPoolName, "gateway", host.NATTransitGatewayIP)
			cfg.ExternalPools = append(cfg.ExternalPools, external.ExternalPoolConfig{
				Name: host.NATTransitPoolName, Gateway: host.NATTransitGatewayIP, PrefixLen: 24,
			})
		}
	}

	if err := ensureExternalCIDRReady(ctx, cfg.ExternalMode, wanBridge); err != nil {
		return err
	}

	if cfg.ExternalMode != "" {
		slog.Info("External network enabled", "mode", cfg.ExternalMode, "pools", len(cfg.ExternalPools))
	}
	// Fail-start if no chassis found — missing chassis means ovn-controller hasn't registered yet (boot race).
	chassisNames, err := discoverChassis(cfg.OVNSBAddr)
	if err != nil {
		return fmt.Errorf("vpcd: discover OVN chassis: %w", err)
	}
	if len(chassisNames) == 0 {
		return fmt.Errorf("vpcd: no OVN chassis registered in SBDB — is ovn-controller running and connected?")
	}
	slog.Info("vpcd: gateway chassis discovered", "chassis", chassisNames)

	uplinkMode := host.UplinkModePhysical
	switch bridgeMode {
	case BridgeModeVeth:
		uplinkMode = host.UplinkModeVeth
	case BridgeModeNAT:
		uplinkMode = host.UplinkModeRouted
	}
	natMode := policy.NATModeFromUplinkMode(uplinkMode)

	var topoOpts []topology.Option
	if dns := resolverDNSServer(cfg); dns != "" {
		topoOpts = append(topoOpts, topology.WithDNSServer(func() string { return dns }))
	}
	topoMgr := topology.NewLiveManager(liveClient, topoOpts...)

	igwPool, publicPool := selectExternalPools(cfg.ExternalMode, cfg.ExternalPools)

	sgMgr := policy.NewSecurityGroupManager(liveClient)
	natOpts := []policy.Option{
		policy.WithFlowsBarrier(waitForFlowsHV),
		policy.WithNeighFlusher(neighFlusher(wanBridge)),
		policy.WithNeighPrimer(neighPrimer(wanBridge)),
	}
	if natMode == policy.NATModeRouted {
		natOpts = append(natOpts, policy.WithSNATExemptSet(policy.NATExemptSetName,
			append([]string{host.NATTransitCIDR}, cfg.NATExemptCIDRs...)))
	}
	if natMode == policy.NATModeRouted && publicPool != nil {
		natOpts = append(natOpts, policy.WithHostEIPBinder(hostEIPBinder(publicPool)))
	}
	natMgr, err := policy.NewNATManager(liveClient, natMode, natOpts...)
	if err != nil {
		return fmt.Errorf("construct NAT manager: %w", err)
	}
	routeMgr := policy.NewRouteManager(liveClient)

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("get JetStream context: %w", err)
	}

	// vpcd holds the network capabilities needed for IMDS; STS/IAM stay in awsgw over NATS.
	imdsCtx, cancelIMDS := context.WithCancel(ctx)
	defer cancelIMDS()
	// listTaps drives the per-tap responder reconcile from the live OVS tap set on
	// this chassis (the IMDS patch ports on br-int carry the full ENI).
	listTaps := func(ctx context.Context) (map[string]string, error) {
		taps, err := host.ListIMDSTaps(ctx, host.NewExecRunner())
		if err != nil {
			return nil, err
		}
		live := make(map[string]string, len(taps))
		for _, t := range taps {
			live[t.ENIID] = t.Endpoint
		}
		return live, nil
	}
	imdsSvc, err := handlers_imds.NewIMDSServiceImpl(
		nc,
		handlers_imds.NewNATSSTSAssumer(nc),
		handlers_imds.NewNATSProfileLookup(nc),
		handlers_imds.NewNATSPublicKeyLookup(nc),
		max(len(chassisNames), 1),
		listTaps,
		cfg.NorthstarBaseDomain,
		cfg.NorthstarInternalDomain,
		cfg.ResolverNameservers,
	)
	if err != nil {
		return fmt.Errorf("construct IMDS service: %w", err)
	}
	go func() {
		if err := imdsSvc.Run(imdsCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("vpcd: IMDS service exited", "err", err)
		}
	}()

	dhcpMgr, dhcpSubs, err := startDHCPManagerIfNeeded(ctx, nc, js, cfg)
	if err != nil {
		return fmt.Errorf("start dhcp manager: %w", err)
	}
	defer func() {
		for _, s := range dhcpSubs {
			_ = s.Unsubscribe()
		}
		if dhcpMgr != nil {
			dhcpMgr.Stop()
		}
	}()

	// Host ingress plumbing only exists in routed-NAT mode; other modes keep
	// nil hooks so the IGW manager never touches host routes.
	var routedIngress *external.RoutedIngressHooks
	if bridgeMode == BridgeModeNAT {
		ingressRunner := host.NewExecRunner()
		// Duplicate VPC CIDRs (every account's default VPC is 172.31.0.0/16)
		// collide on the single host route table: the bootstrap VPC always
		// wins; other VPCs only install when the CIDR is free and only remove
		// a route their own gateway IP holds.
		preferredVPC := ""
		if cfg.Bootstrap != nil {
			preferredVPC = cfg.Bootstrap.VpcId
		}
		routedIngress = &external.RoutedIngressHooks{
			Ensure: func(ctx context.Context, vpcID, vpcCIDR, gwLrpIP string) error {
				if vpcID != preferredVPC {
					holder, err := host.VPCIngressRouteVia(ctx, ingressRunner, vpcCIDR)
					if err == nil && holder != "" && holder != gwLrpIP {
						slog.Warn("vpcd: host ingress route conflict, keeping existing holder",
							"vpc_id", vpcID, "vpc_cidr", vpcCIDR, "holder_gw", holder, "skipped_gw", gwLrpIP)
						return nil
					}
				}
				return host.EnsureVPCIngressRoute(ctx, ingressRunner, vpcCIDR, gwLrpIP)
			},
			Remove: func(ctx context.Context, vpcID, vpcCIDR, gwLrpIP string) error {
				if gwLrpIP != "" {
					holder, err := host.VPCIngressRouteVia(ctx, ingressRunner, vpcCIDR)
					if err == nil && holder != "" && holder != gwLrpIP {
						slog.Info("vpcd: leaving host ingress route held by another VPC",
							"vpc_id", vpcID, "vpc_cidr", vpcCIDR, "holder_gw", holder)
						return nil
					}
				}
				return host.RemoveVPCIngressRoute(ctx, ingressRunner, vpcCIDR)
			},
		}
	}

	gwAllocator := pickGatewayAllocator(igwPool, liveClient, dhcpMgr)
	igwMgr, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN:           liveClient,
		Routes:        routeMgr,
		NAT:           natMgr,
		Pool:          igwPool,
		Allocator:     gwAllocator,
		Chassis:       chassisNames,
		NATMode:       natMode,
		FlowsBarrier:  waitForFlowsHV,
		RoutedIngress: routedIngress,
		NexthopSeed: func(ctx context.Context, lrpName, nexthopIP string) error {
			return host.SeedNexthopMAC(ctx, host.NewExecRunner(), lrpName, nexthopIP)
		},
	})
	if err != nil {
		return fmt.Errorf("construct IGW manager: %w", err)
	}
	eipMgr, err := external.NewEIPManager(natMgr, waitForFlowsHV)
	if err != nil {
		return fmt.Errorf("construct EIP manager: %w", err)
	}
	natgwMgr, err := external.NewNATGWManager(natMgr)
	if err != nil {
		return fmt.Errorf("construct NATGW manager: %w", err)
	}

	// Elect one vpcd for startup reconcile; without this, concurrent Get-then-Create on Logical_Router
	// produces duplicate rows that ovn-nbctl rejects. Runtime events still fan out via queue groups.
	holder, _ := os.Hostname()
	releaseLeader, isLeader := reconcile.AcquireLeader(nc, reconcile.KVBucketVPCDReconcile, holder)

	subscriber, err := subscribers.New(subscribers.Config{
		Topology: topoMgr,
		SG:       sgMgr,
		EIP:      eipMgr,
		NATGW:    natgwMgr,
		IGW:      igwMgr,
		MAC:      host.NewGatewayClaimProber(cfg.OVNSBAddr),
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
		GatewayClaim: host.NewGatewayClaimProber(cfg.OVNSBAddr),
		DNSServer:    resolverDNSServer(cfg),
	})
	if err != nil {
		return fmt.Errorf("construct reconciler: %w", err)
	}

	// Startup reconcile (leader-gated, apply-only). Orphan pruning is skipped because intent may be stale:
	// a peer's vpc.create-sg could be mid-flight and a prune would sweep those port groups as orphans.
	// Drift loop uses full Reconcile.
	if isLeader {
		intent, intentErr := reconcile.LoadIntentFromKV(ctx, js, cfg.AZ)
		if intentErr != nil {
			slog.Warn("vpcd: startup intent load failed", "err", intentErr)
		} else if err := rec.ReconcileApplyOnly(ctx, intent); err != nil {
			slog.Warn("vpcd: startup reconcile failed", "err", err)
		}
		releaseLeader()
	}

	// Periodic drift detection, leader-gated so only one vpcd scans at a time.
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	loopDone := make(chan struct{})
	go func() {
		reconcile.DriftLoop(loopCtx, rec, nc, cfg.AZ, holder)
		close(loopDone)
	}()

	// Per-host ovn-controller wedge watchdog. Not leader-gated: a stale-SB wedge is
	// local to this chassis and usually strikes a non-leader placement host, so the
	// recovery must run on every node. Backstops the bring-up settle pass for runtime
	// SB re-bootstraps (snapshot install / compaction) with no deploy in flight.
	go runOVNControllerWatchdog(loopCtx, newOVNWatchdog(host.NewGatewayClaimProber(cfg.OVNSBAddr)))

	slog.Info("vpcd service started, waiting for VPC lifecycle events",
		"subscriptions", len(subs))

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	slog.Info("vpcd service shutting down")
	loopCancel()
	<-loopDone
	return nil
}

// resolverDNSServer returns the OVN dhcp_options dns_server advertised to
// instances. With northstar configured, guests get the link-local VPC DNS
// address served by the per-tap shim (which forwards to ResolverNameservers);
// absent northstar it falls back to the upstream pool DNS.
func resolverDNSServer(cfg *Config) string {
	if len(cfg.ResolverNameservers) > 0 {
		return handlers_imds.VPCDNSServerIP
	}
	return pickDNSServer(cfg.ExternalPools)
}

// pickDNSServer returns the OVN dhcp_options dns_server from the first unscoped pool with DNS servers.
// Empty falls back to topology.NewLiveManager's default.
func pickDNSServer(pools []external.ExternalPoolConfig) string {
	for _, p := range pools {
		if p.Region == "" && p.AZ == "" && len(p.DNSServers) > 0 {
			return "{" + strings.Join(p.DNSServers, ", ") + "}"
		}
	}
	return ""
}

// startDHCPManagerIfNeeded starts the per-AZ DHCP Manager when any pool has Source="dhcp".
// Returns (nil, nil, nil) when not needed.
func startDHCPManagerIfNeeded(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *Config) (*dhcp.Manager, []*nats.Subscription, error) {
	if cfg == nil || cfg.ExternalMode == "" {
		return nil, nil, nil
	}
	wantDHCP := false
	for _, p := range cfg.ExternalPools {
		if p.Source == external.SourceDHCP {
			wantDHCP = true
			break
		}
	}
	if !wantDHCP {
		return nil, nil, nil
	}

	store, err := dhcp.NewStore(js, cfg.AZ)
	if err != nil {
		return nil, nil, fmt.Errorf("create dhcp lease store: %w", err)
	}
	mgr, err := dhcp.NewManager(dhcp.ManagerConfig{
		Client: dhcp.NewNClient4(0),
		Store:  store,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create dhcp manager: %w", err)
	}
	if err := mgr.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("start dhcp manager: %w", err)
	}
	subs, err := mgr.Subscribe(nc)
	if err != nil {
		mgr.Stop()
		return nil, nil, fmt.Errorf("subscribe dhcp manager: %w", err)
	}
	slog.Info("vpcd: dhcp manager started", "az", cfg.AZ, "subscriptions", len(subs))
	return mgr, subs, nil
}

// pickGatewayAllocator returns a DHCPGatewayLRPAllocator for DHCP-sourced pools; otherwise StaticRangeAllocator.
func pickGatewayAllocator(pool *external.ExternalPoolConfig, ovnClient ovn.Client, mgr *dhcp.Manager) external.GatewayIPAllocator {
	if pool.IsDHCP() && mgr != nil {
		return dhcp.NewDHCPGatewayLRPAllocator(mgr)
	}
	return external.NewStaticRangeAllocator(ovnClient)
}

// resolveBridgeConfig picks bridge mode (auto-detecting when unset) and the WAN
// bridge: "br-wan" for bridged modes, the transit veth host end for nat mode.
func resolveBridgeConfig(cfgBridgeMode, externalIface string) (string, string) {
	bridgeMode := cfgBridgeMode
	if bridgeMode == "" && externalIface != "" {
		bridgeMode = detectBridgeMode(externalIface)
	}
	if bridgeMode == BridgeModeNAT {
		return bridgeMode, host.NATTransitHostEnd
	}
	return bridgeMode, "br-wan"
}

// selectExternalPools splits configured pools by role. In nat mode the IGW
// gateway-LRP allocator draws from the transit pool (matched by name, never
// by index); any other pool carries public EIPs delivered via host plumbing.
// Other modes keep the first pool for the IGW and have no public split.
func selectExternalPools(externalMode string, pools []external.ExternalPoolConfig) (igwPool, publicPool *external.ExternalPoolConfig) {
	for i := range pools {
		p := pools[i]
		if externalMode == "nat" && p.Name != host.NATTransitPoolName {
			if publicPool == nil {
				publicPool = &p
			}
			continue
		}
		if igwPool == nil {
			igwPool = &p
		}
	}
	return igwPool, publicPool
}

// hostEIPBinder builds the routed-mode host plumbing hooks for EIPs on the
// public pool: /32 route into OVN plus proxy-ARP on the uplink. Static pools
// locate the uplink via the pool gateway; dhcp pools via their bind bridge.
func hostEIPBinder(pool *external.ExternalPoolConfig) policy.HostEIPBinder {
	runner := host.NewExecRunner()
	gateway, uplinkHint := pool.Gateway, pool.BindBridge
	return policy.HostEIPBinder{
		Bind: func(eip policy.EIPSpec, gwLrpIP string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return host.EnsureEIPIngress(ctx, runner, eip.ExternalIP, gwLrpIP, gateway, uplinkHint)
		},
		Unbind: func(externalIP string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return host.RemoveEIPIngress(ctx, runner, externalIP, gateway, uplinkHint)
		},
	}
}

// neighFlusher builds the ARP-flush hook for AddEIP/DeleteEIP so recycled IPs re-resolve L2 immediately.
// No-op when wanBridge is unset.
func neighFlusher(wanBridge string) policy.NeighFlusher {
	return func(externalIP string) error {
		if wanBridge == "" {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return host.FlushNeigh(ctx, nil, wanBridge, externalIP)
	}
}

// neighPrimer builds the ARP-prime hook for distributed EIPs so recycled IPs are reachable immediately
// without waiting for an ARP reply. No-op when wanBridge or MAC is unset.
func neighPrimer(wanBridge string) policy.NeighPrimer {
	return func(eip policy.EIPSpec) error {
		if wanBridge == "" || eip.MAC == "" {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return host.ReplaceNeigh(ctx, nil, wanBridge, eip.ExternalIP, eip.MAC)
	}
}

// ifaceExists returns true when the kernel reports the named link.
var ifaceExists = func(name string) bool {
	return exec.Command("ip", "link", "show", name).Run() == nil
}

// detectBridgeMode infers bridge mode: nat when spx-nat-ovs exists, veth when
// veth-wan-ovs exists, direct otherwise.
// Each branch logs at Info/Warn so `journalctl | grep bridge` shows the full detection trail.
func detectBridgeMode(externalIface string) string {
	if ifaceExists(host.NATTransitOVSEnd) {
		slog.Info("vpcd: detected routed-NAT transit veth", "mode", BridgeModeNAT)
		return BridgeModeNAT
	}
	if ifaceExists("veth-wan-ovs") {
		slog.Info("vpcd: detected veth pair linking Linux bridge to OVS", "mode", BridgeModeVeth)
		return BridgeModeVeth
	}
	slog.Warn("vpcd: no veth interface found, assuming direct bridge mode",
		"external_iface", externalIface, "checked_veth", "veth-wan-ovs",
		"mode", BridgeModeDirect)
	return BridgeModeDirect
}

// portToBr returns the OVS bridge owning port, or "" if not in OVSDB.
// Uses Output() not CombinedOutput(): AmbientCapabilities causes sudo PAM stderr that would corrupt the bridge name.
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

// readLinkMaster returns the master of a kernel link via /sys/class/net/<iface>/master, or "" if none.
var readLinkMaster = func(iface string) (string, error) {
	target, err := os.Readlink(filepath.Join("/sys/class/net", iface, "master"))
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// verifyBridgeMode validates that the chosen mode matches host plumbing. Fail-start on mismatch:
// direct requires ExternalInterface on the WAN OVS bridge; veth requires veth-wan-ovs on OvnExternalBridge
// and veth-wan-br enslaved to the WAN Linux bridge.
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
	case BridgeModeNAT:
		br, err := portToBr(host.NATTransitOVSEnd)
		if err != nil {
			return fmt.Errorf("vpcd: nat bridge mode: %s not on OVS — run setup-ovn.sh --nat-uplink: %w",
				host.NATTransitOVSEnd, err)
		}
		if br != OvnExternalBridge {
			return fmt.Errorf("vpcd: nat bridge mode: %s is on OVS bridge %q, expected %q",
				host.NATTransitOVSEnd, br, OvnExternalBridge)
		}
		if !ifaceExists(host.NATTransitHostEnd) {
			return fmt.Errorf("vpcd: nat bridge mode: %s link missing — run setup-ovn.sh --nat-uplink",
				host.NATTransitHostEnd)
		}
		return nil
	default:
		return fmt.Errorf("vpcd: unknown bridge_mode %q — supported values: %q, %q, %q",
			mode, BridgeModeDirect, BridgeModeVeth, BridgeModeNAT)
	}
}
