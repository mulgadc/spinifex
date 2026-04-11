package handlers_elbv2

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	// elbv2ManagedByTag is the tag key used to mark ENIs as system-managed by ELBv2.
	elbv2ManagedByTag = "spinifex:managed-by"
	// elbv2ManagedByValue is the tag value for ELBv2-managed ENIs.
	elbv2ManagedByValue = "elbv2"
	// elbv2LBTag is the tag key storing the parent LB ARN on managed ENIs.
	elbv2LBTag = "spinifex:lb-arn"

	// heartbeatPersistInterval controls how often a no-op heartbeat (no state
	// change) writes the full LoadBalancerRecord back to KV. State transitions
	// always persist immediately.
	heartbeatPersistInterval = 60 * time.Second
)

// lbVMUserData generates cloud-config user data for a load balancer VM.
// Uses write_files to populate /etc/conf.d/lb-agent with the lb-id, gateway
// URL, and system credentials for SigV4 auth. The CA cert is already injected
// by the instance service's cloud-init template (same as regular EC2 VMs).
// Cloud-init guarantees write_files runs before runcmd. The service is NOT
// enabled at boot in the image — cloud-init is the sole trigger so the env
// vars are always present before the agent starts.
func (s *ELBv2ServiceImpl) lbVMUserData(lbID string) (string, error) {
	if s.GatewayURL == "" || s.SystemAccessKey == "" || s.SystemSecretKey == "" {
		return "", fmt.Errorf("missing system credentials: gatewayURL=%q accessKey=%q secretKey-set=%t",
			s.GatewayURL, s.SystemAccessKey, s.SystemSecretKey != "")
	}

	cfg := fmt.Sprintf(`#cloud-config
write_files:
  - path: /etc/conf.d/lb-agent
    content: |
      LB_LB_ID=%s
      LB_GATEWAY_URL=%s
      LB_ACCESS_KEY=%s
      LB_SECRET_KEY=%s
      LB_REGION=%s
`, lbID, s.GatewayURL, s.SystemAccessKey, s.SystemSecretKey, s.region)

	// When AWSGW binds to a specific IP (multi-node), add a host route via
	// the management NIC so the agent can reach the gateway. bootcmd runs
	// early enough that networking is configured before lb-agent starts.
	if s.MgmtRouteGateway != "" && s.MgmtRouteTarget != "" {
		cfg += fmt.Sprintf(`bootcmd:
  - [ "ip", "route", "add", "%s/32", "via", "%s" ]
`, s.MgmtRouteTarget, s.MgmtRouteGateway)
	}

	cfg += `runcmd:
  - [ "rc-service", "lb-agent", "start" ]
`
	return cfg, nil
}

// Ensure ELBv2ServiceImpl implements ELBv2Service at compile time.
var _ ELBv2Service = (*ELBv2ServiceImpl)(nil)

// ELBv2ServiceImpl implements ELBv2 operations with NATS JetStream persistence.
type ELBv2ServiceImpl struct {
	config                     *config.Config
	store                      *Store
	nc                         *nats.Conn                       // NATS connection for JetStream KV store
	VPCService                 *handlers_ec2_vpc.VPCServiceImpl // nil-safe: ENI ops skipped when nil (e.g. in tests)
	InstanceLauncher           SystemInstanceLauncher           // nil-safe: system VM ops skipped when nil
	SystemAccessKey            string                           // System account access key for ALB agent SigV4 auth
	SystemSecretKey            string                           // System account secret key for ALB agent SigV4 auth
	GatewayURL                 string                           // AWS gateway URL for ALB agent outbound connections
	MgmtRouteGateway           string                           // br-mgmt IP (next-hop for mgmt route); empty when AWSGW is on 0.0.0.0
	MgmtRouteTarget            string                           // AWSGW bind IP to route via mgmt NIC
	nodeID                     string
	region                     string
	systemAMI                  string        // AMI ID for system VMs (ALB VMs); resolved lazily via systemAMIFunc
	systemAMIFunc              func() string // returns the current system AMI ID (queries image store)
	systemAMIMu                sync.Mutex    // guards lazy resolution of systemAMI
	systemAMIResolved          bool          // true once systemAMI has been resolved to a non-empty value
	systemInstanceType         string        // instance type for system VMs; resolved lazily via systemInstanceTypeFunc
	systemInstanceTypeFunc     func() string // returns the smallest available instance type
	systemInstanceTypeMu       sync.Mutex    // guards lazy resolution of systemInstanceType
	systemInstanceTypeResolved bool          // true once systemInstanceType has been resolved to a non-empty value
	ctx                        context.Context
	cancel                     context.CancelFunc
	hc                         *healthChecker
}

// NewELBv2ServiceImplWithNATS creates an ELBv2 service backed by JetStream KV.
func NewELBv2ServiceImplWithNATS(cfg *config.Config, nc *nats.Conn) (*ELBv2ServiceImpl, error) {
	store, err := NewStore(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to create ELBv2 store: %w", err)
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
		config: cfg,
		store:  store,
		nc:     nc,
		nodeID: nodeID,
		region: region,
		ctx:    ctx,
		cancel: cancel,
		hc:     hc,
	}, nil
}

// Close cancels background goroutines.
func (s *ELBv2ServiceImpl) Close() {
	if s.cancel != nil {
		s.cancel()
	}
}

// SetSystemAMIFunc sets a function that resolves the current system AMI ID.
// This is called at request time so the AMI is discovered even if imported
// after the daemon starts.
func (s *ELBv2ServiceImpl) SetSystemAMIFunc(fn func() string) {
	s.systemAMIMu.Lock()
	defer s.systemAMIMu.Unlock()
	s.systemAMIFunc = fn
	s.systemAMI = ""
	s.systemAMIResolved = false
}

// getSystemAMI returns the system AMI ID, resolving it lazily if needed.
// Only caches non-empty results so the resolver retries when the image
// has not been imported yet.
func (s *ELBv2ServiceImpl) getSystemAMI() string {
	s.systemAMIMu.Lock()
	defer s.systemAMIMu.Unlock()
	if s.systemAMIResolved {
		return s.systemAMI
	}
	if s.systemAMIFunc != nil {
		s.systemAMI = s.systemAMIFunc()
	}
	if s.systemAMI != "" {
		s.systemAMIResolved = true
	}
	return s.systemAMI
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

	// Collect target groups referenced by listeners
	tgByArn := make(map[string]*TargetGroupRecord)
	for _, l := range listeners {
		for _, a := range l.DefaultActions {
			if a.TargetGroupArn == "" {
				continue
			}
			if _, ok := tgByArn[a.TargetGroupArn]; ok {
				continue
			}
			tg, tgErr := s.store.GetTargetGroupByArn(a.TargetGroupArn)
			if tgErr != nil || tg == nil {
				slog.Debug("updateStoredConfig: target group not found", "tgArn", a.TargetGroupArn)
				continue
			}
			tgByArn[a.TargetGroupArn] = tg
		}
	}

	configContent, err := GenerateHAProxyConfig(lb, listeners, tgByArn, bindAddr)
	if err != nil {
		slog.Error("updateStoredConfig: failed to generate config", "lbId", lb.LoadBalancerID, "err", err)
		return fmt.Errorf("generate config: %w", err)
	}

	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(configContent)))
	lb.ConfigText = configContent
	lb.ConfigHash = hash

	if err := s.store.PutLoadBalancer(lb); err != nil {
		slog.Error("updateStoredConfig: failed to persist LB", "lbId", lb.LoadBalancerID, "err", err)
		return fmt.Errorf("persist LB: %w", err)
	}

	slog.Info("updateStoredConfig: config stored",
		"lbId", lb.LoadBalancerID, "hash", hash[:12], "size", len(configContent))
	return nil
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
	lb, err := s.store.GetLoadBalancer(lbID)
	if err != nil {
		slog.Error("LBAgentHeartbeat: failed to get LB", "lbId", lbID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil || (lb.AccountID != accountID && accountID != utils.GlobalAccountID) {
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
		ConfigText: aws.String(lb.ConfigText),
		ConfigHash: aws.String(lb.ConfigHash),
	}, nil
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
)

// elbv2ResourceTypeFromArn extracts the resource type from an ELBv2 ARN.
// ELBv2 ARNs have the form
// "arn:aws:elasticloadbalancing:{region}:{account}:{type}/...". Returns one
// of "loadbalancer", "targetgroup", "listener" — anything else yields
// ErrorInvalidParameterValue so callers can surface a proper API error.
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
	case elbv2ResourceLoadBalancer, elbv2ResourceTargetGroup, elbv2ResourceListener:
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

	tags := make(map[string]string)
	for _, tag := range input.Tags {
		if tag.Key != nil && tag.Value != nil {
			tags[*tag.Key] = *tag.Value
		}
	}

	// Create ENIs in each subnet (when VPC service is available)
	var eniIDs []string
	var availZones []AvailZoneInfo
	vpcID := ""
	if s.VPCService != nil && len(subnets) > 0 {
		for _, subnetID := range subnets {
			eniOut, eniErr := s.VPCService.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
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
			}, accountID)
			if eniErr != nil {
				// Rollback: delete any ENIs already created
				for _, rollbackENI := range eniIDs {
					if _, delErr := s.VPCService.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
						NetworkInterfaceId: aws.String(rollbackENI),
					}, accountID); delErr != nil {
						slog.Error("CreateLoadBalancer: rollback failed to delete ENI", "eni", rollbackENI, "err", delErr)
					}
				}
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
	var albInstanceID string
	var albVPCIP string
	var hostPorts map[int]int
	var launchFailed bool
	if s.InstanceLauncher != nil && s.getSystemAMI() != "" && len(eniIDs) > 0 && len(subnets) > 0 {
		// Resolve MAC/IP for every ENI in one describe call.
		eniDetails := make(map[string]*ec2.NetworkInterface, len(eniIDs))
		if s.VPCService != nil {
			eniPtrs := make([]*string, 0, len(eniIDs))
			for _, id := range eniIDs {
				eniPtrs = append(eniPtrs, aws.String(id))
			}
			result, descErr := s.VPCService.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
				NetworkInterfaceIds: eniPtrs,
			}, accountID)
			if descErr == nil {
				for _, eni := range result.NetworkInterfaces {
					if eni.NetworkInterfaceId != nil {
						eniDetails[*eni.NetworkInterfaceId] = eni
					}
				}
			}
		}

		primary := eniDetails[eniIDs[0]]
		primaryIP := ""
		primaryMAC := ""
		if primary != nil {
			primaryIP = aws.StringValue(primary.PrivateIpAddress)
			primaryMAC = aws.StringValue(primary.MacAddress)
		}

		extraENIInputs := make([]ExtraENIInput, 0, len(eniIDs)-1)
		for i := 1; i < len(eniIDs); i++ {
			eni := eniDetails[eniIDs[i]]
			if eni == nil {
				continue
			}
			extraENIInputs = append(extraENIInputs, ExtraENIInput{
				ENIID:    eniIDs[i],
				ENIMac:   aws.StringValue(eni.MacAddress),
				ENIIP:    aws.StringValue(eni.PrivateIpAddress),
				SubnetID: aws.StringValue(eni.SubnetId),
			})
		}

		userData, udErr := s.lbVMUserData(lbID)
		if udErr != nil {
			slog.Error("CreateLoadBalancer: system credentials not configured — cannot launch ALB VM", "lbId", lbID, "err", udErr)
			launchFailed = true
		} else {
			launchInput := &SystemInstanceInput{
				InstanceType: s.getSystemInstanceType(),
				ImageID:      s.getSystemAMI(),
				SubnetID:     subnets[0],
				UserData:     userData,
				ENIID:        eniIDs[0],
				ENIMac:       primaryMAC,
				ENIIP:        primaryIP,
				ExtraENIs:    extraENIInputs,
				Scheme:       scheme,
				AccountID:    accountID,
			}
			// Dev-mode only: forward HTTP/HTTPS ports from host for local testing.
			// In production (VPC networking), traffic reaches the ALB VM's VPC IP directly.
			if s.config != nil && s.config.Daemon.DevNetworking {
				launchInput.HostfwdPorts = []int{80, 443}
			}
			out, launchErr := s.InstanceLauncher.LaunchSystemInstance(launchInput)
			if launchErr != nil {
				slog.Error("CreateLoadBalancer: failed to launch ALB VM", "lbId", lbID, "err", launchErr)
				launchFailed = true
			} else {
				albInstanceID = out.InstanceID
				albVPCIP = out.PrivateIP
				hostPorts = out.HostfwdMap
				// Record public IP on the first AZ entry for internet-facing ALBs.
				if out.PublicIP != "" && len(availZones) > 0 {
					availZones[0].PublicIP = out.PublicIP
				}
				slog.Info("CreateLoadBalancer: ALB VM launched", "lbId", lbID, "instanceId", albInstanceID, "ip", out.PrivateIP, "publicIp", out.PublicIP, "hostfwd", hostPorts)
			}
		}
	}

	// ALB starts in provisioning until the agent inside the VM connects and
	// responds to a ping. If no VM was launched (no launcher/AMI or launch
	// failed), set state to failed so the API reflects the broken data-plane.
	state := StateActive
	if albInstanceID != "" {
		state = StateProvisioning
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
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
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

	// Delete all listeners for this LB
	listeners, err := s.store.ListListenersByLB(lb.LoadBalancerArn)
	if err != nil {
		slog.Warn("Failed to list listeners for cleanup", "lbArn", lb.LoadBalancerArn, "err", err)
	}
	for _, l := range listeners {
		if err := s.store.DeleteListener(l.ListenerID); err != nil {
			slog.Warn("Failed to delete listener during LB cleanup", "listenerID", l.ListenerID, "err", err)
		}
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

	targetType := "instance"
	if input.TargetType != nil && *input.TargetType != "" {
		targetType = *input.TargetType
	}

	// Use NLB health check defaults for NLB protocols.
	var hc HealthCheckConfig
	switch protocol {
	case ProtocolTCP, ProtocolUDP, ProtocolTLS, ProtocolTCPUDP:
		hc = DefaultNLBHealthCheck()
	default:
		hc = DefaultHealthCheck()
	}
	if input.HealthCheckProtocol != nil {
		hc.Protocol = *input.HealthCheckProtocol
	}
	if input.HealthCheckPort != nil {
		hc.Port = *input.HealthCheckPort
	}
	if input.HealthCheckPath != nil {
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
		hc.Matcher = *input.Matcher.HttpCode
	}

	tgID := utils.GenerateResourceID("tg")
	tgArn := buildTGArn(s.region, accountID, name, tgID)

	tags := make(map[string]string)
	for _, tag := range input.Tags {
		if tag.Key != nil && tag.Value != nil {
			tags[*tag.Key] = *tag.Value
		}
	}

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
		return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	}

	// Check if any listener references this target group
	listeners, err := s.store.ListListeners()
	if err != nil {
		slog.Error("DeleteTargetGroup: failed to list listeners for in-use check", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	for _, l := range listeners {
		for _, action := range l.DefaultActions {
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

		// Resolve instance ID → private IP via ENI lookup
		privateIP := s.resolveTargetIP(*td.Id, accountID)

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

	listenerID := utils.GenerateResourceID("lst")
	listenerArn := buildListenerArn(s.region, accountID, lb.Name, lb.LoadBalancerID, listenerID, lb.Type)

	var actions []ListenerAction
	for _, a := range input.DefaultActions {
		action := ListenerAction{}
		if a.Type != nil {
			action.Type = *a.Type
		}
		if a.TargetGroupArn != nil {
			action.TargetGroupArn = *a.TargetGroupArn
		}
		actions = append(actions, action)
	}

	record := &ListenerRecord{
		ListenerArn:     listenerArn,
		ListenerID:      listenerID,
		LoadBalancerArn: lb.LoadBalancerArn,
		Protocol:        protocol,
		Port:            port,
		DefaultActions:  actions,
		AccountID:       accountID,
		CreatedAt:       time.Now().UTC(),
	}

	if err := s.store.PutListener(record); err != nil {
		slog.Error("CreateListener: failed to persist record", "listenerId", listenerID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
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
		return nil, errors.New(awserrors.ErrorELBv2ListenerNotFound)
	}

	if err := s.store.DeleteListener(listener.ListenerID); err != nil {
		slog.Error("DeleteListener: failed to delete record", "listenerId", listener.ListenerID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Reload or stop HAProxy after listener removal
	lb, lbErr := s.store.GetLoadBalancerByArn(listener.LoadBalancerArn)
	if lbErr == nil && lb != nil {
		if err := s.updateStoredConfig(lb); err != nil {
			slog.Error("DeleteListener: failed to update config", "listenerArn", *input.ListenerArn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	slog.Info("DeleteListener completed", "listenerArn", *input.ListenerArn, "accountID", accountID)

	return &elbv2.DeleteListenerOutput{}, nil
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
// target groups, listeners). Tag data is read from the existing record stores
// — Spinifex doesn't have a separate tag KV. Listeners currently never store
// tags, so they always return an empty Tags slice (matches AWS behaviour for
// untagged resources). Cross-account or unknown ARNs return the per-resource
// not-found error so existence isn't leaked across accounts.
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
				ownerAccount = l.AccountID
				// Listeners don't store tags yet — leave map nil.
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

// --- SDK type conversion helpers ---

func (s *ELBv2ServiceImpl) ModifyTargetGroupAttributes(input *elbv2.ModifyTargetGroupAttributesInput, accountID string) (*elbv2.ModifyTargetGroupAttributesOutput, error) {
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	tg, err := s.store.GetTargetGroupByArn(*input.TargetGroupArn)
	if err != nil {
		slog.Error("ModifyTargetGroupAttributes: failed to get TG", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if tg == nil || tg.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	}

	knownTGAttrs := DefaultTargetGroupAttributes()
	var submitted []*elbv2.TargetGroupAttribute
	dirty := false
	for _, attr := range input.Attributes {
		if attr == nil {
			slog.Warn("ModifyTargetGroupAttributes: skipping nil attribute element", "arn", *input.TargetGroupArn)
			continue
		}
		if attr.Key == nil || attr.Value == nil {
			slog.Warn("ModifyTargetGroupAttributes: skipping attribute with nil Key or Value", "arn", *input.TargetGroupArn)
			continue
		}
		if _, ok := knownTGAttrs[*attr.Key]; !ok {
			slog.Warn("ModifyTargetGroupAttributes: rejecting unknown attribute key", "arn", *input.TargetGroupArn, "key", *attr.Key)
			return nil, errors.New(awserrors.ErrorValidationError)
		}
		submitted = append(submitted, &elbv2.TargetGroupAttribute{
			Key:   attr.Key,
			Value: attr.Value,
		})
		existing, exists := tg.Attributes[*attr.Key]
		if exists && existing == *attr.Value {
			continue // value already matches stored — no mutation needed
		}
		if tg.Attributes == nil {
			tg.Attributes = make(map[string]string)
		}
		tg.Attributes[*attr.Key] = *attr.Value
		dirty = true
	}

	// If the caller sent attributes but every single one was rejected by the
	// nil guard above, surface that as an error instead of silently returning
	// a successful empty response — otherwise the caller thinks the write
	// landed when nothing was actually applied.
	if len(input.Attributes) > 0 && len(submitted) == 0 {
		slog.Warn("ModifyTargetGroupAttributes: all submitted attributes were invalid", "arn", *input.TargetGroupArn, "submitted_count", len(input.Attributes))
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Skip the NATS/KV write when nothing changed. Terraform re-applies the same
	// attribute set on every drift check, so this kills ~all Modify traffic
	// during steady state and narrows the read-modify-write race window.
	if dirty {
		if err := s.store.PutTargetGroup(tg); err != nil {
			slog.Error("ModifyTargetGroupAttributes: failed to persist TG", "arn", *input.TargetGroupArn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	return &elbv2.ModifyTargetGroupAttributesOutput{
		Attributes: submitted,
	}, nil
}

func (s *ELBv2ServiceImpl) DescribeTargetGroupAttributes(input *elbv2.DescribeTargetGroupAttributesInput, accountID string) (*elbv2.DescribeTargetGroupAttributesOutput, error) {
	if input.TargetGroupArn == nil || *input.TargetGroupArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	tg, err := s.store.GetTargetGroupByArn(*input.TargetGroupArn)
	if err != nil {
		slog.Error("DescribeTargetGroupAttributes: failed to get TG", "arn", *input.TargetGroupArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if tg == nil || tg.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	}

	merged := DefaultTargetGroupAttributes()
	maps.Copy(merged, tg.Attributes)

	// Sort keys for deterministic output — Terraform diffs and snapshot tests
	// depend on stable attribute ordering.
	keys := slices.Sorted(maps.Keys(merged))
	attrs := make([]*elbv2.TargetGroupAttribute, 0, len(keys))
	for _, k := range keys {
		attrs = append(attrs, &elbv2.TargetGroupAttribute{
			Key:   aws.String(k),
			Value: aws.String(merged[k]),
		})
	}

	return &elbv2.DescribeTargetGroupAttributesOutput{
		Attributes: attrs,
	}, nil
}

func (s *ELBv2ServiceImpl) ModifyLoadBalancerAttributes(input *elbv2.ModifyLoadBalancerAttributesInput, accountID string) (*elbv2.ModifyLoadBalancerAttributesOutput, error) {
	if input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	lb, err := s.store.GetLoadBalancerByArn(*input.LoadBalancerArn)
	if err != nil {
		slog.Error("ModifyLoadBalancerAttributes: failed to get LB", "arn", *input.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil || lb.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}

	knownLBAttrs := DefaultLoadBalancerAttributes(lb.Type)
	var submitted []*elbv2.LoadBalancerAttribute
	dirty := false
	for _, attr := range input.Attributes {
		if attr == nil {
			slog.Warn("ModifyLoadBalancerAttributes: skipping nil attribute element", "arn", *input.LoadBalancerArn)
			continue
		}
		if attr.Key == nil || attr.Value == nil {
			slog.Warn("ModifyLoadBalancerAttributes: skipping attribute with nil Key or Value", "arn", *input.LoadBalancerArn)
			continue
		}
		if _, ok := knownLBAttrs[*attr.Key]; !ok {
			slog.Warn("ModifyLoadBalancerAttributes: rejecting unknown attribute key", "arn", *input.LoadBalancerArn, "key", *attr.Key, "lbType", lb.Type)
			return nil, errors.New(awserrors.ErrorValidationError)
		}
		submitted = append(submitted, &elbv2.LoadBalancerAttribute{
			Key:   attr.Key,
			Value: attr.Value,
		})
		existing, exists := lb.Attributes[*attr.Key]
		if exists && existing == *attr.Value {
			continue
		}
		if lb.Attributes == nil {
			lb.Attributes = make(map[string]string)
		}
		lb.Attributes[*attr.Key] = *attr.Value
		dirty = true
	}

	// If the caller sent attributes but every single one was rejected by the
	// nil guard above, surface that as an error instead of silently returning
	// a successful empty response.
	if len(input.Attributes) > 0 && len(submitted) == 0 {
		slog.Warn("ModifyLoadBalancerAttributes: all submitted attributes were invalid", "arn", *input.LoadBalancerArn, "submitted_count", len(input.Attributes))
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Skip the NATS/KV write when nothing changed. See
	// ModifyTargetGroupAttributes for the Terraform-drift-check motivation.
	if dirty {
		if err := s.store.PutLoadBalancer(lb); err != nil {
			slog.Error("ModifyLoadBalancerAttributes: failed to persist LB", "arn", *input.LoadBalancerArn, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	return &elbv2.ModifyLoadBalancerAttributesOutput{
		Attributes: submitted,
	}, nil
}

func (s *ELBv2ServiceImpl) DescribeLoadBalancerAttributes(input *elbv2.DescribeLoadBalancerAttributesInput, accountID string) (*elbv2.DescribeLoadBalancerAttributesOutput, error) {
	if input.LoadBalancerArn == nil || *input.LoadBalancerArn == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	lb, err := s.store.GetLoadBalancerByArn(*input.LoadBalancerArn)
	if err != nil {
		slog.Error("DescribeLoadBalancerAttributes: failed to get LB", "arn", *input.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if lb == nil || lb.AccountID != accountID {
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}

	merged := DefaultLoadBalancerAttributes(lb.Type)
	maps.Copy(merged, lb.Attributes)

	// Sort keys for deterministic output — Terraform diffs and snapshot tests
	// depend on stable attribute ordering.
	keys := slices.Sorted(maps.Keys(merged))
	attrs := make([]*elbv2.LoadBalancerAttribute, 0, len(keys))
	for _, k := range keys {
		attrs = append(attrs, &elbv2.LoadBalancerAttribute{
			Key:   aws.String(k),
			Value: aws.String(merged[k]),
		})
	}

	return &elbv2.DescribeLoadBalancerAttributesOutput{
		Attributes: attrs,
	}, nil
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
		action := &elbv2.Action{
			Type:           aws.String(a.Type),
			TargetGroupArn: aws.String(a.TargetGroupArn),
		}
		listener.DefaultActions = append(listener.DefaultActions, action)
	}

	return listener
}
