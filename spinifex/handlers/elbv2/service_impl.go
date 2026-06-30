package handlers_elbv2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
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
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
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

	// heartbeatPersistInterval is how often a no-op heartbeat writes to KV.
	heartbeatPersistInterval = 60 * time.Second

	// Health check fields are interpolated into the HAProxy template; restrict
	// to characters that cannot terminate or inject directives.
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

// resolveMgmtRoute returns the (gateway, target) for the lb-agent's host route to AWSGW.
// Multi-node: returns MgmtRoute{Gateway,Target}. Single-node internet-facing: no route
// (EIP provides SNAT). Single-node internal: route forced via br-mgmt IP.
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
	acmStore                   *handlers_acm.Store                    // resolves listener cert ARNs → PEM; nil-safe (HTTPS unavailable when nil)
	nc                         *nats.Conn                             // NATS connection for JetStream KV store
	VPCService                 *handlers_ec2_vpc.VPCServiceImpl       // nil-safe: ENI ops skipped when nil (e.g. in tests)
	InstanceLauncher           SystemInstanceLauncher                 // nil-safe: system VM ops skipped when nil
	IAM                        handlers_iam.SystemInstanceRoleEnsurer // nil-safe: LB VM falls back to baked static creds when nil
	SystemAccessKey            string                                 // System account access key for ALB agent SigV4 auth
	SystemSecretKey            string                                 // System account secret key for ALB agent SigV4 auth
	GatewayURL                 string                                 // AWS gateway URL for ALB agent outbound connections
	MgmtRouteGateway           string                                 // br-mgmt IP (next-hop for mgmt route); empty when AWSGW is on 0.0.0.0
	MgmtRouteTarget            string                                 // AWSGW bind IP to route via mgmt NIC
	MgmtBridgeIP               string                                 // br-mgmt IP, populated whenever br-mgmt exists (single + multi node) for the internal-scheme fallback route
	AdvertiseIP                string                                 // AdvertiseIP / WAN gateway, populated whenever set; used as the internal-scheme fallback route target on single-node
	CACert                     string                                 // PEM-encoded CA certificate delivered to microvm guests via fw_cfg
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

	// launchWG tracks in-flight LB-VM launches from CreateLoadBalancer; not awaited
	// at shutdown to avoid blocking Close on a multi-minute boot. Exposed via
	// WaitLaunches so tests can join the background launch deterministically.
	launchWG sync.WaitGroup
}

// WaitLaunches blocks until every in-flight asynchronous LB-VM launch started by
// CreateLoadBalancer has finished. Test-only join point.
func (s *ELBv2ServiceImpl) WaitLaunches() { s.launchWG.Wait() }

// NewELBv2ServiceImplWithNATS creates an ELBv2 service backed by JetStream KV.
func NewELBv2ServiceImplWithNATS(cfg *config.Config, nc *nats.Conn) (*ELBv2ServiceImpl, error) {
	store, err := NewStore(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to create ELBv2 store: %w", err)
	}

	// ACM store shares JetStream KV. Non-fatal: failure only disables HTTPS termination.
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

// ResetTargetHealthOnStartup resets every non-draining target to "initial" so
// stale "healthy" state doesn't mislead DescribeTargetHealth after a restart;
// the next lb-agent heartbeat re-evaluates and converges within one interval.
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
			// Draining state is driven by DeregisterTargets; don't clobber it.
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

// SetSystemInstanceTypeFunc sets a function that resolves the smallest available instance type.
func (s *ELBv2ServiceImpl) SetSystemInstanceTypeFunc(fn func() string) {
	s.systemInstanceTypeMu.Lock()
	defer s.systemInstanceTypeMu.Unlock()
	s.systemInstanceTypeFunc = fn
	s.systemInstanceType = ""
	s.systemInstanceTypeResolved = false
}

// getSystemInstanceType returns the instance type for system VMs, caching only non-empty results.
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

// buildLBAgentEnv returns the KEY=value blob written to /etc/conf.d/lb-agent via
// fw_cfg on the direct-boot path. Every lb-agent heartbeats over its on-link
// br-mgmt NIC (the mgmt-bridge URL): the heartbeat is control-plane and must
// survive a host reboot that strands the OVN/EIP data plane, so it never rides
// the WAN. mgmtGatewayURL falls back to the WAN URL when no mgmt bridge exists.
func (s *ELBv2ServiceImpl) buildLBAgentEnv(lbID string, staticCreds bool) string {
	// staticCreds false ⇒ no LB_ACCESS_KEY/LB_SECRET_KEY: the agent's signer
	// falls back to the AWS SDK chain, which reads IMDS instance-role creds.
	if !staticCreds {
		return fmt.Sprintf("LB_LB_ID=%s\nLB_GATEWAY_URL=%s\nLB_REGION=%s\n",
			lbID, s.mgmtGatewayURL(), s.region)
	}
	return fmt.Sprintf("LB_LB_ID=%s\nLB_GATEWAY_URL=%s\nLB_ACCESS_KEY=%s\nLB_SECRET_KEY=%s\nLB_REGION=%s\n",
		lbID, s.mgmtGatewayURL(), s.SystemAccessKey, s.SystemSecretKey, s.region)
}

// mgmtGatewayURL is the AWS gateway URL an lb-agent reaches over its on-link
// br-mgmt NIC (MgmtBridgeIP). The port is taken from the configured WAN
// GatewayURL so both paths target the same AWSGW listener. Falls back to the
// WAN URL when no mgmt bridge exists (e.g. dev-shim networking).
func (s *ELBv2ServiceImpl) mgmtGatewayURL() string {
	if s.MgmtBridgeIP == "" {
		return s.GatewayURL
	}
	port := "9999"
	if u, err := url.Parse(s.GatewayURL); err == nil && u.Port() != "" {
		port = u.Port()
	}
	return "https://" + net.JoinHostPort(s.MgmtBridgeIP, port)
}

// subnetCIDRForIP returns "ip/prefixlen" from a host IP and subnet CIDR block.
// Returns empty string on parse failure.
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

// buildMicrovmNICs constructs the NIC slice for a direct-boot microvm.
// NIC[0] is the primary VPC ENI; NIC[1] is the mgmt NIC (MAC/CIDR filled
// post-allocation); NIC[2+] are extra ENIs for multi-subnet ALBs.
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

	// NIC[1]: mgmt NIC — daemon fills MAC/CIDR. Empty return from resolveMgmtRoute
	// intentionally skips the route for single-node internet-facing LBs.
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
		// Cross-account extras carry their own AccountID; the subnet record is
		// account-keyed, so resolve under it. Empty falls back to accountID.
		extraAccount := extra.AccountID
		if extraAccount == "" {
			extraAccount = accountID
		}
		if s.VPCService != nil && extra.SubnetID != "" {
			if subnet, err := s.VPCService.GetSubnet(extraAccount, extra.SubnetID); err == nil && subnet != nil {
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

// describeENIs resolves ENI IDs in one VPC call, keyed by NetworkInterfaceId.
// Returns an empty map when the VPC service is unavailable or the call errors.
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

// buildExtraENIInputs builds ExtraENIInput for all non-primary ENIs (eniIDs[0] excluded).
// ENIs missing from eniDetails are skipped.
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

// lbVMLaunch is the outcome of booting the LB system VM. failed is true when
// the VM could not be launched.
type lbVMLaunch struct {
	instanceID string
	vpcIP      string
	publicIP   string
	hostPorts  map[int]int
	failed     bool
}

// launchLBVM boots the system VM for a load balancer. The first ENI is the
// primary NIC; extras give multi-subnet data-plane presence. No-op when no
// launcher is configured. Shared by CreateLoadBalancer and SetSubnets.
func (s *ELBv2ServiceImpl) launchLBVM(lbID, scheme string, eniIDs, subnets []string, accountID string, crossAccountENIs []ExtraENIInput) lbVMLaunch {
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

	// Same-account extras come from the (already-described) primary ENI set;
	// cross-account extras arrive fully populated (the caller created them in the
	// other account) and are not in eniDetails, so append them directly.
	extraENIInputs := buildExtraENIInputs(eniIDs, eniDetails)
	extraENIInputs = append(extraENIInputs, crossAccountENIs...)

	if s.GatewayURL == "" || s.SystemAccessKey == "" || s.SystemSecretKey == "" {
		slog.Error("launchLBVM: system credentials not configured — cannot launch LB VM", "lbId", lbID)
		res.failed = true
		return res
	}

	// Prefer IMDS instance-role creds: attach a system instance profile so the
	// lb-agent authenticates with scoped, rotating credentials and no static
	// secret rides in fw_cfg. Falls back to baked system keys when IAM is unwired.
	profileARN := s.ensureLBInstanceProfile(accountID)

	nics := s.buildMicrovmNICs(primaryIP, primaryMAC, subnets[0], eniIDs[0], scheme, extraENIInputs, accountID)
	launchInput := &SystemInstanceInput{
		InstanceType:          s.getSystemInstanceType(),
		SubnetID:              subnets[0],
		ENIID:                 eniIDs[0],
		ENIMac:                primaryMAC,
		ENIIP:                 primaryIP,
		ExtraENIs:             extraENIInputs,
		Scheme:                scheme,
		AccountID:             accountID,
		NICs:                  nics,
		LBAgentEnv:            s.buildLBAgentEnv(lbID, profileARN == ""),
		CACert:                s.CACert,
		IamInstanceProfileArn: profileARN,
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

// loadRecoveryLBIndex returns the instanceID→LB map, populated lazily.
// Errors are not cached so transient JetStream failures don't condemn recovery.
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

// RebuildSystemInstanceInput reconstructs the launch input for host-reboot recovery.
// The instance→LB map is memoised so concurrent recovery candidates share one ListLoadBalancers call.
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
	// For multi-ENI LBs the VPC store must supply all MAC/IP/subnet details.
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
	// Cross-account extras live in another account and aren't in lb.ENIs/eniDetails;
	// they were persisted fully populated, so re-attach them as-is.
	extraENIs = append(extraENIs, lb.CrossAccountENIs...)

	nics := s.buildMicrovmNICs(lb.VPCIP, primaryMAC, lb.Subnets[0], lb.ENIs[0], lb.Scheme, extraENIs, lb.AccountID)
	// Re-inject mgmt NIC MAC/CIDR — buildMicrovmNICs leaves them blank.
	if len(nics) > 1 && ctx.MgmtMAC != "" {
		nics[1].MAC = ctx.MgmtMAC
		if ctx.MgmtIP != "" {
			nics[1].CIDR = ctx.MgmtIP + "/24"
		}
	}

	// Re-ensure the instance profile so a recovered LB VM keeps IMDS creds; the
	// ensure is idempotent and converges on the existing role/profile.
	profileARN := s.ensureLBInstanceProfile(lb.AccountID)

	return &SystemInstanceInput{
		InstanceType:          ctx.InstanceType,
		SubnetID:              lb.Subnets[0],
		ENIID:                 lb.ENIs[0],
		ENIMac:                primaryMAC,
		ENIIP:                 lb.VPCIP,
		ExtraENIs:             extraENIs,
		Scheme:                lb.Scheme,
		AccountID:             lb.AccountID,
		NICs:                  nics,
		LBAgentEnv:            s.buildLBAgentEnv(lb.LoadBalancerID, profileARN == ""),
		CACert:                s.CACert,
		IamInstanceProfileArn: profileARN,
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

// updateStoredConfig generates the data-plane config, hashes it, and stores it
// on the LB record. The agent fetches it on the next heartbeat when the hash changes.
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
	// NLBs: the agent actively probes targets (nginx has no upstream probing).
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

// resolveListenerCerts resolves each distinct certificate ARN to its combined PEM.
// Returns nil for HTTP-only listeners.
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

// resolveCertPEM loads a certificate from ACM and returns its combined PEM
// (leaf + chain + key). Cross-account certs are treated as absent.
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

// validateListenerCerts confirms every certificate ARN resolves in the ACM store
// and is owned by the account. Rejects at the API boundary to avoid silently
// freezing data-plane convergence at config-render time.
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

// configCertHash hashes the config and all cert files so a cert rotation triggers
// an agent reload even when the config text is unchanged.
func configCertHash(configContent string, certFiles map[string]string) string {
	h := sha256.New()
	h.Write([]byte(configContent))
	paths := slices.Sorted(maps.Keys(certFiles))
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

	// Rules may forward to a TG even if the listener default action does not.
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

// LBAgentHeartbeat processes a heartbeat from an LB agent. On first heartbeat
// transitions LB provisioning→active, processes health, and returns the config hash.
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
		// Log to distinguish a stuck-in-provisioning LB from one whose heartbeat never arrived.
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
	heartbeatStale := now.Sub(lb.LastHeartbeat) >= heartbeatPersistInterval
	lb.LastHeartbeat = now

	switch {
	case stateChanged:
		// Build data-plane config on activation: reactive calls during create
		// no-op while InstanceID is empty, so we build it now from existing listeners/targets.
		if err := s.updateStoredConfig(lb); err != nil {
			slog.Error("LBAgentHeartbeat: failed to build config on activation", "lbId", lbID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	case heartbeatStale:
		// Refresh the heartbeat timestamp only; avoid writing the full record on every tick.
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

// certFilesToSDK converts the stored path→PEM map into a sorted CertFile slice.
func certFilesToSDK(certFiles map[string]string) []*CertFile {
	if len(certFiles) == 0 {
		return nil
	}
	paths := slices.Sorted(maps.Keys(certFiles))
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

// resolveRegisteredTargetIP returns the private IP for a target being registered.
// For ip-type targets the ID is the IP; for instance targets it is resolved via
// the primary ENI. Returns an error if the ID doesn't match the target type.
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

// elbv2ResourceTypeFromArn extracts the resource type from an ELBv2 ARN,
// returning one of "loadbalancer", "targetgroup", "listener", "listener-rule".
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

// isCompatibleProtocol reports whether a listener protocol is compatible with a target group protocol.
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

// CreateLoadBalancer provisions the LB and returns immediately with it in
// `provisioning` while the data-plane VM boots on a background goroutine
// (267.4: an inline multi-minute boot head-of-line-blocks the shared gateway
// responder, which then times out and re-publishes the create). Callers that
// need the LB's front-end address before proceeding — e.g. EKS, which bakes the
// IP into the control-plane apiserver cert SAN — must use CreateLoadBalancerSync.
func (s *ELBv2ServiceImpl) CreateLoadBalancer(input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error) {
	return s.createLoadBalancer(input, accountID, false, nil)
}

// CreateLoadBalancerSync creates the LB and drives its data-plane launch
// synchronously, so the returned LB already carries its LoadBalancerAddresses
// (LaunchSystemInstance is a bounded request/reply that returns the allocated
// front-end IP before the multi-minute guest boot). Use only from an
// off-responder worker — the launch blocks up to the system-instance timeout, so
// running it on the shared gateway responder would reintroduce 267.4. The LB
// still flips provisioning → active on the lb-agent's first heartbeat.
func (s *ELBv2ServiceImpl) CreateLoadBalancerSync(input *elbv2.CreateLoadBalancerInput, accountID string) (*elbv2.CreateLoadBalancerOutput, error) {
	return s.createLoadBalancer(input, accountID, true, nil)
}

// CreateClusterNLBSync creates an NLB synchronously (like CreateLoadBalancerSync)
// and also threads one or more cross-account ENIs onto the LB VM at launch. EKS
// uses it to give an otherwise system-VPC cluster NLB a customer-VPC (Set A)
// front-end, so in-VPC clients and workers reach the control plane privately
// while inheriting CP-level HA from the NLB target group. Each ExtraENIInput
// carries its own AccountID (the customer account that owns the Set A ENI).
func (s *ELBv2ServiceImpl) CreateClusterNLBSync(input *elbv2.CreateLoadBalancerInput, accountID string, crossAccountENIs []ExtraENIInput) (*elbv2.CreateLoadBalancerOutput, error) {
	return s.createLoadBalancer(input, accountID, true, crossAccountENIs)
}

func (s *ELBv2ServiceImpl) createLoadBalancer(input *elbv2.CreateLoadBalancerInput, accountID string, syncLaunch bool, crossAccountENIs []ExtraENIInput) (*elbv2.CreateLoadBalancerOutput, error) {
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

	// Atomically claim the name before ENI/VM work. SDK retries lose the claim;
	// orphaned claims from crashed creates are reclaimed. Every failure releases it.
	claimOK, claimDup, claimErr := s.store.ClaimLBName(name, accountID, lbID)
	if claimErr != nil {
		slog.Error("CreateLoadBalancer: name claim failed", "name", name, "err", claimErr)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if claimDup {
		return nil, errors.New(awserrors.ErrorELBv2DuplicateLoadBalancer)
	}
	if !claimOK {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	subnets := flattenSubnetIDs(input.Subnets, input.SubnetMappings)

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
		// NLBs reject customer SGs, so mint a dedicated managed SG for all LB ENIs;
		// CreateListener opens listener ports on it. Without this, ENIs fall back
		// to the VPC default SG and inbound listener traffic is dropped.
		eniGroups := securityGroups
		if lbType == LoadBalancerTypeNetwork {
			sgID, sgErr := s.createNLBManagedSG(lbID, lbArn, subnets[0], accountID)
			if sgErr != nil {
				slog.Error("CreateLoadBalancer: failed to create managed NLB SG", "lbId", lbID, "err", sgErr)
				s.releaseLBNameClaim(name, accountID)
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
			// SG enforcement gates inbound traffic via ENI port-group membership set at
			// creation. NLBs use the managed SG; ALBs use customer SGs. Empty Groups
			// falls back to the VPC default SG inside CreateNetworkInterface.
			if len(eniGroups) > 0 {
				eniIn.Groups = aws.StringSlice(eniGroups)
			}
			eniOut, eniErr := s.VPCService.CreateNetworkInterface(eniIn, accountID)
			if eniErr != nil {
				// Rollback so a partial ENI creation doesn't leak resources.
				s.rollbackLBInfra(eniIDs, nlbManagedSGID, name, accountID)
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

	// VM boot is async to avoid blocking the gateway responder. The create
	// returns provisioning immediately; launcher-less deployments are active on the spot.
	state := StateActive
	willLaunch := s.InstanceLauncher != nil && len(eniIDs) > 0 && len(subnets) > 0
	if willLaunch {
		state = StateProvisioning
		// Internal LBs without a mgmt return route would hang in provisioning;
		// mark failed immediately so the broken path is visible.
		if scheme == SchemeInternal {
			if gw, tgt := s.resolveMgmtRoute(scheme); gw == "" || tgt == "" {
				slog.Error("CreateLoadBalancer: internal LB has no mgmt return route; marking failed (lb-agent cannot heartbeat AWSGW)",
					"lbId", lbID,
					"mgmtBridgeIP", s.MgmtBridgeIP,
					"advertiseIP", s.AdvertiseIP)
				state = StateFailed
			}
		}
	}

	// Attributes left nil — defaults derived from lb.Type on read.
	// InstanceID/VPCIP/HostPorts are filled by the async launch.
	record := &LoadBalancerRecord{
		LoadBalancerArn:  lbArn,
		LoadBalancerID:   lbID,
		DNSName:          dnsName,
		Name:             name,
		Scheme:           scheme,
		Type:             lbType,
		State:            state,
		VpcId:            vpcID,
		SecurityGroups:   securityGroups,
		NLBManagedSGID:   nlbManagedSGID,
		Subnets:          subnets,
		AvailZones:       availZones,
		ENIs:             eniIDs,
		CrossAccountENIs: crossAccountENIs,
		IPAddressType:    IPAddressTypeIPv4,
		NodeID:           s.nodeID,
		Tags:             tags,
		AccountID:        accountID,
		CreatedAt:        time.Now().UTC(),
	}

	if err := s.store.PutLoadBalancer(record); err != nil {
		slog.Error("CreateLoadBalancer: failed to persist record", "lbId", lbID, "err", err)
		// Rollback so unowned ENIs/SG don't leak.
		s.rollbackLBInfra(eniIDs, nlbManagedSGID, name, accountID)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if willLaunch {
		// Snapshot the launch inputs; the async goroutine must not read the record
		// the caller is about to return.
		lc := lbLaunchCtx{lbID: lbID, lbArn: lbArn, scheme: scheme, eniIDs: eniIDs, subnets: subnets, accountID: accountID, crossAccountENIs: crossAccountENIs}
		if syncLaunch {
			// Drive the data-plane launch inline so the returned record carries the
			// allocated front-end IP. The caller is off the gateway responder, so
			// 267.4 does not apply. provisionLBDataPlane leaves the record marked
			// failed on launch failure for diagnosis.
			if err := s.provisionLBDataPlane(lc); err != nil {
				slog.Error("CreateLoadBalancerSync: data-plane launch failed", "lbArn", lbArn, "err", err)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			launched, lerr := s.store.GetLoadBalancerByArn(lbArn)
			if lerr != nil || launched == nil {
				slog.Error("CreateLoadBalancerSync: reload after launch failed", "lbArn", lbArn, "err", lerr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			record = launched
		} else {
			s.launchWG.Go(func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("CreateLoadBalancer: async launch panic", "lbId", lc.lbID, "panic", r)
						s.markLBFailed(lc.lbArn)
					}
				}()
				s.launchLBVMAsync(lc)
			})
		}
	}

	// Agent heartbeat will transition provisioning → active on first contact.

	slog.Info("CreateLoadBalancer accepted", "name", name, "lbArn", lbArn, "enis", len(eniIDs), "state", record.State, "accountID", accountID, "sync", syncLaunch)

	return &elbv2.CreateLoadBalancerOutput{
		LoadBalancers: []*elbv2.LoadBalancer{s.lbRecordToSDK(record)},
	}, nil
}

// lbLaunchCtx carries the immutable inputs for an asynchronous LB-VM launch.
type lbLaunchCtx struct {
	lbID      string
	lbArn     string
	scheme    string
	eniIDs    []string
	subnets   []string
	accountID string
	// crossAccountENIs are extra ENIs owned by a different account than eniIDs
	// (each carries its own AccountID), threaded onto the LB VM at launch.
	crossAccountENIs []ExtraENIInput
}

// provisionLBDataPlane boots the LB-VM and folds the result into the persisted
// record: on success the instance/VPC-IP/host-ports and the per-AZ front-end IP
// are recorded and the LB stays provisioning until the lb-agent heartbeats; on
// failure the record is marked failed so the API reflects the broken data plane.
// Synchronous — launchLBVM (a bounded LaunchSystemInstance request/reply) returns
// the allocated front-end IP before the guest boot completes. Callers choose
// whether to run it inline (CreateLoadBalancerSync) or on a background goroutine
// (launchLBVMAsync).
func (s *ELBv2ServiceImpl) provisionLBDataPlane(lc lbLaunchCtx) error {
	launch := s.launchLBVM(lc.lbID, lc.scheme, lc.eniIDs, lc.subnets, lc.accountID, lc.crossAccountENIs)

	record, err := s.store.GetLoadBalancerByArn(lc.lbArn)
	if err != nil || record == nil {
		return fmt.Errorf("reload record for %s: %w", lc.lbArn, err)
	}

	if launch.failed {
		record.State = StateFailed
		if putErr := s.store.PutLoadBalancer(record); putErr != nil {
			return fmt.Errorf("persist failed state for %s: %w", lc.lbArn, putErr)
		}
		return fmt.Errorf("lb-vm launch failed for %s", lc.lbArn)
	}

	record.InstanceID = launch.instanceID
	record.VPCIP = launch.vpcIP
	record.HostPorts = launch.hostPorts
	if launch.publicIP != "" && len(record.AvailZones) > 0 {
		record.AvailZones[0].PublicIP = launch.publicIP
	}
	if putErr := s.store.PutLoadBalancer(record); putErr != nil {
		return fmt.Errorf("persist launch result for %s: %w", lc.lbArn, putErr)
	}
	return nil
}

// launchLBVMAsync runs provisionLBDataPlane on the background goroutine spawned
// by CreateLoadBalancer; failures are logged (the record is already marked
// failed inside provisionLBDataPlane). Runs after CreateLoadBalancer has returned.
func (s *ELBv2ServiceImpl) launchLBVMAsync(lc lbLaunchCtx) {
	if err := s.provisionLBDataPlane(lc); err != nil {
		slog.Error("CreateLoadBalancer: async launch failed", "lbArn", lc.lbArn, "err", err)
	}
}

// rollbackLBInfra tears down ENIs, the NLB managed SG, and the name claim created
// by a partial CreateLoadBalancer. Each step is best-effort and idempotent.
func (s *ELBv2ServiceImpl) rollbackLBInfra(eniIDs []string, nlbManagedSGID, name, accountID string) {
	if s.VPCService != nil {
		for _, eniID := range eniIDs {
			if _, delErr := s.VPCService.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
				NetworkInterfaceId: aws.String(eniID),
			}, accountID); delErr != nil {
				slog.Error("CreateLoadBalancer: rollback failed to delete ENI", "eni", eniID, "err", delErr)
			}
		}
	}
	s.deleteNLBManagedSG(nlbManagedSGID, accountID)
	s.releaseLBNameClaim(name, accountID)
}

// markLBFailed reloads the LB record and flips it to the failed state.
func (s *ELBv2ServiceImpl) markLBFailed(lbArn string) {
	record, err := s.store.GetLoadBalancerByArn(lbArn)
	if err != nil || record == nil {
		return
	}
	record.State = StateFailed
	if putErr := s.store.PutLoadBalancer(record); putErr != nil {
		slog.Error("CreateLoadBalancer: failed to persist failed state", "lbArn", lbArn, "err", putErr)
	}
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
		// Idempotent: return success for absent/unowned LBs.
		return &elbv2.DeleteLoadBalancerOutput{}, nil
	}

	// Cascade-delete all listeners and their rules so no orphan pins a TG as ResourceInUse.
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

	// Reap floating-IP NAT now; conditional VM/ENI teardown paths can miss it.
	s.reapFloatingIPNAT(lb)

	// Terminate ALB VM in background; shutdown can take seconds (QMP powerdown).
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

	// Release the name claim so the name is reusable. Idempotent on a missing
	// key, so a delete that races the record removal still converges.
	s.releaseLBNameClaim(lb.Name, accountID)

	slog.Info("DeleteLoadBalancer completed", "lbArn", *input.LoadBalancerArn, "enis", len(lb.ENIs), "accountID", accountID)

	return &elbv2.DeleteLoadBalancerOutput{}, nil
}

// releaseLBNameClaim drops the per-account LB name claim. Failures are logged but
// not fatal; leaked claims are reclaimed by the crash-orphan path on next create.
func (s *ELBv2ServiceImpl) releaseLBNameClaim(name, accountID string) {
	if err := s.store.ReleaseLBName(name, accountID); err != nil {
		slog.Warn("failed to release LB name claim", "name", name, "accountID", accountID, "err", err)
	}
}

// reapFloatingIPNAT removes the OVN dnat_and_snat rule for an internet-facing
// LB's floating IP. Idempotent; internal LBs are a no-op.
func (s *ELBv2ServiceImpl) reapFloatingIPNAT(lb *LoadBalancerRecord) {
	publicIP := ""
	for _, az := range lb.AvailZones {
		if az.PublicIP != "" {
			publicIP = az.PublicIP
			break
		}
	}
	if publicIP == "" || lb.VPCIP == "" || lb.VpcId == "" {
		return
	}
	portName := ""
	if len(lb.ENIs) > 0 {
		portName = topology.Port(lb.ENIs[0])
	}
	utils.PublishNATEvent(s.nc, "vpc.delete-nat", lb.VpcId, publicIP, lb.VPCIP, portName, "")
	slog.Info("DeleteLoadBalancer: reaped floating-IP NAT",
		"lbId", lb.LoadBalancerID, "externalIp", publicIP, "logicalIp", lb.VPCIP)
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

	// ProtocolVersion applies only to HTTP/HTTPS target groups (AWS defaults to
	// HTTP1). It must round-trip: the load balancer controller always sends it and
	// recreates the TG if Describe reads it back empty.
	protocolVersion := ""
	if protocol == ProtocolHTTP || protocol == ProtocolHTTPS {
		protocolVersion = ProtocolVersionHTTP1
		if input.ProtocolVersion != nil && *input.ProtocolVersion != "" {
			protocolVersion = *input.ProtocolVersion
		}
		switch protocolVersion {
		case ProtocolVersionHTTP1, ProtocolVersionHTTP2, ProtocolVersionGRPC:
			// valid
		default:
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
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
		TargetGroupArn:  tgArn,
		TargetGroupID:   tgID,
		Name:            name,
		Protocol:        protocol,
		ProtocolVersion: protocolVersion,
		Port:            port,
		VpcId:           vpcID,
		TargetType:      targetType,
		HealthCheck:     hc,
		Tags:            tags,
		AccountID:       accountID,
		CreatedAt:       time.Now().UTC(),
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
		// Idempotent: return success for absent/unowned TGs.
		return &elbv2.DeleteTargetGroupOutput{}, nil
	}

	// Only live listeners/rules (whose LB still exists) pin the TG as ResourceInUse.
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

	// A target group not forwarded to by any listener serves no traffic; its
	// targets report "unused" (AWS Target.NotInUse), not "initial".
	inUse, err := s.store.TargetGroupInUse(tg.TargetGroupArn)
	if err != nil {
		slog.Error("DescribeTargetHealth: failed to check TG association", "arn", tg.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	descriptions := make([]*elbv2.TargetHealthDescription, 0)
	for _, t := range tg.Targets {
		if len(targetFilter) > 0 && !targetFilter[t.Id] {
			continue
		}

		state, healthDesc := t.HealthState, t.HealthDesc
		var reason string
		if !inUse && t.HealthState != TargetHealthDraining {
			state = TargetHealthUnused
			healthDesc = "Target group is not configured to receive traffic from a load balancer"
			reason = "Target.NotInUse"
		}

		health := &elbv2.TargetHealth{
			State:       aws.String(state),
			Description: aws.String(healthDesc),
		}
		if reason != "" {
			health.Reason = aws.String(reason)
		}
		desc := &elbv2.TargetHealthDescription{
			Target: &elbv2.TargetDescription{
				Id:   aws.String(t.Id),
				Port: aws.Int64(t.Port),
			},
			TargetHealth: health,
		}
		descriptions = append(descriptions, desc)
	}

	return &elbv2.DescribeTargetHealthOutput{
		TargetHealthDescriptions: descriptions,
	}, nil
}

// --- Listener operations ---

// listenerActionFromSDK converts an SDK listener action to stored form,
// preserving fixed-response/redirect config to avoid dangling backends.
func listenerActionFromSDK(a *elbv2.Action) ListenerAction {
	action := ListenerAction{}
	if a.Type != nil {
		action.Type = *a.Type
	}
	if a.TargetGroupArn != nil {
		action.TargetGroupArn = *a.TargetGroupArn
	}
	// Forward actions may carry the target group via ForwardConfig (the modern
	// weighted shape the LB controller emits) instead of the flat field. Flatten
	// the first target group so the single-TG model resolves it.
	if action.TargetGroupArn == "" && a.ForwardConfig != nil {
		for _, tg := range a.ForwardConfig.TargetGroups {
			if tg != nil && tg.TargetGroupArn != nil && *tg.TargetGroupArn != "" {
				action.TargetGroupArn = *tg.TargetGroupArn
				break
			}
		}
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

// validateListenerAction enforces the per-type action contract. Redirect fields
// are validated here so invalid input fails at the API, not at render time.
func validateListenerAction(a ListenerAction) error {
	if a.Type == ActionTypeRedirect {
		if a.Redirect == nil {
			return errors.New(awserrors.ErrorMissingParameter)
		}
		return validateRedirectAction(a.Redirect)
	}
	return nil
}

// validateRedirectAction rejects an unsupported status code or render-unsafe fields.
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

// buildListenerCertificates validates certificates and SSL policy for a listener.
// Secure protocols require exactly one default cert and receive a default SslPolicy when unset.
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
	var authorizedCIDRs []string
	if lb.Type == LoadBalancerTypeNetwork && lb.NLBManagedSGID != "" && s.VPCService != nil {
		cidrs, cidrErr := s.resolveNLBIngressCIDRs(lb)
		if cidrErr != nil {
			slog.Error("CreateListener: resolve ingress CIDRs failed", "lbArn", lb.LoadBalancerArn, "err", cidrErr)
			s.rollbackListener(record, lb, protocol, port, nil, accountID)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		if authErr := s.authorizeNLBListenerPort(lb, protocol, port, cidrs, accountID); authErr != nil {
			slog.Error("CreateListener: authorize listener port failed", "lbArn", lb.LoadBalancerArn, "port", port, "err", authErr)
			s.rollbackListener(record, lb, protocol, port, nil, accountID)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		authorizedCIDRs = cidrs
	}

	// Start or reload HAProxy now that a listener exists
	if err := s.updateStoredConfig(lb); err != nil {
		slog.Error("CreateListener: failed to update config", "listenerId", listenerID, "err", err)
		s.rollbackListener(record, lb, protocol, port, authorizedCIDRs, accountID)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateListener completed", "listenerArn", listenerArn, "lbArn", lb.LoadBalancerArn, "port", port, "accountID", accountID)

	return &elbv2.CreateListenerOutput{
		Listeners: []*elbv2.Listener{s.listenerRecordToSDK(record)},
	}, nil
}

// rollbackListener removes a listener persisted before post-persist wiring failed.
// authorizedCIDRs is non-empty when the NLB port was already opened — it is
// revoked first. Cleanup errors are logged but not returned.
func (s *ELBv2ServiceImpl) rollbackListener(record *ListenerRecord, lb *LoadBalancerRecord, protocol string, port int64, authorizedCIDRs []string, accountID string) {
	if len(authorizedCIDRs) > 0 {
		if err := s.revokeNLBListenerPort(lb, protocol, port, authorizedCIDRs, accountID); err != nil {
			slog.Error("CreateListener: rollback failed to revoke listener port", "lbArn", lb.LoadBalancerArn, "port", port, "err", err)
		}
	}
	if err := s.deleteListenerCascade(record); err != nil {
		slog.Error("CreateListener: rollback failed to delete listener", "listenerArn", record.ListenerArn, "err", err)
	}
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
		// Idempotent: return success for absent/unowned listeners.
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

	// Cert/policy: switching to non-secure clears material; staying/switching to secure validates it.
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

// DescribeTags returns tags for ELBv2 resources read from the record stores.
// Cross-account or unknown ARNs return a not-found error.
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
			} else if lArn, isDefault := listenerArnFromDefaultRuleArn(arn); isDefault {
				// Synthetic default rule: not stored, carries no tags. Resolve via
				// its parent listener so a controller's post-create rule-tag sync
				// gets an empty TagDescription instead of an error.
				l, lErr := s.store.GetListenerByArn(lArn)
				if lErr != nil {
					slog.Error("DescribeTags: failed to get listener", "arn", lArn, "err", lErr)
					return nil, errors.New(awserrors.ErrorServerInternal)
				}
				if l != nil {
					found = true
					tags = nil
					ownerAccount = l.AccountID
				}
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

// tagsFromSDK converts an SDK Tag slice into a tag map, skipping nil key/value entries.
func tagsFromSDK(tags []*elbv2.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, tag := range tags {
		if tag.Key != nil && tag.Value != nil {
			m[*tag.Key] = *tag.Value
		}
	}
	return m
}

// tagsMapToSDK converts a tag map into a sorted SDK Tag slice; returns nil for empty input.
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

	// Build subnet → private IP map so each AZ entry surfaces the ENI's private IP.
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
	tg := &elbv2.TargetGroup{
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

	// Omitted for NLB (TCP/UDP/TLS) target groups, matching AWS, which returns
	// ProtocolVersion only for HTTP/HTTPS target groups.
	if r.ProtocolVersion != "" {
		tg.ProtocolVersion = aws.String(r.ProtocolVersion)
	}

	return tg
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

// listenerActionToSDK converts a stored action to the AWS SDK shape.
// Shared by listeners and rules; preserves all action types for round-trip fidelity.
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
