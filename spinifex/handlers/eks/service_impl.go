package handlers_eks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// SubnetVPCResolver resolves a subnet to its VPC ID and CIDR.
type SubnetVPCResolver interface {
	GetSubnetVPC(ctx context.Context, accountID, subnetID string) (vpcID string, err error)
	GetVPCCIDR(ctx context.Context, accountID, vpcID string) (cidr string, err error)
	GetSubnetAZ(ctx context.Context, accountID, subnetID string) (az string, err error)
}

// EKSServiceDeps wires the external collaborators EKSServiceImpl needs.
type EKSServiceDeps struct {
	Config         *config.Config
	NATSConn       *nats.Conn
	MasterKey      []byte
	GatewayBaseURL string
	Region         string
	HolderID       string

	// ClusterSize is the daemon's node count, used as the JetStream replica
	// count for the lazily-created per-account and leader KV buckets so they
	// match the cluster's other R3 streams instead of staying stuck at R1.
	ClusterSize int

	// InternalSuffix is the AWS-parity internal DNS suffix (e.g. spinifex.internal)
	// used to compose the worker's ECR registry host.
	InternalSuffix string

	// Gateway broker config for the K3s server VM (SigV4 HTTPS to AWSGW).
	// SystemGatewayURL is the mgmt-reachable endpoint; GatewayCACert signs its TLS cert.
	SystemGatewayURL string
	SystemAccessKey  string
	SystemSecretKey  string
	GatewayCACert    string

	// SystemPredastoreURL is the mgmt-bridge-reachable predastore endpoint baked
	// into the CP VM's etcd-snapshot.env (SystemAccessKey/SystemSecretKey double
	// as the predastore SigV4 creds — same system credential used for the gateway).
	SystemPredastoreURL string

	// SnapshotStore lists/reads etcd snapshots from the eks-backups-system bucket
	// for restore-snapshot's latest-snapshot resolution. Nil disables that lookup
	// (callers must pass --snapshot explicitly).
	SnapshotStore objectstore.ObjectStore

	VPCSG     sgProvisioner
	VPCK3s    k3sVPCProvisioner
	VPCSubnet SubnetVPCResolver
	NLB       nlbProvisioner
	Instance  k3sInstanceLauncher
	Image     k3sAMIResolver
	EIP       eipProvisioner
	IGW       igwProvisioner
	Worker    WorkerLauncher

	// IAM backs a nodegroup's node role with an instance profile so workers expose
	// the role over IMDS for the ECR credential provider. Nil disables the wiring
	// (workers launch without a profile and cannot pull from the internal ECR).
	// Tests set this directly; production prefers IAMProvider.
	IAM instanceProfileEnsurer

	// IAMProvider lazily resolves the IAM ensurer at cluster-launch time so it
	// cannot race the NATS KV backend at daemon startup. Preferred over IAM when
	// set; its concrete service satisfies instanceProfileEnsurer.
	IAMProvider func() handlers_iam.SystemInstanceRoleEnsurer

	// VPCMgr / NATGW / RouteTable compose the managed control-plane VPC ("Set B")
	// from the real EC2 VPC-family APIs under the system account. The daemon
	// adapts its concrete VPC / NAT-gateway / route-table services onto these.
	VPCMgr     vpcProvisioner
	NATGW      natGatewayProvisioner
	RouteTable routeTableProvisioner

	// PlacementGroup reserves distinct hosts for HA control-plane spread;
	// Scheduler answers the capacity + placement fan-out the spread path needs.
	// The daemon wires the NATS implementations.
	PlacementGroup controlPlanePlacer
	Scheduler      HostScheduler

	// AddonInstaller delivers managed-addon manifests; nil defaults to the KV staging installer.
	AddonInstaller AddonInstaller

	// CPControl lets the reconciler recover a wedged control-plane VM: describe
	// its state and restart it. Nil disables auto-restart (health is still
	// reflected). The daemon wires a NATS-backed impl whose DescribeInstances
	// fans out across every host and whose RecoverInstance restarts the CP on
	// its owning node whatever its state.
	CPControl cpInstanceController
}

// cpInstanceController is the narrow EC2 instance surface the EKS reconciler uses
// to recover a wedged control-plane VM. The daemon wires a NATS-backed impl:
// DescribeInstances fans out across all hosts so a CP on any node is observed;
// RecoverInstance restarts the CP on its owning node whatever its state — a live
// error/running owner in place via ec2.cmd.<id>, or a stopped instance rehydrated
// from the shared KV via ec2.start.
type cpInstanceController interface {
	DescribeInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error)
	RecoverInstance(instanceID, accountID string) error
	// StopInstance gracefully powers off a running CP so the restart path boots it
	// clean and the boot-time recovery agent applies a pending directive (the etcd
	// reset path).
	StopInstance(instanceID, accountID string) error
}

// cpControlAdapter binds a cpInstanceController + accountID to the reconciler's
// CPInstanceControl surface (whose InstanceState/StartInstance take no accountID
// — one reconciler serves one account).
type cpControlAdapter struct {
	ctl       cpInstanceController
	accountID string
}

var _ CPInstanceControl = cpControlAdapter{}

// InstanceState returns the CP instance's EC2 lifecycle state name, or an error
// if the instance is not visible to the account.
func (a cpControlAdapter) InstanceState(_ context.Context, instanceID string) (string, error) {
	out, err := a.ctl.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, a.accountID)
	if err != nil {
		return "", err
	}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			if aws.StringValue(inst.InstanceId) == instanceID && inst.State != nil {
				return aws.StringValue(inst.State.Name), nil
			}
		}
	}
	return "", fmt.Errorf("eks: control-plane instance %s not found", instanceID)
}

// StartInstance restarts a wedged CP via the instance service, which routes to
// the CP's owning node — recovering a live error/running owner in place or a
// stopped instance from the shared KV — and re-mounts the same root volume.
func (a cpControlAdapter) StartInstance(_ context.Context, instanceID string) error {
	return a.ctl.RecoverInstance(instanceID, a.accountID)
}

// StopInstance gracefully powers off a running CP (QMP system_powerdown) so the
// in-place restart path boots it clean and the on-VM recovery agent applies its
// pending directive — the etcd reset path. A graceful stop unmounts cleanly, so the
// next boot is not fsck-corrupted the way a hard reboot would leave it.
func (a cpControlAdapter) StopInstance(_ context.Context, instanceID string) error {
	return a.ctl.StopInstance(instanceID, a.accountID)
}

// WorkerLauncher is the narrow EC2 surface for launching nodegroup worker instances.
// Workers use the customer-owned RunInstances path (no ManagedBy tag, no mgmt NIC).
type WorkerLauncher interface {
	RunWorkerInstance(ctx context.Context, input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error)
	// RunWorkerInstanceOnNode launches the worker on a specific host for nodegroup
	// host spread. An empty nodeID launches on the local node like RunWorkerInstance.
	RunWorkerInstanceOnNode(ctx context.Context, nodeID string, input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error)
	TerminateWorkerInstances(ctx context.Context, instanceIDs []string, accountID string) error
}

// iamEnsurer resolves the IAM ensurer, preferring the test-injected deps.IAM,
// then the lazy IAMProvider (built at launch time to dodge the daemon-startup
// NATS-KV race). Returns nil when neither yields a service, so callers fall back
// to static creds / skip the profile. The provider's concrete service satisfies
// instanceProfileEnsurer (identical method set to SystemInstanceRoleEnsurer).
func (s *EKSServiceImpl) iamEnsurer() instanceProfileEnsurer {
	if s.deps.IAM != nil {
		return s.deps.IAM
	}
	if s.deps.IAMProvider != nil {
		if e := s.deps.IAMProvider(); e != nil {
			return e
		}
	}
	return nil
}

// instanceProfileEnsurer is the narrow IAM surface EKS needs to find-or-create the
// instance profile that fronts a nodegroup's node role. Real EKS creates this
// profile implicitly for a node role; Spinifex does the same at worker launch.
type instanceProfileEnsurer interface {
	GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error)
	CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error)
	AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error)

	// Role find-or-create surface for the system-managed control-plane role.
	// Unlike worker node roles (customer-supplied), the k3s server role is
	// created by Spinifex with the IMDS-scoped gateway permissions it needs.
	GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error)
	CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error)
	PutRolePolicy(accountID string, input *iam.PutRolePolicyInput) (*iam.PutRolePolicyOutput, error)
}

// eipProvisioner is the narrow EIP surface for allocating a CP VM egress IP.
type eipProvisioner interface {
	AllocateAddress(ctx context.Context, input *ec2.AllocateAddressInput, accountID string) (*ec2.AllocateAddressOutput, error)
	ReleaseAddress(ctx context.Context, input *ec2.ReleaseAddressInput, accountID string) (*ec2.ReleaseAddressOutput, error)
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

	// nodegroupReadyTimeout / nodegroupReadyPoll bound how long launchNodegroupInfra
	// waits for its workers to register Ready before marking the nodegroup
	// CREATE_FAILED. Tests inject small values.
	nodegroupReadyTimeout time.Duration
	nodegroupReadyPoll    time.Duration
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
	leaderKV, err := InitLeaderBucket(js, max(deps.ClusterSize, 1))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &EKSServiceImpl{
		deps:                  deps,
		leaderKV:              leaderKV,
		registry:              NewReconcilerRegistry(),
		bgCtx:                 ctx,
		bgCancel:              cancel,
		nodegroupReadyTimeout: defaultNodegroupReadyTimeout,
		nodegroupReadyPoll:    defaultNodegroupReadyPoll,
	}, nil
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
			s.reclaimOrphanedNodegroups(context.Background(), accountID, acctKV, cluster)
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
	// IAM is built from MasterKey but can stay nil when its KV backend was not
	// ready at boot. Gate on the resolved ensurer (deps.IAM or the lazy
	// IAMProvider) so a node without it rejects nodegroup orchestration instead
	// of launching workers with no instance profile (no IMDS role, so the
	// load-balancer controller cannot create an ALB).
	if s.iamEnsurer() == nil {
		missing = append(missing, "IAM")
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

func (s *EKSServiceImpl) CreateCluster(ctx context.Context, input *eks.CreateClusterInput, accountID, callerPrincipalARN string) (*eks.CreateClusterOutput, error) {
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
	acctKV, err := GetOrCreateAccountBucket(js, accountID, max(s.deps.ClusterSize, 1))
	if err != nil {
		return nil, logCreateErr(name, accountID, "get account bucket", err)
	}

	vpcID, err := s.deps.VPCSubnet.GetSubnetVPC(ctx, accountID, subnetIDs[0])
	if err != nil {
		// Subnet resolve failure is a client fault (bad/foreign subnet).
		slog.ErrorContext(ctx, "CreateCluster: resolve subnet VPC failed",
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
		Tags:      aws.StringValueMap(input.Tags),
		CreatedAt: time.Now().UTC(),
	}
	// Claim the cluster name before any launching; duplicate/retry handlers lose the claim.
	if err := s.claimClusterName(ctx, accountID, acctKV, meta); err != nil {
		return nil, err
	}

	// Respond CREATING immediately; run the slow launch on a background goroutine.
	// The goroutine owns meta from here on — do not touch it after this point.
	out := &eks.CreateClusterOutput{Cluster: clusterMetaToAWS(meta)}
	s.launchWG.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				s.failClusterLaunch(context.Background(), acctKV, name, accountID, meta, "launch panic", fmt.Errorf("%v", r))
			}
		}()
		s.launchClusterInfra(context.Background(), clusterLaunchCtx{
			accountID:          accountID,
			callerPrincipalARN: callerPrincipalARN,
			name:               name,
			region:             region,
			subnetIDs:          subnetIDs,
			vpcID:              vpcID,
			publicAccess:       publicAccess,
			privateAccess:      privateAccess,
			publicCidrs:        publicCidrs,
			input:              input,
			meta:               meta,
			acctKV:             acctKV,
		})
	})
	return out, nil
}

// dedupSubnetsByAZ resolves each subnet's AZ and keeps the first subnet per AZ,
// preserving input order. AWS ALB rejects two subnets in the same AZ, so the
// alb IngressClassParams must carry at most one per AZ; the ALBSingleSubnet gate
// then relaxes the minimum to one. A resolve error drops that subnet (and logs),
// leaving an empty result that falls back to LBC tag auto-discovery.
func dedupSubnetsByAZ(ctx context.Context, resolver SubnetVPCResolver, accountID string, subnetIDs []string) []string {
	if resolver == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(subnetIDs))
	out := make([]string, 0, len(subnetIDs))
	for _, id := range subnetIDs {
		az, err := resolver.GetSubnetAZ(ctx, accountID, id)
		if err != nil || az == "" {
			slog.Warn("dedupSubnetsByAZ: skip subnet, AZ unresolved", "subnet", id, "err", err)
			continue
		}
		if _, ok := seen[az]; ok {
			continue
		}
		seen[az] = struct{}{}
		out = append(out, id)
	}
	return out
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
	privateAccess      bool
	publicCidrs        []string
	input              *eks.CreateClusterInput
	meta               *ClusterMeta
	acctKV             nats.KeyValue
}

// failClusterLaunch records an asynchronous control-plane launch failure: it
// marks the cluster FAILED (with reason, so DescribeCluster surfaces the cause
// and the 267.3 reclaim path can recreate cleanly) and logs the stage. The
// launch runs after the CREATING response was already sent, so a failure can
// only surface as status, never as the CreateCluster return value.
func (s *EKSServiceImpl) failClusterLaunch(ctx context.Context, kv nats.KeyValue, name, accountID string, meta *ClusterMeta, stage string, err error) {
	_ = logCreateErr(name, accountID, stage, err)
	// Release any billable infra already provisioned this launch (LB VM + its
	// associated EIP, egress EIP, NLB, CP VMs, OIDC) so a failed create never
	// strands resources. Cluster names are unique, so the same-name reclaim that
	// would otherwise purge a FAILED attempt never fires — without this, the
	// internet-facing LB VM's EIP leaks permanently. Operate on the in-memory
	// meta: a failure in PutClusterMeta itself leaves the freshest refs only
	// here, not in the KV record. Best-effort; the FAILED status is recorded
	// regardless (deleteMeta=false keeps the meta for DescribeCluster/reclaim).
	if meta != nil {
		if perr := s.purgeClusterInfra(ctx, accountID, name, meta, kv, false); perr != nil {
			slog.WarnContext(ctx, "failClusterLaunch: infra purge incomplete", "cluster", name, "stage", stage, "err", perr)
		}
	}
	if mErr := MarkClusterFailed(kv, name, stage+": "+err.Error()); mErr != nil && !errors.Is(mErr, ErrClusterNotFound) {
		slog.WarnContext(ctx, "launchClusterInfra: MarkClusterFailed failed", "cluster", name, "err", mErr)
	}
}

// launchClusterInfra is CreateCluster's slow phase (SGs, NLB, OIDC, CP VMs, egress, bootstrap).
// Runs on a background goroutine; every failure marks the cluster FAILED.
func (s *EKSServiceImpl) launchClusterInfra(ctx context.Context, lc clusterLaunchCtx) {
	accountID := lc.accountID
	callerPrincipalARN := lc.callerPrincipalARN
	name := lc.name
	region := lc.region
	publicAccess := lc.publicAccess
	privateAccess := lc.privateAccess
	publicCidrs := lc.publicCidrs
	input := lc.input
	meta := lc.meta
	acctKV := lc.acctKV

	// Build the managed control-plane VPC ("Set B") under the system account: the
	// AWS-managed-account analogue the customer never provisions. The
	// internet-facing NLB lives in its public subnet and the control-plane VM(s)
	// in its private subnet, so the NLB→CP hop is in-VPC (unaffected by the
	// private-subnet egress drop) and the private CP egresses via the NAT gateway
	// (DNS + docker.io image pulls). Composed from the real EC2 VPC-family APIs,
	// so the per-subnet egress policies are wired by the topology subscribers.
	sysAcct := admin.SystemAccountID()
	cpRefs, err := EnsureClusterCPVPC(ctx, s.cpVPCDeps(), sysAcct, name, region, cpVPCPrivateSubnetCount)
	if err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "ensure managed CP VPC", err)
		return
	}
	meta.ManagedCPVPC = managedCPVPCFromRefs(cpRefs)
	// Persist the CP VPC refs before any further fallible step so teardown can
	// reclaim the VPC/subnets/IGW/NAT GW even if the launch fails after here.
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "persist managed CP VPC", err)
		return
	}

	cpSG, ngSG, err := EnsureClusterSGs(ctx, s.deps.VPCSG, sysAcct, name, cpRefs.VpcID)
	if err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "ensure cluster SGs", err)
		return
	}
	meta.ResourcesVpcConfig.SecurityGroupIds = []string{cpSG, ngSG}

	// The NLB's backing LB VM forwards the published endpoint to the apiserver
	// from inside the CP VPC, so the control-plane SG must admit that hop (CP VPC
	// CIDR) or the NLB target stays unhealthy and the endpoint never serves.
	if err := EnsureControlPlaneIngress(ctx, s.deps.VPCSG, sysAcct, cpSG, cpRefs.VpcCIDR); err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "ensure control-plane ingress", err)
		return
	}
	// HA control planes run servers 2..N as join servers whose embedded etcd must
	// peer with the quorum; without these self-referencing CP-SG rules a join
	// registers but its etcd never replicates, so the node never reports Ready.
	if err := EnsureControlPlaneHAIngress(ctx, s.deps.VPCSG, sysAcct, cpSG); err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "ensure control-plane HA ingress", err)
		return
	}

	// Private endpoint (301): when private access is on, give the cluster NLB a
	// customer-VPC (Set A) front-end so in-VPC workers + kubectl reach the control
	// plane without the public hairpin / NAT GW egress. Provision the customer-
	// account ENI (admitted by a customer-VPC SG on :443) before the NLB + CP VMs so
	// its IP is a known cert SAN, then thread it onto the LB VM as a cross-account
	// extra NIC. Persist its refs immediately so teardown reclaims them on any later
	// failure. nginx(L4) on the LB VM binds all addresses, so it answers the Set A
	// NIC and proxies to the CP target group with no data-plane change.
	var crossAccountENIs []sysinstance.ExtraENIInput
	if privateAccess {
		pe, perr := EnsurePrivateEndpointENI(ctx, s.deps.VPCK3s, s.deps.VPCSG, s.deps.VPCSubnet, accountID, name, lc.subnetIDs[0], lc.vpcID)
		if perr != nil {
			s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "ensure private endpoint ENI", perr)
			return
		}
		meta.PrivateEndpointENIID = pe.ENIID
		meta.PrivateEndpointIP = pe.ENIIP
		if err := PutClusterMeta(acctKV, meta); err != nil {
			s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "persist private endpoint refs", err)
			return
		}
		crossAccountENIs = []sysinstance.ExtraENIInput{{
			ENIID:     pe.ENIID,
			ENIMac:    pe.ENIMac,
			ENIIP:     pe.ENIIP,
			SubnetID:  lc.subnetIDs[0],
			AccountID: accountID,
		}}
	}

	// The cluster NLB lives in the CP VPC public subnet. publicAccess selects the
	// scheme (internet-facing external-pool IP vs internal VPC IP); the CP VPC IGW
	// (attached by EnsureClusterCPVPC) makes an internet-facing front-end IP
	// answerable on the wire.
	nlb, err := EnsureClusterNLB(ctx, s.deps.NLB, sysAcct, name, []string{cpRefs.PublicSubnetID}, publicAccess, publicCidrs, crossAccountENIs)
	if err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "ensure cluster NLB", err)
		return
	}
	// Endpoint resolution (301): publish the reachable front-end. With public access
	// on (public-only or public+private) that is the NLB's public front-end IP. A
	// private-only cluster publishes its Set A private endpoint instead — the
	// internal NLB's Set B IP is unreachable from the customer VPC. Either way the
	// host is a cert SAN, so TLS validates with verification on.
	if publicAccess {
		meta.Endpoint = "https://" + net.JoinHostPort(nlb.FrontendIP, strconv.FormatInt(clusterNLBListenPort, 10))
		meta.EndpointIP = nlb.FrontendIP
	} else {
		meta.Endpoint = "https://" + net.JoinHostPort(meta.PrivateEndpointIP, strconv.FormatInt(clusterNLBListenPort, 10))
		meta.EndpointIP = meta.PrivateEndpointIP
	}
	meta.NLBArn = nlb.LoadBalancerArn
	meta.NLBTargetGroupArn = nlb.TargetGroupArn
	meta.KonnTargetGroupArn = nlb.KonnTargetGroupArn

	// Persist NLB ARNs early so DeleteCluster can reclaim them on any later failure.
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "persist NLB arns", err)
		return
	}

	oidcIssuer, err := ClusterOIDCIssuer(s.deps.GatewayBaseURL, region, accountID, name)
	if err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "build OIDC issuer", err)
		return
	}
	meta.OIDCIssuer = oidcIssuer

	privPEM, _, err := GenerateClusterOIDCKeypair(acctKV, name, s.deps.MasterKey)
	if err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "generate OIDC keypair", err)
		return
	}
	pubPEM, err := PublicKeyPEMFromPrivate(privPEM)
	if err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "derive OIDC public key", err)
		return
	}

	joinToken, err := GenerateK3sClusterToken()
	if err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "generate k3s cluster token", err)
		return
	}

	// Dedup the customer's ELB-eligible subnets to one per AZ for the alb
	// IngressClassParams. LBC's tag auto-discovery never honors ALBSingleSubnet, so
	// a single-AZ cluster collapses to 1<2 subnets and fails; the explicit-subnet
	// path (driven by these IDs) is the only one that threads the gate. Best-effort:
	// an unresolved AZ falls back to auto-discovery rather than risk a dup-AZ error.
	elbSubnets := dedupSubnetsByAZ(ctx, s.deps.VPCSubnet, accountID, lc.subnetIDs)

	serverIn := K3sServerInput{
		AccountID:           sysAcct,
		ClusterAccountID:    accountID,
		ClusterName:         name,
		Region:              region,
		SubnetID:            cpRefs.PrivateSubnetIDs[0],
		VpcID:               meta.ResourcesVpcConfig.VpcId,
		ELBSubnetIDs:        elbSubnets,
		ControlPlaneSGID:    cpSG,
		NLBDNS:              nlb.DNSName,
		EndpointIP:          nlb.FrontendIP,
		PrivateEndpointIP:   meta.PrivateEndpointIP,
		OIDCIssuer:          oidcIssuer,
		OIDCPrivateKeyPEM:   privPEM,
		OIDCPublicKeyPEM:    pubPEM,
		GatewayURL:          s.deps.SystemGatewayURL,
		AddonGatewayURL:     s.deps.GatewayBaseURL,
		GatewayCACert:       s.deps.GatewayCACert,
		JoinToken:           joinToken,
		PredastoreEndpoint:  s.deps.SystemPredastoreURL,
		PredastoreAccessKey: s.deps.SystemAccessKey,
		PredastoreSecretKey: s.deps.SystemSecretKey,
	}

	// Prefer IMDS instance-role creds: attach a system instance profile so the
	// CP VM authenticates with scoped, rotating credentials and no static secret
	// rides in user-data. Falls back to baked system keys when IAM is unwired.
	if profileARN := s.ensureCPInstanceProfile(sysAcct); profileARN != "" {
		serverIn.IamInstanceProfileArn = profileARN
	} else {
		serverIn.AccessKey = s.deps.SystemAccessKey
		serverIn.SecretKey = s.deps.SystemSecretKey
	}

	cpNodes, spreadGroup, err := s.placeControlPlane(ctx, sysAcct, name, serverIn)
	if err != nil {
		if errors.Is(err, ErrEKSServerAMINotFound) {
			s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "eks-server AMI not found", err)
			return
		}
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "launch K3s VM", err)
		return
	}
	// Mirror the primary ([0]) into the scalar fields the reconciler and teardown
	// read today. All CP nodes are NLB target-registered (failover).
	primary := cpNodes[0]
	meta.ControlPlaneNodes = cpNodes
	meta.ControlPlaneSpreadGroup = spreadGroup
	meta.ControlPlaneInstanceID = primary.InstanceID
	meta.ControlPlaneENIID = primary.ENIID
	meta.ControlPlaneENIIP = primary.ENIIP
	meta.ControlPlaneMgmtIP = primary.MgmtIP

	// Persist the launch template so the reconciler can replay it to provision a
	// replacement CP member that joins the surviving quorum (member-count
	// reconcile). Per-node fields and rotating creds are cleared — the reconciler
	// sets the join target/host and re-derives creds at provision time.
	tmpl := serverIn
	tmpl.TargetNodeID = ""
	tmpl.ServerURL = ""
	tmpl.KonnServerCount = 0
	tmpl.AccessKey = ""
	tmpl.SecretKey = ""
	tmpl.IamInstanceProfileArn = ""
	tmpl.PredastoreAccessKey = ""
	tmpl.PredastoreSecretKey = ""
	meta.ControlPlaneTemplate = &tmpl

	// Persist the CP VM + ENI + spread-group refs now, before any further fallible
	// step. The VMs are live the moment placeControlPlane returns; without this a
	// failure between here and the next PutClusterMeta leaves them launched but
	// unrecorded, so neither DeleteCluster nor the FAILED-cluster reclaim could
	// reach them and the sys.medium VMs leak.
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "persist control-plane ids", err)
		return
	}

	// The control-plane VM pulls container images over the CP VPC's NAT gateway:
	// it lives in the private subnet whose route table sends 0.0.0.0/0 → NAT GW,
	// so no per-instance egress wiring is needed (the prior hidden-pool /32 SNAT
	// bandaid is gone — the topology now expresses egress).
	cpENIIPs := make([]string, 0, len(cpNodes))
	for _, n := range cpNodes {
		cpENIIPs = append(cpENIIPs, n.ENIIP)
	}
	if err := RegisterClusterTargets(ctx, s.deps.NLB, sysAcct, nlb.TargetGroupArn, cpENIIPs, k3sAPIServerPort); err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "register NLB targets", err)
		return
	}
	// Register the same CP ENIs on the konnectivity TG (:8132): worker agents dial
	// the NLB private endpoint and the NLB fans them to every apiserver's konn-server.
	if err := RegisterClusterTargets(ctx, s.deps.NLB, sysAcct, nlb.KonnTargetGroupArn, cpENIIPs, konnectivityAgentPort); err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "register konnectivity NLB targets", err)
		return
	}

	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "persist final meta", err)
		return
	}

	// Seed creator system:masters AccessEntry so the token webhook can authenticate the creator immediately.
	if bootstrapCreatorAdmin(input) && callerPrincipalARN != "" {
		rec := newAccessEntryRecord(region, accountID, name, callerPrincipalARN, "",
			[]string{"system:masters"}, AccessEntryTypeStandard, nil, time.Now().UTC())
		if err := PutAccessEntryRecord(acctKV, rec); err != nil {
			s.failClusterLaunch(ctx, acctKV, name, accountID, meta, "seed cluster-creator admin access entry", err)
			return
		}
	} else if bootstrapCreatorAdmin(input) {
		slog.WarnContext(ctx, "CreateCluster: bootstrapClusterCreatorAdminPermissions set but caller principal ARN unknown; skipping creator-admin AccessEntry",
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

func (s *EKSServiceImpl) DescribeCluster(ctx context.Context, input *eks.DescribeClusterInput, accountID string) (*eks.DescribeClusterOutput, error) {
	name := aws.StringValue(input.Name)
	if name == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID, max(s.deps.ClusterSize, 1))
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

func (s *EKSServiceImpl) ListClusters(ctx context.Context, input *eks.ListClustersInput, accountID string) (*eks.ListClustersOutput, error) {
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, eksReadUnavailableOr(err, "jetstream")
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID, max(s.deps.ClusterSize, 1))
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

func (s *EKSServiceImpl) DeleteCluster(ctx context.Context, input *eks.DeleteClusterInput, accountID string) (*eks.DeleteClusterOutput, error) {
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
	acctKV, err := GetOrCreateAccountBucket(js, accountID, max(s.deps.ClusterSize, 1))
	if err != nil {
		return nil, fmt.Errorf("get account bucket: %w", err)
	}

	meta, err := GetClusterMeta(acctKV, name)
	if err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			// Idempotent: a cluster already swept from KV is "deleted". Returning
			// success lets a tofu destroy retry / double-targeted delete converge
			// instead of failing the run (Common Resource Lifecycle Contract #1).
			return &eks.DeleteClusterOutput{}, nil
		}
		return nil, err
	}

	// Single-flight the teardown: SDK retries of the same DeleteCluster are fanned
	// across nodes by the worker queue group, so without this gate multiple
	// purgeClusterInfra runs race the same cluster (duplicate ENI/NLB teardown,
	// risking a double-release on the billable NAT-GW EIP). The loser returns the
	// cluster as DELETING — AWS-async delete semantics — and the winner (or the
	// DELETING backstop reaper) drives the teardown to completion.
	release, ok := s.acquireTeardownLease(accountID, name)
	if !ok {
		meta.Status = ClusterStatusDeleting
		return &eks.DeleteClusterOutput{Cluster: clusterMetaToAWS(meta)}, nil
	}
	defer release()

	if err := SetClusterStatus(acctKV, name, ClusterStatusDeleting); err != nil {
		return nil, fmt.Errorf("set DELETING: %w", err)
	}
	meta.Status = ClusterStatusDeleting

	if err := s.purgeClusterInfra(ctx, accountID, name, meta, acctKV, true); err != nil {
		slog.ErrorContext(ctx, "DeleteCluster: teardown incomplete; leaving cluster DELETING for retry",
			"cluster", name, "err", err)
		return nil, fmt.Errorf("eks: DeleteCluster %s: %w", name, err)
	}

	return &eks.DeleteClusterOutput{Cluster: clusterMetaToAWS(meta)}, nil
}

// teardownLeaderKey is the per-cluster single-flight gate for a DELETING
// teardown. It is distinct from reconcilerLeaderKey so a delete never contends
// with the still-live reconciler, which holds the reconciler lease until it
// observes DELETING and exits; only concurrent purgeClusterInfra runs meet here.
func teardownLeaderKey(accountID, clusterName string) string {
	return reconcilerLeaderKey(accountID, clusterName) + "/teardown"
}

// acquireTeardownLease takes the per-cluster teardown lease so only one
// purgeClusterInfra runs at a time for a cluster — the gate against the
// concurrent-handler herd from SDK-retried DeleteClusters fanned across nodes by
// the worker queue group. Shared by the synchronous DeleteCluster path and the
// DELETING backstop reaper. CAS Create fails when another holder owns it. A nil
// leaderKV (single-node/test) skips gating; the bucket TTL reaps a lease whose
// release never runs.
func (s *EKSServiceImpl) acquireTeardownLease(accountID, clusterName string) (func(), bool) {
	if s.leaderKV == nil {
		return func() {}, true
	}
	key := teardownLeaderKey(accountID, clusterName)
	if _, err := s.leaderKV.Create(key, []byte(s.deps.HolderID)); err != nil {
		return nil, false
	}
	return func() {
		if err := s.leaderKV.Delete(key); err != nil {
			slog.Warn("eks: teardown lease release failed (TTL will reap)", "key", key, "err", err)
		}
	}, true
}

// claimClusterName atomically claims the cluster meta key before any launch work.
// A prior FAILED attempt is reclaimed via CAS (FAILED→CREATING); live clusters reject with ResourceInUse.
func (s *EKSServiceImpl) claimClusterName(ctx context.Context, accountID string, acctKV nats.KeyValue, meta *ClusterMeta) error {
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
		slog.InfoContext(ctx, "CreateCluster: cluster already exists",
			"name", name, "accountID", accountID)
		return errors.New(awserrors.ErrorEKSResourceInUse)
	}

	slog.InfoContext(ctx, "CreateCluster: reclaiming FAILED cluster before recreate",
		"name", name, "accountID", accountID)
	// Purge the failed attempt's infra but KEEP the meta claim (deleteMeta=false)
	// — the CREATING record is our lock for the rest of this create.
	if perr := s.purgeClusterInfra(ctx, accountID, name, oldRefs, acctKV, false); perr != nil {
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
// material), NLB, VM, SG, IGW, and CP-VPC teardown are all blocking: their
// failures are joined and returned BEFORE the KV sweep, so neither a billable
// resource nor a VPC-pinning SG is orphaned without the meta record that owns
// its IDs. The cluster SGs sit in the customer VPC, so a leaked SG pins the VPC
// on DependencyViolation — leaving the cluster DELETING for a retry is correct.
// Shared by DeleteCluster and the FAILED-cluster reclaim path in CreateCluster.
func (s *EKSServiceImpl) purgeClusterInfra(ctx context.Context, accountID, name string, meta *ClusterMeta, acctKV nats.KeyValue, deleteMeta bool) error {
	s.registry.Stop(accountID, name)

	// infraAcct owns the NLB, CP VMs, and CP VPC. Clusters built with the managed
	// CP VPC ("Set B") hold them under the system account; legacy clusters
	// (ManagedCPVPC nil) under the customer account, matching how they launched.
	infraAcct := accountID
	if meta.ManagedCPVPC != nil {
		infraAcct = admin.SystemAccountID()
	}

	var teardownErrs []error

	if err := ZeroizeClusterOIDCKey(acctKV, name); err != nil {
		teardownErrs = append(teardownErrs, fmt.Errorf("zeroize OIDC key: %w", err))
	}

	if meta.NLBArn != "" {
		// Deregister is best-effort: DeleteClusterNLB tears down the whole NLB
		// + target group, so a stale target registration cannot leak past it.
		if meta.NLBTargetGroupArn != "" && meta.ControlPlaneENIIP != "" {
			if err := DeregisterClusterTarget(ctx, s.deps.NLB, infraAcct, meta.NLBTargetGroupArn, meta.ControlPlaneENIIP, k3sAPIServerPort); err != nil {
				slog.WarnContext(ctx, "purgeClusterInfra: deregister NLB target failed", "cluster", name, "err", err)
			}
		}
		if err := DeleteClusterNLB(ctx, s.deps.NLB, infraAcct, name); err != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("delete NLB: %w", err))
		}
	}

	// Private endpoint (301): the Set A ENI lives in the customer VPC under the
	// customer account and was attached to the cluster NLB's LB VM (terminated by
	// DeleteClusterNLB above). It is an extra NIC, not the LB VM's primary ENI, so
	// the instance-terminate cascade never reclaims it — detach (store-clear) the
	// stale attachment first, then delete before its SG. Detach is best-effort: a
	// missing/already-detached ENI is fine; the delete is the authoritative gate.
	if meta.PrivateEndpointENIID != "" {
		if err := detachAndDeleteServerENI(ctx, s.deps.VPCK3s, accountID, meta.PrivateEndpointENIID); err != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("delete private-endpoint ENI: %w", err))
		}
	}

	for _, cp := range controlPlaneTeardownNodes(meta) {
		if err := TerminateK3sServerVM(ctx, s.deps.VPCK3s, s.deps.Instance,
			infraAcct, cp.InstanceID, cp.ENIID); err != nil {
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
			if _, err := s.deps.EIP.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{
				AllocationId: aws.String(meta.EgressEIPAllocationID),
			}, accountID); err != nil {
				switch {
				case awserrors.IsErrorCode(err, awserrors.ErrorInvalidAllocationIDNotFound),
					awserrors.IsErrorCode(err, awserrors.ErrorInvalidAddressIDNotFound),
					awserrors.IsErrorCode(err, awserrors.ErrorInvalidAddressNotFound):
					// A prior retry (or the egress-delete cascade) already released
					// the allocation. Idempotent success — must NOT block the SG +
					// KV sweep, or the cluster wedges in DELETING permanently.
					slog.DebugContext(ctx, "purgeClusterInfra: egress EIP already released",
						"cluster", name, "allocationId", meta.EgressEIPAllocationID)
				default:
					teardownErrs = append(teardownErrs, fmt.Errorf("release egress EIP: %w", err))
				}
			}
		}
	}

	// Private endpoint (301): reclaim the customer-VPC SG now its ENI is gone.
	// Best-effort — its only billable dependant (the ENI) is already deleted.
	if meta.ResourcesVpcConfig != nil && meta.ResourcesVpcConfig.VpcId != "" {
		if err := DeletePrivateEndpointSG(ctx, s.deps.VPCSG, accountID, name, meta.ResourcesVpcConfig.VpcId); err != nil {
			slog.WarnContext(ctx, "purgeClusterInfra: delete private-endpoint SG failed", "cluster", name, "err", err)
		}
	}

	// Reap LBC-orphaned ALBs (k8s-toc-*) in the customer VPC before the SG sweep:
	// the in-cluster AWS LoadBalancer Controller runs under the customer account and
	// spinifex never tracks its ALBs, so nothing else deletes them and they pin the
	// VPC (and their k8s-traffic-toc-* SGs) undeletable. Must precede DeleteClusterSGs
	// so the ALB no longer references the SG it reaps.
	if meta.ResourcesVpcConfig != nil && meta.ResourcesVpcConfig.VpcId != "" {
		if err := ReapLBCLoadBalancers(ctx, s.deps.NLB, accountID, name, meta.ResourcesVpcConfig.VpcId); err != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("reap LBC ALBs: %w", err))
		}
	}

	// SG / IGW / CP-VPC cleanup runs even when an earlier step failed: the cluster
	// SGs sit in the customer VPC, so skipping them would pin the VPC. Failures are
	// joined into teardownErrs (not swallowed) so the meta survives for a retry.
	if meta.ManagedCPVPC != nil && meta.ManagedCPVPC.VpcId != "" {
		// New topology: SGs + IGW + subnets + route tables + NAT GW + VPC all live
		// in the managed CP VPC under the system account. DeleteClusterCPVPC removes
		// the IGW + VPC after the SGs, so SG cleanup must precede it.
		cpSGErr := DeleteClusterSGs(ctx, s.deps.VPCSG, infraAcct, name, meta.ManagedCPVPC.VpcId)
		if cpSGErr != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("delete cluster SGs: %w", cpSGErr))
		}
		// launchNodegroupInfra also creates the cluster SGs in the customer VPC for
		// worker<->control-plane networking; reclaim them here too, or they orphan
		// (cross-referencing each other) and pin the customer VPC on destroy.
		if meta.ResourcesVpcConfig != nil && meta.ResourcesVpcConfig.VpcId != "" {
			if err := DeleteClusterSGs(ctx, s.deps.VPCSG, accountID, name, meta.ResourcesVpcConfig.VpcId); err != nil {
				teardownErrs = append(teardownErrs, fmt.Errorf("delete customer-VPC cluster SGs: %w", err))
			}
		}
		cpVPCErr := DeleteClusterCPVPC(ctx, s.cpVPCDeps(), infraAcct, name, meta.ManagedCPVPC)
		if cpVPCErr != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("delete managed CP VPC: %w", cpVPCErr))
		}
		if cpSGErr == nil && cpVPCErr == nil {
			// The CP VPC (including when already gone — tolerated as success)
			// and its own SGs are both confirmed torn down. Clear the internal
			// cp-vpc state record right now, independent of whatever else in
			// this run still fails below: otherwise a re-drive keeps retrying
			// SG/VPC API calls against a stale VpcId forever, turning a single
			// DependencyViolation into a permanent DELETING loop.
			meta.ManagedCPVPC = nil
			if perr := ClearClusterManagedCPVPC(acctKV, name); perr != nil && !errors.Is(perr, ErrClusterNotFound) {
				teardownErrs = append(teardownErrs, fmt.Errorf("clear managed CP VPC state: %w", perr))
			}
		}
	} else if meta.ResourcesVpcConfig != nil && meta.ResourcesVpcConfig.VpcId != "" {
		// Legacy topology: CP lived in the customer VPC; reclaim its SGs + the
		// cluster-owned IGW (ownership-scoped — a reused customer IGW is left
		// intact). IGW delete must precede any VPC delete to avoid DependencyViolation.
		if err := DeleteClusterSGs(ctx, s.deps.VPCSG, accountID, name, meta.ResourcesVpcConfig.VpcId); err != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("delete cluster SGs: %w", err))
		}
		if s.deps.IGW != nil {
			if err := DeleteClusterIGW(ctx, s.deps.IGW, accountID, meta.ResourcesVpcConfig.VpcId, name); err != nil {
				teardownErrs = append(teardownErrs, fmt.Errorf("delete cluster IGW: %w", err))
			}
		}
	}

	// Release + delete the HA spread placement group (no-op for single-CP
	// clusters). Best-effort: the VMs are already gone, so a leaked internal
	// group strands nothing.
	s.teardownSpreadGroup(ctx, meta)

	// Any blocking-teardown failure (billable infra or a VPC-pinning SG) keeps the
	// meta — and the IDs that own the stranded resources — alive for a delete retry
	// or the DELETING backstop, rather than completing deletion with infra leaked.
	if len(teardownErrs) > 0 {
		return errors.Join(teardownErrs...)
	}

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

func (s *EKSServiceImpl) UpdateClusterConfig(ctx context.Context, _ *eks.UpdateClusterConfigInput, _ string) (*eks.UpdateClusterConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateClusterVersion(ctx context.Context, _ *eks.UpdateClusterVersionInput, _ string) (*eks.UpdateClusterVersionOutput, error) {
	return nil, notImpl()
}

// --- Nodegroup ---

func (s *EKSServiceImpl) CreateNodegroup(ctx context.Context, input *eks.CreateNodegroupInput, accountID string) (*eks.CreateNodegroupOutput, error) {
	if err := s.requireOrchestrationDeps("CreateNodegroup"); err != nil {
		return nil, err
	}
	acctKV, err := s.nodegroupAcctKV(accountID)
	if err != nil {
		return nil, err
	}
	return s.createNodegroup(ctx, acctKV, input, accountID)
}

func (s *EKSServiceImpl) DescribeNodegroup(ctx context.Context, input *eks.DescribeNodegroupInput, accountID string) (*eks.DescribeNodegroupOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.nodegroupAcctKV(accountID)
	if err != nil {
		return nil, err
	}
	return s.describeNodegroup(acctKV, input)
}

func (s *EKSServiceImpl) ListNodegroups(ctx context.Context, input *eks.ListNodegroupsInput, accountID string) (*eks.ListNodegroupsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.nodegroupAcctKV(accountID)
	if err != nil {
		return nil, err
	}
	return s.listNodegroups(acctKV, input)
}

func (s *EKSServiceImpl) UpdateNodegroupConfig(ctx context.Context, input *eks.UpdateNodegroupConfigInput, accountID string) (*eks.UpdateNodegroupConfigOutput, error) {
	if err := s.requireOrchestrationDeps("UpdateNodegroupConfig"); err != nil {
		return nil, err
	}
	acctKV, err := s.nodegroupAcctKV(accountID)
	if err != nil {
		return nil, err
	}
	return s.updateNodegroupConfig(ctx, acctKV, input, accountID)
}

func (s *EKSServiceImpl) UpdateNodegroupVersion(ctx context.Context, input *eks.UpdateNodegroupVersionInput, accountID string) (*eks.UpdateNodegroupVersionOutput, error) {
	return s.updateNodegroupVersion(input)
}

func (s *EKSServiceImpl) DeleteNodegroup(ctx context.Context, input *eks.DeleteNodegroupInput, accountID string) (*eks.DeleteNodegroupOutput, error) {
	if err := s.requireOrchestrationDeps("DeleteNodegroup"); err != nil {
		return nil, err
	}
	acctKV, err := s.nodegroupAcctKV(accountID)
	if err != nil {
		return nil, err
	}
	return s.deleteNodegroup(ctx, acctKV, input, accountID)
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
	acctKV, err := GetOrCreateAccountBucket(js, accountID, max(s.deps.ClusterSize, 1))
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

func (s *EKSServiceImpl) CreateAccessEntry(ctx context.Context, input *eks.CreateAccessEntryInput, accountID string) (*eks.CreateAccessEntryOutput, error) {
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

func (s *EKSServiceImpl) DescribeAccessEntry(ctx context.Context, input *eks.DescribeAccessEntryInput, accountID string) (*eks.DescribeAccessEntryOutput, error) {
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

func (s *EKSServiceImpl) ListAccessEntries(ctx context.Context, input *eks.ListAccessEntriesInput, accountID string) (*eks.ListAccessEntriesOutput, error) {
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

func (s *EKSServiceImpl) UpdateAccessEntry(ctx context.Context, input *eks.UpdateAccessEntryInput, accountID string) (*eks.UpdateAccessEntryOutput, error) {
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

func (s *EKSServiceImpl) DeleteAccessEntry(ctx context.Context, input *eks.DeleteAccessEntryInput, accountID string) (*eks.DeleteAccessEntryOutput, error) {
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

func (s *EKSServiceImpl) AssociateAccessPolicy(ctx context.Context, input *eks.AssociateAccessPolicyInput, accountID string) (*eks.AssociateAccessPolicyOutput, error) {
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

func (s *EKSServiceImpl) DisassociateAccessPolicy(ctx context.Context, input *eks.DisassociateAccessPolicyInput, accountID string) (*eks.DisassociateAccessPolicyOutput, error) {
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

func (s *EKSServiceImpl) ListAssociatedAccessPolicies(ctx context.Context, input *eks.ListAssociatedAccessPoliciesInput, accountID string) (*eks.ListAssociatedAccessPoliciesOutput, error) {
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

func (s *EKSServiceImpl) ListAccessPolicies(ctx context.Context, _ *eks.ListAccessPoliciesInput, _ string) (*eks.ListAccessPoliciesOutput, error) {
	arns := slices.Sorted(maps.Keys(supportedAccessPolicies))
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

func (s *EKSServiceImpl) AssociateIdentityProviderConfig(ctx context.Context, _ *eks.AssociateIdentityProviderConfigInput, _ string) (*eks.AssociateIdentityProviderConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeIdentityProviderConfig(ctx context.Context, _ *eks.DescribeIdentityProviderConfigInput, _ string) (*eks.DescribeIdentityProviderConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListIdentityProviderConfigs(ctx context.Context, _ *eks.ListIdentityProviderConfigsInput, _ string) (*eks.ListIdentityProviderConfigsOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DisassociateIdentityProviderConfig(ctx context.Context, _ *eks.DisassociateIdentityProviderConfigInput, _ string) (*eks.DisassociateIdentityProviderConfigOutput, error) {
	return nil, notImpl()
}

// --- Tags ---
//
// Store-only against the cluster meta record or nodegroup record (no
// enforcement). Together with DescribeCluster/DescribeNodegroup echoing
// create-time tags this gives a stock terraform-aws provider a clean
// default_tags round-trip instead of perpetual drift. Cluster and nodegroup
// ARNs are backed; other EKS resource ARNs return NotImplemented.

func (s *EKSServiceImpl) TagResource(ctx context.Context, input *eks.TagResourceInput, accountID string) (*eks.TagResourceOutput, error) {
	arn := aws.StringValue(input.ResourceArn)
	add := aws.StringValueMap(input.Tags)

	if name, ok := clusterNameFromARN(arn); ok {
		acctKV, err := s.accountBucket(accountID)
		if err != nil {
			return nil, err
		}
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

	if cluster, ng, ok := nodegroupRefFromARN(arn); ok {
		if err := s.casUpdateNodegroupTags(accountID, cluster, ng, func(tags map[string]string) (map[string]string, bool) {
			if len(add) == 0 {
				return tags, false
			}
			if tags == nil {
				tags = make(map[string]string, len(add))
			}
			maps.Copy(tags, add)
			return tags, true
		}); err != nil {
			return nil, err
		}
		return &eks.TagResourceOutput{}, nil
	}

	return nil, notImpl()
}

func (s *EKSServiceImpl) UntagResource(ctx context.Context, input *eks.UntagResourceInput, accountID string) (*eks.UntagResourceOutput, error) {
	arn := aws.StringValue(input.ResourceArn)
	keys := aws.StringValueSlice(input.TagKeys)

	if name, ok := clusterNameFromARN(arn); ok {
		acctKV, err := s.accountBucket(accountID)
		if err != nil {
			return nil, err
		}
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

	if cluster, ng, ok := nodegroupRefFromARN(arn); ok {
		if err := s.casUpdateNodegroupTags(accountID, cluster, ng, func(tags map[string]string) (map[string]string, bool) {
			changed := false
			for _, k := range keys {
				if _, ok := tags[k]; ok {
					delete(tags, k)
					changed = true
				}
			}
			return tags, changed
		}); err != nil {
			return nil, err
		}
		return &eks.UntagResourceOutput{}, nil
	}

	return nil, notImpl()
}

func (s *EKSServiceImpl) ListTagsForResource(ctx context.Context, input *eks.ListTagsForResourceInput, accountID string) (*eks.ListTagsForResourceOutput, error) {
	arn := aws.StringValue(input.ResourceArn)

	if name, ok := clusterNameFromARN(arn); ok {
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

	if cluster, ng, ok := nodegroupRefFromARN(arn); ok {
		acctKV, err := s.accountBucket(accountID)
		if err != nil {
			return nil, err
		}
		rec, err := GetNodegroupRecord(acctKV, cluster, ng)
		if err != nil {
			if errors.Is(err, ErrNodegroupNotFound) {
				return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
			}
			return nil, err
		}
		return &eks.ListTagsForResourceOutput{Tags: aws.StringMap(rec.Tags)}, nil
	}

	return nil, notImpl()
}

// casUpdateNodegroupTags applies mutate to the nodegroup record's tag map under
// compare-and-swap. mutate returns the new tag map and whether it changed; a
// no-op skips the write. A missing nodegroup surfaces as ResourceNotFound.
func (s *EKSServiceImpl) casUpdateNodegroupTags(accountID, cluster, ng string, mutate func(map[string]string) (map[string]string, bool)) error {
	acctKV, err := s.accountBucket(accountID)
	if err != nil {
		return err
	}
	for range ngCASMaxRetries {
		rec, rev, err := getNodegroupEntry(acctKV, cluster, ng)
		if err != nil {
			if errors.Is(err, ErrNodegroupNotFound) {
				return errors.New(awserrors.ErrorEKSResourceNotFound)
			}
			return err
		}
		tags, changed := mutate(rec.Tags)
		if !changed {
			return nil
		}
		rec.Tags = tags
		rec.ModifiedAt = time.Now().UTC()
		if ok, err := s.casPutNodegroup(acctKV, rec, rev); err != nil {
			return err
		} else if ok {
			return nil
		}
	}
	return errors.New(awserrors.ErrorServerInternal)
}

// accountBucket returns the per-account KV bucket for accountID.
func (s *EKSServiceImpl) accountBucket(accountID string) (nats.KeyValue, error) {
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return GetOrCreateAccountBucket(js, accountID, max(s.deps.ClusterSize, 1))
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

// nodegroupRefFromARN extracts (cluster, nodegroup) from an EKS nodegroup ARN
// (arn:aws:eks:<region>:<acct>:nodegroup/<cluster>/<ng>/<uuid>), reporting false
// for any other ARN shape.
func nodegroupRefFromARN(arn string) (string, string, bool) {
	const prefix = "arn:aws:eks:"
	if !strings.HasPrefix(arn, prefix) {
		return "", "", false
	}
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 {
		return "", "", false
	}
	resType, rest, found := strings.Cut(parts[5], "/")
	if !found || resType != "nodegroup" {
		return "", "", false
	}
	seg := strings.SplitN(rest, "/", 3)
	if len(seg) < 2 || seg[0] == "" || seg[1] == "" {
		return "", "", false
	}
	return seg[0], seg[1], true
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

// clusterJoinEndpoint is the apiserver URL nodegroup workers join through. With
// private access on, workers prefer the customer-VPC (Set A) private endpoint on
// :443 so a private-only cluster needs no NAT GW egress — the core 301 win.
// Otherwise (public-only, or no provisioned private endpoint) the published
// endpoint is used. For a private-only cluster meta.Endpoint already equals the
// private endpoint, so the two agree.
func clusterJoinEndpoint(meta *ClusterMeta) string {
	if meta.ResourcesVpcConfig != nil && meta.ResourcesVpcConfig.EndpointPrivateAccess && meta.PrivateEndpointIP != "" {
		return "https://" + net.JoinHostPort(meta.PrivateEndpointIP, strconv.FormatInt(clusterNLBListenPort, 10))
	}
	return meta.Endpoint
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
	acctKV, err := GetOrCreateAccountBucket(js, accountID, max(s.deps.ClusterSize, 1))
	if err != nil {
		slog.Error("spawnReconciler: account bucket", "err", err)
		return
	}
	// Health gates on the control plane's NATS self-report, not an HTTP probe:
	// k3s binds the apiserver to the VPC node-ip, unreachable from the host. The
	// CP publishes {healthz,node_count} on the mgmt bus the daemon already shares.
	stateSubject := StateSubject(accountID, clusterName)
	addonStatusSubject := AddonStatusSubject(accountID, clusterName)
	opts := []ReconcilerOption{
		WithStateSource(s.deps.NATSConn, stateSubject),
		WithAddonStatusSource(s.deps.NATSConn, addonStatusSubject),
	}
	if s.deps.CPControl != nil {
		// The control-plane VMs are launched under the system account (see
		// placeControlPlane), not the customer account that owns the cluster
		// record. CP describe/recover must therefore run as the system account —
		// the customer account cannot see or own its own cluster's CP VMs.
		opts = append(opts, WithCPInstanceControl(cpControlAdapter{ctl: s.deps.CPControl, accountID: admin.SystemAccountID()}))
		// Member-count reconcile: replace a terminated/gone CP member with a fresh
		// one that joins the surviving quorum. The service replays the persisted
		// create template; gated on CPControl since replacement needs member describe.
		opts = append(opts, WithCPProvisioner(s))
		// Last-resort etcd quorum-reformation: when every member is VM-running but
		// etcd never reformed after a simultaneous restart, drive a k3s cluster-reset
		// via per-member recovery directives the on-VM agent applies on boot.
		opts = append(opts, WithEtcdResetRecovery(0, 0, 0))
	}
	spawn := func(ctx context.Context, _, _ string) (func(), <-chan struct{}, error) {
		return RunClusterReconciler(ctx, s.leaderKV, acctKV, accountID, clusterName, s.deps.HolderID, "", opts...)
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
