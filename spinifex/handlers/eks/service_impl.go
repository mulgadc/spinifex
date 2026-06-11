package handlers_eks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// SubnetVPCResolver resolves a subnet to its VPC ID and CIDR.
type SubnetVPCResolver interface {
	GetSubnetVPC(accountID, subnetID string) (vpcID string, err error)
	GetVPCCIDR(accountID, vpcID string) (cidr string, err error)
}

// EKSServiceDeps wires the external collaborators EKSServiceImpl needs.
type EKSServiceDeps struct {
	Config         *config.Config
	NATSConn       *nats.Conn
	MasterKey      []byte
	GatewayBaseURL string
	Region         string
	HolderID       string

	// Gateway broker config for the K3s server VM (SigV4 HTTPS to AWSGW).
	// SystemGatewayURL is the mgmt-reachable endpoint; GatewayCACert signs its TLS cert.
	SystemGatewayURL string
	SystemAccessKey  string
	SystemSecretKey  string
	GatewayCACert    string

	VPCSG     sgProvisioner
	VPCK3s    k3sVPCProvisioner
	VPCSubnet SubnetVPCResolver
	NLB       nlbProvisioner
	Instance  k3sInstanceLauncher
	Image     k3sAMIResolver
	EIP       eipProvisioner
	IGW       igwProvisioner
	Worker    WorkerLauncher

	// PlacementGroup and Scheduler support HA control-plane spread (NATS-backed).
	PlacementGroup controlPlanePlacer
	Scheduler      HostScheduler

	// AddonInstaller delivers managed-addon manifests; nil defaults to the KV staging installer.
	AddonInstaller AddonInstaller
}

// WorkerLauncher is the narrow EC2 surface for launching nodegroup worker instances.
// Workers use the customer-owned RunInstances path (no ManagedBy tag, no mgmt NIC).
type WorkerLauncher interface {
	RunWorkerInstance(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error)
	TerminateWorkerInstances(instanceIDs []string, accountID string) error
}

// eipProvisioner is the narrow EIP surface for allocating a CP VM egress IP.
type eipProvisioner interface {
	AllocateAddress(input *ec2.AllocateAddressInput, accountID string) (*ec2.AllocateAddressOutput, error)
	ReleaseAddress(input *ec2.ReleaseAddressInput, accountID string) (*ec2.ReleaseAddressOutput, error)
}

// EKSServiceImpl is the daemon-side EKSService implementation.
type EKSServiceImpl struct {
	deps     EKSServiceDeps
	leaderKV nats.KeyValue
	registry *ReconcilerRegistry

	mu       sync.Mutex
	bgCtx    context.Context
	bgCancel context.CancelFunc

	// launchWG tracks in-flight async create launches for test determinism (WaitLaunches).
	launchWG sync.WaitGroup
}

var _ EKSService = (*EKSServiceImpl)(nil)

// WaitLaunches blocks until all async create launches finish. Intended for tests.
func (s *EKSServiceImpl) WaitLaunches() { s.launchWG.Wait() }

const defaultK8sVersion = "1.32"

// NewEKSServiceImpl initialises EKSServiceImpl, wiring the leader KV and reconciler registry.
func NewEKSServiceImpl(deps EKSServiceDeps) (*EKSServiceImpl, error) {
	if deps.NATSConn == nil {
		return nil, errors.New("eks: NewEKSServiceImpl nil NATSConn")
	}
	js, err := deps.NATSConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}
	leaderKV, err := InitLeaderBucket(js)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &EKSServiceImpl{
		deps:     deps,
		leaderKV: leaderKV,
		registry: NewReconcilerRegistry(),
		bgCtx:    ctx,
		bgCancel: cancel,
	}, nil
}

// NewEKSServiceImplWithNATS is a back-compat shim for tests that only need NATS wiring.
func NewEKSServiceImplWithNATS(cfg *config.Config, nc *nats.Conn) (*EKSServiceImpl, error) {
	return NewEKSServiceImpl(EKSServiceDeps{Config: cfg, NATSConn: nc})
}

// Shutdown stops the per-cluster reconciler goroutines and cancels every
// background bootstrap goroutine. Safe to call multiple times.
func (s *EKSServiceImpl) Shutdown() {
	s.mu.Lock()
	cancel := s.bgCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.registry.StopAll()
}

// SpawnRegisteredReconcilers resumes reconciler goroutines for all CREATING/ACTIVE clusters on daemon boot.
func (s *EKSServiceImpl) SpawnRegisteredReconcilers() error {
	if !s.depsReadyForOrchestration() {
		slog.Debug("SpawnRegisteredReconcilers: deps not ready, skipping")
		return nil
	}
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	buckets := js.KeyValueStoreNames()
	for name := range buckets {
		if !strings.HasPrefix(name, KVBucketEKSAccountPrefix) {
			continue
		}
		accountID := strings.TrimPrefix(name, KVBucketEKSAccountPrefix)
		acctKV, err := js.KeyValue(name)
		if err != nil {
			slog.Warn("SpawnRegisteredReconcilers: open bucket failed", "bucket", name, "err", err)
			continue
		}
		clusters, err := listClusterNames(acctKV)
		if err != nil {
			slog.Warn("SpawnRegisteredReconcilers: list clusters failed", "bucket", name, "err", err)
			continue
		}
		for _, cluster := range clusters {
			meta, err := GetClusterMeta(acctKV, cluster)
			if err != nil {
				continue
			}
			if meta.Status != ClusterStatusCreating && meta.Status != ClusterStatusActive {
				continue
			}
			// Reclaim any nodegroup workers stranded by the restart: a launch that
			// was in flight when the prior process died left a CREATING (or
			// partially-launched CREATE_FAILED) record whose workers nothing else
			// will ever terminate.
			s.reclaimOrphanedNodegroups(accountID, acctKV, cluster)
			// Re-subscribe to missing artifacts; K3s publishes each one-shot message once.
			if meta.Status == ClusterStatusCreating {
				if pending := BootstrapPendingKinds(acctKV, cluster, meta); len(pending) > 0 {
					slog.Info("SpawnRegisteredReconcilers: resuming bootstrap",
						"cluster", cluster, "pending", pending)
					s.spawnBootstrap(accountID, cluster, acctKV, pending)
				}
			}
			s.spawnReconciler(accountID, cluster, meta)
		}
	}
	return nil
}

func (s *EKSServiceImpl) depsReadyForOrchestration() bool {
	return len(s.missingOrchestrationDeps()) == 0
}

// requireOrchestrationDeps returns ServiceUnavailable with missing deps logged when orchestration is not wired.
func (s *EKSServiceImpl) requireOrchestrationDeps(op string) error {
	missing := s.missingOrchestrationDeps()
	if len(missing) == 0 {
		return nil
	}
	slog.Error("EKS orchestration deps not ready", "op", op, "missing", missing)
	return errors.New(awserrors.ErrorServiceUnavailable)
}

// missingOrchestrationDeps returns names of unset orchestration deps.
func (s *EKSServiceImpl) missingOrchestrationDeps() []string {
	var missing []string
	if s.deps.VPCSG == nil {
		missing = append(missing, "VPCSG")
	}
	if s.deps.VPCK3s == nil {
		missing = append(missing, "VPCK3s")
	}
	if s.deps.VPCSubnet == nil {
		missing = append(missing, "VPCSubnet")
	}
	if s.deps.NLB == nil {
		missing = append(missing, "NLB")
	}
	if s.deps.Instance == nil {
		missing = append(missing, "Instance")
	}
	if s.deps.Image == nil {
		missing = append(missing, "Image")
	}
	if s.deps.EIP == nil {
		missing = append(missing, "EIP")
	}
	if s.deps.Worker == nil {
		missing = append(missing, "Worker")
	}
	if s.deps.PlacementGroup == nil {
		missing = append(missing, "PlacementGroup")
	}
	if s.deps.Scheduler == nil {
		missing = append(missing, "Scheduler")
	}
	if len(s.deps.MasterKey) == 0 {
		missing = append(missing, "MasterKey")
	}
	if s.deps.GatewayBaseURL == "" {
		missing = append(missing, "GatewayBaseURL")
	}
	if s.deps.Region == "" {
		missing = append(missing, "Region")
	}
	if s.deps.HolderID == "" {
		missing = append(missing, "HolderID")
	}
	return missing
}

// --- Cluster ---

// logCreateErr logs a CreateCluster step failure at ERROR and returns it wrapped with the stage name.
func logCreateErr(name, accountID, stage string, err error) error {
	slog.Error("CreateCluster: "+stage+" failed", "cluster", name, "accountID", accountID, "err", err)
	return fmt.Errorf("%s: %w", stage, err)
}

// managedIngressTagKey controls K3s built-in traefik+servicelb; "false" opts out for AWS parity.
const managedIngressTagKey = "spinifex.io/managed-ingress"

// builtinIngressEnabled reports whether a cluster keeps the K3s built-in ingress
// stack. Default ON; the tag opts out with "false".
func builtinIngressEnabled(tags map[string]*string) bool {
	return !strings.EqualFold(aws.StringValue(tags[managedIngressTagKey]), "false")
}

func (s *EKSServiceImpl) CreateCluster(input *eks.CreateClusterInput, accountID, callerPrincipalARN string) (*eks.CreateClusterOutput, error) {
	if err := s.requireOrchestrationDeps("CreateCluster"); err != nil {
		return nil, err
	}
	if err := validateCreateClusterInput(input); err != nil {
		return nil, err
	}
	name := aws.StringValue(input.Name)
	subnetIDs := aws.StringValueSlice(input.ResourcesVpcConfig.SubnetIds)

	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, logCreateErr(name, accountID, "jetstream", err)
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID)
	if err != nil {
		return nil, logCreateErr(name, accountID, "get account bucket", err)
	}

	vpcID, err := s.deps.VPCSubnet.GetSubnetVPC(accountID, subnetIDs[0])
	if err != nil {
		// Subnet resolve failure is a client fault (bad/foreign subnet).
		slog.Error("CreateCluster: resolve subnet VPC failed",
			"cluster", name, "accountID", accountID, "subnet", subnetIDs[0], "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	region := s.deps.Region
	arn := fmt.Sprintf("arn:aws:eks:%s:%s:cluster/%s", region, accountID, name)

	publicAccess, privateAccess := endpointAccess(input.ResourcesVpcConfig)
	publicCidrs := publicAccessCidrs(input.ResourcesVpcConfig, publicAccess)

	meta := &ClusterMeta{
		Name:    name,
		Arn:     arn,
		Status:  ClusterStatusCreating,
		Version: deref(input.Version, defaultK8sVersion),
		RoleArn: aws.StringValue(input.RoleArn),
		ResourcesVpcConfig: &ClusterVpcConfig{
			SubnetIds:             subnetIDs,
			VpcId:                 vpcID,
			EndpointPublicAccess:  publicAccess,
			EndpointPrivateAccess: privateAccess,
			PublicAccessCidrs:     publicCidrs,
		},
		BuiltinIngress: builtinIngressEnabled(input.Tags),
		Tags:           aws.StringValueMap(input.Tags),
		CreatedAt:      time.Now().UTC(),
	}
	// Claim the cluster name before any launching; duplicate/retry handlers lose the claim.
	if err := s.claimClusterName(accountID, acctKV, meta); err != nil {
		return nil, err
	}

	// Respond CREATING immediately; run the slow launch on a background goroutine.
	// The goroutine owns meta from here on — do not touch it after this point.
	out := &eks.CreateClusterOutput{Cluster: clusterMetaToAWS(meta)}
	s.launchWG.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				s.failClusterLaunch(acctKV, name, accountID, "launch panic", fmt.Errorf("%v", r))
			}
		}()
		s.launchClusterInfra(clusterLaunchCtx{
			accountID:          accountID,
			callerPrincipalARN: callerPrincipalARN,
			name:               name,
			region:             region,
			subnetIDs:          subnetIDs,
			vpcID:              vpcID,
			publicAccess:       publicAccess,
			publicCidrs:        publicCidrs,
			input:              input,
			meta:               meta,
			acctKV:             acctKV,
		})
	})
	return out, nil
}

// clusterLaunchCtx carries the inputs the asynchronous control-plane launch
// needs, captured at the end of CreateCluster's fast (claim) phase.
type clusterLaunchCtx struct {
	accountID          string
	callerPrincipalARN string
	name               string
	region             string
	subnetIDs          []string
	vpcID              string
	publicAccess       bool
	publicCidrs        []string
	input              *eks.CreateClusterInput
	meta               *ClusterMeta
	acctKV             nats.KeyValue
}

// failClusterLaunch logs a launch failure and marks the cluster FAILED so DescribeCluster surfaces the cause.
func (s *EKSServiceImpl) failClusterLaunch(kv nats.KeyValue, name, accountID, stage string, err error) {
	_ = logCreateErr(name, accountID, stage, err)
	if mErr := MarkClusterFailed(kv, name, stage+": "+err.Error()); mErr != nil && !errors.Is(mErr, ErrClusterNotFound) {
		slog.Warn("launchClusterInfra: MarkClusterFailed failed", "cluster", name, "err", mErr)
	}
}

// launchClusterInfra is CreateCluster's slow phase (SGs, NLB, OIDC, CP VMs, egress, bootstrap).
// Runs on a background goroutine; every failure marks the cluster FAILED.
func (s *EKSServiceImpl) launchClusterInfra(lc clusterLaunchCtx) {
	accountID := lc.accountID
	callerPrincipalARN := lc.callerPrincipalARN
	name := lc.name
	region := lc.region
	subnetIDs := lc.subnetIDs
	vpcID := lc.vpcID
	publicAccess := lc.publicAccess
	publicCidrs := lc.publicCidrs
	input := lc.input
	meta := lc.meta
	acctKV := lc.acctKV

	cpSG, ngSG, err := EnsureClusterSGs(s.deps.VPCSG, accountID, name, vpcID)
	if err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "ensure cluster SGs", err)
		return
	}
	meta.ResourcesVpcConfig.SecurityGroupIds = []string{cpSG, ngSG}

	vpcCIDR, err := s.deps.VPCSubnet.GetVPCCIDR(accountID, vpcID)
	if err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "resolve vpc cidr", err)
		return
	}
	if err := EnsureControlPlaneIngress(s.deps.VPCSG, accountID, cpSG, vpcCIDR); err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "ensure control-plane ingress", err)
		return
	}
	if err := EnsureControlPlaneHAIngress(s.deps.VPCSG, accountID, cpSG); err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "ensure control-plane HA ingress", err)
		return
	}

	// Public access ⇒ internet-facing NLB (external-pool front-end IP, reachable
	// on the LAN/edge network); private-only ⇒ internal NLB (VPC-only). An
	// internet-facing front-end IP is only answerable on the physical wire once
	// the VPC has an attached IGW (it builds the OVN external switch + localnet +
	// gateway LRP); the control-plane NLB is a system instance, so ensure one.
	if publicAccess && s.deps.IGW != nil {
		if err := EnsureClusterIGW(s.deps.IGW, accountID, vpcID, name); err != nil {
			s.failClusterLaunch(acctKV, name, accountID, "ensure cluster IGW", err)
			return
		}
	}
	nlb, err := EnsureClusterNLB(s.deps.NLB, accountID, name, subnetIDs, publicAccess, publicCidrs)
	if err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "ensure cluster NLB", err)
		return
	}
	// The reachable front-end IP is the kubeconfig endpoint host so the apiserver
	// serving cert (which SANs this IP) validates with TLS verification on.
	// EnsureClusterNLB guarantees a non-empty FrontendIP (it fails the launch
	// otherwise), so there is no DNS-name fallback to bake an unresolvable,
	// non-SANed endpoint.
	meta.Endpoint = "https://" + net.JoinHostPort(nlb.FrontendIP, strconv.FormatInt(clusterNLBListenPort, 10))
	meta.EndpointIP = nlb.FrontendIP
	meta.NLBArn = nlb.LoadBalancerArn
	meta.NLBTargetGroupArn = nlb.TargetGroupArn

	// Persist NLB ARNs early so DeleteCluster can reclaim them on any later failure.
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "persist NLB arns", err)
		return
	}

	oidcIssuer, err := ClusterOIDCIssuer(s.deps.GatewayBaseURL, region, accountID, name)
	if err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "build OIDC issuer", err)
		return
	}
	meta.OIDCIssuer = oidcIssuer

	privPEM, _, err := GenerateClusterOIDCKeypair(acctKV, name, s.deps.MasterKey)
	if err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "generate OIDC keypair", err)
		return
	}
	pubPEM, err := PublicKeyPEMFromPrivate(privPEM)
	if err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "derive OIDC public key", err)
		return
	}

	joinToken, err := GenerateK3sClusterToken()
	if err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "generate k3s cluster token", err)
		return
	}

	cpNodes, spreadGroup, err := s.placeControlPlane(accountID, name, K3sServerInput{
		AccountID:         accountID,
		ClusterName:       name,
		Region:            region,
		SubnetID:          subnetIDs[0],
		ControlPlaneSGID:  cpSG,
		NLBDNS:            nlb.DNSName,
		EndpointIP:        nlb.FrontendIP,
		OIDCIssuer:        oidcIssuer,
		OIDCPrivateKeyPEM: privPEM,
		OIDCPublicKeyPEM:  pubPEM,
		GatewayURL:        s.deps.SystemGatewayURL,
		AccessKey:         s.deps.SystemAccessKey,
		SecretKey:         s.deps.SystemSecretKey,
		GatewayCACert:     s.deps.GatewayCACert,
		BuiltinIngress:    meta.BuiltinIngress,
		JoinToken:         joinToken,
	})
	if err != nil {
		if errors.Is(err, ErrEKSServerAMINotFound) {
			s.failClusterLaunch(acctKV, name, accountID, "eks-server AMI not found", err)
			return
		}
		s.failClusterLaunch(acctKV, name, accountID, "launch K3s VM", err)
		return
	}
	// Mirror primary CP ([0]) into scalar fields; all nodes are NLB-registered for failover.
	primary := cpNodes[0]
	meta.ControlPlaneNodes = cpNodes
	meta.ControlPlaneSpreadGroup = spreadGroup
	meta.ControlPlaneInstanceID = primary.InstanceID
	meta.ControlPlaneENIID = primary.ENIID
	meta.ControlPlaneENIIP = primary.ENIIP
	meta.ControlPlaneMgmtIP = primary.MgmtIP

	// Persist CP IDs immediately; VMs are live now and must not be orphaned on later failure.
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "persist control-plane ids", err)
		return
	}

	egressIP, egressAllocID, err := s.allocateClusterEgressIP(accountID, name)
	if err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "allocate cluster egress IP", err)
		return
	}
	meta.EgressEIPAllocationID = egressAllocID
	meta.EgressEIPPublicIP = egressIP
	utils.PublishEvent(s.deps.NATSConn, "vpc.add-system-egress", systemEgressEvent{
		VpcId:      vpcID,
		SubnetId:   subnetIDs[0],
		InstanceIp: primary.ENIIP,
		ExternalIp: egressIP,
	})

	// Persist egress allocation so DeleteCluster can reclaim it on later failure.
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "persist egress allocation", err)
		return
	}

	cpENIIPs := make([]string, 0, len(cpNodes))
	for _, n := range cpNodes {
		cpENIIPs = append(cpENIIPs, n.ENIIP)
	}
	if err := RegisterClusterTargets(s.deps.NLB, accountID, nlb.TargetGroupArn, cpENIIPs); err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "register NLB targets", err)
		return
	}

	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.failClusterLaunch(acctKV, name, accountID, "persist final meta", err)
		return
	}

	// Seed creator system:masters AccessEntry so the token webhook can authenticate the creator immediately.
	if bootstrapCreatorAdmin(input) && callerPrincipalARN != "" {
		rec := newAccessEntryRecord(region, accountID, name, callerPrincipalARN, "",
			[]string{"system:masters"}, AccessEntryTypeStandard, nil, time.Now().UTC())
		if err := PutAccessEntryRecord(acctKV, rec); err != nil {
			s.failClusterLaunch(acctKV, name, accountID, "seed cluster-creator admin access entry", err)
			return
		}
	} else if bootstrapCreatorAdmin(input) {
		slog.Warn("CreateCluster: bootstrapClusterCreatorAdminPermissions set but caller principal ARN unknown; skipping creator-admin AccessEntry",
			"cluster", name, "accountID", accountID)
	}

	s.spawnBootstrap(accountID, name, acctKV, nil)
	s.spawnReconciler(accountID, name, meta)
}

// bootstrapCreatorAdmin reports whether the cluster-creator-admin AccessEntry
// should be minted. AWS defaults this to true when unspecified.
func bootstrapCreatorAdmin(input *eks.CreateClusterInput) bool {
	if input.AccessConfig == nil || input.AccessConfig.BootstrapClusterCreatorAdminPermissions == nil {
		return true
	}
	return *input.AccessConfig.BootstrapClusterCreatorAdminPermissions
}

// systemEgressEvent is the wire shape for vpc.add-system-egress /
// vpc.delete-system-egress (mirrors network/subscribers.SystemEgressEvent;
// kept local to avoid importing the network layer from a handler).
type systemEgressEvent struct {
	VpcId      string `json:"vpc_id"`
	SubnetId   string `json:"subnet_id"`
	InstanceIp string `json:"instance_ip"`
	ExternalIp string `json:"external_ip"`
}

// allocateClusterEgressIP allocates a hidden pool address for the cluster's
// control-plane VM egress, tagged ManagedBy=eks so it stays out of customer
// DescribeAddresses listings.
func (s *EKSServiceImpl) allocateClusterEgressIP(accountID, name string) (publicIP, allocationID string, err error) {
	out, err := s.deps.EIP.AllocateAddress(&ec2.AllocateAddressInput{
		Domain: aws.String("vpc"),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("elastic-ip"),
			Tags:         []*ec2.Tag{{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)}},
		}},
	}, accountID)
	if err != nil {
		return "", "", err
	}
	if out == nil || aws.StringValue(out.PublicIp) == "" || aws.StringValue(out.AllocationId) == "" {
		return "", "", errors.New("eks: AllocateAddress returned incomplete EIP")
	}
	return aws.StringValue(out.PublicIp), aws.StringValue(out.AllocationId), nil
}

func (s *EKSServiceImpl) DescribeCluster(input *eks.DescribeClusterInput, accountID string) (*eks.DescribeClusterOutput, error) {
	name := aws.StringValue(input.Name)
	if name == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID)
	if err != nil {
		return nil, fmt.Errorf("get account bucket: %w", err)
	}
	meta, err := GetClusterMeta(acctKV, name)
	if err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return &eks.DescribeClusterOutput{Cluster: clusterMetaToAWS(meta)}, nil
}

func (s *EKSServiceImpl) ListClusters(input *eks.ListClustersInput, accountID string) (*eks.ListClustersOutput, error) {
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, eksReadUnavailableOr(err, "jetstream")
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID)
	if err != nil {
		return nil, eksReadUnavailableOr(err, "get account bucket")
	}
	names, err := listClusterNames(acctKV)
	if err != nil {
		return nil, eksReadUnavailableOr(err, "list cluster names")
	}
	sort.Strings(names)

	maxResults := int(aws.Int64Value(input.MaxResults))
	if maxResults <= 0 || maxResults > 100 {
		maxResults = 100
	}
	startToken := aws.StringValue(input.NextToken)

	page := make([]string, 0, maxResults)
	var nextToken string
	startIdx := 0
	if startToken != "" {
		startIdx = sort.SearchStrings(names, startToken)
	}
	endIdx := min(startIdx+maxResults, len(names))
	page = append(page, names[startIdx:endIdx]...)
	if endIdx < len(names) {
		nextToken = names[endIdx]
	}

	out := &eks.ListClustersOutput{Clusters: aws.StringSlice(page)}
	if nextToken != "" {
		out.NextToken = aws.String(nextToken)
	}
	return out, nil
}

func (s *EKSServiceImpl) DeleteCluster(input *eks.DeleteClusterInput, accountID string) (*eks.DeleteClusterOutput, error) {
	if err := s.requireOrchestrationDeps("DeleteCluster"); err != nil {
		return nil, err
	}
	name := aws.StringValue(input.Name)
	if name == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID)
	if err != nil {
		return nil, fmt.Errorf("get account bucket: %w", err)
	}

	meta, err := GetClusterMeta(acctKV, name)
	if err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}

	if err := SetClusterStatus(acctKV, name, ClusterStatusDeleting); err != nil {
		return nil, fmt.Errorf("set DELETING: %w", err)
	}
	meta.Status = ClusterStatusDeleting

	if err := s.purgeClusterInfra(accountID, name, meta, acctKV, true); err != nil {
		slog.Error("DeleteCluster: teardown incomplete; leaving cluster DELETING for retry",
			"cluster", name, "err", err)
		return nil, fmt.Errorf("eks: DeleteCluster %s: %w", name, err)
	}

	return &eks.DeleteClusterOutput{Cluster: clusterMetaToAWS(meta)}, nil
}

// claimClusterName atomically claims the cluster meta key before any launch work.
// A prior FAILED attempt is reclaimed via CAS (FAILED→CREATING); live clusters reject with ResourceInUse.
func (s *EKSServiceImpl) claimClusterName(accountID string, acctKV nats.KeyValue, meta *ClusterMeta) error {
	name := meta.Name
	data, err := json.Marshal(meta)
	if err != nil {
		return logCreateErr(name, accountID, "marshal initial meta", err)
	}

	if _, cerr := acctKV.Create(ClusterMetaKey(name), data); cerr == nil {
		return nil // won a fresh claim
	} else if !errors.Is(cerr, nats.ErrKeyExists) {
		return logCreateErr(name, accountID, "claim cluster name", cerr)
	}

	// Key already present: reclaim a FAILED attempt via CAS; old refs survive for teardown.
	var oldRefs *ClusterMeta
	reclaimed := false
	if err := casUpdateMeta(acctKV, name, func(m *ClusterMeta) bool {
		// casUpdateMeta re-runs this closure on every CAS-conflict retry, so reset
		// the outputs each pass: a retry that now observes CREATING (a concurrent
		// reclaimer won) must leave reclaimed=false, or we double-own the cluster.
		reclaimed = false
		oldRefs = nil
		if m.Status != ClusterStatusFailed {
			return false
		}
		snapshot := *m
		oldRefs = &snapshot
		m.Status = ClusterStatusCreating
		reclaimed = true
		return true
	}); err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			// The FAILED record was swept underneath us (a concurrent reclaim or
			// delete). Treat as in-use; the caller may retry.
			return errors.New(awserrors.ErrorEKSResourceInUse)
		}
		return logCreateErr(name, accountID, "reclaim failed cluster", err)
	}
	if !reclaimed {
		slog.Info("CreateCluster: cluster already exists",
			"name", name, "accountID", accountID)
		return errors.New(awserrors.ErrorEKSResourceInUse)
	}

	slog.Info("CreateCluster: reclaiming FAILED cluster before recreate",
		"name", name, "accountID", accountID)
	// Purge the failed attempt's infra but KEEP the meta claim (deleteMeta=false)
	// — the CREATING record is our lock for the rest of this create.
	if perr := s.purgeClusterInfra(accountID, name, oldRefs, acctKV, false); perr != nil {
		s.markFailed(acctKV, name)
		return logCreateErr(name, accountID, "reclaim failed cluster", perr)
	}
	// Infra gone: overwrite with the fresh CREATING meta (clears the old refs).
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.markFailed(acctKV, name)
		return logCreateErr(name, accountID, "persist reclaimed meta", err)
	}
	return nil
}

// purgeClusterInfra tears down a cluster's live AWS resources and erases its
// per-cluster KV state. Zeroize (a security guarantee — no recoverable key
// material) plus NLB and VM teardown are blocking: their failures are joined
// and returned BEFORE the KV sweep, so a billable resource is never orphaned
// without the meta record that owns its ARNs/IDs. SG cleanup is best-effort
// (a leaked SG strands no owning record). Shared by DeleteCluster and the
// FAILED-cluster reclaim path in CreateCluster.
func (s *EKSServiceImpl) purgeClusterInfra(accountID, name string, meta *ClusterMeta, acctKV nats.KeyValue, deleteMeta bool) error {
	s.registry.Stop(accountID, name)

	var teardownErrs []error

	if err := ZeroizeClusterOIDCKey(acctKV, name); err != nil {
		teardownErrs = append(teardownErrs, fmt.Errorf("zeroize OIDC key: %w", err))
	}

	if meta.NLBArn != "" {
		// Deregister is best-effort: DeleteClusterNLB tears down the whole NLB
		// + target group, so a stale target registration cannot leak past it.
		if meta.NLBTargetGroupArn != "" && meta.ControlPlaneENIIP != "" {
			if err := DeregisterClusterTarget(s.deps.NLB, accountID, meta.NLBTargetGroupArn, meta.ControlPlaneENIIP); err != nil {
				slog.Warn("purgeClusterInfra: deregister NLB target failed", "cluster", name, "err", err)
			}
		}
		if err := DeleteClusterNLB(s.deps.NLB, accountID, name); err != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("delete NLB: %w", err))
		}
	}

	for _, cp := range controlPlaneTeardownNodes(meta) {
		if err := TerminateK3sServerVM(s.deps.VPCK3s, s.deps.Instance,
			accountID, cp.InstanceID, cp.ENIID); err != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("terminate K3s VM %s: %w", cp.InstanceID, err))
		}
	}

	// Egress: drop the OVN reroute+snat (fire-and-forget event, prune-safe) and
	// release the hidden pool address. ReleaseAddress is blocking — the EIP is
	// billable and its allocation ID lives in the meta we are about to erase, so
	// a release failure must not let the KV sweep orphan it.
	if meta.EgressEIPPublicIP != "" || meta.EgressEIPAllocationID != "" {
		if meta.ResourcesVpcConfig != nil && meta.ResourcesVpcConfig.VpcId != "" && meta.ControlPlaneENIIP != "" {
			subnetID := ""
			if len(meta.ResourcesVpcConfig.SubnetIds) > 0 {
				subnetID = meta.ResourcesVpcConfig.SubnetIds[0]
			}
			utils.PublishEvent(s.deps.NATSConn, "vpc.delete-system-egress", systemEgressEvent{
				VpcId:      meta.ResourcesVpcConfig.VpcId,
				SubnetId:   subnetID,
				InstanceIp: meta.ControlPlaneENIIP,
				ExternalIp: meta.EgressEIPPublicIP,
			})
		}
		if meta.EgressEIPAllocationID != "" {
			if _, err := s.deps.EIP.ReleaseAddress(&ec2.ReleaseAddressInput{
				AllocationId: aws.String(meta.EgressEIPAllocationID),
			}, accountID); err != nil {
				switch {
				case awserrors.IsErrorCode(err, awserrors.ErrorInvalidAllocationIDNotFound),
					awserrors.IsErrorCode(err, awserrors.ErrorInvalidAddressIDNotFound),
					awserrors.IsErrorCode(err, awserrors.ErrorInvalidAddressNotFound):
					// A prior retry (or the egress-delete cascade) already released
					// the allocation. Idempotent success — must NOT block the SG +
					// KV sweep, or the cluster wedges in DELETING permanently.
					slog.Debug("purgeClusterInfra: egress EIP already released",
						"cluster", name, "allocationId", meta.EgressEIPAllocationID)
				default:
					teardownErrs = append(teardownErrs, fmt.Errorf("release egress EIP: %w", err))
				}
			}
		}
	}

	if len(teardownErrs) > 0 {
		return errors.Join(teardownErrs...)
	}

	if meta.ResourcesVpcConfig != nil && meta.ResourcesVpcConfig.VpcId != "" {
		if err := DeleteClusterSGs(s.deps.VPCSG, accountID, name, meta.ResourcesVpcConfig.VpcId); err != nil {
			slog.Warn("purgeClusterInfra: DeleteClusterSGs failed", "cluster", name, "err", err)
		}
		// Reclaim the cluster-owned IGW (ownership-scoped: a reused customer IGW
		// is left intact). Must precede the VPC delete or that delete would hit
		// DependencyViolation on the still-attached gateway.
		if s.deps.IGW != nil {
			if err := DeleteClusterIGW(s.deps.IGW, accountID, meta.ResourcesVpcConfig.VpcId, name); err != nil {
				slog.Warn("purgeClusterInfra: DeleteClusterIGW failed", "cluster", name, "err", err)
			}
		}
	}

	// Release + delete the HA spread placement group (no-op for single-CP
	// clusters). Best-effort: the VMs are already gone, so a leaked internal
	// group strands nothing.
	s.teardownSpreadGroup(meta)

	// Only now, with NLB + VM confirmed gone, erase the meta + per-cluster KV.
	// The reclaim path passes deleteMeta=false: it has already CAS-claimed the
	// meta key (status CREATING) as its lock for the recreate, so the key must
	// survive — only the failed attempt's infra is purged here.
	if deleteMeta {
		if err := DeleteClusterPrefix(acctKV, name); err != nil {
			return fmt.Errorf("delete cluster prefix: %w", err)
		}
	}
	return nil
}

func (s *EKSServiceImpl) UpdateClusterConfig(_ *eks.UpdateClusterConfigInput, _ string) (*eks.UpdateClusterConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateClusterVersion(_ *eks.UpdateClusterVersionInput, _ string) (*eks.UpdateClusterVersionOutput, error) {
	return nil, notImpl()
}

// --- Nodegroup ---

func (s *EKSServiceImpl) CreateNodegroup(input *eks.CreateNodegroupInput, accountID string) (*eks.CreateNodegroupOutput, error) {
	if err := s.requireOrchestrationDeps("CreateNodegroup"); err != nil {
		return nil, err
	}
	acctKV, err := s.nodegroupAcctKV(accountID)
	if err != nil {
		return nil, err
	}
	return s.createNodegroup(acctKV, input, accountID)
}

func (s *EKSServiceImpl) DescribeNodegroup(input *eks.DescribeNodegroupInput, accountID string) (*eks.DescribeNodegroupOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.nodegroupAcctKV(accountID)
	if err != nil {
		return nil, err
	}
	return s.describeNodegroup(acctKV, input)
}

func (s *EKSServiceImpl) ListNodegroups(input *eks.ListNodegroupsInput, accountID string) (*eks.ListNodegroupsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.nodegroupAcctKV(accountID)
	if err != nil {
		return nil, err
	}
	return s.listNodegroups(acctKV, input)
}

func (s *EKSServiceImpl) UpdateNodegroupConfig(input *eks.UpdateNodegroupConfigInput, accountID string) (*eks.UpdateNodegroupConfigOutput, error) {
	if err := s.requireOrchestrationDeps("UpdateNodegroupConfig"); err != nil {
		return nil, err
	}
	acctKV, err := s.nodegroupAcctKV(accountID)
	if err != nil {
		return nil, err
	}
	return s.updateNodegroupConfig(acctKV, input, accountID)
}

func (s *EKSServiceImpl) UpdateNodegroupVersion(input *eks.UpdateNodegroupVersionInput, accountID string) (*eks.UpdateNodegroupVersionOutput, error) {
	return s.updateNodegroupVersion(input)
}

func (s *EKSServiceImpl) DeleteNodegroup(input *eks.DeleteNodegroupInput, accountID string) (*eks.DeleteNodegroupOutput, error) {
	if err := s.requireOrchestrationDeps("DeleteNodegroup"); err != nil {
		return nil, err
	}
	acctKV, err := s.nodegroupAcctKV(accountID)
	if err != nil {
		return nil, err
	}
	return s.deleteNodegroup(acctKV, input, accountID)
}

// --- AccessEntry + AccessPolicy ---

// acctKVForCluster opens the per-account bucket and verifies the cluster exists.
func (s *EKSServiceImpl) acctKVForCluster(accountID, cluster string) (nats.KeyValue, error) {
	if cluster == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID)
	if err != nil {
		return nil, fmt.Errorf("get account bucket: %w", err)
	}
	if _, err := GetClusterMeta(acctKV, cluster); err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return acctKV, nil
}

func (s *EKSServiceImpl) CreateAccessEntry(input *eks.CreateAccessEntryInput, accountID string) (*eks.CreateAccessEntryOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	principalARN := aws.StringValue(input.PrincipalArn)
	if principalARN == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	entryType := aws.StringValue(input.Type)
	if entryType == "" {
		entryType = AccessEntryTypeStandard
	}
	if entryType != AccessEntryTypeStandard {
		// Non-standard types (EC2_LINUX etc.) are not yet implemented.
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	if _, err := GetAccessEntryRecord(acctKV, cluster, principalARN); err == nil {
		return nil, errors.New(awserrors.ErrorEKSResourceInUse)
	} else if !errors.Is(err, ErrAccessEntryNotFound) {
		return nil, err
	}
	rec := newAccessEntryRecord(s.deps.Region, accountID, cluster, principalARN,
		aws.StringValue(input.Username), aws.StringValueSlice(input.KubernetesGroups),
		entryType, aws.StringValueMap(input.Tags), time.Now().UTC())
	if err := PutAccessEntryRecord(acctKV, rec); err != nil {
		return nil, err
	}
	return &eks.CreateAccessEntryOutput{AccessEntry: accessEntryRecordToAWS(rec)}, nil
}

func (s *EKSServiceImpl) DescribeAccessEntry(input *eks.DescribeAccessEntryInput, accountID string) (*eks.DescribeAccessEntryOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	principalARN := aws.StringValue(input.PrincipalArn)
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	rec, err := GetAccessEntryRecord(acctKV, cluster, principalARN)
	if err != nil {
		if errors.Is(err, ErrAccessEntryNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return &eks.DescribeAccessEntryOutput{AccessEntry: accessEntryRecordToAWS(rec)}, nil
}

func (s *EKSServiceImpl) ListAccessEntries(input *eks.ListAccessEntriesInput, accountID string) (*eks.ListAccessEntriesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	recs, err := ListAccessEntryRecords(acctKV, cluster)
	if err != nil {
		return nil, err
	}
	filter := aws.StringValue(input.AssociatedPolicyArn)
	arns := make([]string, 0, len(recs))
	for _, rec := range recs {
		if filter != "" && !hasAssociatedPolicy(rec, filter) {
			continue
		}
		arns = append(arns, rec.PrincipalARN)
	}
	return &eks.ListAccessEntriesOutput{AccessEntries: aws.StringSlice(arns)}, nil
}

func (s *EKSServiceImpl) UpdateAccessEntry(input *eks.UpdateAccessEntryInput, accountID string) (*eks.UpdateAccessEntryOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	principalARN := aws.StringValue(input.PrincipalArn)
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec, err := casUpdateAccessEntry(acctKV, cluster, principalARN, func(r *AccessEntryRecord) bool {
		if input.KubernetesGroups != nil {
			r.KubernetesGroups = aws.StringValueSlice(input.KubernetesGroups)
		}
		if u := aws.StringValue(input.Username); u != "" {
			r.KubernetesUsername = u
		}
		r.ModifiedAt = now
		return true
	})
	if err != nil {
		if errors.Is(err, ErrAccessEntryNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return &eks.UpdateAccessEntryOutput{AccessEntry: accessEntryRecordToAWS(rec)}, nil
}

func (s *EKSServiceImpl) DeleteAccessEntry(input *eks.DeleteAccessEntryInput, accountID string) (*eks.DeleteAccessEntryOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	principalARN := aws.StringValue(input.PrincipalArn)
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	if err := DeleteAccessEntryRecord(acctKV, cluster, principalARN); err != nil {
		if errors.Is(err, ErrAccessEntryNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return &eks.DeleteAccessEntryOutput{}, nil
}

func (s *EKSServiceImpl) AssociateAccessPolicy(input *eks.AssociateAccessPolicyInput, accountID string) (*eks.AssociateAccessPolicyOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	principalARN := aws.StringValue(input.PrincipalArn)
	policyARN := aws.StringValue(input.PolicyArn)
	if _, ok := supportedAccessPolicies[policyARN]; !ok {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	scope, err := validateAccessScope(input.AccessScope)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var assoc AssociatedAccessPolicy
	_, err = casUpdateAccessEntry(acctKV, cluster, principalARN, func(r *AccessEntryRecord) bool {
		assoc = AssociatedAccessPolicy{PolicyARN: policyARN, AccessScope: scope, AssociatedAt: now, ModifiedAt: now}
		for i := range r.AssociatedPolicies {
			if r.AssociatedPolicies[i].PolicyARN == policyARN {
				assoc.AssociatedAt = r.AssociatedPolicies[i].AssociatedAt
				r.AssociatedPolicies[i] = assoc
				r.ModifiedAt = now
				return true
			}
		}
		r.AssociatedPolicies = append(r.AssociatedPolicies, assoc)
		r.ModifiedAt = now
		return true
	})
	if err != nil {
		if errors.Is(err, ErrAccessEntryNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return &eks.AssociateAccessPolicyOutput{
		ClusterName:            aws.String(cluster),
		PrincipalArn:           aws.String(principalARN),
		AssociatedAccessPolicy: associatedPolicyToAWS(assoc),
	}, nil
}

func (s *EKSServiceImpl) DisassociateAccessPolicy(input *eks.DisassociateAccessPolicyInput, accountID string) (*eks.DisassociateAccessPolicyOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	principalARN := aws.StringValue(input.PrincipalArn)
	policyARN := aws.StringValue(input.PolicyArn)
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	_, err = casUpdateAccessEntry(acctKV, cluster, principalARN, func(r *AccessEntryRecord) bool {
		for i := range r.AssociatedPolicies {
			if r.AssociatedPolicies[i].PolicyARN == policyARN {
				r.AssociatedPolicies = append(r.AssociatedPolicies[:i], r.AssociatedPolicies[i+1:]...)
				r.ModifiedAt = now
				return true
			}
		}
		return false
	})
	if err != nil {
		if errors.Is(err, ErrAccessEntryNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return &eks.DisassociateAccessPolicyOutput{}, nil
}

func (s *EKSServiceImpl) ListAssociatedAccessPolicies(input *eks.ListAssociatedAccessPoliciesInput, accountID string) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	principalARN := aws.StringValue(input.PrincipalArn)
	acctKV, err := s.acctKVForCluster(accountID, cluster)
	if err != nil {
		return nil, err
	}
	rec, err := GetAccessEntryRecord(acctKV, cluster, principalARN)
	if err != nil {
		if errors.Is(err, ErrAccessEntryNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	policies := make([]*eks.AssociatedAccessPolicy, 0, len(rec.AssociatedPolicies))
	for _, p := range rec.AssociatedPolicies {
		policies = append(policies, associatedPolicyToAWS(p))
	}
	return &eks.ListAssociatedAccessPoliciesOutput{
		ClusterName:              aws.String(cluster),
		PrincipalArn:             aws.String(principalARN),
		AssociatedAccessPolicies: policies,
	}, nil
}

func (s *EKSServiceImpl) ListAccessPolicies(_ *eks.ListAccessPoliciesInput, _ string) (*eks.ListAccessPoliciesOutput, error) {
	arns := make([]string, 0, len(supportedAccessPolicies))
	for arn := range supportedAccessPolicies {
		arns = append(arns, arn)
	}
	sort.Strings(arns)
	policies := make([]*eks.AccessPolicy, 0, len(arns))
	for _, arn := range arns {
		policies = append(policies, &eks.AccessPolicy{
			Arn:  aws.String(arn),
			Name: aws.String(accessPolicyName(arn)),
		})
	}
	return &eks.ListAccessPoliciesOutput{AccessPolicies: policies}, nil
}

// hasAssociatedPolicy reports whether the entry has the given policy ARN bound.
func hasAssociatedPolicy(rec *AccessEntryRecord, policyARN string) bool {
	for _, p := range rec.AssociatedPolicies {
		if p.PolicyARN == policyARN {
			return true
		}
	}
	return false
}

// accessPolicyName extracts the policy short name from its ARN
// (…/cluster-access-policy/AmazonEKSViewPolicy → AmazonEKSViewPolicy).
func accessPolicyName(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

// --- Addons (see addons.go) ---

// --- OIDC identity-provider configs ---

func (s *EKSServiceImpl) AssociateIdentityProviderConfig(_ *eks.AssociateIdentityProviderConfigInput, _ string) (*eks.AssociateIdentityProviderConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeIdentityProviderConfig(_ *eks.DescribeIdentityProviderConfigInput, _ string) (*eks.DescribeIdentityProviderConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListIdentityProviderConfigs(_ *eks.ListIdentityProviderConfigsInput, _ string) (*eks.ListIdentityProviderConfigsOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DisassociateIdentityProviderConfig(_ *eks.DisassociateIdentityProviderConfigInput, _ string) (*eks.DisassociateIdentityProviderConfigOutput, error) {
	return nil, notImpl()
}

// --- Tags ---
//
// Store-only against the cluster meta record (no enforcement). Together with
// DescribeCluster echoing create-time tags this gives a stock terraform-aws
// provider a clean default_tags round-trip instead of perpetual drift. Only
// cluster ARNs are backed; other EKS resource ARNs return NotImplemented.

func (s *EKSServiceImpl) TagResource(input *eks.TagResourceInput, accountID string) (*eks.TagResourceOutput, error) {
	name, ok := clusterNameFromARN(aws.StringValue(input.ResourceArn))
	if !ok {
		return nil, notImpl()
	}
	acctKV, err := s.accountBucket(accountID)
	if err != nil {
		return nil, err
	}
	add := aws.StringValueMap(input.Tags)
	if err := casUpdateMeta(acctKV, name, func(m *ClusterMeta) bool {
		if len(add) == 0 {
			return false
		}
		if m.Tags == nil {
			m.Tags = make(map[string]string, len(add))
		}
		maps.Copy(m.Tags, add)
		return true
	}); err != nil {
		return nil, eksTagErr(err)
	}
	return &eks.TagResourceOutput{}, nil
}

func (s *EKSServiceImpl) UntagResource(input *eks.UntagResourceInput, accountID string) (*eks.UntagResourceOutput, error) {
	name, ok := clusterNameFromARN(aws.StringValue(input.ResourceArn))
	if !ok {
		return nil, notImpl()
	}
	acctKV, err := s.accountBucket(accountID)
	if err != nil {
		return nil, err
	}
	keys := aws.StringValueSlice(input.TagKeys)
	if err := casUpdateMeta(acctKV, name, func(m *ClusterMeta) bool {
		changed := false
		for _, k := range keys {
			if _, ok := m.Tags[k]; ok {
				delete(m.Tags, k)
				changed = true
			}
		}
		return changed
	}); err != nil {
		return nil, eksTagErr(err)
	}
	return &eks.UntagResourceOutput{}, nil
}

func (s *EKSServiceImpl) ListTagsForResource(input *eks.ListTagsForResourceInput, accountID string) (*eks.ListTagsForResourceOutput, error) {
	name, ok := clusterNameFromARN(aws.StringValue(input.ResourceArn))
	if !ok {
		return nil, notImpl()
	}
	acctKV, err := s.accountBucket(accountID)
	if err != nil {
		return nil, err
	}
	meta, err := GetClusterMeta(acctKV, name)
	if err != nil {
		return nil, eksTagErr(err)
	}
	return &eks.ListTagsForResourceOutput{Tags: aws.StringMap(meta.Tags)}, nil
}

// accountBucket returns the per-account KV bucket for accountID.
func (s *EKSServiceImpl) accountBucket(accountID string) (nats.KeyValue, error) {
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return GetOrCreateAccountBucket(js, accountID)
}

// clusterNameFromARN extracts the cluster name from an EKS cluster ARN
// (arn:aws:eks:<region>:<acct>:cluster/<name>), reporting false for any other
// ARN shape (e.g. nodegroup) the tag store does not back.
func clusterNameFromARN(arn string) (string, bool) {
	const prefix = "arn:aws:eks:"
	if !strings.HasPrefix(arn, prefix) {
		return "", false
	}
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 {
		return "", false
	}
	resType, name, found := strings.Cut(parts[5], "/")
	if !found || resType != "cluster" || name == "" {
		return "", false
	}
	return name, true
}

// eksTagErr maps a meta-store error to the AWS-visible tag-op error: a missing
// cluster surfaces as ResourceNotFound, everything else passes through.
func eksTagErr(err error) error {
	if errors.Is(err, ErrClusterNotFound) {
		return errors.New(awserrors.ErrorEKSResourceNotFound)
	}
	return err
}

// --- helpers ---

func notImpl() error { return errors.New(awserrors.ErrorNotImplemented) }

func validateCreateClusterInput(input *eks.CreateClusterInput) error {
	if input == nil || input.Name == nil || *input.Name == "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.RoleArn == nil || !strings.HasPrefix(*input.RoleArn, "arn:aws:iam:") {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if input.ResourcesVpcConfig == nil || len(input.ResourcesVpcConfig.SubnetIds) == 0 {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	for _, sn := range input.ResourcesVpcConfig.SubnetIds {
		if sn == nil || *sn == "" {
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}
	if input.AccessConfig != nil && input.AccessConfig.AuthenticationMode != nil {
		mode := *input.AccessConfig.AuthenticationMode
		if mode != "" && mode != eks.AuthenticationModeApi {
			return errors.New(awserrors.ErrorInvalidParameter)
		}
	}
	// AWS rejects disabling both public and private endpoint access — the
	// control plane would be unreachable.
	if v := input.ResourcesVpcConfig; v != nil &&
		v.EndpointPublicAccess != nil && !*v.EndpointPublicAccess &&
		v.EndpointPrivateAccess != nil && !*v.EndpointPrivateAccess {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

// defaultPublicAccessCidr is the AWS-default allowed source range for a public
// cluster endpoint when the caller specifies none.
const defaultPublicAccessCidr = "0.0.0.0/0"

// endpointAccess resolves apiserver endpoint exposure from the request,
// applying AWS defaults (public on, private off) to omitted fields.
func endpointAccess(vpc *eks.VpcConfigRequest) (public, private bool) {
	public, private = true, false
	if vpc == nil {
		return public, private
	}
	if vpc.EndpointPublicAccess != nil {
		public = *vpc.EndpointPublicAccess
	}
	if vpc.EndpointPrivateAccess != nil {
		private = *vpc.EndpointPrivateAccess
	}
	return public, private
}

// publicAccessCidrs returns the allowed source ranges for a public endpoint,
// defaulting to 0.0.0.0/0 (AWS parity) when public access is on and none given.
// Nil for a private-only endpoint.
func publicAccessCidrs(vpc *eks.VpcConfigRequest, public bool) []string {
	if !public {
		return nil
	}
	if vpc != nil && len(vpc.PublicAccessCidrs) > 0 {
		return aws.StringValueSlice(vpc.PublicAccessCidrs)
	}
	return []string{defaultPublicAccessCidr}
}

func (s *EKSServiceImpl) markFailed(kv nats.KeyValue, name string) {
	if err := SetClusterStatus(kv, name, ClusterStatusFailed); err != nil {
		slog.Warn("CreateCluster: SetClusterStatus(FAILED) failed", "cluster", name, "err", err)
	}
}

// spawnBootstrap launches the one-shot NATS bootstrap subscriber for a cluster.
// kinds nil means a fresh CreateCluster (wait on all four subjects); a non-nil
// subset is a daemon-restart resume that waits only on the missing artifacts.
func (s *EKSServiceImpl) spawnBootstrap(accountID, clusterName string, kv nats.KeyValue, kinds []string) {
	boot, err := NewNATSBootstrap(s.deps.NATSConn, kv, s.deps.MasterKey, accountID, clusterName)
	if err != nil {
		slog.Error("spawnBootstrap: NewNATSBootstrap failed", "cluster", clusterName, "err", err)
		return
	}
	go func() {
		err := boot.RunForKinds(s.bgCtx, kinds)
		if err == nil || errors.Is(err, context.Canceled) {
			return
		}
		// Bootstrap died without collecting all requested artifacts. Without
		// this the cluster sits in CREATING forever — fail it so
		// DescribeCluster surfaces the cause and DeleteCluster can reclaim the
		// resources.
		slog.Warn("NATSBootstrap exited; marking cluster FAILED", "cluster", clusterName, "err", err)
		if markErr := MarkClusterFailed(kv, clusterName, "bootstrap failed: "+err.Error()); markErr != nil &&
			!errors.Is(markErr, ErrClusterNotFound) {
			slog.Error("spawnBootstrap: MarkClusterFailed", "cluster", clusterName, "err", markErr)
		}
	}()
}

func (s *EKSServiceImpl) spawnReconciler(accountID, clusterName string, _ *ClusterMeta) {
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		slog.Error("spawnReconciler: jetstream", "err", err)
		return
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID)
	if err != nil {
		slog.Error("spawnReconciler: account bucket", "err", err)
		return
	}
	// Health gates on the control plane's NATS self-report, not an HTTP probe:
	// k3s binds the apiserver to the VPC node-ip, unreachable from the host. The
	// CP publishes {healthz,node_count} on the mgmt bus the daemon already shares.
	stateSubject := StateSubject(accountID, clusterName)
	addonStatusSubject := AddonStatusSubject(accountID, clusterName)
	spawn := func(ctx context.Context, _, _ string) (func(), error) {
		return RunClusterReconciler(ctx, s.leaderKV, acctKV, accountID, clusterName, s.deps.HolderID, "",
			WithStateSource(s.deps.NATSConn, stateSubject),
			WithAddonStatusSource(s.deps.NATSConn, addonStatusSubject))
	}
	if err := s.registry.Spawn(s.bgCtx, accountID, clusterName, spawn); err != nil {
		slog.Error("spawnReconciler: registry spawn", "cluster", clusterName, "err", err)
	}
}

func clusterMetaToAWS(meta *ClusterMeta) *eks.Cluster {
	if meta == nil {
		return nil
	}
	out := &eks.Cluster{
		Name:      aws.String(meta.Name),
		Arn:       aws.String(meta.Arn),
		Status:    aws.String(string(meta.Status)),
		Version:   aws.String(meta.Version),
		RoleArn:   aws.String(meta.RoleArn),
		CreatedAt: aws.Time(meta.CreatedAt),
	}
	if meta.Endpoint != "" {
		out.Endpoint = aws.String(meta.Endpoint)
	}
	if meta.OIDCIssuer != "" {
		out.Identity = &eks.Identity{Oidc: &eks.OIDC{Issuer: aws.String(meta.OIDCIssuer)}}
	}
	if meta.CertificateAuthorityB64 != "" {
		out.CertificateAuthority = &eks.Certificate{Data: aws.String(meta.CertificateAuthorityB64)}
	}
	if meta.ResourcesVpcConfig != nil {
		out.ResourcesVpcConfig = &eks.VpcConfigResponse{
			SubnetIds:             aws.StringSlice(meta.ResourcesVpcConfig.SubnetIds),
			SecurityGroupIds:      aws.StringSlice(meta.ResourcesVpcConfig.SecurityGroupIds),
			VpcId:                 aws.String(meta.ResourcesVpcConfig.VpcId),
			EndpointPublicAccess:  aws.Bool(meta.ResourcesVpcConfig.EndpointPublicAccess),
			EndpointPrivateAccess: aws.Bool(meta.ResourcesVpcConfig.EndpointPrivateAccess),
			PublicAccessCidrs:     aws.StringSlice(meta.ResourcesVpcConfig.PublicAccessCidrs),
		}
	}
	if meta.HealthIssue != "" {
		out.Health = &eks.ClusterHealth{
			Issues: []*eks.ClusterIssue{{
				Code:    aws.String(eks.ClusterIssueCodeClusterUnreachable),
				Message: aws.String(meta.HealthIssue),
			}},
		}
	}
	if len(meta.Tags) > 0 {
		out.Tags = aws.StringMap(meta.Tags)
	}
	return out
}

// natsTransient reports whether err is a transient NATS/JetStream condition
// rather than a genuine fault. These occur in the post-restart window before
// the KV stream's Raft group has elected a leader: JetStream requests get no
// responder, time out, or the stream is briefly unreachable.
func natsTransient(err error) bool {
	return err != nil && (errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, nats.ErrTimeout) ||
		errors.Is(err, nats.ErrNoStreamResponse) ||
		errors.Is(err, nats.ErrConnectionClosed))
}

// eksReadUnavailableOr maps a transient NATS/JetStream failure to a retryable
// ServiceUnavailable (503) so AWS clients back off and retry, instead of the
// daemon sanitizing an unrecognised wrapped error to a misleading 500
// InternalError. Non-transient errors pass through wrapped so real faults stay
// visible.
func eksReadUnavailableOr(err error, op string) error {
	if natsTransient(err) {
		slog.Warn("EKS read temporarily unavailable", "op", op, "err", err)
		return errors.New(awserrors.ErrorServiceUnavailable)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func listClusterNames(kv nats.KeyValue) ([]string, error) {
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("kv keys: %w", err)
	}
	suffix := "/meta"
	names := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if !strings.HasPrefix(k, "clusters/") || !strings.HasSuffix(k, suffix) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(k, "clusters/"), suffix)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names, nil
}

func deref(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}
