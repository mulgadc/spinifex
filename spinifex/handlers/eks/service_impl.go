package handlers_eks

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/nats-io/nats.go"
)

// SubnetVPCResolver resolves a subnet ID to its parent VPC ID. Narrow so
// EKSServiceImpl can stay free of the wider handlers/ec2/vpc surface; the
// daemon adapts VPCServiceImpl.GetSubnet onto this contract.
type SubnetVPCResolver interface {
	GetSubnetVPC(accountID, subnetID string) (vpcID string, err error)
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

	// NATS auth handed to the K3s server VM so it can publish its one-shot
	// bootstrap messages. Shared token + CA (PEM content) + a VM-reachable
	// URL — spinifex has no per-principal nkeys hierarchy to scope against.
	NATSURL    string
	NATSToken  string
	NATSCACert string

	VPCSG     sgProvisioner
	VPCK3s    k3sVPCProvisioner
	VPCSubnet SubnetVPCResolver
	NLB       nlbProvisioner
	Instance  k3sInstanceLauncher
	Image     k3sAMIResolver
}

// EKSServiceImpl is the daemon-side EKSService. CreateCluster /
// DescribeCluster / ListClusters / DeleteCluster have real bodies; every
// other action is still NotImplemented pending follow-up sprints.
type EKSServiceImpl struct {
	deps     EKSServiceDeps
	store    *Store
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
	store, err := NewStore(deps.NATSConn)
	if err != nil {
		return nil, fmt.Errorf("failed to create EKS store: %w", err)
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
		store:    store,
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
	return s.deps.VPCSG != nil && s.deps.VPCK3s != nil && s.deps.VPCSubnet != nil &&
		s.deps.NLB != nil && s.deps.Instance != nil && s.deps.Image != nil &&
		len(s.deps.MasterKey) > 0 && s.deps.GatewayBaseURL != "" && s.deps.Region != "" && s.deps.HolderID != ""
}

// --- Cluster ---

func (s *EKSServiceImpl) CreateCluster(input *eks.CreateClusterInput, accountID string) (*eks.CreateClusterOutput, error) {
	if !s.depsReadyForOrchestration() {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := validateCreateClusterInput(input); err != nil {
		return nil, err
	}
	name := aws.StringValue(input.Name)
	subnetIDs := aws.StringValueSlice(input.ResourcesVpcConfig.SubnetIds)

	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID)
	if err != nil {
		return nil, fmt.Errorf("get account bucket: %w", err)
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
			return nil, fmt.Errorf("reclaim failed cluster %s: %w", name, perr)
		}
	} else if !errors.Is(err, ErrClusterNotFound) {
		return nil, fmt.Errorf("preflight get meta: %w", err)
	}

	vpcID, err := s.deps.VPCSubnet.GetSubnetVPC(accountID, subnetIDs[0])
	if err != nil {
		return nil, fmt.Errorf("resolve subnet VPC: %w", err)
	}

	region := s.deps.Region
	arn := fmt.Sprintf("arn:aws:eks:%s:%s:cluster/%s", region, accountID, name)

	meta := &ClusterMeta{
		Name:    name,
		Arn:     arn,
		Status:  ClusterStatusCreating,
		Version: deref(input.Version, defaultK8sVersion),
		RoleArn: aws.StringValue(input.RoleArn),
		ResourcesVpcConfig: &ClusterVpcConfig{
			SubnetIds: subnetIDs,
			VpcId:     vpcID,
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := PutClusterMeta(acctKV, meta); err != nil {
		return nil, err
	}

	cpSG, ngSG, err := EnsureClusterSGs(s.deps.VPCSG, accountID, name, vpcID)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, fmt.Errorf("ensure cluster SGs: %w", err)
	}
	meta.ResourcesVpcConfig.SecurityGroupIds = []string{cpSG, ngSG}

	nlb, err := EnsureClusterNLB(s.deps.NLB, accountID, name, subnetIDs)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, fmt.Errorf("ensure cluster NLB: %w", err)
	}
	meta.Endpoint = "https://" + net.JoinHostPort(nlb.DNSName, strconv.FormatInt(clusterNLBListenPort, 10))
	meta.NLBArn = nlb.LoadBalancerArn
	meta.NLBTargetGroupArn = nlb.TargetGroupArn

	// Persist the NLB ARNs now, before any further fallible step. Without this
	// a failure between here and the final PutClusterMeta would leave the NLB +
	// target group + listener + backing LB VM created but unrecorded, so
	// DeleteCluster (which keys teardown off the persisted ARNs) could not
	// reclaim them.
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.markFailed(acctKV, name)
		return nil, fmt.Errorf("persist NLB arns: %w", err)
	}

	oidcIssuer, err := ClusterOIDCIssuer(s.deps.GatewayBaseURL, region, accountID, name)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, fmt.Errorf("build OIDC issuer: %w", err)
	}
	meta.OIDCIssuer = oidcIssuer

	if _, err := GenerateClusterOIDCKeypair(acctKV, name, s.deps.MasterKey); err != nil {
		s.markFailed(acctKV, name)
		return nil, fmt.Errorf("generate OIDC keypair: %w", err)
	}

	privPEM, err := s.loadOIDCPrivateKeyPEM(acctKV, name)
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, err
	}

	k3sOut, err := LaunchK3sServerVM(s.deps.VPCK3s, s.deps.Instance, s.deps.Image, K3sServerInput{
		AccountID:         accountID,
		ClusterName:       name,
		Region:            region,
		SubnetID:          subnetIDs[0],
		ControlPlaneSGID:  cpSG,
		NLBDNS:            nlb.DNSName,
		OIDCIssuer:        oidcIssuer,
		OIDCPrivateKeyPEM: privPEM,
		NATSURL:           s.deps.NATSURL,
		NATSToken:         s.deps.NATSToken,
		NATSCACert:        s.deps.NATSCACert,
	})
	if err != nil {
		s.markFailed(acctKV, name)
		return nil, fmt.Errorf("launch K3s VM: %w", err)
	}
	meta.ControlPlaneInstanceID = k3sOut.InstanceID
	meta.ControlPlaneENIID = k3sOut.ENIID
	meta.ControlPlaneENIIP = k3sOut.ENIIP

	// Record the VM + ENI before registering the target so a failed register
	// (or anything after) leaves them recoverable by DeleteCluster.
	if err := PutClusterMeta(acctKV, meta); err != nil {
		s.markFailed(acctKV, name)
		return nil, fmt.Errorf("persist control-plane ids: %w", err)
	}

	if err := RegisterClusterTarget(s.deps.NLB, accountID, nlb.TargetGroupArn, k3sOut.ENIIP); err != nil {
		s.markFailed(acctKV, name)
		return nil, fmt.Errorf("register NLB target: %w", err)
	}

	if err := PutClusterMeta(acctKV, meta); err != nil {
		return nil, err
	}

	s.spawnBootstrap(accountID, name, acctKV, nil)
	s.spawnReconciler(accountID, name, meta)

	return &eks.CreateClusterOutput{Cluster: clusterMetaToAWS(meta)}, nil
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
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID)
	if err != nil {
		return nil, fmt.Errorf("get account bucket: %w", err)
	}
	names, err := listClusterNames(acctKV)
	if err != nil {
		return nil, err
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
	if !s.depsReadyForOrchestration() {
		return nil, errors.New(awserrors.ErrorServerInternal)
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

	if meta.ControlPlaneInstanceID != "" || meta.ControlPlaneENIID != "" {
		if err := TerminateK3sServerVM(s.deps.VPCK3s, s.deps.Instance,
			accountID, meta.ControlPlaneInstanceID, meta.ControlPlaneENIID); err != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("terminate K3s VM: %w", err))
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

func (s *EKSServiceImpl) CreateNodegroup(_ *eks.CreateNodegroupInput, _ string) (*eks.CreateNodegroupOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeNodegroup(_ *eks.DescribeNodegroupInput, _ string) (*eks.DescribeNodegroupOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListNodegroups(_ *eks.ListNodegroupsInput, _ string) (*eks.ListNodegroupsOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateNodegroupConfig(_ *eks.UpdateNodegroupConfigInput, _ string) (*eks.UpdateNodegroupConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateNodegroupVersion(_ *eks.UpdateNodegroupVersionInput, _ string) (*eks.UpdateNodegroupVersionOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DeleteNodegroup(_ *eks.DeleteNodegroupInput, _ string) (*eks.DeleteNodegroupOutput, error) {
	return nil, notImpl()
}

// --- AccessEntry + AccessPolicy ---

func (s *EKSServiceImpl) CreateAccessEntry(_ *eks.CreateAccessEntryInput, _ string) (*eks.CreateAccessEntryOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeAccessEntry(_ *eks.DescribeAccessEntryInput, _ string) (*eks.DescribeAccessEntryOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListAccessEntries(_ *eks.ListAccessEntriesInput, _ string) (*eks.ListAccessEntriesOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateAccessEntry(_ *eks.UpdateAccessEntryInput, _ string) (*eks.UpdateAccessEntryOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DeleteAccessEntry(_ *eks.DeleteAccessEntryInput, _ string) (*eks.DeleteAccessEntryOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) AssociateAccessPolicy(_ *eks.AssociateAccessPolicyInput, _ string) (*eks.AssociateAccessPolicyOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DisassociateAccessPolicy(_ *eks.DisassociateAccessPolicyInput, _ string) (*eks.DisassociateAccessPolicyOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListAssociatedAccessPolicies(_ *eks.ListAssociatedAccessPoliciesInput, _ string) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListAccessPolicies(_ *eks.ListAccessPoliciesInput, _ string) (*eks.ListAccessPoliciesOutput, error) {
	return nil, notImpl()
}

// --- Addons ---

func (s *EKSServiceImpl) ListAddons(_ *eks.ListAddonsInput, _ string) (*eks.ListAddonsOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeAddonVersions(_ *eks.DescribeAddonVersionsInput, _ string) (*eks.DescribeAddonVersionsOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) CreateAddon(_ *eks.CreateAddonInput, _ string) (*eks.CreateAddonOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DeleteAddon(_ *eks.DeleteAddonInput, _ string) (*eks.DeleteAddonOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeAddon(_ *eks.DescribeAddonInput, _ string) (*eks.DescribeAddonOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateAddon(_ *eks.UpdateAddonInput, _ string) (*eks.UpdateAddonOutput, error) {
	return nil, notImpl()
}

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
	return nil
}

func (s *EKSServiceImpl) markFailed(kv nats.KeyValue, name string) {
	if err := SetClusterStatus(kv, name, ClusterStatusFailed); err != nil {
		slog.Warn("CreateCluster: SetClusterStatus(FAILED) failed", "cluster", name, "err", err)
	}
}

func (s *EKSServiceImpl) loadOIDCPrivateKeyPEM(kv nats.KeyValue, name string) (string, error) {
	priv, err := LoadClusterOIDCPrivateKey(kv, name, s.deps.MasterKey)
	if err != nil {
		return "", fmt.Errorf("load OIDC private key: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", fmt.Errorf("marshal pkcs8: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})), nil
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

func (s *EKSServiceImpl) spawnReconciler(accountID, clusterName string, meta *ClusterMeta) {
	healthURL := ""
	if meta.Endpoint != "" {
		healthURL = strings.TrimRight(meta.Endpoint, "/") + "/healthz"
	}
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
	spawn := func(ctx context.Context, _, _ string) (func(), error) {
		return RunClusterReconciler(ctx, s.leaderKV, acctKV, accountID, clusterName, s.deps.HolderID, healthURL)
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
			SubnetIds:        aws.StringSlice(meta.ResourcesVpcConfig.SubnetIds),
			SecurityGroupIds: aws.StringSlice(meta.ResourcesVpcConfig.SecurityGroupIds),
			VpcId:            aws.String(meta.ResourcesVpcConfig.VpcId),
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
	for _, k := range keys {
		if !strings.HasPrefix(k, "clusters/") || !strings.HasSuffix(k, suffix) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(k, "clusters/"), suffix)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names, nil
}

func deref(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}
