package handlers_elbv2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_acm "github.com/mulgadc/spinifex/spinifex/handlers/acm"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	// elbv2ManagedByTag is an alias for the shared tag key (kept for readability).
	elbv2ManagedByTag = tags.ManagedByKey
	// elbv2ManagedByValue is the tag value for ELBv2-managed resources.
	elbv2ManagedByValue = tags.ManagedByELBv2
	// elbv2LBTag is the tag key storing the parent LB ARN on managed ENIs.
	elbv2LBTag = tags.LBARNKey

	// heartbeatPersistInterval controls how often a no-op heartbeat (no state
	// change) writes the full LoadBalancerRecord back to KV. State transitions
	// always persist immediately.
	heartbeatPersistInterval = 60 * time.Second

	// Health check fields are interpolated verbatim into the HAProxy template
	// (see haproxy.go); restrict them to characters that cannot terminate a
	// directive or introduce a new one. Length caps bound abuse and match the
	// surface AWS exposes for these fields.
	maxHealthCheckPathLen    = 1024
	maxHealthCheckMatcherLen = 64
)

var (
	healthCheckPathRegex    = regexp.MustCompile(`^[A-Za-z0-9._~/?#%&=+-]+$`)
	healthCheckMatcherRegex = regexp.MustCompile(`^[0-9,-]+$`)
)

func validateHealthCheckPath(p string) error {
	if len(p) == 0 || len(p) > maxHealthCheckPathLen || !healthCheckPathRegex.MatchString(p) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

func validateHealthCheckMatcher(m string) error {
	if len(m) == 0 || len(m) > maxHealthCheckMatcherLen || !healthCheckMatcherRegex.MatchString(m) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

// resolveMgmtRoute returns the (gateway, target) pair for the host route the
// lb-agent uses to reach AWSGW from inside the LB microVM, or empty strings
// to skip the route. Consumed by buildMicrovmNICs to populate NIC[1]'s
// RouteDst/RouteVia for the netcfg fw_cfg blob.
//
// Multi-node (AWSGW on dedicated mgmt IP): MgmtRoute{Gateway,Target} are set
// by the daemon for both schemes — return them.
//
// Single-node (AWSGW on advertiseIP, MgmtRoute fields empty): internet-facing
// LBs reach AWSGW via VPC → external (their EIP gives OVN a SNAT pair for the
// reply), so they need no mgmt route — and adding a /32 to advertiseIP would
// steal the host's WAN return path. Internal LBs have no EIP and no SNAT pair,
// so the VPC egress reply targets the VM's private IP from the host's WAN with
// no route back → packet drops, lb-agent never heartbeats, LB sticks in
// provisioning. Force the mgmt route for internal scheme via the br-mgmt IP.
func (s *ELBv2ServiceImpl) resolveMgmtRoute(scheme string) (gateway, target string) {
	if s.MgmtRouteGateway != "" && s.MgmtRouteTarget != "" {
		return s.MgmtRouteGateway, s.MgmtRouteTarget
	}
	if scheme == SchemeInternal && s.MgmtBridgeIP != "" && s.AdvertiseIP != "" {
		return s.MgmtBridgeIP, s.AdvertiseIP
	}
	if scheme == SchemeInternal {
		slog.Warn("resolveMgmtRoute: internal LB has no mgmt return route; lb-agent cannot reach AWSGW and the LB will stay provisioning",
			"mgmtRouteGateway", s.MgmtRouteGateway,
			"mgmtRouteTarget", s.MgmtRouteTarget,
			"mgmtBridgeIP", s.MgmtBridgeIP,
			"advertiseIP", s.AdvertiseIP)
	}
	return "", ""
}

// Ensure ELBv2ServiceImpl implements ELBv2Service at compile time.
var _ ELBv2Service = (*ELBv2ServiceImpl)(nil)

// ELBv2ServiceImpl implements ELBv2 operations with NATS JetStream persistence.
type ELBv2ServiceImpl struct {
	config                     *config.Config
	store                      *Store
	acmStore                   *handlers_acm.Store              // resolves listener cert ARNs → PEM; nil-safe (HTTPS unavailable when nil)
	nc                         *nats.Conn                       // NATS connection for JetStream KV store
	VPCService                 *handlers_ec2_vpc.VPCServiceImpl // nil-safe: ENI ops skipped when nil (e.g. in tests)
	InstanceLauncher           SystemInstanceLauncher           // nil-safe: system VM ops skipped when nil
	SystemAccessKey            string                           // System account access key for ALB agent SigV4 auth
	SystemSecretKey            string                           // System account secret key for ALB agent SigV4 auth
	GatewayURL                 string                           // AWS gateway URL for ALB agent outbound connections
	MgmtRouteGateway           string                           // br-mgmt IP (next-hop for mgmt route); empty when AWSGW is on 0.0.0.0
	MgmtRouteTarget            string                           // AWSGW bind IP to route via mgmt NIC
	MgmtBridgeIP               string                           // br-mgmt IP, populated whenever br-mgmt exists (single + multi node) for the internal-scheme fallback route
	AdvertiseIP                string                           // AdvertiseIP / WAN gateway, populated whenever set; used as the internal-scheme fallback route target on single-node
	CACert                     string                           // PEM-encoded CA certificate delivered to microvm guests via fw_cfg
	nodeID                     string
	region                     string
	systemInstanceType         string        // instance type for system VMs; resolved lazily via systemInstanceTypeFunc
	systemInstanceTypeFunc     func() string // returns the smallest available instance type
	systemInstanceTypeMu       sync.Mutex    // guards lazy resolution of systemInstanceType
	systemInstanceTypeResolved bool          // true once systemInstanceType has been resolved to a non-empty value
	ctx                        context.Context
	cancel                     context.CancelFunc
	hc                         *healthChecker

	recoveryLBIndexMu sync.Mutex
	recoveryLBIndex   map[string]*LoadBalancerRecord
}

// NewELBv2ServiceImplWithNATS creates an ELBv2 service backed by JetStream KV.
func NewELBv2ServiceImplWithNATS(cfg *config.Config, nc *nats.Conn) (*ELBv2ServiceImpl, error) {
	store, err := NewStore(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to create ELBv2 store: %w", err)
	}

	// ACM store shares the same JetStream KV — read cert PEM directly, no
	// cross-service NATS hop. Non-fatal: a failure here only disables HTTPS
	// termination (resolveCertPEM returns an error), it must not block the
	// daemon from serving ELBv2 control-plane requests.
	acmStore, acmErr := handlers_acm.NewStore(nc)
	if acmErr != nil {
		slog.Warn("ELBv2: ACM store unavailable, HTTPS listeners cannot resolve certs", "err", acmErr)
		acmStore = nil
	}

	region := "us-east-1"
	nodeID := ""
	if cfg != nil {
		if cfg.Region != "" {
			region = cfg.Region
		}
		nodeID = cfg.Node
	}

	ctx, cancel := context.WithCancel(context.Background())
	hc := newHealthChecker(store)

	return &ELBv2ServiceImpl{
		config:   cfg,
		store:    store,
		acmStore: acmStore,
		nc:       nc,
		nodeID:   nodeID,
		region:   region,
		ctx:      ctx,
		cancel:   cancel,
		hc:       hc,
	}, nil
}

// Close cancels background goroutines.
func (s *ELBv2ServiceImpl) Close() {
	if s.cancel != nil {
		s.cancel()
	}
}

// ResetTargetHealthOnStartup invalidates persisted target HealthState across
// all target groups by resetting every non-draining target to "initial". Run
// once during daemon startup before the service begins serving requests.
//
// Rationale: target HealthState is persisted in JetStream KV, but the
// healthChecker's per-target counters live only in process memory. After a
// daemon restart (e.g. host reboot), the counters are gone but the stored
// "healthy" claim remains — DescribeTargetHealth returns "healthy"
// immediately even though the daemon has zero active health observations
// and the LB system VM's lb-agent hasn't posted a fresh report yet. Tests
// that gate on WaitForTargetsHealthy advance prematurely, then ALB traffic
// lands on a backend that isn't actually up.
//
// Resetting to "initial" is the correct AWS-semantics representation of
// "we don't know yet, give us a moment" — the next lb-agent heartbeat
// drives the state machine forward through evaluateHealth.
//
// Multi-node caveat: in a multi-daemon cluster, each daemon that restarts
// resets the SHARED KV state. A sibling daemon's in-memory counters would
// then be out-of-sync with the persisted "initial" state until the next
// heartbeat re-converges. Acceptable trade-off — heartbeats arrive within
// one health-check interval (default 5-30s) and round-robin transiently
// excluding a backend is safer than serving traffic to a dead one.
func (s *ELBv2ServiceImpl) ResetTargetHealthOnStartup(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	tgs, err := s.store.ListTargetGroups()
	if err != nil {
		return fmt.Errorf("list target groups: %w", err)
	}
	resetTGs := 0
	resetTargets := 0
	for _, tg := range tgs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		changed := false
		for i := range tg.Targets {
			t := &tg.Targets[i]
			// Skip targets actively being drained — that state is driven by
			// DeregisterTargets and must not be clobbered by a daemon restart.
			if t.HealthState == TargetHealthDraining {
				continue
			}
			if t.HealthState == TargetHealthInitial {
				continue
			}
			t.HealthState = TargetHealthInitial
			t.HealthDesc = "Target registration is in progress"
			changed = true
			resetTargets++
		}
		if changed {
			if err := s.store.PutTargetGroup(tg); err != nil {
				slog.Error("ResetTargetHealthOnStartup: persist failed",
					"tgId", tg.TargetGroupID, "err", err)
				continue
			}
			resetTGs++
		}
	}
	if resetTargets > 0 {
		slog.Info("ELBv2: reset target health on startup",
			"targetsReset", resetTargets, "targetGroupsTouched", resetTGs)
	}
	return nil
}

// SetSystemInstanceTypeFunc sets a function that resolves the smallest available
// instance type. Called at request time so it adapts to node capacity.
func (s *ELBv2ServiceImpl) SetSystemInstanceTypeFunc(fn func() string) {
	s.systemInstanceTypeMu.Lock()
	defer s.systemInstanceTypeMu.Unlock()
	s.systemInstanceTypeFunc = fn
	s.systemInstanceType = ""
	s.systemInstanceTypeResolved = false
}

// getSystemInstanceType returns the instance type for system VMs.
// Only caches non-empty results so the resolver retries when no capacity
// is available yet.
func (s *ELBv2ServiceImpl) getSystemInstanceType() string {
	s.systemInstanceTypeMu.Lock()
	defer s.systemInstanceTypeMu.Unlock()
	if s.systemInstanceTypeResolved {
		return s.systemInstanceType
	}
	if s.systemInstanceTypeFunc != nil {
		s.systemInstanceType = s.systemInstanceTypeFunc()
	}
	if s.systemInstanceType != "" {
		s.systemInstanceTypeResolved = true
	}
	return s.systemInstanceType
}

// buildLBAgentEnv returns the KEY=value blob written to /etc/conf.d/lb-agent
// via fw_cfg on the direct-boot path.
func (s *ELBv2ServiceImpl) buildLBAgentEnv(lbID string) string {
	return fmt.Sprintf("LB_LB_ID=%s\nLB_GATEWAY_URL=%s\nLB_ACCESS_KEY=%s\nLB_SECRET_KEY=%s\nLB_REGION=%s\n",
		lbID, s.GatewayURL, s.SystemAccessKey, s.SystemSecretKey, s.region)
}

// subnetCIDRForIP computes "ip/prefixlen" given a host IP and the subnet's
// CIDR block (e.g. "10.0.1.0/24" → "10.0.1.5/24"). Returns empty string on
// parse failure so the caller can log and continue without panicking.
func subnetCIDRForIP(hostIP, subnetCIDR string) string {
	_, ipNet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return ""
	}
	ones, _ := ipNet.Mask.Size()
	return fmt.Sprintf("%s/%d", hostIP, ones)
}

// subnetGatewayIP derives the gateway IP (network address +1) from a subnet
// CIDR block. Returns empty string on parse failure.
func subnetGatewayIP(subnetCIDR string) string {
	_, ipNet, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return ""
	}
	gw := ipNet.IP.To4()
	if gw == nil {
		return ""
	}
	gwCopy := make(net.IP, len(gw))
	copy(gwCopy, gw)
	gwCopy[3]++
	return gwCopy.String()
}

// buildMicrovmNICs constructs the NIC slice for a direct-boot microvm launch.
//
// NIC[0] is the primary VPC ENI (IsDefault=true): MAC/IP from the ENI record,
// CIDR from ip+subnet prefix, gateway = subnet network address +1.
//
// NIC[1] is the management NIC (IsDefault=false): MAC and CIDR are left empty
// here — the daemon injects them after allocating the per-instance mgmt IP.
// RouteDst/RouteVia encode the host route the lb-agent needs to reach AWSGW
// via the management interface (same logic as resolveMgmtRoute).
//
// NIC[2+] are extra VPC ENIs for multi-subnet ALBs.
func (s *ELBv2ServiceImpl) buildMicrovmNICs(primaryIP, primaryMAC, primarySubnetID, _, scheme string, extraENIs []ExtraENIInput, accountID string) []NICConfig {
	// Resolve primary subnet CIDR for NIC[0] prefix and gateway.
	primaryCIDR := ""
	primaryGateway := ""
	if s.VPCService != nil && primarySubnetID != "" {
		if subnet, err := s.VPCService.GetSubnet(accountID, primarySubnetID); err == nil && subnet != nil {
			primaryCIDR = subnetCIDRForIP(primaryIP, subnet.CidrBlock)
			primaryGateway = subnetGatewayIP(subnet.CidrBlock)
		} else {
			slog.Warn("buildMicrovmNICs: could not look up primary subnet", "subnetId", primarySubnetID, "err", err)
		}
	}

	nics := []NICConfig{
		// NIC[0]: primary VPC ENI — default route owner.
		{
			MAC:       primaryMAC,
			CIDR:      primaryCIDR,
			Gateway:   primaryGateway,
			IsDefault: true,
		},
	}

	// NIC[1]: management NIC — daemon fills MAC/CIDR after IP allocation.
	// RouteDst = AWSGW IP (destination), RouteVia = br-mgmt IP (next-hop).
	// Honor resolveMgmtRoute's deliberate empty return for single-node
	// internet-facing: adding a /32 to AdvertiseIP via mgmt steals the host's
	// WAN return path for reply traffic, breaking same-chassis ingress.
	mgmtRouteGW, mgmtRouteTarget := s.resolveMgmtRoute(scheme)
	nics = append(nics, NICConfig{
		IsDefault: false,
		RouteDst:  mgmtRouteTarget, // AWSGW IP — destination to reach
		RouteVia:  mgmtRouteGW,     // br-mgmt IP — next-hop
	})

	// NIC[2+]: extra ENIs for multi-subnet ALBs.
	for _, extra := range extraENIs {
		extraCIDR := ""
		extraGateway := ""
		if s.VPCService != nil && extra.SubnetID != "" {
			if subnet, err := s.VPCService.GetSubnet(accountID, extra.SubnetID); err == nil && subnet != nil {
				extraCIDR = subnetCIDRForIP(extra.ENIIP, subnet.CidrBlock)
				extraGateway = subnetGatewayIP(subnet.CidrBlock)
			} else {
				slog.Warn("buildMicrovmNICs: could not look up extra subnet", "subnetId", extra.SubnetID, "err", err)
			}
		}
		nics = append(nics, NICConfig{
			MAC:       extra.ENIMac,
			CIDR:      extraCIDR,
			Gateway:   extraGateway,
			IsDefault: false,
		})
	}

	return nics
}

// describeENIs resolves the supplied ENI IDs in one VPC describe call and
// returns them keyed by NetworkInterfaceId. Returns an empty map when the
// VPC service is unwired or the describe errors — callers must tolerate
// missing entries.
func (s *ELBv2ServiceImpl) describeENIs(eniIDs []string, accountID string) map[string]*ec2.NetworkInterface {
	eniDetails := make(map[string]*ec2.NetworkInterface, len(eniIDs))
	if s.VPCService == nil || len(eniIDs) == 0 {
		return eniDetails
	}
	eniPtrs := make([]*string, 0, len(eniIDs))
	for _, id := range eniIDs {
		eniPtrs = append(eniPtrs, aws.String(id))
	}
	result, err := s.VPCService.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: eniPtrs,
	}, accountID)
	if err != nil {
		slog.Error("VPC describe ENIs failed", "count", len(eniIDs), "err", err)
		return eniDetails
	}
	for _, eni := range result.NetworkInterfaces {
		if eni.NetworkInterfaceId != nil {
			eniDetails[*eni.NetworkInterfaceId] = eni
		}
	}
	return eniDetails
}

// buildExtraENIInputs threads every non-primary ENI in eniIDs into an
// ExtraENIInput, using eniDetails as the MAC/IP/SubnetID source. eniIDs[0]
// is the primary ENI and is intentionally excluded. ENIs missing from
// eniDetails are skipped (matching CreateLoadBalancer's original behaviour
// when DescribeNetworkInterfaces drops an entry).
func buildExtraENIInputs(eniIDs []string, eniDetails map[string]*ec2.NetworkInterface) []ExtraENIInput {
	if len(eniIDs) <= 1 {
		return nil
	}
	extras := make([]ExtraENIInput, 0, len(eniIDs)-1)
	for i := 1; i < len(eniIDs); i++ {
		eni := eniDetails[eniIDs[i]]
		if eni == nil {
			continue
		}
		extras = append(extras, ExtraENIInput{
			ENIID:    eniIDs[i],
			ENIMac:   aws.StringValue(eni.MacAddress),
			ENIIP:    aws.StringValue(eni.PrivateIpAddress),
			SubnetID: aws.StringValue(eni.SubnetId),
		})
	}
	return extras
}

// lbVMLaunch is the outcome of booting the system VM that backs a load
// balancer. failed is true when the VM could not be launched (missing system
// credentials or a launcher error) so the caller can mark the LB failed.
type lbVMLaunch struct {
	instanceID string
	vpcIP      string
	publicIP   string
	hostPorts  map[int]int
	failed     bool
}

// launchLBVM boots the system VM backing a load balancer with the given ENI
// set. The first ENI is the primary NIC; any additional ENIs are passed as
// ExtraENIs so the daemon sets up a tap + QEMU NIC per subnet, giving the LB VM
// data-plane presence in every subnet it spans. When no launcher is configured
// the call is a no-op (zero value, failed=false) so launcher-less deployments
// keep the LB Active. Shared by CreateLoadBalancer and the SetSubnets relaunch.
func (s *ELBv2ServiceImpl) launchLBVM(lbID, scheme string, eniIDs, subnets []string, accountID string) lbVMLaunch {
	var res lbVMLaunch
	if s.InstanceLauncher == nil || len(eniIDs) == 0 || len(subnets) == 0 {
		return res
	}

	eniDetails := s.describeENIs(eniIDs, accountID)
	primary := eniDetails[eniIDs[0]]
	primaryIP := ""
	primaryMAC := ""
	if primary != nil {
		primaryIP = aws.StringValue(primary.PrivateIpAddress)
		primaryMAC = aws.StringValue(primary.MacAddress)
	}

	extraENIInputs := buildExtraENIInputs(eniIDs, eniDetails)

	if s.GatewayURL == "" || s.SystemAccessKey == "" || s.SystemSecretKey == "" {
		slog.Error("launchLBVM: system credentials not configured — cannot launch LB VM", "lbId", lbID)
		res.failed = true
		return res
	}

	nics := s.buildMicrovmNICs(primaryIP, primaryMAC, subnets[0], eniIDs[0], scheme, extraENIInputs, accountID)
	launchInput := &SystemInstanceInput{
		InstanceType: s.getSystemInstanceType(),
		SubnetID:     subnets[0],
		ENIID:        eniIDs[0],
		ENIMac:       primaryMAC,
		ENIIP:        primaryIP,
		ExtraENIs:    extraENIInputs,
		Scheme:       scheme,
		AccountID:    accountID,
		NICs:         nics,
		LBAgentEnv:   s.buildLBAgentEnv(lbID),
		CACert:       s.CACert,
	}
	// Dev-mode only: forward HTTP/HTTPS ports from host for local testing.
	// In production (VPC networking), traffic reaches the LB VM's VPC IP directly.
	if s.config != nil && s.config.Daemon.DevNetworking {
		launchInput.HostfwdPorts = []int{80, 443}
	}

	out, launchErr := s.InstanceLauncher.LaunchSystemInstance(launchInput)
	if launchErr != nil {
		slog.Error("launchLBVM: failed to launch LB VM", "lbId", lbID, "err", launchErr)
		res.failed = true
		return res
	}

	res.instanceID = out.InstanceID
	res.vpcIP = out.PrivateIP
	res.publicIP = out.PublicIP
	res.hostPorts = out.HostfwdMap
	slog.Info("launchLBVM: LB VM launched", "lbId", lbID, "instanceId", out.InstanceID, "ip", out.PrivateIP, "publicIp", out.PublicIP, "hostfwd", out.HostfwdMap)
	return res
}

// loadRecoveryLBIndex returns the instanceID->LB map, populated lazily on
// first successful call. Errors are not cached: a transient JetStream
// failure at startup must not condemn every subsequent recovery candidate
// in the same batch — the next caller retries.
func (s *ELBv2ServiceImpl) loadRecoveryLBIndex() (map[string]*LoadBalancerRecord, error) {
	s.recoveryLBIndexMu.Lock()
	defer s.recoveryLBIndexMu.Unlock()
	if s.recoveryLBIndex != nil {
		return s.recoveryLBIndex, nil
	}
	lbs, err := s.store.ListLoadBalancers()
	if err != nil {
		return nil, fmt.Errorf("list lb records: %w", err)
	}
	index := make(map[string]*LoadBalancerRecord, len(lbs))
	for _, r := range lbs {
		if r.InstanceID != "" {
			index[r.InstanceID] = r
		}
	}
	s.recoveryLBIndex = index
	return s.recoveryLBIndex, nil
}

// RebuildSystemInstanceInput reconstructs the launch input for an existing
// system instance during host-reboot recovery. The instance->LB map is
// memoised in s.recoveryLBIndex so N concurrent recovery candidates share a
// single ListLoadBalancers call instead of issuing N×L JetStream round-trips.
func (s *ELBv2ServiceImpl) RebuildSystemInstanceInput(ctx RecoveryContext) (*SystemInstanceInput, error) {
	index, err := s.loadRecoveryLBIndex()
	if err != nil {
		return nil, err
	}
	lb := index[ctx.InstanceID]
	if lb == nil {
		return nil, fmt.Errorf("no LB record references instance %s", ctx.InstanceID)
	}
	if len(lb.Subnets) == 0 || len(lb.ENIs) == 0 {
		return nil, fmt.Errorf("lb %s has no subnets/ENIs to rebuild from", lb.LoadBalancerID)
	}

	eniDetails := s.describeENIs(lb.ENIs, lb.AccountID)
	// Multi-ENI rebuild needs an authoritative MAC/IP/subnet per extra, which
	// only the VPC store supplies. Single-ENI ALBs can fall back to ctx.ENIMac
	// + lb.VPCIP for the primary, so we only enforce the strict count match
	// when extras are present.
	if len(lb.ENIs) > 1 && len(eniDetails) < len(lb.ENIs) {
		return nil, fmt.Errorf("describe lb %s ENIs: got %d/%d", lb.LoadBalancerID, len(eniDetails), len(lb.ENIs))
	}
	primaryMAC := ctx.ENIMac
	if primary := eniDetails[lb.ENIs[0]]; primary != nil {
		if mac := aws.StringValue(primary.MacAddress); mac != "" {
			primaryMAC = mac
		}
	}
	if primaryMAC == "" {
		return nil, fmt.Errorf("no MAC for primary ENI %s of lb %s", lb.ENIs[0], lb.LoadBalancerID)
	}
	extraENIs := buildExtraENIInputs(lb.ENIs, eniDetails)

	nics := s.buildMicrovmNICs(lb.VPCIP, primaryMAC, lb.Subnets[0], lb.ENIs[0], lb.Scheme, extraENIs, lb.AccountID)
	// Re-inject the daemon-allocated mgmt NIC values so writeFwCfgBlobs
	// emits the same netcfg the original launch produced — buildMicrovmNICs
	// leaves NIC[1] MAC/CIDR blank because they are only known after the
	// daemon's per-instance mgmt IP allocation.
	if len(nics) > 1 && ctx.MgmtMAC != "" {
		nics[1].MAC = ctx.MgmtMAC
		if ctx.MgmtIP != "" {
			nics[1].CIDR = ctx.MgmtIP + "/24"
		}
	}

	return &SystemInstanceInput{
		InstanceType: ctx.InstanceType,
		SubnetID:     lb.Subnets[0],
		ENIID:        lb.ENIs[0],
		ENIMac:       primaryMAC,
		ENIIP:        lb.VPCIP,
		ExtraENIs:    extraENIs,
		Scheme:       lb.Scheme,
		AccountID:    lb.AccountID,
		NICs:         nics,
		LBAgentEnv:   s.buildLBAgentEnv(lb.LoadBalancerID),
		CACert:       s.CACert,
	}, nil
}

// resolveENIBindAddr looks up the private IP of the first ENI belonging to the ALB.
// Returns empty string if VPC service is unavailable or no ENIs exist.
func (s *ELBv2ServiceImpl) resolveENIBindAddr(lb *LoadBalancerRecord) string {
	if s.VPCService == nil || len(lb.ENIs) == 0 {
		return ""
	}

	result, err := s.VPCService.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(lb.ENIs[0])},
	}, lb.AccountID)
	if err != nil || len(result.NetworkInterfaces) == 0 {
		slog.Debug("Could not resolve ALB ENI bind address", "eniId", lb.ENIs[0], "err", err)
		return ""
	}

	if result.NetworkInterfaces[0].PrivateIpAddress != nil {
		return *result.NetworkInterfaces[0].PrivateIpAddress
	}
	return ""
}

// updateStoredConfig generates the HAProxy config for an ALB, hashes it, and
// stores both ConfigText and ConfigHash on the LB record. The agent pulls this
// on its next heartbeat when it detects a hash change. Safe to call when
// instanceLauncher is nil or the LB has no VM.
func (s *ELBv2ServiceImpl) updateStoredConfig(lb *LoadBalancerRecord) error {
	if lb.InstanceID == "" {
		return nil
	}

	listeners, err := s.store.ListListenersByLB(lb.LoadBalancerArn)
	if err != nil {
		slog.Error("updateStoredConfig: failed to list listeners", "lbArn", lb.LoadBalancerArn, "err", err)
		return fmt.Errorf("list listeners: %w", err)
	}

	bindAddr := s.resolveENIBindAddr(lb)

	// Collect rules per listener and target groups referenced by listeners + rules.
	rulesByListener := make(map[string][]*RuleRecord)
	tgByArn := make(map[string]*TargetGroupRecord)

	loadTG := func(tgArn string) {
		if tgArn == "" {
			return
		}
		if _, ok := tgByArn[tgArn]; ok {
			return
		}
		tg, tgErr := s.store.GetTargetGroupByArn(tgArn)
		if tgErr != nil || tg == nil {
			slog.Debug("updateStoredConfig: target group not found", "tgArn", tgArn)
			return
		}
		tgByArn[tgArn] = tg
	}

	for _, l := range listeners {
		for _, a := range l.DefaultActions {
			loadTG(a.TargetGroupArn)
		}
		rules, rErr := s.store.ListRulesByListener(l.ListenerArn)
		if rErr != nil {
			slog.Error("updateStoredConfig: failed to list rules", "listenerArn", l.ListenerArn, "err", rErr)
			return fmt.Errorf("list rules: %w", rErr)
		}
		if len(rules) > 0 {
			rulesByListener[l.ListenerArn] = rules
		}
		for _, r := range rules {
			for _, a := range r.Actions {
				loadTG(a.TargetGroupArn)
			}
		}
	}

	certPEMByArn, err := s.resolveListenerCerts(listeners, lb.AccountID)
	if err != nil {
		slog.Error("updateStoredConfig: failed to resolve certs", "lbId", lb.LoadBalancerID, "err", err)
		return fmt.Errorf("resolve certs: %w", err)
	}

	configContent, certFiles, err := GenerateHAProxyConfigWithCerts(lb, listeners, tgByArn, rulesByListener, bindAddr, certPEMByArn)
	if err != nil {
		slog.Error("updateStoredConfig: failed to generate config", "lbId", lb.LoadBalancerID, "err", err)
		return fmt.Errorf("generate config: %w", err)
	}

	hash := configCertHash(configContent, certFiles)
	lb.ConfigText = configContent
	lb.ConfigHash = hash
	lb.CertFiles = certFiles
	// NLBs run nginx, which has no active upstream probing — the agent
	// probes these targets and reports health. ALBs report via HAProxy stats.
	if lb.Type == LoadBalancerTypeNetwork {
		lb.HealthTargets = buildNLBHealthTargets(tgByArn)
	} else {
		lb.HealthTargets = nil
	}

	if err := s.store.PutLoadBalancer(lb); err != nil {
		slog.Error("updateStoredConfig: failed to persist LB", "lbId", lb.LoadBalancerID, "err", err)
		return fmt.Errorf("persist LB: %w", err)
	}

	slog.Info("updateStoredConfig: config stored",
		"lbId", lb.LoadBalancerID, "hash", hash[:12], "size", len(configContent), "certs", len(certFiles))
	return nil
}

// resolveListenerCerts resolves every distinct certificate ARN referenced by
// the listeners to its combined PEM via the ACM store. Returns nil when no
// listener carries a certificate (the common HTTP-only case).
func (s *ELBv2ServiceImpl) resolveListenerCerts(listeners []*ListenerRecord, accountID string) (map[string]string, error) {
	var out map[string]string
	for _, l := range listeners {
		for _, c := range l.Certificates {
			if _, ok := out[c.CertificateArn]; ok {
				continue
			}
			pem, err := s.resolveCertPEM(c.CertificateArn, accountID)
			if err != nil {
				return nil, fmt.Errorf("cert %s: %w", c.CertificateArn, err)
			}
			if out == nil {
				out = make(map[string]string)
			}
			out[c.CertificateArn] = pem
		}
	}
	return out, nil
}

// resolveCertPEM loads a certificate by ARN from the ACM store and returns its
// combined PEM (leaf + chain + key) in the order HAProxy `ssl crt` expects.
// Ownership is enforced: a cert belonging to another account is treated as
// absent.
func (s *ELBv2ServiceImpl) resolveCertPEM(arn, accountID string) (string, error) {
	if s.acmStore == nil {
		return "", errors.New(awserrors.ErrorELBv2CertificateNotFound)
	}
	rec, err := s.acmStore.GetCert(arn)
	if err != nil {
		return "", fmt.Errorf("get cert: %w", err)
	}
	if rec == nil || rec.AccountID != accountID {
		return "", errors.New(awserrors.ErrorELBv2CertificateNotFound)
	}

	var b strings.Builder
	b.WriteString(strings.TrimRight(rec.Certificate, "\n"))
	b.WriteByte('\n')
	if rec.CertificateChain != "" {
		b.WriteString(strings.TrimRight(rec.CertificateChain, "\n"))
		b.WriteByte('\n')
	}
	b.WriteString(strings.TrimRight(rec.PrivateKey, "\n"))
	b.WriteByte('\n')
	return b.String(), nil
}

// validateListenerCerts confirms every certificate ARN resolves in the ACM
// store and is owned by the account, rejecting an unknown ARN at the API
// boundary (CreateListener/ModifyListener) with ErrorELBv2CertificateNotFound.
// Without this, an unresolvable ARN is accepted into the listener and only
// fails later at config-render time, which aborts updateStoredConfig wholesale
// and silently freezes the LB's data-plane convergence. Skipped when the ACM
// store is unavailable (cannot validate; matches the nil-safe resolve path).
func (s *ELBv2ServiceImpl) validateListenerCerts(certs []ListenerCertificate, accountID string) error {
	if s.acmStore == nil {
		return nil
	}
	for _, c := range certs {
		if _, err := s.resolveCertPEM(c.CertificateArn, accountID); err != nil {
			return errors.New(awserrors.ErrorELBv2CertificateNotFound)
		}
	}
	return nil
}

// configCertHash hashes the rendered config plus every delivered cert file
// (path + content, in sorted path order) so a cert rotation changes the hash
// and triggers an agent reload even when the config text is unchanged.
func configCertHash(configContent string, certFiles map[string]string) string {
	h := sha256.New()
	h.Write([]byte(configContent))
	paths := make([]string, 0, len(certFiles))
	for p := range certFiles {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write([]byte(certFiles[p]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// updateStoredConfigForTargetGroup finds all LBs that reference the given target
// group (via listeners) and updates their stored config.
func (s *ELBv2ServiceImpl) updateStoredConfigForTargetGroup(tgArn string) error {
	allListeners, err := s.store.ListListeners()
	if err != nil {
		slog.Error("updateStoredConfigForTargetGroup: failed to list listeners", "err", err)
		return fmt.Errorf("list listeners: %w", err)
	}

	lbArns := make(map[string]bool)
	for _, l := range allListeners {
		for _, a := range l.DefaultActions {
			if a.TargetGroupArn == tgArn {
				lbArns[l.LoadBalancerArn] = true
			}
		}
	}

	// Rules can independently forward to a TG even if the listener default does not.
	allRules, rulesErr := s.store.ListRules()
	if rulesErr != nil {
		slog.Error("updateStoredConfigForTargetGroup: failed to list rules", "err", rulesErr)
		return fmt.Errorf("list rules: %w", rulesErr)
	}
	listenerLB := make(map[string]string, len(allListeners))
	for _, l := range allListeners {
		listenerLB[l.ListenerArn] = l.LoadBalancerArn
	}
	for _, r := range allRules {
		for _, a := range r.Actions {
			if a.TargetGroupArn != tgArn {
				continue
			}
			if lbArn, ok := listenerLB[r.ListenerArn]; ok {
				lbArns[lbArn] = true
			}
		}
	}

	for lbArn := range lbArns {
		lb, lbErr := s.store.GetLoadBalancerByArn(lbArn)
		if lbErr != nil || lb == nil {
			continue
		}
		if err := s.updateStoredConfig(lb); err != nil {
			return err
		}
	}
	return nil
}

// LBAgentHeartbeat processes a heartbeat from an LB agent. On first heartbeat,
// transitions LB from provisioning → active. Always updates LastHeartbeat and
// processes the health report. Returns the current config hash so the agent
// knows whether to fetch new config.
func (s *ELBv2ServiceImpl) LBAgentHeartbeat(input *LBAgentHeartbeatInput, accountID string) (*LBAgentHeartbeatOutput, error) {
	if input.LBID == nil || *input.LBID == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	lbID := *input.LBID
	slog.Debug("LBAgentHeartbeat received", "lbId", lbID, "accountId", accountID)
	lb, err := s.store.GetLoadBalancer(lbID)
	if err != nil {
		slog.Error("LBAgentHeartbeat: failed to get LB", "lbId", lbID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil || (lb.AccountID != accountID && accountID != utils.GlobalAccountID) {
		// The heartbeat reached AWSGW (so the mgmt return path works) but
		// cannot transition any LB. Without this the reject is silent and a
		// stuck-in-provisioning LB looks identical to one whose heartbeat
		// never arrived.
		slog.Warn("LBAgentHeartbeat: LB not found or account mismatch",
			"lbId", lbID, "accountId", accountID, "found", lb != nil)
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}

	// First heartbeat: transition provisioning → active
	stateChanged := false
	if lb.State == StateProvisioning {
		lb.State = StateActive
		stateChanged = true
		slog.Info("LB transitioned to active via heartbeat", "lbId", lbID)
	}

	now := time.Now().UTC()

	// Only persist to KV on state transitions or when the stored heartbeat
	// timestamp is stale. This avoids writing the full record (including
	// ConfigText) every 5 seconds when nothing changed.
	if stateChanged || now.Sub(lb.LastHeartbeat) >= heartbeatPersistInterval {
		lb.LastHeartbeat = now
		if err := s.store.PutLoadBalancer(lb); err != nil {
			slog.Error("LBAgentHeartbeat: failed to persist LB", "lbId", lbID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	// Process health report directly — no JSON round-trip needed.
	if len(input.Servers) > 0 {
		s.hc.handleHealthReportDirect(input.toHealthReport())
	}

	return &LBAgentHeartbeatOutput{
		Status:     aws.String(lb.State),
		ConfigHash: aws.String(lb.ConfigHash),
	}, nil
}

// GetLBConfig returns the stored HAProxy config and hash for a load balancer.
func (s *ELBv2ServiceImpl) GetLBConfig(input *GetLBConfigInput, accountID string) (*GetLBConfigOutput, error) {
	if input.LBID == nil || *input.LBID == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	lbID := *input.LBID
	lb, err := s.store.GetLoadBalancer(lbID)
	if err != nil {
		slog.Error("GetLBConfig: failed to get LB", "lbId", lbID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil || (lb.AccountID != accountID && accountID != utils.GlobalAccountID) {
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}

	return &GetLBConfigOutput{
		ConfigText:    aws.String(lb.ConfigText),
		ConfigHash:    aws.String(lb.ConfigHash),
		CertFiles:     certFilesToSDK(lb.CertFiles),
		Engine:        aws.String(engineForType(lb.Type)),
		HealthTargets: healthTargetsToSDK(lb.HealthTargets),
	}, nil
}

// healthTargetsToSDK converts the stored health-target specs into the SDK
// HealthTarget slice delivered to the nginx agent.
func healthTargetsToSDK(specs []HealthTargetSpec) []*HealthTarget {
	if len(specs) == 0 {
		return nil
	}
	out := make([]*HealthTarget, 0, len(specs))
	for i := range specs {
		out = append(out, &HealthTarget{
			ServerName: aws.String(specs[i].ServerName),
			Address:    aws.String(specs[i].Address),
			Protocol:   aws.String(specs[i].Protocol),
			Path:       aws.String(specs[i].Path),
		})
	}
	return out
}

// certFilesToSDK converts the stored path→PEM map into the sorted CertFile
// slice delivered to the agent. Sorted for deterministic output.
func certFilesToSDK(certFiles map[string]string) []*CertFile {
	if len(certFiles) == 0 {
		return nil
	}
	paths := make([]string, 0, len(certFiles))
	for p := range certFiles {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]*CertFile, 0, len(paths))
	for _, p := range paths {
		out = append(out, &CertFile{Path: aws.String(p), PEM: aws.String(certFiles[p])})
	}
	return out
}

// lbArnPathSegment returns the ARN path segment for the given LB type:
// "app" for application LBs, "net" for network LBs.
func lbArnPathSegment(lbType string) string {
	if lbType == LoadBalancerTypeNetwork {
		return "net"
	}
	return "app"
}

// buildLBArn constructs a load balancer ARN from components.
func buildLBArn(region, accountID, name, lbID, lbType string) string {
	return fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:loadbalancer/%s/%s/%s", region, accountID, lbArnPathSegment(lbType), name, lbID)
}

// resolveTargetIP looks up the private IP for an instance by finding its primary ENI.
// Returns empty string if VPC service is unavailable or no ENI found.
func (s *ELBv2ServiceImpl) resolveTargetIP(instanceID, accountID string) string {
	if s.VPCService == nil {
		return ""
	}

	result, err := s.VPCService.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("attachment.instance-id"), Values: []*string{aws.String(instanceID)}},
		},
	}, accountID)
	if err != nil || len(result.NetworkInterfaces) == 0 {
		slog.Warn("Could not resolve target IP — target will not receive traffic until ENI is attached", "instanceId", instanceID, "err", err)
		return ""
	}

	// Use the first (primary) ENI's private IP
	if result.NetworkInterfaces[0].PrivateIpAddress != nil {
		return *result.NetworkInterfaces[0].PrivateIpAddress
	}
	return ""
}

// resolveRegisteredTargetIP returns the private IP to route to for a target
// being registered, based on the target group's target type. For ip targets the
// supplied ID is the IP itself; for instance targets it is resolved via the
// instance's primary ENI. Returns an error when the ID's shape does not match
// the target type.
func (s *ELBv2ServiceImpl) resolveRegisteredTargetIP(targetType, id, accountID string) (string, error) {
	switch targetType {
	case TargetTypeIP:
		if net.ParseIP(id) == nil {
			slog.Warn("RegisterTargets: ip target id is not a valid IP", "id", id)
			return "", errors.New(awserrors.ErrorInvalidParameterValue)
		}
		return id, nil
	default: // instance
		if net.ParseIP(id) != nil {
			slog.Warn("RegisterTargets: instance target group given an IP address", "id", id)
			return "", errors.New(awserrors.ErrorInvalidParameterValue)
		}
		return s.resolveTargetIP(id, accountID), nil
	}
}

// buildTGArn constructs a target group ARN from components.
func buildTGArn(region, accountID, name, tgID string) string {
	return fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:targetgroup/%s/%s", region, accountID, name, tgID)
}

// buildListenerArn constructs a listener ARN from components.
func buildListenerArn(region, accountID, lbName, lbID, listenerID, lbType string) string {
	return fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:listener/%s/%s/%s/%s", region, accountID, lbArnPathSegment(lbType), lbName, lbID, listenerID)
}

// ELBv2 resource types as they appear in the resource segment of an ARN.
const (
	elbv2ResourceLoadBalancer = "loadbalancer"
	elbv2ResourceTargetGroup  = "targetgroup"
	elbv2ResourceListener     = "listener"
	elbv2ResourceListenerRule = "listener-rule"
)

// elbv2ResourceTypeFromArn extracts the resource type from an ELBv2 ARN.
// ELBv2 ARNs have the form
// "arn:aws:elasticloadbalancing:{region}:{account}:{type}/...". Returns one
// of "loadbalancer", "targetgroup", "listener", "listener-rule" — anything
// else yields ErrorInvalidParameterValue so callers can surface a proper API
// error.
func elbv2ResourceTypeFromArn(arn string) (string, error) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 || parts[0] != "arn" || parts[2] != "elasticloadbalancing" {
		return "", errors.New(awserrors.ErrorInvalidParameterValue)
	}
	resourceSegment := parts[5]
	slash := strings.Index(resourceSegment, "/")
	if slash <= 0 {
		return "", errors.New(awserrors.ErrorInvalidParameterValue)
	}
	resourceType := resourceSegment[:slash]
	switch resourceType {
	case elbv2ResourceLoadBalancer, elbv2ResourceTargetGroup, elbv2ResourceListener, elbv2ResourceListenerRule:
		return resourceType, nil
	default:
		return "", errors.New(awserrors.ErrorInvalidParameterValue)
	}
}

// isCompatibleProtocol checks whether a listener protocol is compatible with a
// target group protocol. ALB: HTTP/HTTPS listeners can forward to HTTP or HTTPS
// TGs. NLB: TCP→TCP, UDP→UDP, TLS→TCP or TLS, TCP_UDP→TCP_UDP.
func isCompatibleProtocol(listenerProto, tgProto string) bool {
	switch listenerProto {
	case ProtocolHTTP, ProtocolHTTPS:
		return tgProto == ProtocolHTTP || tgProto == ProtocolHTTPS
	case ProtocolTCP:
		return tgProto == ProtocolTCP
	case ProtocolUDP:
		return tgProto == ProtocolUDP
	case ProtocolTLS:
		return tgProto == ProtocolTCP || tgProto == ProtocolTLS
	case ProtocolTCPUDP:
		return tgProto == ProtocolTCPUDP
	default:
		return false
	}
}

// --- Load Balancer operations ---

func (s *ELBv2ServiceImpl) CreateLoadBalancer(input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error) {
	if input.Name == nil || *input.Name == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	name := *input.Name

	// Check for duplicate name
	existing, err := s.store.GetLoadBalancerByName(name, accountID)
	if err != nil {
		slog.Error("CreateLoadBalancer: failed to check duplicate name", "name", name, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if existing != nil {
		return nil, errors.New(awserrors.ErrorELBv2DuplicateLoadBalancer)
	}

	// Resolve load balancer type — default to application (ALB).
	lbType := LoadBalancerTypeApplication
	if input.Type != nil && *input.Type != "" {
		lbType = *input.Type
		if lbType != LoadBalancerTypeApplication && lbType != LoadBalancerTypeNetwork {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	// NLBs do not support security groups.
	if lbType == LoadBalancerTypeNetwork && len(input.SecurityGroups) > 0 {
		return nil, errors.New(awserrors.ErrorELBv2InvalidConfigurationRequest)
	}

	scheme := SchemeInternetFacing
	if input.Scheme != nil && *input.Scheme != "" {
		scheme = *input.Scheme
		if scheme != SchemeInternetFacing && scheme != SchemeInternal {
			return nil, errors.New(awserrors.ErrorELBv2InvalidScheme)
		}
	}

	lbID := utils.GenerateResourceID("lb")
	lbArn := buildLBArn(s.region, accountID, name, lbID, lbType)
	arnPathSegment := lbArnPathSegment(lbType)
	dnsPrefix := ""
	if scheme == SchemeInternal {
		dnsPrefix = "internal-"
	}
	dnsName := fmt.Sprintf("%s%s-%s.%s.elb.spinifex.local", dnsPrefix, name, lbID, s.region)

	var subnets []string
	for _, sn := range input.Subnets {
		if sn != nil {
			subnets = append(subnets, *sn)
		}
	}

	var securityGroups []string
	for _, sg := range input.SecurityGroups {
		if sg != nil {
			securityGroups = append(securityGroups, *sg)
		}
	}

	tags := tagsFromSDK(input.Tags)

	// Create ENIs in each subnet (when VPC service is available)
	var eniIDs []string
	var availZones []AvailZoneInfo
	vpcID := ""
	var nlbManagedSGID string
	if s.VPCService != nil && len(subnets) > 0 {
		// NLBs reject customer SGs (rejected above), so the ENIs would fall back
		// to the VPC default SG whose intra-SG-only ingress drops inbound
		// listener traffic. Mint a dedicated managed SG up front and attach it to
		// every LB ENI; CreateListener opens the listener ports on it.
		eniGroups := securityGroups
		if lbType == LoadBalancerTypeNetwork {
			sgID, sgErr := s.createNLBManagedSG(lbID, lbArn, subnets[0], accountID)
			if sgErr != nil {
				slog.Error("CreateLoadBalancer: failed to create managed NLB SG", "lbId", lbID, "err", sgErr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			nlbManagedSGID = sgID
			eniGroups = []string{sgID}
		}
		for _, subnetID := range subnets {
			eniIn := &ec2.CreateNetworkInterfaceInput{
				SubnetId:    aws.String(subnetID),
				Description: aws.String(fmt.Sprintf("ELB %s/%s/%s", arnPathSegment, name, lbID)),
				TagSpecifications: []*ec2.TagSpecification{
					{
						ResourceType: aws.String("network-interface"),
						Tags: []*ec2.Tag{
							{Key: aws.String(elbv2ManagedByTag), Value: aws.String(elbv2ManagedByValue)},
							{Key: aws.String(elbv2LBTag), Value: aws.String(lbArn)},
						},
					},
				},
			}
			// SG enforcement gates inbound traffic by the ENI's port-group
			// membership, which is set at ENI creation. For NLBs this is the
			// managed SG minted above; for ALBs it is the customer SGs. Empty
			// Groups intentionally falls back to the VPC default SG inside
			// eni.CreateNetworkInterface.
			if len(eniGroups) > 0 {
				eniIn.Groups = aws.StringSlice(eniGroups)
			}
			eniOut, eniErr := s.VPCService.CreateNetworkInterface(eniIn, accountID)
			if eniErr != nil {
				// Rollback: delete any ENIs already created
				for _, rollbackENI := range eniIDs {
					if _, delErr := s.VPCService.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
						NetworkInterfaceId: aws.String(rollbackENI),
					}, accountID); delErr != nil {
						slog.Error("CreateLoadBalancer: rollback failed to delete ENI", "eni", rollbackENI, "err", delErr)
					}
				}
				s.deleteNLBManagedSG(nlbManagedSGID, accountID)
				slog.Error("CreateLoadBalancer: failed to create ENI", "subnet", subnetID, "err", eniErr)
				return nil, errors.New(awserrors.ErrorELBv2SubnetNotFound)
			}

			eni := eniOut.NetworkInterface
			eniIDs = append(eniIDs, *eni.NetworkInterfaceId)

			if eni.VpcId != nil && vpcID == "" {
				vpcID = *eni.VpcId
			}
			az := ""
			if eni.AvailabilityZone != nil {
				az = *eni.AvailabilityZone
			}
			availZones = append(availZones, AvailZoneInfo{
				ZoneName: az,
				SubnetId: subnetID,
			})
		}
	}

	// Launch ALB VM with every ENI (when instance launcher is available).
	// The first ENI is the primary NIC; any additional ENIs are passed as
	// ExtraENIs so the daemon sets up a tap + QEMU NIC for each, giving the
	// ALB VM data-plane presence in every subnet the LB spans.
	launch := s.launchLBVM(lbID, scheme, eniIDs, subnets, accountID)
	albInstanceID := launch.instanceID
	albVPCIP := launch.vpcIP
	hostPorts := launch.hostPorts
	launchFailed := launch.failed
	// Record public IP on the first AZ entry for internet-facing LBs.
	if launch.publicIP != "" && len(availZones) > 0 {
		availZones[0].PublicIP = launch.publicIP
	}

	// ALB starts in provisioning until the agent inside the VM connects and
	// responds to a ping. If no VM was launched (no launcher/AMI or launch
	// failed), set state to failed so the API reflects the broken data-plane.
	state := StateActive
	if albInstanceID != "" {
		state = StateProvisioning
		// Internal LBs only leave provisioning once the lb-agent heartbeats,
		// which needs the mgmt return route. If it can't be resolved the LB
		// hangs in provisioning forever — fail loud instead so the broken
		// return path is visible immediately rather than as a generic timeout.
		if scheme == SchemeInternal {
			if gw, tgt := s.resolveMgmtRoute(scheme); gw == "" || tgt == "" {
				slog.Error("CreateLoadBalancer: internal LB has no mgmt return route; marking failed (lb-agent cannot heartbeat AWSGW)",
					"lbId", lbID,
					"mgmtBridgeIP", s.MgmtBridgeIP,
					"advertiseIP", s.AdvertiseIP)
				state = StateFailed
			}
		}
	} else if launchFailed {
		state = StateFailed
	}

	// Attributes are intentionally left nil — DescribeLoadBalancerAttributes
	// derives per-type defaults from lb.Type via DefaultLoadBalancerAttributes.
	// Callers that want a non-default value must call ModifyLoadBalancerAttributes.
	record := &LoadBalancerRecord{
		LoadBalancerArn: lbArn,
		LoadBalancerID:  lbID,
		DNSName:         dnsName,
		Name:            name,
		Scheme:          scheme,
		Type:            lbType,
		State:           state,
		VpcId:           vpcID,
		SecurityGroups:  securityGroups,
		NLBManagedSGID:  nlbManagedSGID,
		Subnets:         subnets,
		AvailZones:      availZones,
		ENIs:            eniIDs,
		InstanceID:      albInstanceID,
		VPCIP:           albVPCIP,
		HostPorts:       hostPorts,
		IPAddressType:   IPAddressTypeIPv4,
		NodeID:          s.nodeID,
		Tags:            tags,
		AccountID:       accountID,
		CreatedAt:       time.Now().UTC(),
	}

	if err := s.store.PutLoadBalancer(record); err != nil {
		slog.Error("CreateLoadBalancer: failed to persist record", "lbId", lbID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Agent heartbeat will transition provisioning → active on first contact.

	slog.Info("CreateLoadBalancer completed", "name", name, "lbArn", lbArn, "enis", len(eniIDs), "state", state, "accountID", accountID)

	return &elbv2.CreateLoadBalancerOutput{
		LoadBalancers: []*elbv2.LoadBalancer{s.lbRecordToSDK(record)},
	}, nil
}

func (s *ELBv2ServiceImpl) DeleteLoadBalancer(input *elbv2.DeleteLoadBalancerInput, accountID string) (*elbv2.DeleteLoadBalancerOutput, error) {
	if input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	lb, err := s.store.GetLoadBalancerByArn(*input.LoadBalancerArn)
	if err != nil {
		slog.Error("DeleteLoadBalancer: failed to get LB", "arn", *input.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil || lb.AccountID != accountID {
		// Idempotent: AWS ELBv2 delete returns success on an absent (or
		// not-owned) load balancer, so tofu destroy retries converge.
		return &elbv2.DeleteLoadBalancerOutput{}, nil
	}

	// Delete all listeners (and their rules) for this LB before tearing down
	// backing artifacts. Cascade through the shared helper, not
	// store.DeleteListener directly, so rules don't survive and pin their
	// target groups as ResourceInUse after the LB is gone. A list or cascade
	// failure aborts the delete with the record intact, so tofu retries
	// converge instead of leaving orphaned rules behind.
	listeners, err := s.store.ListListenersByLB(lb.LoadBalancerArn)
	if err != nil {
		slog.Error("DeleteLoadBalancer: failed to list listeners for cascade", "lbArn", lb.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	for _, l := range listeners {
		if err := s.deleteListenerCascade(l); err != nil {
			slog.Error("DeleteLoadBalancer: failed to cascade listener delete", "listenerID", l.ListenerID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	// Terminate ALB VM in background (VM termination also cleans up tap device).
	// Must be async because VM shutdown can take seconds (QMP powerdown + QEMU exit)
	// and we don't want to block the NATS request handler.
	if lb.InstanceID != "" && s.InstanceLauncher != nil {
		instanceID := lb.InstanceID
		go func() {
			if err := s.InstanceLauncher.TerminateSystemInstance(instanceID); err != nil {
				slog.Warn("Failed to terminate ALB VM during LB deletion", "lbId", lb.LoadBalancerID, "instanceId", instanceID, "err", err)
			}
		}()
	}

	// Delete system-managed ENIs. Detach first to clear in-use status.
	if s.VPCService != nil {
		for _, eniID := range lb.ENIs {
			if detachErr := s.VPCService.DetachENI(accountID, eniID); detachErr != nil {
				slog.Warn("Failed to detach ALB ENI during cleanup", "eniId", eniID, "err", detachErr)
			}
			if _, eniErr := s.VPCService.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: aws.String(eniID),
			}, accountID); eniErr != nil {
				slog.Warn("Failed to delete ALB ENI during cleanup", "eniId", eniID, "err", eniErr)
			}
		}
		// The managed NLB SG can only be removed once its ENIs are gone.
		s.deleteNLBManagedSG(lb.NLBManagedSGID, accountID)
	}

	if err := s.store.DeleteLoadBalancer(lb.LoadBalancerID); err != nil {
		slog.Error("DeleteLoadBalancer: failed to delete record", "lbId", lb.LoadBalancerID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("DeleteLoadBalancer completed", "lbArn", *input.LoadBalancerArn, "enis", len(lb.ENIs), "accountID", accountID)

	return &elbv2.DeleteLoadBalancerOutput{}, nil
}

func (s *ELBv2ServiceImpl) DescribeLoadBalancers(input *elbv2.DescribeLoadBalancersInput, accountID string) (*elbv2.DescribeLoadBalancersOutput, error) {
	allLBs, err := s.store.ListLoadBalancers()
	if err != nil {
		slog.Error("DescribeLoadBalancers: failed to list LBs", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Build filter sets
	arnFilter := make(map[string]bool)
	for _, arn := range input.LoadBalancerArns {
		if arn != nil {
			arnFilter[*arn] = true
		}
	}
	nameFilter := make(map[string]bool)
	for _, name := range input.Names {
		if name != nil {
			nameFilter[*name] = true
		}
	}

	result := make([]*elbv2.LoadBalancer, 0)
	for _, lb := range allLBs {
		if lb.AccountID != accountID {
			continue
		}
		if len(arnFilter) > 0 && !arnFilter[lb.LoadBalancerArn] {
			continue
		}
		if len(nameFilter) > 0 && !nameFilter[lb.Name] {
			continue
		}
		result = append(result, s.lbRecordToSDK(lb))
	}

	return &elbv2.DescribeLoadBalancersOutput{
		LoadBalancers: result,
	}, nil
}

// --- Target Group operations ---

func (s *ELBv2ServiceImpl) CreateTargetGroup(input *elbv2.CreateTargetGroupInput, accountID string) (*elbv2.CreateTargetGroupOutput, error) {
	if input.Name == nil || *input.Name == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	name := *input.Name

	protocol := ProtocolHTTP
	if input.Protocol != nil && *input.Protocol != "" {
		protocol = *input.Protocol
	}

	// Validate protocol.
	switch protocol {
	case ProtocolHTTP, ProtocolHTTPS, ProtocolTCP, ProtocolUDP, ProtocolTLS, ProtocolTCPUDP:
		// valid
	default:
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	port := int64(80)
	if input.Port != nil {
		port = *input.Port
	}

	vpcID := ""
	if input.VpcId != nil {
		vpcID = *input.VpcId
	}

	// Check duplicate name within VPC
	existing, err := s.store.GetTargetGroupByName(name, vpcID)
	if err != nil {
		slog.Error("CreateTargetGroup: failed to check duplicate name", "name", name, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if existing != nil {
		return nil, errors.New(awserrors.ErrorELBv2DuplicateTargetGroup)
	}

	targetType := TargetTypeInstance
	if input.TargetType != nil && *input.TargetType != "" {
		targetType = *input.TargetType
	}
	if targetType != TargetTypeInstance && targetType != TargetTypeIP {
		slog.Warn("CreateTargetGroup: unsupported target type", "targetType", targetType)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Use NLB health check defaults for NLB protocols.
	var hc HealthCheckConfig
	switch protocol {
	case ProtocolTCP, ProtocolUDP, ProtocolTLS, ProtocolTCPUDP:
		hc = DefaultNLBHealthCheck()
	default:
		hc = DefaultHealthCheck()
	}
	if input.HealthCheckEnabled != nil {
		hc.Enabled = *input.HealthCheckEnabled
	}
	if input.HealthCheckProtocol != nil {
		hc.Protocol = *input.HealthCheckProtocol
	}
	if input.HealthCheckPort != nil {
		hc.Port = *input.HealthCheckPort
	}
	if input.HealthCheckPath != nil {
		if err := validateHealthCheckPath(*input.HealthCheckPath); err != nil {
			return nil, err
		}
		hc.Path = *input.HealthCheckPath
	}
	if input.HealthCheckIntervalSeconds != nil {
		hc.IntervalSeconds = *input.HealthCheckIntervalSeconds
	}
	if input.HealthCheckTimeoutSeconds != nil {
		hc.TimeoutSeconds = *input.HealthCheckTimeoutSeconds
	}
	if input.HealthyThresholdCount != nil {
		hc.HealthyThreshold = *input.HealthyThresholdCount
	}
	if input.UnhealthyThresholdCount != nil {
		hc.UnhealthyThreshold = *input.UnhealthyThresholdCount
	}
	if input.Matcher != nil && input.Matcher.HttpCode != nil {
		if err := validateHealthCheckMatcher(*input.Matcher.HttpCode); err != nil {
			return nil, err
		}
		hc.Matcher = *input.Matcher.HttpCode
	}

	tgID := utils.GenerateResourceID("tg")
	tgArn := buildTGArn(s.region, accountID, name, tgID)

	tags := tagsFromSDK(input.Tags)

	record := &TargetGroupRecord{
		TargetGroupArn: tgArn,
		TargetGroupID:  tgID,
		Name:           name,
		Protocol:       protocol,
		Port:           port,
		VpcId:          vpcID,
		TargetType:     targetType,
		HealthCheck:    hc,
		Tags:           tags,
		AccountID:      accountID,
		CreatedAt:      time.Now().UTC(),
	}

	if err := s.store.PutTargetGroup(record); err != nil {
		slog.Error("CreateTargetGroup: failed to persist record", "tgId", tgID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateTargetGroup completed", "name", name, "tgArn", tgArn, "accountID", accountID)

	return &elbv2.CreateTargetGroupOutput{
		TargetGroups: []*elbv2.TargetGroup{s.tgRecordToSDK(record)},
	}, nil
}

func (s *ELBv2ServiceImpl) ModifyTargetGroup(input *elbv2.ModifyTargetGroupInput, accountID string) (*elbv2.ModifyTargetGroupOutput, error) {
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	tg, err := s.store.GetTargetGroupByArn(*input.TargetGroupArn)
	if err != nil {
		slog.Error("ModifyTargetGroup: failed to get TG", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if tg == nil || tg.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	}

	hc := tg.HealthCheck
	if input.HealthCheckEnabled != nil {
		hc.Enabled = *input.HealthCheckEnabled
	}
	if input.HealthCheckProtocol != nil {
		hc.Protocol = *input.HealthCheckProtocol
	}
	if input.HealthCheckPort != nil {
		hc.Port = *input.HealthCheckPort
	}
	if input.HealthCheckPath != nil {
		if err := validateHealthCheckPath(*input.HealthCheckPath); err != nil {
			return nil, err
		}
		hc.Path = *input.HealthCheckPath
	}
	if input.HealthCheckIntervalSeconds != nil {
		hc.IntervalSeconds = *input.HealthCheckIntervalSeconds
	}
	if input.HealthCheckTimeoutSeconds != nil {
		hc.TimeoutSeconds = *input.HealthCheckTimeoutSeconds
	}
	if input.HealthyThresholdCount != nil {
		hc.HealthyThreshold = *input.HealthyThresholdCount
	}
	if input.UnhealthyThresholdCount != nil {
		hc.UnhealthyThreshold = *input.UnhealthyThresholdCount
	}
	if input.Matcher != nil && input.Matcher.HttpCode != nil {
		if err := validateHealthCheckMatcher(*input.Matcher.HttpCode); err != nil {
			return nil, err
		}
		hc.Matcher = *input.Matcher.HttpCode
	}
	tg.HealthCheck = hc

	if err := s.store.PutTargetGroup(tg); err != nil {
		slog.Error("ModifyTargetGroup: failed to persist record", "arn", tg.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("ModifyTargetGroup completed", "arn", tg.TargetGroupArn, "accountID", accountID)

	return &elbv2.ModifyTargetGroupOutput{
		TargetGroups: []*elbv2.TargetGroup{s.tgRecordToSDK(tg)},
	}, nil
}

func (s *ELBv2ServiceImpl) DeleteTargetGroup(input *elbv2.DeleteTargetGroupInput, accountID string) (*elbv2.DeleteTargetGroupOutput, error) {
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	tg, err := s.store.GetTargetGroupByArn(*input.TargetGroupArn)
	if err != nil {
		slog.Error("DeleteTargetGroup: failed to get TG", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if tg == nil || tg.AccountID != accountID {
		// Idempotent: AWS ELBv2 delete returns success on an absent (or
		// not-owned) target group, so tofu destroy retries converge.
		return &elbv2.DeleteTargetGroupOutput{}, nil
	}

	// Only a *live* listener or rule pins the target group. A listener (or
	// rule) whose owning load balancer no longer exists is treated as already
	// torn down, not a live reference, so an orphan left by a partial teardown
	// can never pin this target group as ResourceInUse permanently.
	lbs, err := s.store.ListLoadBalancers()
	if err != nil {
		slog.Error("DeleteTargetGroup: failed to list load balancers for in-use check", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	liveLB := make(map[string]bool, len(lbs))
	for _, lb := range lbs {
		liveLB[lb.LoadBalancerArn] = true
	}

	// Check if any live listener references this target group.
	listeners, err := s.store.ListListeners()
	if err != nil {
		slog.Error("DeleteTargetGroup: failed to list listeners for in-use check", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	liveListener := make(map[string]bool, len(listeners))
	for _, l := range listeners {
		if !liveLB[l.LoadBalancerArn] {
			continue
		}
		liveListener[l.ListenerArn] = true
		for _, action := range l.DefaultActions {
			if action.TargetGroupArn == tg.TargetGroupArn {
				return nil, errors.New(awserrors.ErrorELBv2TargetGroupInUse)
			}
		}
	}

	// Block deletion when a live rule still forwards to the target group.
	allRules, err := s.store.ListRules()
	if err != nil {
		slog.Error("DeleteTargetGroup: failed to list rules for in-use check", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	for _, r := range allRules {
		if !liveListener[r.ListenerArn] {
			continue
		}
		for _, action := range r.Actions {
			if action.TargetGroupArn == tg.TargetGroupArn {
				return nil, errors.New(awserrors.ErrorELBv2TargetGroupInUse)
			}
		}
	}

	if err := s.store.DeleteTargetGroup(tg.TargetGroupID); err != nil {
		slog.Error("DeleteTargetGroup: failed to delete record", "tgId", tg.TargetGroupID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("DeleteTargetGroup completed", "tgArn", *input.TargetGroupArn, "accountID", accountID)

	return &elbv2.DeleteTargetGroupOutput{}, nil
}

func (s *ELBv2ServiceImpl) DescribeTargetGroups(input *elbv2.DescribeTargetGroupsInput, accountID string) (*elbv2.DescribeTargetGroupsOutput, error) {
	allTGs, err := s.store.ListTargetGroups()
	if err != nil {
		slog.Error("DescribeTargetGroups: failed to list TGs", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	arnFilter := make(map[string]bool)
	for _, arn := range input.TargetGroupArns {
		if arn != nil {
			arnFilter[*arn] = true
		}
	}
	nameFilter := make(map[string]bool)
	for _, name := range input.Names {
		if name != nil {
			nameFilter[*name] = true
		}
	}

	result := make([]*elbv2.TargetGroup, 0)
	for _, tg := range allTGs {
		if tg.AccountID != accountID {
			continue
		}
		if len(arnFilter) > 0 && !arnFilter[tg.TargetGroupArn] {
			continue
		}
		if len(nameFilter) > 0 && !nameFilter[tg.Name] {
			continue
		}
		// Filter by LB ARN if specified
		if input.LoadBalancerArn != nil && *input.LoadBalancerArn != "" {
			// Check if any listener on this LB references this TG
			listeners, _ := s.store.ListListenersByLB(*input.LoadBalancerArn)
			found := false
			for _, l := range listeners {
				for _, a := range l.DefaultActions {
					if a.TargetGroupArn == tg.TargetGroupArn {
						found = true
					}
				}
			}
			if !found {
				continue
			}
		}
		result = append(result, s.tgRecordToSDK(tg))
	}

	return &elbv2.DescribeTargetGroupsOutput{
		TargetGroups: result,
	}, nil
}

// --- Target registration ---

func (s *ELBv2ServiceImpl) RegisterTargets(input *elbv2.RegisterTargetsInput, accountID string) (*elbv2.RegisterTargetsOutput, error) {
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	tg, err := s.store.GetTargetGroupByArn(*input.TargetGroupArn)
	if err != nil {
		slog.Error("RegisterTargets: failed to get TG", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if tg == nil {
		return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	}

	// Build map of existing targets for dedup
	existing := make(map[string]int) // id:port -> index
	for i, t := range tg.Targets {
		key := fmt.Sprintf("%s:%d", t.Id, t.Port)
		existing[key] = i
	}

	for _, td := range input.Targets {
		if td.Id == nil {
			continue
		}
		port := tg.Port
		if td.Port != nil {
			port = *td.Port
		}
		key := fmt.Sprintf("%s:%d", *td.Id, port)
		if _, exists := existing[key]; exists {
			continue // Already registered
		}

		// Resolve target ID → private IP (instance ENI lookup, or raw IP for ip-type TGs)
		privateIP, err := s.resolveRegisteredTargetIP(tg.TargetType, *td.Id, accountID)
		if err != nil {
			return nil, err
		}

		tg.Targets = append(tg.Targets, Target{
			Id:          *td.Id,
			Port:        port,
			HealthState: TargetHealthInitial,
			HealthDesc:  "Target registration is in progress",
			PrivateIP:   privateIP,
		})
	}

	if err := s.store.PutTargetGroup(tg); err != nil {
		slog.Error("RegisterTargets: failed to persist TG", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Reload HAProxy for any LBs that reference this target group
	if err := s.updateStoredConfigForTargetGroup(tg.TargetGroupArn); err != nil {
		slog.Error("RegisterTargets: failed to update config", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("RegisterTargets completed", "tgArn", *input.TargetGroupArn, "targetsAdded", len(input.Targets), "accountID", accountID)

	return &elbv2.RegisterTargetsOutput{}, nil
}

func (s *ELBv2ServiceImpl) DeregisterTargets(input *elbv2.DeregisterTargetsInput, accountID string) (*elbv2.DeregisterTargetsOutput, error) {
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	tg, err := s.store.GetTargetGroupByArn(*input.TargetGroupArn)
	if err != nil {
		slog.Error("DeregisterTargets: failed to get TG", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if tg == nil {
		return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	}

	// Build removal set
	removeSet := make(map[string]bool)
	for _, td := range input.Targets {
		if td.Id == nil {
			continue
		}
		port := tg.Port
		if td.Port != nil {
			port = *td.Port
		}
		removeSet[fmt.Sprintf("%s:%d", *td.Id, port)] = true
	}

	var remaining []Target
	for _, t := range tg.Targets {
		key := fmt.Sprintf("%s:%d", t.Id, t.Port)
		if removeSet[key] {
			s.hc.removeTarget(tg.TargetGroupID, t.Id, t.Port)
		} else {
			remaining = append(remaining, t)
		}
	}
	tg.Targets = remaining

	if err := s.store.PutTargetGroup(tg); err != nil {
		slog.Error("DeregisterTargets: failed to persist TG", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Reload HAProxy for any LBs that reference this target group
	if err := s.updateStoredConfigForTargetGroup(tg.TargetGroupArn); err != nil {
		slog.Error("DeregisterTargets: failed to update config", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("DeregisterTargets completed", "tgArn", *input.TargetGroupArn, "targetsRemoved", len(input.Targets), "accountID", accountID)

	return &elbv2.DeregisterTargetsOutput{}, nil
}

func (s *ELBv2ServiceImpl) DescribeTargetHealth(input *elbv2.DescribeTargetHealthInput, accountID string) (*elbv2.DescribeTargetHealthOutput, error) {
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	tg, err := s.store.GetTargetGroupByArn(*input.TargetGroupArn)
	if err != nil {
		slog.Error("DescribeTargetHealth: failed to get TG", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if tg == nil {
		return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	}

	// Optional: filter to specific targets
	targetFilter := make(map[string]bool)
	for _, td := range input.Targets {
		if td.Id != nil {
			targetFilter[*td.Id] = true
		}
	}

	descriptions := make([]*elbv2.TargetHealthDescription, 0)
	for _, t := range tg.Targets {
		if len(targetFilter) > 0 && !targetFilter[t.Id] {
			continue
		}

		desc := &elbv2.TargetHealthDescription{
			Target: &elbv2.TargetDescription{
				Id:   aws.String(t.Id),
				Port: aws.Int64(t.Port),
			},
			TargetHealth: &elbv2.TargetHealth{
				State:       aws.String(t.HealthState),
				Description: aws.String(t.HealthDesc),
			},
		}
		descriptions = append(descriptions, desc)
	}

	return &elbv2.DescribeTargetHealthOutput{
		TargetHealthDescriptions: descriptions,
	}, nil
}

// --- Listener operations ---

// listenerActionFromSDK converts an SDK listener action into the stored form,
// preserving fixed-response config so HAProxy generation and tofu read-back see
// the full action (a forward-only conversion drops the fixed-response default
// and produces a dangling backend + perpetual plan diff).
func listenerActionFromSDK(a *elbv2.Action) ListenerAction {
	action := ListenerAction{}
	if a.Type != nil {
		action.Type = *a.Type
	}
	if a.TargetGroupArn != nil {
		action.TargetGroupArn = *a.TargetGroupArn
	}
	if a.FixedResponseConfig != nil {
		fr := &FixedResponseAction{}
		if a.FixedResponseConfig.StatusCode != nil {
			fr.StatusCode = *a.FixedResponseConfig.StatusCode
		}
		if a.FixedResponseConfig.ContentType != nil {
			fr.ContentType = *a.FixedResponseConfig.ContentType
		}
		if a.FixedResponseConfig.MessageBody != nil {
			fr.MessageBody = *a.FixedResponseConfig.MessageBody
		}
		action.FixedResponse = fr
	}
	if a.RedirectConfig != nil {
		rd := &RedirectAction{}
		if a.RedirectConfig.Protocol != nil {
			rd.Protocol = *a.RedirectConfig.Protocol
		}
		if a.RedirectConfig.Host != nil {
			rd.Host = *a.RedirectConfig.Host
		}
		if a.RedirectConfig.Port != nil {
			rd.Port = *a.RedirectConfig.Port
		}
		if a.RedirectConfig.Path != nil {
			rd.Path = *a.RedirectConfig.Path
		}
		if a.RedirectConfig.Query != nil {
			rd.Query = *a.RedirectConfig.Query
		}
		if a.RedirectConfig.StatusCode != nil {
			rd.StatusCode = *a.RedirectConfig.StatusCode
		}
		action.Redirect = rd
	}
	return action
}

// validateListenerAction enforces the per-type action contract for listener
// default actions and rule actions. Forward actions are validated against the
// target group elsewhere; this covers redirect (status code + render-safe
// fields) so invalid input fails at the API instead of being silently
// defaulted by the renderer.
func validateListenerAction(a ListenerAction) error {
	if a.Type == ActionTypeRedirect {
		if a.Redirect == nil {
			return errors.New(awserrors.ErrorMissingParameter)
		}
		return validateRedirectAction(a.Redirect)
	}
	return nil
}

// validateRedirectAction rejects an unsupported status code or any field that
// would break the HAProxy redirect directive once the known AWS placeholders
// are stripped.
func validateRedirectAction(rd *RedirectAction) error {
	if rd == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	switch rd.StatusCode {
	case "HTTP_301", "HTTP_302":
	default:
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	for _, f := range []string{rd.Protocol, rd.Host, rd.Port, rd.Path, rd.Query} {
		if !validRedirectField(f) {
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	return nil
}

// buildListenerCertificates validates and normalises the certificate set and
// SSL policy for a listener of the given protocol. Secure protocols (HTTPS,
// TLS) require at least one certificate and receive a default SslPolicy when
// unset; non-secure protocols must carry neither certificates nor an SslPolicy.
// Exactly one certificate is marked default.
func buildListenerCertificates(protocol string, in []*elbv2.Certificate, sslPolicy *string) ([]ListenerCertificate, string, error) {
	policy := ""
	if sslPolicy != nil {
		policy = *sslPolicy
	}

	if !protocolRequiresCert(protocol) {
		if len(in) > 0 || policy != "" {
			return nil, "", errors.New(awserrors.ErrorInvalidParameterValue)
		}
		return nil, "", nil
	}

	if len(in) == 0 {
		return nil, "", errors.New(awserrors.ErrorELBv2CertificateNotFound)
	}

	certs := make([]ListenerCertificate, 0, len(in))
	defaults := 0
	for _, c := range in {
		if c == nil || c.CertificateArn == nil || *c.CertificateArn == "" {
			return nil, "", errors.New(awserrors.ErrorInvalidParameterValue)
		}
		isDefault := c.IsDefault != nil && *c.IsDefault
		if isDefault {
			defaults++
		}
		certs = append(certs, ListenerCertificate{
			CertificateArn: *c.CertificateArn,
			IsDefault:      isDefault,
		})
	}
	switch defaults {
	case 0:
		certs[0].IsDefault = true
	case 1:
	default:
		return nil, "", errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if policy == "" {
		policy = DefaultSslPolicy
	} else if !isKnownSslPolicy(policy) {
		return nil, "", errors.New(awserrors.ErrorELBv2SSLPolicyNotFound)
	}

	return certs, policy, nil
}

func (s *ELBv2ServiceImpl) CreateListener(input *elbv2.CreateListenerInput, accountID string) (*elbv2.CreateListenerOutput, error) {
	if input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.DefaultActions) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	lb, err := s.store.GetLoadBalancerByArn(*input.LoadBalancerArn)
	if err != nil {
		slog.Error("CreateListener: failed to get LB", "arn", *input.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil {
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}

	protocol := ProtocolHTTP
	if input.Protocol != nil && *input.Protocol != "" {
		protocol = *input.Protocol
	}

	// Validate protocol is appropriate for the LB type.
	switch lb.Type {
	case LoadBalancerTypeNetwork:
		switch protocol {
		case ProtocolTCP, ProtocolUDP, ProtocolTLS, ProtocolTCPUDP:
			// valid NLB protocols
		default:
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	default: // application
		switch protocol {
		case ProtocolHTTP, ProtocolHTTPS:
			// valid ALB protocols
		default:
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	port := int64(80)
	if input.Port != nil {
		port = *input.Port
	}

	// Check for duplicate listener on same port
	existingListeners, err := s.store.ListListenersByLB(lb.LoadBalancerArn)
	if err != nil {
		slog.Error("CreateListener: failed to list existing listeners", "lbArn", lb.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	for _, l := range existingListeners {
		if l.Port == port {
			return nil, errors.New(awserrors.ErrorELBv2DuplicateListener)
		}
	}

	// Validate listener-to-target-group protocol compatibility.
	for _, a := range input.DefaultActions {
		if a.Type != nil && *a.Type == ActionTypeForward && a.TargetGroupArn != nil {
			tg, tgErr := s.store.GetTargetGroupByArn(*a.TargetGroupArn)
			if tgErr != nil {
				slog.Error("CreateListener: failed to get target group", "arn", *a.TargetGroupArn, "err", tgErr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			if tg == nil {
				return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
			}
			if !isCompatibleProtocol(protocol, tg.Protocol) {
				return nil, errors.New(awserrors.ErrorInvalidParameterValue)
			}
		}
	}

	certs, sslPolicy, err := buildListenerCertificates(protocol, input.Certificates, input.SslPolicy)
	if err != nil {
		return nil, err
	}
	if err := s.validateListenerCerts(certs, accountID); err != nil {
		return nil, err
	}

	listenerID := utils.GenerateResourceID("lst")
	listenerArn := buildListenerArn(s.region, accountID, lb.Name, lb.LoadBalancerID, listenerID, lb.Type)

	var actions []ListenerAction
	for _, a := range input.DefaultActions {
		action := listenerActionFromSDK(a)
		if err := validateListenerAction(action); err != nil {
			return nil, err
		}
		actions = append(actions, action)
	}

	tags := tagsFromSDK(input.Tags)

	record := &ListenerRecord{
		ListenerArn:     listenerArn,
		ListenerID:      listenerID,
		LoadBalancerArn: lb.LoadBalancerArn,
		Protocol:        protocol,
		Port:            port,
		DefaultActions:  actions,
		AccountID:       accountID,
		CreatedAt:       time.Now().UTC(),
		Certificates:    certs,
		SslPolicy:       sslPolicy,
		Tags:            tags,
	}

	if err := s.store.PutListener(record); err != nil {
		slog.Error("CreateListener: failed to persist record", "listenerId", listenerID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Open the listener port on the NLB's managed front-end SG so inbound
	// traffic from the configured client CIDRs is admitted by the OVN ACL
	// (NLB ENIs would otherwise sit in the intra-SG-only default SG).
	if lb.Type == LoadBalancerTypeNetwork && lb.NLBManagedSGID != "" && s.VPCService != nil {
		cidrs, cidrErr := s.resolveNLBIngressCIDRs(lb)
		if cidrErr != nil {
			slog.Error("CreateListener: resolve ingress CIDRs failed", "lbArn", lb.LoadBalancerArn, "err", cidrErr)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if authErr := s.authorizeNLBListenerPort(lb, protocol, port, cidrs, accountID); authErr != nil {
			slog.Error("CreateListener: authorize listener port failed", "lbArn", lb.LoadBalancerArn, "port", port, "err", authErr)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	// Start or reload HAProxy now that a listener exists
	if err := s.updateStoredConfig(lb); err != nil {
		slog.Error("CreateListener: failed to update config", "listenerId", listenerID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateListener completed", "listenerArn", listenerArn, "lbArn", lb.LoadBalancerArn, "port", port, "accountID", accountID)

	return &elbv2.CreateListenerOutput{
		Listeners: []*elbv2.Listener{s.listenerRecordToSDK(record)},
	}, nil
}

// deleteListenerCascade removes a listener and all of its rules. Shared by
// DeleteListener and DeleteLoadBalancer so LB teardown never bypasses the rule
// cascade and leaves orphan rules that pin a target group as ResourceInUse.
func (s *ELBv2ServiceImpl) deleteListenerCascade(listener *ListenerRecord) error {
	rules, err := s.store.ListRulesByListener(listener.ListenerArn)
	if err != nil {
		return fmt.Errorf("list rules for listener %s: %w", listener.ListenerArn, err)
	}
	for _, r := range rules {
		if err := s.store.DeleteRule(r.RuleID); err != nil {
			return fmt.Errorf("delete rule %s: %w", r.RuleID, err)
		}
	}
	if err := s.store.DeleteListener(listener.ListenerID); err != nil {
		return fmt.Errorf("delete listener %s: %w", listener.ListenerID, err)
	}
	return nil
}

func (s *ELBv2ServiceImpl) DeleteListener(input *elbv2.DeleteListenerInput, accountID string) (*elbv2.DeleteListenerOutput, error) {
	if input.ListenerArn == nil || *input.ListenerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	listener, err := s.store.GetListenerByArn(*input.ListenerArn)
	if err != nil {
		slog.Error("DeleteListener: failed to get listener", "arn", *input.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if listener == nil || listener.AccountID != accountID {
		// Idempotent: AWS ELBv2 delete returns success on an absent (or
		// not-owned) listener, so tofu destroy retries converge.
		return &elbv2.DeleteListenerOutput{}, nil
	}

	// Cascade-delete rules so a recreated listener doesn't inherit orphans.
	if err := s.deleteListenerCascade(listener); err != nil {
		slog.Error("DeleteListener: failed to cascade-delete listener", "listenerArn", listener.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Reload or stop HAProxy after listener removal
	lb, lbErr := s.store.GetLoadBalancerByArn(listener.LoadBalancerArn)
	if lbErr == nil && lb != nil {
		// Close the listener port on the NLB's managed front-end SG.
		if lb.Type == LoadBalancerTypeNetwork && lb.NLBManagedSGID != "" && s.VPCService != nil {
			if cidrs, cidrErr := s.resolveNLBIngressCIDRs(lb); cidrErr == nil {
				if revokeErr := s.revokeNLBListenerPort(lb, listener.Protocol, listener.Port, cidrs, accountID); revokeErr != nil {
					slog.Warn("DeleteListener: revoke listener port failed", "lbArn", lb.LoadBalancerArn, "port", listener.Port, "err", revokeErr)
				}
			} else {
				slog.Warn("DeleteListener: resolve ingress CIDRs failed", "lbArn", lb.LoadBalancerArn, "err", cidrErr)
			}
		}
		if err := s.updateStoredConfig(lb); err != nil {
			slog.Error("DeleteListener: failed to update config", "listenerArn", *input.ListenerArn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	slog.Info("DeleteListener completed", "listenerArn", *input.ListenerArn, "accountID", accountID)

	return &elbv2.DeleteListenerOutput{}, nil
}

func (s *ELBv2ServiceImpl) ModifyListener(input *elbv2.ModifyListenerInput, accountID string) (*elbv2.ModifyListenerOutput, error) {
	if input == nil || input.ListenerArn == nil || *input.ListenerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	listener, err := s.store.GetListenerByArn(*input.ListenerArn)
	if err != nil {
		slog.Error("ModifyListener: failed to get listener", "arn", *input.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if listener == nil || listener.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2ListenerNotFound)
	}

	lb, err := s.store.GetLoadBalancerByArn(listener.LoadBalancerArn)
	if err != nil {
		slog.Error("ModifyListener: failed to get LB", "arn", listener.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil {
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}

	updated := *listener

	if input.Protocol != nil && *input.Protocol != "" {
		proto := *input.Protocol
		switch lb.Type {
		case LoadBalancerTypeNetwork:
			switch proto {
			case ProtocolTCP, ProtocolUDP, ProtocolTLS, ProtocolTCPUDP:
			default:
				return nil, errors.New(awserrors.ErrorInvalidParameterValue)
			}
		default:
			switch proto {
			case ProtocolHTTP, ProtocolHTTPS:
			default:
				return nil, errors.New(awserrors.ErrorInvalidParameterValue)
			}
		}
		updated.Protocol = proto
	}

	if input.Port != nil {
		newPort := *input.Port
		if newPort != updated.Port {
			existingListeners, listErr := s.store.ListListenersByLB(lb.LoadBalancerArn)
			if listErr != nil {
				slog.Error("ModifyListener: failed to list existing listeners", "lbArn", lb.LoadBalancerArn, "err", listErr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			for _, l := range existingListeners {
				if l.ListenerID == updated.ListenerID {
					continue
				}
				if l.Port == newPort {
					return nil, errors.New(awserrors.ErrorELBv2DuplicateListener)
				}
			}
			updated.Port = newPort
		}
	}

	if len(input.DefaultActions) > 0 {
		var actions []ListenerAction
		for _, a := range input.DefaultActions {
			action := listenerActionFromSDK(a)
			if err := validateListenerAction(action); err != nil {
				return nil, err
			}
			if action.Type == ActionTypeForward && action.TargetGroupArn != "" {
				tg, tgErr := s.store.GetTargetGroupByArn(action.TargetGroupArn)
				if tgErr != nil {
					slog.Error("ModifyListener: failed to get target group", "arn", action.TargetGroupArn, "err", tgErr)
					return nil, errors.New(awserrors.ErrorServerInternal)
				}
				if tg == nil {
					return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
				}
				if !isCompatibleProtocol(updated.Protocol, tg.Protocol) {
					return nil, errors.New(awserrors.ErrorInvalidParameterValue)
				}
			}
			actions = append(actions, action)
		}
		updated.DefaultActions = actions
	} else if input.Protocol != nil && updated.Protocol != listener.Protocol {
		for _, a := range updated.DefaultActions {
			if a.Type != ActionTypeForward || a.TargetGroupArn == "" {
				continue
			}
			tg, tgErr := s.store.GetTargetGroupByArn(a.TargetGroupArn)
			if tgErr != nil {
				slog.Error("ModifyListener: failed to get target group", "arn", a.TargetGroupArn, "err", tgErr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			if tg == nil {
				return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
			}
			if !isCompatibleProtocol(updated.Protocol, tg.Protocol) {
				return nil, errors.New(awserrors.ErrorInvalidParameterValue)
			}
		}
	}

	// Certificates / SSL policy. The effective protocol after any change drives
	// the requirement: switching to a non-secure protocol clears cert material;
	// staying on or switching to a secure protocol validates and defaults it.
	if !protocolRequiresCert(updated.Protocol) {
		if len(input.Certificates) > 0 || (input.SslPolicy != nil && *input.SslPolicy != "") {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		updated.Certificates = nil
		updated.SslPolicy = ""
	} else {
		inCerts := input.Certificates
		if inCerts == nil {
			for _, c := range updated.Certificates {
				inCerts = append(inCerts, &elbv2.Certificate{
					CertificateArn: aws.String(c.CertificateArn),
					IsDefault:      aws.Bool(c.IsDefault),
				})
			}
		}
		sslPolicy := input.SslPolicy
		if sslPolicy == nil && updated.SslPolicy != "" {
			sslPolicy = aws.String(updated.SslPolicy)
		}
		certs, policy, certErr := buildListenerCertificates(updated.Protocol, inCerts, sslPolicy)
		if certErr != nil {
			return nil, certErr
		}
		if certErr := s.validateListenerCerts(certs, accountID); certErr != nil {
			return nil, certErr
		}
		updated.Certificates = certs
		updated.SslPolicy = policy
	}

	if err := s.store.PutListener(&updated); err != nil {
		slog.Error("ModifyListener: failed to persist record", "listenerId", updated.ListenerID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.updateStoredConfig(lb); err != nil {
		slog.Error("ModifyListener: failed to update config", "listenerArn", updated.ListenerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("ModifyListener completed", "listenerArn", updated.ListenerArn, "lbArn", lb.LoadBalancerArn, "port", updated.Port, "protocol", updated.Protocol, "accountID", accountID)

	return &elbv2.ModifyListenerOutput{
		Listeners: []*elbv2.Listener{s.listenerRecordToSDK(&updated)},
	}, nil
}

func (s *ELBv2ServiceImpl) DescribeListeners(input *elbv2.DescribeListenersInput, accountID string) (*elbv2.DescribeListenersOutput, error) {
	var listeners []*ListenerRecord
	var err error

	if input.LoadBalancerArn != nil && *input.LoadBalancerArn != "" {
		listeners, err = s.store.ListListenersByLB(*input.LoadBalancerArn)
	} else {
		listeners, err = s.store.ListListeners()
	}
	if err != nil {
		slog.Error("DescribeListeners: failed to list listeners", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Filter by ARN if specified
	arnFilter := make(map[string]bool)
	for _, arn := range input.ListenerArns {
		if arn != nil {
			arnFilter[*arn] = true
		}
	}

	result := make([]*elbv2.Listener, 0)
	for _, l := range listeners {
		if l.AccountID != accountID {
			continue
		}
		if len(arnFilter) > 0 && !arnFilter[l.ListenerArn] {
			continue
		}
		result = append(result, s.listenerRecordToSDK(l))
	}

	return &elbv2.DescribeListenersOutput{
		Listeners: result,
	}, nil
}

// --- Tag operations ---

// DescribeTags returns tags for one or more ELBv2 resources (load balancers,
// target groups, listeners, listener rules). Tag data is read from the existing
// record stores — Spinifex doesn't have a separate tag KV. Untagged resources
// return an empty Tags slice (matches AWS behaviour). Cross-account or unknown
// ARNs return the per-resource not-found error so existence isn't leaked across
// accounts.
func (s *ELBv2ServiceImpl) DescribeTags(input *elbv2.DescribeTagsInput, accountID string) (*elbv2.DescribeTagsOutput, error) {
	if input == nil || len(input.ResourceArns) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	descriptions := make([]*elbv2.TagDescription, 0, len(input.ResourceArns))
	for _, arnPtr := range input.ResourceArns {
		if arnPtr == nil || *arnPtr == "" {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		arn := *arnPtr

		resourceType, err := elbv2ResourceTypeFromArn(arn)
		if err != nil {
			return nil, err
		}

		var (
			tags          map[string]string
			ownerAccount  string
			notFoundError string
			found         bool
		)
		switch resourceType {
		case elbv2ResourceLoadBalancer:
			notFoundError = awserrors.ErrorELBv2LoadBalancerNotFound
			lb, lbErr := s.store.GetLoadBalancerByArn(arn)
			if lbErr != nil {
				slog.Error("DescribeTags: failed to get LB", "arn", arn, "err", lbErr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			if lb != nil {
				found = true
				tags = lb.Tags
				ownerAccount = lb.AccountID
			}
		case elbv2ResourceTargetGroup:
			notFoundError = awserrors.ErrorELBv2TargetGroupNotFound
			tg, tgErr := s.store.GetTargetGroupByArn(arn)
			if tgErr != nil {
				slog.Error("DescribeTags: failed to get target group", "arn", arn, "err", tgErr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			if tg != nil {
				found = true
				tags = tg.Tags
				ownerAccount = tg.AccountID
			}
		case elbv2ResourceListener:
			notFoundError = awserrors.ErrorELBv2ListenerNotFound
			l, lErr := s.store.GetListenerByArn(arn)
			if lErr != nil {
				slog.Error("DescribeTags: failed to get listener", "arn", arn, "err", lErr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			if l != nil {
				found = true
				tags = l.Tags
				ownerAccount = l.AccountID
			}
		case elbv2ResourceListenerRule:
			notFoundError = awserrors.ErrorELBv2RuleNotFound
			r, rErr := s.store.GetRuleByArn(arn)
			if rErr != nil {
				slog.Error("DescribeTags: failed to get rule", "arn", arn, "err", rErr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			if r != nil {
				found = true
				tags = r.Tags
				ownerAccount = r.AccountID
			}
		}

		if !found || ownerAccount != accountID {
			return nil, errors.New(notFoundError)
		}

		descriptions = append(descriptions, &elbv2.TagDescription{
			ResourceArn: aws.String(arn),
			Tags:        tagsMapToSDK(tags),
		})
	}

	return &elbv2.DescribeTagsOutput{
		TagDescriptions: descriptions,
	}, nil
}

// tagsFromSDK converts an SDK Tag slice into a tag map, skipping entries with a
// nil key or value. Returns a non-nil (possibly empty) map for storage.
func tagsFromSDK(tags []*elbv2.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, tag := range tags {
		if tag.Key != nil && tag.Value != nil {
			m[*tag.Key] = *tag.Value
		}
	}
	return m
}

// tagsMapToSDK converts a tag map into the SDK Tag slice with deterministic
// (sorted-by-key) ordering. Returns nil for empty/nil input so the response
// matches AWS for untagged resources.
func tagsMapToSDK(tags map[string]string) []*elbv2.Tag {
	if len(tags) == 0 {
		return nil
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]*elbv2.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, &elbv2.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	return out
}

func (s *ELBv2ServiceImpl) lbRecordToSDK(r *LoadBalancerRecord) *elbv2.LoadBalancer {
	lb := &elbv2.LoadBalancer{
		LoadBalancerArn:  aws.String(r.LoadBalancerArn),
		LoadBalancerName: aws.String(r.Name),
		DNSName:          aws.String(r.DNSName),
		Scheme:           aws.String(r.Scheme),
		Type:             aws.String(r.Type),
		IpAddressType:    aws.String(r.IPAddressType),
		CreatedTime:      aws.Time(r.CreatedAt),
		VpcId:            aws.String(r.VpcId),
		State: &elbv2.LoadBalancerState{
			Code: aws.String(r.State),
		},
	}

	for _, sg := range r.SecurityGroups {
		lb.SecurityGroups = append(lb.SecurityGroups, aws.String(sg))
	}

	// Build a subnet → private IP map by looking up each ENI. This lets each
	// AvailabilityZone entry surface the private IP of the ENI that lives in
	// its subnet, not just the public IP recorded at create time.
	privateIPBySubnet := map[string]string{}
	if s.VPCService != nil && len(r.ENIs) > 0 {
		eniPtrs := make([]*string, 0, len(r.ENIs))
		for _, eniID := range r.ENIs {
			eniPtrs = append(eniPtrs, aws.String(eniID))
		}
		result, err := s.VPCService.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: eniPtrs,
		}, r.AccountID)
		if err != nil {
			slog.Debug("lbRecordToSDK: failed to describe ALB ENIs — private IPs omitted from response",
				"lbArn", r.LoadBalancerArn, "err", err)
		} else {
			for _, eni := range result.NetworkInterfaces {
				if eni.SubnetId != nil && eni.PrivateIpAddress != nil {
					privateIPBySubnet[*eni.SubnetId] = *eni.PrivateIpAddress
				}
			}
		}
	}

	for _, az := range r.AvailZones {
		sdkAZ := &elbv2.AvailabilityZone{
			ZoneName: aws.String(az.ZoneName),
			SubnetId: aws.String(az.SubnetId),
		}
		if privateIP, ok := privateIPBySubnet[az.SubnetId]; ok && privateIP != "" {
			sdkAZ.LoadBalancerAddresses = append(sdkAZ.LoadBalancerAddresses, &elbv2.LoadBalancerAddress{
				PrivateIPv4Address: aws.String(privateIP),
			})
		}
		if az.PublicIP != "" {
			sdkAZ.LoadBalancerAddresses = append(sdkAZ.LoadBalancerAddresses, &elbv2.LoadBalancerAddress{
				IpAddress: aws.String(az.PublicIP),
			})
		}
		lb.AvailabilityZones = append(lb.AvailabilityZones, sdkAZ)
	}

	return lb
}

func (s *ELBv2ServiceImpl) tgRecordToSDK(r *TargetGroupRecord) *elbv2.TargetGroup {
	return &elbv2.TargetGroup{
		TargetGroupArn:             aws.String(r.TargetGroupArn),
		TargetGroupName:            aws.String(r.Name),
		Protocol:                   aws.String(r.Protocol),
		Port:                       aws.Int64(r.Port),
		VpcId:                      aws.String(r.VpcId),
		TargetType:                 aws.String(r.TargetType),
		HealthCheckEnabled:         aws.Bool(r.HealthCheck.Enabled),
		HealthCheckProtocol:        aws.String(r.HealthCheck.Protocol),
		HealthCheckPort:            aws.String(r.HealthCheck.Port),
		HealthCheckPath:            aws.String(r.HealthCheck.Path),
		HealthCheckIntervalSeconds: aws.Int64(r.HealthCheck.IntervalSeconds),
		HealthCheckTimeoutSeconds:  aws.Int64(r.HealthCheck.TimeoutSeconds),
		HealthyThresholdCount:      aws.Int64(r.HealthCheck.HealthyThreshold),
		UnhealthyThresholdCount:    aws.Int64(r.HealthCheck.UnhealthyThreshold),
		Matcher: &elbv2.Matcher{
			HttpCode: aws.String(r.HealthCheck.Matcher),
		},
	}
}

func (s *ELBv2ServiceImpl) listenerRecordToSDK(r *ListenerRecord) *elbv2.Listener {
	listener := &elbv2.Listener{
		ListenerArn:     aws.String(r.ListenerArn),
		LoadBalancerArn: aws.String(r.LoadBalancerArn),
		Protocol:        aws.String(r.Protocol),
		Port:            aws.Int64(r.Port),
	}

	for _, a := range r.DefaultActions {
		listener.DefaultActions = append(listener.DefaultActions, listenerActionToSDK(a))
	}

	if len(r.Certificates) > 0 {
		listener.Certificates = listenerCertsToSDK(r.Certificates)
	}
	if r.SslPolicy != "" {
		listener.SslPolicy = aws.String(r.SslPolicy)
	}

	return listener
}

// listenerActionToSDK converts a stored action into the AWS SDK shape,
// preserving forward / fixed-response / redirect detail so DescribeListeners
// and DescribeRules round-trip cleanly. Shared by listeners and rules.
func listenerActionToSDK(a ListenerAction) *elbv2.Action {
	action := &elbv2.Action{Type: aws.String(a.Type)}
	if a.TargetGroupArn != "" {
		action.TargetGroupArn = aws.String(a.TargetGroupArn)
	}
	if a.FixedResponse != nil {
		fr := &elbv2.FixedResponseActionConfig{
			StatusCode: aws.String(a.FixedResponse.StatusCode),
		}
		if a.FixedResponse.ContentType != "" {
			fr.ContentType = aws.String(a.FixedResponse.ContentType)
		}
		if a.FixedResponse.MessageBody != "" {
			fr.MessageBody = aws.String(a.FixedResponse.MessageBody)
		}
		action.FixedResponseConfig = fr
	}
	if a.Redirect != nil {
		rd := &elbv2.RedirectActionConfig{
			StatusCode: aws.String(a.Redirect.StatusCode),
		}
		if a.Redirect.Protocol != "" {
			rd.Protocol = aws.String(a.Redirect.Protocol)
		}
		if a.Redirect.Host != "" {
			rd.Host = aws.String(a.Redirect.Host)
		}
		if a.Redirect.Port != "" {
			rd.Port = aws.String(a.Redirect.Port)
		}
		if a.Redirect.Path != "" {
			rd.Path = aws.String(a.Redirect.Path)
		}
		if a.Redirect.Query != "" {
			rd.Query = aws.String(a.Redirect.Query)
		}
		action.RedirectConfig = rd
	}
	return action
}
