package handlers_eks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// SubnetVPCResolver resolves a subnet ID to its parent VPC ID and the VPC's
// CIDR block. Narrow so EKSServiceImpl can stay free of the wider
// handlers/ec2/vpc surface; the daemon adapts VPCServiceImpl onto this
// contract.
type SubnetVPCResolver interface {
	GetSubnetVPC(accountID, subnetID string) (vpcID string, err error)
	GetVPCCIDR(accountID, vpcID string) (cidr string, err error)
}

// EKSServiceDeps wires every external collaborator EKSServiceImpl needs.
// The narrow per-area interfaces (sgProvisioner, nlbProvisioner, etc.) are
// already defined alongside their consumers; the daemon adapts each
// service's concrete type onto the interface.
type EKSServiceDeps struct {
	Config         *config.Config
	NATSConn       *nats.Conn
	MasterKey      []byte
	GatewayBaseURL string
	Region         string
	HolderID       string

	// Gateway broker config handed to the K3s server VM so it can publish its
	// bootstrap envelopes + state reports via SigV4-signed HTTPS POST to the AWS
	// gateway (the ELBv2 lb-agent model) instead of dialing core NATS.
	// SystemGatewayURL is the mgmt-reachable AWSGW endpoint (distinct from
	// GatewayBaseURL, which is the OIDC issuer host); SystemAccessKey/SecretKey
	// are the system (Predastore) SigV4 creds; GatewayCACert is the PEM that
	// signs the gateway server cert.
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
	Worker    WorkerLauncher

	// PlacementGroup reserves distinct hosts for HA control-plane spread;
	// Scheduler answers the capacity + placement fan-out the spread path needs.
	// The daemon wires the NATS implementations.
	PlacementGroup controlPlanePlacer
	Scheduler      HostScheduler

	// AddonInstaller delivers managed-addon manifests to clusters. Optional:
	// when nil the service defaults to the KV staging installer
	// (newStagingInstaller) bound to NATSConn.
	AddonInstaller AddonInstaller
}

// WorkerLauncher is the narrow EC2 surface CreateNodegroup needs to launch and
// terminate nodegroup worker instances. Workers go through the normal
// customer-owned RunInstances path (no ManagedBy tag, no mgmt NIC) so they are
// visible in DescribeInstances and reclaimed by TerminateInstances like any
// other customer EC2. The daemon adapts its concrete instance service onto this.
// Exported so the daemon can assert *Daemon satisfies it with a compile-time
// check.
type WorkerLauncher interface {
	RunWorkerInstance(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error)
	TerminateWorkerInstances(instanceIDs []string, accountID string) error
}

// eipProvisioner is the narrow EIP surface CreateCluster needs to give the
// hidden K3s control-plane VM an egress-only public IP (the distributed-NAT
// dev/edge topology has no shared masquerade address). The daemon adapts the
// concrete EIP service onto this.
type eipProvisioner interface {
	AllocateAddress(input *ec2.AllocateAddressInput, accountID string) (*ec2.AllocateAddressOutput, error)
	ReleaseAddress(input *ec2.ReleaseAddressInput, accountID string) (*ec2.ReleaseAddressOutput, error)
}

// EKSServiceImpl is the daemon-side EKSService. CreateCluster /
// DescribeCluster / ListClusters / DeleteCluster have real bodies; every
// other action is still NotImplemented pending follow-up sprints.
type EKSServiceImpl struct {
	deps     EKSServiceDeps
	leaderKV nats.KeyValue
	registry *ReconcilerRegistry

	mu       sync.Mutex
	bgCtx    context.Context
	bgCancel context.CancelFunc
}

var _ EKSService = (*EKSServiceImpl)(nil)

const defaultK8sVersion = "1.32"

// NewEKSServiceImpl wires the Store, leader bucket, reconciler registry, and
// per-cluster background-goroutine context. Validates that MasterKey,
// GatewayBaseURL, Region, and HolderID are set when called from the live
// daemon path; the legacy NewEKSServiceImplWithNATS shim leaves them empty
// and bodies that need them return ServerInternal.
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

// NewEKSServiceImplWithNATS is a back-compat shim. Bodies that need
// orchestration deps return ServerInternal when invoked through this shim;
// the daemon-handler routing test calls it to verify NATS subject wiring
// without standing up the full dependency graph.
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

// SpawnRegisteredReconcilers enumerates every cluster in every per-account
// bucket and registers a reconciler goroutine for each cluster in
// CREATING or ACTIVE state. Called by the daemon on boot so a node restart
// resumes lifecycle reconcile without waiting for the next CreateCluster.
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
			// A restart mid-CREATING lost the bootstrap subscriptions; the K3s
			// VM publishes its one-shot core-NATS messages once and to nobody.
			// Re-subscribe to whichever artifacts are still missing so the
			// cluster can still reach ACTIVE instead of hanging in CREATING.
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

// requireOrchestrationDeps returns a client-facing ServiceUnavailable (with the
// specific unmet deps logged at ERROR) when the daemon is missing orchestration
// wiring, so the failure is diagnosable instead of a bare ServerInternal.
func (s *EKSServiceImpl) requireOrchestrationDeps(op string) error {
	missing := s.missingOrchestrationDeps()
	if len(missing) == 0 {
		return nil
	}
	slog.Error("EKS orchestration deps not ready", "op", op, "missing", missing)
	return errors.New(awserrors.ErrorServiceUnavailable)
}

// missingOrchestrationDeps names the deps required for cluster orchestration
// that are unset, so a "deps not ready" rejection can log the precise cause
// instead of an opaque ServerInternal whose only hint is a one-time boot WARN.
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

// logCreateErr records the precise cause of a CreateCluster step failure at
// ERROR. The request surface collapses these wrapped errors to an opaque
// ServerInternal ("An internal error has occurred... contact AWS re:Post"), so
// without this the wrapped error never reaches the operator. Returns the error
// wrapped with the stage for the caller to surface.
func logCreateErr(name, accountID, stage string, err error) error {
	slog.Error("CreateCluster: "+stage+" failed", "cluster", name, "accountID", accountID, "err", err)
	return fmt.Errorf("%s: %w", stage, err)
}

// managedIngressTagKey is the CreateCluster tag that controls K3s' bundled
// traefik + servicelb (interim in-VPC app exposure). They are ON by default so
// workloads are reachable before the AWS Load Balancer Controller add-on ships;
// value "false" (case-insensitive) opts out for AWS parity. TODO: flip the
// default back off (parity) once the aws-lb-controller add-on lands — see
// docs/development/feature/eks-dataplane-ingress.md.
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

	if existing, err := GetClusterMeta(acctKV, name); err == nil {
		if existing.Status != ClusterStatusFailed {
			slog.Info("CreateCluster: cluster already exists",
				"name", name, "accountID", accountID, "status", existing.Status)
			return nil, errors.New(awserrors.ErrorEKSResourceInUse)
		}
		// A prior attempt failed. Reclaim whatever it left behind and clear its
		// state so this create starts clean — makes a retry (including the
		// SDK's own auto-retry) idempotent instead of masking the original
		// failure with a spurious "already exists" error. If teardown of the
		// failed attempt's resources fails, surface that rather than recreating
		// on top of orphans.
		slog.Info("CreateCluster: reclaiming FAILED cluster before recreate",
			"name", name, "accountID", accountID)
		if perr := s.purgeClusterInfra(accountID, name, existing, acctKV); perr != nil {
			return nil, logCreateErr(name, accountID, "reclaim failed cluster", perr)
		}
	} else if !errors.Is(err, ErrClusterNotFound) {
		return nil, logCreateErr(name, accountID, "preflight get meta", err)
	}

	vpcID, err := s.deps.VPCSubnet.GetSubnetVPC(accountID, subnetIDs[0])
	if err != nil {
		// Subnet is a caller-supplied parameter — a resolve failure is a client
		// fault (bad/foreign subnet), not an internal one. Surface it as such
		// instead of collapsing to a retryable ServerInternal the SDK retries.
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
		CreatedAt:      time.Now().UTC(),
	}
	if err := PutClusterMeta(acctKV, meta); err != nil {
		return nil, logCreateErr(name, accountID, "persist initial meta", err)
	}

	cpSG, ngSG, err := EnsureClusterSGs(s.deps.VPCSG, accountID, name, vpcID)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "ensure cluster SGs", err)
	}
	meta.ResourcesVpcConfig.SecurityGroupIds = []string{cpSG, ngSG}

	// The NLB's backing LB VM forwards the published endpoint to the apiserver
	// from inside the VPC, so the control-plane SG must admit that hop or the
	// NLB target stays unhealthy and the endpoint never serves.
	vpcCIDR, err := s.deps.VPCSubnet.GetVPCCIDR(accountID, vpcID)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "resolve vpc cidr", err)
	}
	if err := EnsureControlPlaneIngress(s.deps.VPCSG, accountID, cpSG, vpcCIDR); err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "ensure control-plane ingress", err)
	}

	// Public access ⇒ internet-facing NLB (external-pool front-end IP, reachable
	// on the LAN/edge network); private-only ⇒ internal NLB (VPC-only).
	nlb, err := EnsureClusterNLB(s.deps.NLB, accountID, name, subnetIDs, publicAccess, publicCidrs)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "ensure cluster NLB", err)
	}
	// Prefer the reachable front-end IP as the kubeconfig endpoint host so the
	// apiserver serving cert (which SANs this IP) validates with TLS verification
	// on. Fall back to the synthetic NLB DNS name when no IP was read back.
	endpointHost := nlb.FrontendIP
	if endpointHost == "" {
		endpointHost = nlb.DNSName
	}
	meta.Endpoint = "https://" + net.JoinHostPort(endpointHost, strconv.FormatInt(clusterNLBListenPort, 10))
	meta.EndpointIP = nlb.FrontendIP
	meta.NLBArn = nlb.LoadBalancerArn
	meta.NLBTargetGroupArn = nlb.TargetGroupArn

	// Persist the NLB ARNs now, before any further fallible step. Without this
	// a failure between here and the final PutClusterMeta would leave the NLB +
	// target group + listener + backing LB VM created but unrecorded, so
	// DeleteCluster (which keys teardown off the persisted ARNs) could not
	// reclaim them.
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "persist NLB arns", err)
	}

	oidcIssuer, err := ClusterOIDCIssuer(s.deps.GatewayBaseURL, region, accountID, name)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "build OIDC issuer", err)
	}
	meta.OIDCIssuer = oidcIssuer

	privPEM, _, err := GenerateClusterOIDCKeypair(acctKV, name, s.deps.MasterKey)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "generate OIDC keypair", err)
	}
	pubPEM, err := PublicKeyPEMFromPrivate(privPEM)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "derive OIDC public key", err)
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
	})
	if err != nil {
		s.markFailed(acctKV, name)
		if errors.Is(err, ErrEKSServerAMINotFound) {
			slog.Error("CreateCluster: eks-server AMI not found; cannot launch control plane",
				"cluster", name, "accountID", accountID, "err", err)
			return nil, errors.New(awserrors.ErrorServiceUnavailable)
		}
		slog.Error("CreateCluster: K3s control-plane VM launch failed",
			"cluster", name, "accountID", accountID, "err", err)
		return nil, fmt.Errorf("launch K3s VM: %w", err)
	}
	// Mirror the primary ([0]) into the scalar fields the reconciler, NLB
	// registration, egress wiring, and teardown read today. Until per-node NLB
	// registration (231.7.3) only the primary is target-registered + egress-wired.
	primary := cpNodes[0]
	meta.ControlPlaneNodes = cpNodes
	meta.ControlPlaneSpreadGroup = spreadGroup
	meta.ControlPlaneInstanceID = primary.InstanceID
	meta.ControlPlaneENIID = primary.ENIID
	meta.ControlPlaneENIIP = primary.ENIIP
	meta.ControlPlaneMgmtIP = primary.MgmtIP

	// Egress: the control-plane VM must pull container images. Allocate a
	// hidden pool address and wire an egress-only SNAT for its /32 (no DNAT —
	// the VM stays unreachable inbound; the NLB is its only front door).
	egressIP, egressAllocID, err := s.allocateClusterEgressIP(accountID, name)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "allocate cluster egress IP", err)
	}
	meta.EgressEIPAllocationID = egressAllocID
	meta.EgressEIPPublicIP = egressIP
	utils.PublishEvent(s.deps.NATSConn, "vpc.add-system-egress", systemEgressEvent{
		VpcId:      vpcID,
		SubnetId:   subnetIDs[0],
		InstanceIp: primary.ENIIP,
		ExternalIp: egressIP,
	})

	// Record the VM + ENI + egress IP before registering the target so a failed
	// register (or anything after) leaves them recoverable by DeleteCluster.
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "persist control-plane ids", err)
	}

	if err := RegisterClusterTarget(s.deps.NLB, accountID, nlb.TargetGroupArn, primary.ENIIP); err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "register NLB target", err)
	}

	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.markFailed(acctKV, name)
		return nil, logCreateErr(name, accountID, "persist final meta", err)
	}

	// bootstrapClusterCreatorAdminPermissions (default true): grant the caller
	// system:masters via an AccessEntry so the cluster creator can immediately
	// authenticate through the token webhook (Q9). Keyed by the caller's exact
	// principal ARN — the webhook looks the entry up by the same ARN STS returns.
	if bootstrapCreatorAdmin(input) && callerPrincipalARN != "" {
		rec := newAccessEntryRecord(region, accountID, name, callerPrincipalARN, "",
			[]string{"system:masters"}, AccessEntryTypeStandard, nil, time.Now().UTC())
		if err := PutAccessEntryRecord(acctKV, rec); err != nil {
			s.markFailed(acctKV, name)
			return nil, fmt.Errorf("seed cluster-creator admin access entry: %w", err)
		}
	} else if bootstrapCreatorAdmin(input) {
		slog.Warn("CreateCluster: bootstrapClusterCreatorAdminPermissions set but caller principal ARN unknown; skipping creator-admin AccessEntry",
			"cluster", name, "accountID", accountID)
	}

	s.spawnBootstrap(accountID, name, acctKV, nil)
	s.spawnReconciler(accountID, name, meta)

	return &eks.CreateClusterOutput{Cluster: clusterMetaToAWS(meta)}, nil
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

	if err := s.purgeClusterInfra(accountID, name, meta, acctKV); err != nil {
		slog.Error("DeleteCluster: teardown incomplete; leaving cluster DELETING for retry",
			"cluster", name, "err", err)
		return nil, fmt.Errorf("eks: DeleteCluster %s: %w", name, err)
	}

	return &eks.DeleteClusterOutput{Cluster: clusterMetaToAWS(meta)}, nil
}

// purgeClusterInfra tears down a cluster's live AWS resources and erases its
// per-cluster KV state. Zeroize (a security guarantee — no recoverable key
// material) plus NLB and VM teardown are blocking: their failures are joined
// and returned BEFORE the KV sweep, so a billable resource is never orphaned
// without the meta record that owns its ARNs/IDs. SG cleanup is best-effort
// (a leaked SG strands no owning record). Shared by DeleteCluster and the
// FAILED-cluster reclaim path in CreateCluster.
func (s *EKSServiceImpl) purgeClusterInfra(accountID, name string, meta *ClusterMeta, acctKV nats.KeyValue) error {
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
				teardownErrs = append(teardownErrs, fmt.Errorf("release egress EIP: %w", err))
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
	}

	// Release + delete the HA spread placement group (no-op for single-CP
	// clusters). Best-effort: the VMs are already gone, so a leaked internal
	// group strands nothing.
	s.teardownSpreadGroup(meta)

	// Only now, with NLB + VM confirmed gone, erase the meta + per-cluster KV.
	if err := DeleteClusterPrefix(acctKV, name); err != nil {
		return fmt.Errorf("delete cluster prefix: %w", err)
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

// acctKVForCluster opens the per-account bucket and verifies the cluster exists,
// translating absence to the AWS ResourceNotFoundException shape. The
// AccessEntry handlers all need both.
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
		// Node-type entries (EC2_LINUX/etc.) need the nodegroup join path; reject
		// until Sprint 6d wires them.
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

func (s *EKSServiceImpl) TagResource(_ *eks.TagResourceInput, _ string) (*eks.TagResourceOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UntagResource(_ *eks.UntagResourceInput, _ string) (*eks.UntagResourceOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListTagsForResource(_ *eks.ListTagsForResourceInput, _ string) (*eks.ListTagsForResourceOutput, error) {
	return nil, notImpl()
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
	spawn := func(ctx context.Context, _, _ string) (func(), error) {
		return RunClusterReconciler(ctx, s.leaderKV, acctKV, accountID, clusterName, s.deps.HolderID, "",
			WithStateSource(s.deps.NATSConn, stateSubject))
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
