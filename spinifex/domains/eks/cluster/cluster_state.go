package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// ClusterStatus is the persisted cluster lifecycle state; values match the AWS EKS status enum.
type ClusterStatus string

const (
	ClusterStatusCreating ClusterStatus = "CREATING"
	ClusterStatusActive   ClusterStatus = "ACTIVE"
	ClusterStatusDeleting ClusterStatus = "DELETING"
	ClusterStatusFailed   ClusterStatus = "FAILED"
)

// ClusterVpcConfig is the persisted subset of eks.VpcConfigResponse.
type ClusterVpcConfig struct {
	SubnetIds        []string `json:"subnetIds"`
	SecurityGroupIds []string `json:"securityGroupIds,omitempty"`
	VpcId            string   `json:"vpcId,omitempty"`
	// EndpointPublicAccess/EndpointPrivateAccess mirror AWS resourcesVpcConfig.
	// PublicAccessCidrs stores allowed source ranges for the public endpoint.
	EndpointPublicAccess  bool     `json:"endpointPublicAccess"`
	EndpointPrivateAccess bool     `json:"endpointPrivateAccess"`
	PublicAccessCidrs     []string `json:"publicAccessCidrs,omitempty"`
	// ClusterSecurityGroupId is the EKS-managed primary SG id, created in the
	// caller's own VPC so DescribeSecurityGroups (scoped to the caller's
	// account) can resolve it. Empty for clusters created before this field.
	ClusterSecurityGroupId string `json:"clusterSecurityGroupId,omitempty"`
}

// ClusterNetworkConfig is the persisted subset of eks.KubernetesNetworkConfigResponse,
// captured verbatim from CreateCluster's request. This is a force-new, create-only
// field with no update path, so it is never synthesised after the fact.
type ClusterNetworkConfig struct {
	IpFamily        string `json:"ipFamily,omitempty"`
	ServiceIpv4Cidr string `json:"serviceIpv4Cidr,omitempty"`
	ServiceIpv6Cidr string `json:"serviceIpv6Cidr,omitempty"`
}

// ClusterUpgradePolicyMeta is the persisted subset of eks.UpgradePolicyResponse.
type ClusterUpgradePolicyMeta struct {
	SupportType string `json:"supportType,omitempty"`
}

// ClusterLogSetup mirrors one eks.LogSetup {types, enabled} pairing from a
// CreateCluster request's logging.clusterLogging list, stored verbatim so
// DescribeCluster can echo back exactly what was asked for.
type ClusterLogSetup struct {
	Types   []string `json:"types,omitempty"`
	Enabled bool     `json:"enabled"`
}

// ClusterMeta is the persisted control-plane record at ClusterMetaKey(name); source of truth for DescribeCluster.
type ClusterMeta struct {
	Name         string        `json:"name"`
	Arn          string        `json:"arn"`
	Status       ClusterStatus `json:"status"`
	StatusReason string        `json:"statusReason,omitempty"`
	Version      string        `json:"version"`
	RoleArn      string        `json:"roleArn"`
	Endpoint     string        `json:"endpoint,omitempty"`
	// EndpointIP is the NLB front-end IP (external-pool for public, VPC IP for private).
	// The apiserver serving cert SANs this IP for TLS verification.
	EndpointIP string `json:"endpointIp,omitempty"`
	// EndpointDNSName is the account-qualified apiserver DNS name
	// ({cluster}.{accountID}.{region}.eks.{baseDomain}) registered with northstar → EndpointIP.
	// It is published as Endpoint and SANed on the apiserver cert. Empty otherwise.
	EndpointDNSName string `json:"endpointDnsName,omitempty"`
	// PrivateEndpointIP is the customer-VPC (Set A) IP of the ENI threaded onto the
	// cluster NLB's LB VM when EndpointPrivateAccess is on. In-VPC workers + kubectl
	// reach the control plane here on :443 (no public hairpin / NAT GW). SANed on
	// the apiserver cert. PrivateEndpointENIID is its ENI, kept for teardown.
	PrivateEndpointIP       string            `json:"privateEndpointIp,omitempty"`
	PrivateEndpointENIID    string            `json:"privateEndpointEniId,omitempty"`
	OIDCIssuer              string            `json:"oidcIssuer,omitempty"`
	CertificateAuthorityB64 string            `json:"certificateAuthorityB64,omitempty"`
	ResourcesVpcConfig      *ClusterVpcConfig `json:"resourcesVpcConfig,omitempty"`
	// KubernetesNetworkConfig is the requested service-IP network config,
	// stored verbatim; nil for clusters created before this field or when the
	// caller specified none of it.
	KubernetesNetworkConfig *ClusterNetworkConfig `json:"kubernetesNetworkConfig,omitempty"`
	// BootstrapClusterCreatorAdminPermissions mirrors bootstrapCreatorAdmin's
	// resolved default-true decision, so DescribeCluster echoes exactly what
	// CreateCluster actually did. Nil predates this field and also defaults true.
	BootstrapClusterCreatorAdminPermissions *bool `json:"bootstrapClusterCreatorAdminPermissions,omitempty"`
	// AuthenticationMode is the requested accessConfig.authenticationMode
	// ("API" or "API_AND_CONFIG_MAP"), stored verbatim; empty predates this
	// field and also defaults to API, the only grant path Spinifex ever runs.
	AuthenticationMode string `json:"authenticationMode,omitempty"`
	// UpgradePolicy is the requested extended-support policy, stored verbatim;
	// nil when the caller specified none (DescribeCluster then reports the
	// AWS-standard default).
	UpgradePolicy *ClusterUpgradePolicyMeta `json:"upgradePolicy,omitempty"`
	// Logging is the requested clusterLogging list, stored verbatim including
	// disabled entries; nil when the caller specified none (DescribeCluster
	// then reports every known log type disabled, since none is ever exported).
	Logging                []ClusterLogSetup `json:"logging,omitempty"`
	ControlPlaneInstanceID string            `json:"controlPlaneInstanceId,omitempty"`
	ControlPlaneENIID      string            `json:"controlPlaneEniId,omitempty"`
	ControlPlaneENIIP      string            `json:"controlPlaneEniIp,omitempty"`
	// ControlPlaneMgmtIP is the CP VM's br-mgmt NIC address, used as the /healthz
	// probe target until authoritative in-VPC DNS is available.
	ControlPlaneMgmtIP string `json:"controlPlaneMgmtIp,omitempty"`
	// ManagedCPVPC holds the system-account control-plane VPC ("Set B") refs the
	// NLB + CP VMs live in; nil for clusters created before this topology. Torn
	// down by DeleteCluster.
	ManagedCPVPC *ManagedCPVPC `json:"managedCpVpc,omitempty"`
	// ControlPlaneNodes lists the placed control-plane server VMs. HA spread
	// holds one entry per distinct host; the single-CP path holds one. [0] is the
	// primary the NLB target + egress SNAT wire to until per-node NLB
	// registration (231.7.3). The scalar ControlPlane* fields above mirror [0]
	// for readers that predate this field (reconciler, teardown) and for clusters
	// persisted before HA spread existed (empty ControlPlaneNodes).
	ControlPlaneNodes []ControlPlaneNode `json:"controlPlaneNodes,omitempty"`
	// ControlPlaneSpreadGroup is the spread placement-group name; "" for single-CP.
	ControlPlaneSpreadGroup string `json:"controlPlaneSpreadGroup,omitempty"`
	// ControlPlaneTemplate is the create-time K3sServerInput the reconciler
	// replays to provision a replacement CP member that joins the surviving etcd
	// quorum. Per-node fields (TargetNodeID/ServerURL/KonnServerCount) and rotating
	// creds (AccessKey/SecretKey/IamInstanceProfileArn) are cleared at persist and
	// re-derived at provision. Nil for clusters created before member-count reconcile.
	ControlPlaneTemplate *K3sServerInput `json:"controlPlaneTemplate,omitempty"`
	// EgressEIPAllocationID / EgressEIPPublicIP track the hidden-pool SNAT address
	// for CP VM egress (image pulls). Released on DeleteCluster.
	EgressEIPAllocationID string    `json:"egressEipAllocationId,omitempty"`
	EgressEIPPublicIP     string    `json:"egressEipPublicIp,omitempty"`
	NLBArn                string    `json:"nlbArn,omitempty"`
	NLBTargetGroupArn     string    `json:"nlbTargetGroupArn,omitempty"`
	KonnTargetGroupArn    string    `json:"konnTargetGroupArn,omitempty"`
	CreatedAt             time.Time `json:"createdAt"`
	// DeletingSince stamps when the cluster entered DELETING. No omitempty:
	// encoding/json never treats a time.Time as empty.
	DeletingSince time.Time `json:"deletingSince"`
	// DeleteReapAttempts counts teardown passes against this DELETING cluster
	// since DeletingSince. Drives the reaper's capped exponential backoff.
	DeleteReapAttempts int `json:"deleteReapAttempts,omitempty"`
	// LastDeleteReapAttempt stamps the most recent teardown pass, so backoff
	// grows from the last attempt rather than from DeletingSince.
	// No omitempty: encoding/json never treats a time.Time as empty.
	LastDeleteReapAttempt time.Time `json:"lastDeleteReapAttempt"`
	// DeleteReapExhausted marks a DELETING cluster whose teardown has failed
	// maxDeleteReapAttempts times, for an operator. Retries continue regardless.
	DeleteReapExhausted bool `json:"deleteReapExhausted,omitempty"`
	// LastDeleteReapError is the error from the most recent failed teardown
	// pass, so an operator sees why it is stuck instead of just that it is.
	LastDeleteReapError string `json:"lastDeleteReapError,omitempty"`
	// DeleteFailedStep names, in tenant-facing words, the teardown steps the
	// last failed pass could not complete. DescribeCluster surfaces it; the raw
	// error above carries system-account IDs and is never shown to the tenant.
	DeleteFailedStep string `json:"deleteFailedStep,omitempty"`
	// LaunchOwner names the node whose create launch is still building this
	// record, and LaunchHeartbeat is when that launch last confirmed it was
	// alive. The teardown leaves the record alone while the heartbeat is fresh,
	// so an overtaken launch hands its refs over before the record goes.
	LaunchOwner string `json:"launchOwner,omitempty"`
	// No omitempty: encoding/json never treats a time.Time as empty.
	LaunchHeartbeat time.Time `json:"launchHeartbeat"`
	// HealthIssue is the last health failure reason ("" = healthy).
	// DescribeCluster surfaces it as a ClusterHealth issue.
	HealthIssue string `json:"healthIssue,omitempty"`
	// No omitempty: stdlib encoding/json never treats a time.Time as empty.
	LastHealthProbe time.Time `json:"lastHealthProbe"`
	// NodeCount is the node total from the CP's last NATS state report.
	NodeCount int `json:"nodeCount,omitempty"`
	// NodegroupNodeCounts is the per-nodegroup Ready count from the CP's last NATS
	// state report, keyed by the eks.amazonaws.com/nodegroup node label value.
	// waitWorkersReady gates a nodegroup's ACTIVE transition on ITS OWN entry here,
	// never the cluster-wide NodeCount total — otherwise one nodegroup's Ready
	// workers could mask another nodegroup whose own workers never registered.
	NodegroupNodeCounts map[string]int `json:"nodegroupNodeCounts,omitempty"`
	// Tags are the create-time resource tags, stored verbatim so DescribeCluster
	// echoes them back. Without the round-trip a stock terraform-aws provider
	// reconciling default_tags sees perpetual drift and issues TagResource on
	// every apply. Store-only; no enforcement.
	Tags map[string]string `json:"tags,omitempty"`
}

// ManagedCPVPC records the spinifex-managed control-plane VPC ("Set B") built
// under the system account at CreateCluster: the AWS-managed-account analogue
// the customer never provisions. Holds the resource IDs DeleteCluster tears down
// in dependency order (NAT GW + EIP → route tables → subnets → IGW → VPC). The
// NLB lives in PublicSubnetId; the control-plane VM(s) in PrivateSubnetIds.
type ManagedCPVPC struct {
	VpcId               string   `json:"vpcId,omitempty"`
	IGWId               string   `json:"igwId,omitempty"`
	PublicSubnetId      string   `json:"publicSubnetId,omitempty"`
	PublicRouteTableId  string   `json:"publicRouteTableId,omitempty"`
	PrivateSubnetIds    []string `json:"privateSubnetIds,omitempty"`
	PrivateRouteTableId string   `json:"privateRouteTableId,omitempty"`
	NatGatewayId        string   `json:"natGatewayId,omitempty"`
	NatEIPAllocationID  string   `json:"natEipAllocationId,omitempty"`
	NatEIPPublicIP      string   `json:"natEipPublicIp,omitempty"`
}

// ControlPlaneNode identifies one placed control-plane server VM and the host
// it landed on. NodeID is the Spinifex host — distinct per entry under HA
// spread, empty for a single control plane launched on the local node.
type ControlPlaneNode struct {
	NodeID     string `json:"nodeId,omitempty"`
	InstanceID string `json:"instanceId"`
	ENIID      string `json:"eniId,omitempty"`
	ENIIP      string `json:"eniIp,omitempty"`
	MgmtIP     string `json:"mgmtIp,omitempty"`
}

// ErrClusterNotFound is returned when the cluster meta key is absent.
// Callers translate it to ResourceNotFoundException at the service boundary.
var ErrClusterNotFound = errors.New("eks: cluster not found")

const maxClusterStateCASRetries = 5

// PutClusterMeta writes the meta record unconditionally.
func PutClusterMeta(ctx context.Context, kv jetstream.KeyValue, meta *ClusterMeta) error {
	if meta == nil {
		return errors.New("eks: PutClusterMeta nil meta")
	}
	if meta.Name == "" {
		return errors.New("eks: PutClusterMeta meta.Name empty")
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal cluster meta %s: %w", meta.Name, err)
	}
	if _, err := kv.Put(ctx, ClusterMetaKey(meta.Name), data); err != nil {
		return fmt.Errorf("kv put %s: %w", ClusterMetaKey(meta.Name), err)
	}
	return nil
}

// IsControlPlaneMember reports whether instanceID is one of this cluster's
// control-plane server VMs. The scalar ControlPlaneInstanceID is consulted too,
// for clusters persisted before ControlPlaneNodes existed.
func (m *ClusterMeta) IsControlPlaneMember(instanceID string) bool {
	if m == nil || instanceID == "" {
		return false
	}
	for _, node := range m.ControlPlaneNodes {
		if node.InstanceID == instanceID {
			return true
		}
	}
	return m.ControlPlaneInstanceID == instanceID
}

// GetClusterMeta reads the meta record. Returns ErrClusterNotFound if the
// key is absent.
func GetClusterMeta(ctx context.Context, kv jetstream.KeyValue, name string) (*ClusterMeta, error) {
	if name == "" {
		return nil, errors.New("eks: GetClusterMeta empty name")
	}
	entry, err := kv.Get(ctx, ClusterMetaKey(name))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, ErrClusterNotFound
		}
		return nil, fmt.Errorf("kv get %s: %w", ClusterMetaKey(name), err)
	}
	var meta ClusterMeta
	if err := json.Unmarshal(entry.Value(), &meta); err != nil {
		return nil, fmt.Errorf("unmarshal cluster meta %s: %w", name, err)
	}
	return &meta, nil
}

// casUpdateMeta does a revision-checked read-modify-write of the cluster meta.
// mutate returns true when a field changed. Returns ErrClusterNotFound if the
// cluster is deleted concurrently.
func casUpdateMeta(ctx context.Context, kv jetstream.KeyValue, name string, mutate func(*ClusterMeta) bool) error {
	if name == "" {
		return errors.New("eks: casUpdateMeta empty name")
	}
	_, err := kvutil.Update(ctx, kv, ClusterMetaKey(name), kvutil.CASConfig{
		Attempts: maxClusterStateCASRetries,
		NotFound: ErrClusterNotFound,
		Exhausted: func(string, int) error {
			return fmt.Errorf("eks: casUpdateMeta %s exhausted CAS retries", name)
		},
	}, func(m *ClusterMeta) (bool, error) { return mutate(m), nil })
	return err
}

// ErrClusterDeleting is returned by a status writer that would move a cluster
// off DELETING. A recorded delete is the tenant's decision and nothing but the
// teardown removing the record may end it.
var ErrClusterDeleting = errors.New("eks: cluster is deleting")

// SetClusterStatus does a CAS update of meta.Status. Returns ErrClusterNotFound
// if deleted concurrently, and ErrClusterDeleting rather than leave DELETING.
func SetClusterStatus(ctx context.Context, kv jetstream.KeyValue, name string, status ClusterStatus) error {
	if name == "" {
		return errors.New("eks: SetClusterStatus empty name")
	}
	refused := false
	err := casUpdateMeta(ctx, kv, name, func(m *ClusterMeta) bool {
		refused = false
		if m.Status == status {
			return false
		}
		if m.Status == ClusterStatusDeleting {
			refused = true
			return false
		}
		m.Status = status
		if status == ClusterStatusDeleting && m.DeletingSince.IsZero() {
			m.DeletingSince = time.Now().UTC()
		}
		return true
	})
	if err == nil && refused {
		return ErrClusterDeleting
	}
	return err
}

// errLaunchSuperseded is returned by persistLaunchMeta when the launch no
// longer owns the record it is building: a delete was recorded against it,
// the record is gone, or the name now belongs to a newer cluster.
var errLaunchSuperseded = errors.New("eks: cluster launch superseded")

// errLaunchNewGeneration is errLaunchSuperseded for a record that belongs to a
// different cluster of the same name, created after this launch's was removed.
var errLaunchNewGeneration = fmt.Errorf("%w: the name belongs to a newer cluster", errLaunchSuperseded)

// errLaunchMarkedFailed is returned when the launch's record was marked FAILED
// under it. The refs are merged in and the launch releases what it built.
var errLaunchMarkedFailed = errors.New("eks: cluster marked FAILED during launch")

// sameGeneration reports whether two records describe the same cluster, not
// merely the same name: CreatedAt and the ARN are stamped once, at the claim.
func sameGeneration(a, b *ClusterMeta) bool {
	return a.CreatedAt.Equal(b.CreatedAt) && a.Arn == b.Arn
}

// persistLaunchMeta is the create path's meta write, fenced on the record's
// generation. While its own record is CREATING it writes meta whole. Once a
// delete is recorded it only adds the launch's resource refs, so the teardown
// reaches them, and returns errLaunchSuperseded; a FAILED record also takes
// the refs, with errLaunchMarkedFailed. It never writes a newer cluster's
// record and never recreates a removed one.
func persistLaunchMeta(ctx context.Context, kv jetstream.KeyValue, meta *ClusterMeta) error {
	if meta == nil || meta.Name == "" {
		return errors.New("eks: persistLaunchMeta missing meta")
	}
	var outcome error
	err := casUpdateMeta(ctx, kv, meta.Name, func(m *ClusterMeta) bool {
		outcome = nil
		if !sameGeneration(m, meta) {
			outcome = errLaunchNewGeneration
			return false
		}
		switch m.Status {
		case ClusterStatusCreating:
			*m = *meta
			m.Status = ClusterStatusCreating
			m.LaunchHeartbeat = time.Now().UTC()
			return true
		case ClusterStatusDeleting:
			// Handing the refs over acknowledges the delete: the launch stops.
			outcome = errLaunchSuperseded
			merged := mergeOwnershipRefs(m, meta)
			return clearLaunchOwner(m) || merged
		case ClusterStatusFailed:
			outcome = errLaunchMarkedFailed
			merged := mergeOwnershipRefs(m, meta)
			return clearLaunchOwner(m) || merged
		default:
			return mergeOwnershipRefs(m, meta)
		}
	})
	if errors.Is(err, ErrClusterNotFound) {
		return fmt.Errorf("%w: %w", errLaunchSuperseded, ErrClusterNotFound)
	}
	if err != nil {
		return err
	}
	return outcome
}

func clearLaunchOwner(m *ClusterMeta) bool {
	if m.LaunchOwner == "" {
		return false
	}
	m.LaunchOwner = ""
	return true
}

// launchInFlight reports whether a create launch is still building the record:
// it has an owner whose heartbeat is younger than staleAfter. A launch that
// crashed stops heartbeating and stops counting once staleAfter passes.
func launchInFlight(m *ClusterMeta, staleAfter time.Duration) bool {
	return m.LaunchOwner != "" && time.Since(m.LaunchHeartbeat) < staleAfter
}

// putIfGeneration writes key only while the cluster record under meta's name
// is meta's generation. The key's revision is read before the record is
// checked, so a newer cluster's write in between fails the update and the
// retry sees the newer record.
func putIfGeneration(ctx context.Context, kv jetstream.KeyValue, meta *ClusterMeta, key string, value []byte) error {
	for range maxClusterStateCASRetries {
		var rev uint64
		entry, err := kv.Get(ctx, key)
		switch {
		case err == nil:
			rev = entry.Revision()
		case errors.Is(err, jetstream.ErrKeyNotFound):
		default:
			return fmt.Errorf("kv get %s: %w", key, err)
		}
		cur, err := GetClusterMeta(ctx, kv, meta.Name)
		if err != nil {
			if errors.Is(err, ErrClusterNotFound) {
				return fmt.Errorf("%w: %w", errLaunchSuperseded, ErrClusterNotFound)
			}
			return err
		}
		if !sameGeneration(cur, meta) {
			return errLaunchNewGeneration
		}
		if rev == 0 {
			_, err = kv.Create(ctx, key, value)
		} else {
			_, err = kv.Update(ctx, key, value, rev)
		}
		if err == nil {
			return nil
		}
		if !isRevisionConflict(err) {
			return fmt.Errorf("kv write %s: %w", key, err)
		}
	}
	return fmt.Errorf("eks: write %s kept conflicting", key)
}

// isRevisionConflict reports a revision-conditional write losing its race.
func isRevisionConflict(err error) bool {
	if errors.Is(err, jetstream.ErrKeyExists) || errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
		return true
	}
	if apiErr, ok := errors.AsType[*jetstream.APIError](err); ok {
		return apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence ||
			apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequenceConstant
	}
	return false
}

// clusterOwnershipRefs lists every resource ID the teardown reads from meta to
// find what it must remove. A ref present in a later read and absent from the
// one a teardown pass worked from means that pass did not remove it.
func clusterOwnershipRefs(m *ClusterMeta) map[string]struct{} {
	refs := map[string]struct{}{}
	add := func(kind, id string) {
		if id != "" {
			refs[kind+"="+id] = struct{}{}
		}
	}
	if m == nil {
		return refs
	}
	add("nlb", m.NLBArn)
	add("tg", m.NLBTargetGroupArn)
	add("konn-tg", m.KonnTargetGroupArn)
	add("pe-eni", m.PrivateEndpointENIID)
	add("instance", m.ControlPlaneInstanceID)
	add("eni", m.ControlPlaneENIID)
	for _, n := range m.ControlPlaneNodes {
		add("instance", n.InstanceID)
		add("eni", n.ENIID)
	}
	add("egress-eip", m.EgressEIPAllocationID)
	add("spread", m.ControlPlaneSpreadGroup)
	if m.ManagedCPVPC != nil {
		add("cp-vpc", m.ManagedCPVPC.VpcId)
		add("cp-igw", m.ManagedCPVPC.IGWId)
		add("cp-natgw", m.ManagedCPVPC.NatGatewayId)
		add("cp-nat-eip", m.ManagedCPVPC.NatEIPAllocationID)
	}
	if m.ResourcesVpcConfig != nil {
		add("vpc", m.ResourcesVpcConfig.VpcId)
	}
	return refs
}

// refsAddedSince reports whether after records a ref that before did not.
func refsAddedSince(before, after *ClusterMeta) bool {
	seen := clusterOwnershipRefs(before)
	for ref := range clusterOwnershipRefs(after) {
		if _, ok := seen[ref]; !ok {
			return true
		}
	}
	return false
}

// mergeOwnershipRefs copies src's resource refs into dst where dst lacks them,
// leaving dst's status and delete bookkeeping alone. Reports whether dst changed.
func mergeOwnershipRefs(dst, src *ClusterMeta) bool {
	if !refsAddedSince(dst, src) {
		return false
	}
	setIfEmpty := func(d *string, v string) {
		if *d == "" {
			*d = v
		}
	}
	setIfEmpty(&dst.NLBArn, src.NLBArn)
	setIfEmpty(&dst.NLBTargetGroupArn, src.NLBTargetGroupArn)
	setIfEmpty(&dst.KonnTargetGroupArn, src.KonnTargetGroupArn)
	setIfEmpty(&dst.PrivateEndpointENIID, src.PrivateEndpointENIID)
	setIfEmpty(&dst.PrivateEndpointIP, src.PrivateEndpointIP)
	setIfEmpty(&dst.ControlPlaneInstanceID, src.ControlPlaneInstanceID)
	setIfEmpty(&dst.ControlPlaneENIID, src.ControlPlaneENIID)
	setIfEmpty(&dst.ControlPlaneENIIP, src.ControlPlaneENIIP)
	setIfEmpty(&dst.ControlPlaneMgmtIP, src.ControlPlaneMgmtIP)
	setIfEmpty(&dst.EgressEIPAllocationID, src.EgressEIPAllocationID)
	setIfEmpty(&dst.EgressEIPPublicIP, src.EgressEIPPublicIP)
	setIfEmpty(&dst.ControlPlaneSpreadGroup, src.ControlPlaneSpreadGroup)
	known := map[string]struct{}{}
	for _, n := range dst.ControlPlaneNodes {
		known[n.InstanceID] = struct{}{}
	}
	for _, n := range src.ControlPlaneNodes {
		if _, ok := known[n.InstanceID]; !ok {
			dst.ControlPlaneNodes = append(dst.ControlPlaneNodes, n)
		}
	}
	if dst.ManagedCPVPC == nil && src.ManagedCPVPC != nil {
		cp := *src.ManagedCPVPC
		dst.ManagedCPVPC = &cp
	}
	if dst.ResourcesVpcConfig == nil && src.ResourcesVpcConfig != nil {
		vc := *src.ResourcesVpcConfig
		dst.ResourcesVpcConfig = &vc
	}
	return true
}

// MarkClusterFailed transitions the cluster to FAILED with a reason, but only
// from CREATING — so a late error cannot clobber a concurrent delete or an active cluster.
func MarkClusterFailed(ctx context.Context, kv jetstream.KeyValue, name, reason string) error {
	if name == "" {
		return errors.New("eks: MarkClusterFailed empty name")
	}
	return casUpdateMeta(ctx, kv, name, func(m *ClusterMeta) bool {
		if m.Status != ClusterStatusCreating {
			return false
		}
		m.Status = ClusterStatusFailed
		m.StatusReason = reason
		return true
	})
}

// maxDeleteReapAttempts is how many failed teardown passes a DELETING cluster
// takes before it is flagged DeleteReapExhausted. The flag is a marker for an
// operator; the reaper keeps retrying at its capped interval regardless.
const maxDeleteReapAttempts = 6

// MarkClusterDeleting records the tenant's delete: status DELETING, stamped,
// with the teardown bookkeeping reset. It returns the record as written and
// whether this call made the transition; a record already DELETING is
// returned unchanged so concurrent deletes record one obligation.
func MarkClusterDeleting(ctx context.Context, kv jetstream.KeyValue, name string) (*ClusterMeta, bool, error) {
	if name == "" {
		return nil, false, errors.New("eks: MarkClusterDeleting empty name")
	}
	var out ClusterMeta
	transitioned := false
	err := casUpdateMeta(ctx, kv, name, func(m *ClusterMeta) bool {
		transitioned = false
		if m.Status != ClusterStatusDeleting {
			m.Status = ClusterStatusDeleting
			m.DeletingSince = time.Now().UTC()
			m.DeleteReapAttempts = 0
			m.LastDeleteReapAttempt = time.Time{}
			m.DeleteReapExhausted = false
			m.LastDeleteReapError = ""
			m.DeleteFailedStep = ""
			transitioned = true
		}
		out = *m
		return transitioned
	})
	if err != nil {
		return nil, false, err
	}
	return &out, transitioned, nil
}

// RecordDeleteReapAttempt increments the teardown attempt counter and stamps
// LastDeleteReapAttempt on a DELETING record, returning the updated count.
// Called before each pass so a crash mid-teardown still counts the attempt.
func RecordDeleteReapAttempt(ctx context.Context, kv jetstream.KeyValue, name string) (int, error) {
	if name == "" {
		return 0, errors.New("eks: RecordDeleteReapAttempt empty name")
	}
	var attempts int
	err := casUpdateMeta(ctx, kv, name, func(m *ClusterMeta) bool {
		attempts = m.DeleteReapAttempts
		if m.Status != ClusterStatusDeleting {
			return false
		}
		m.DeleteReapAttempts++
		m.LastDeleteReapAttempt = time.Now().UTC()
		attempts = m.DeleteReapAttempts
		return true
	})
	return attempts, err
}

// RecordDeleteReapFailure persists a failed teardown pass: the error for an
// operator, the tenant-facing names of the failed steps for DescribeCluster,
// and DeleteReapExhausted once the attempt count reaches maxDeleteReapAttempts.
func RecordDeleteReapFailure(ctx context.Context, kv jetstream.KeyValue, name string, steps []string, purgeErr error) error {
	if name == "" {
		return errors.New("eks: RecordDeleteReapFailure empty name")
	}
	return casUpdateMeta(ctx, kv, name, func(m *ClusterMeta) bool {
		if m.Status != ClusterStatusDeleting {
			return false
		}
		m.LastDeleteReapError = purgeErr.Error()
		m.DeleteFailedStep = strings.Join(steps, "; ")
		if m.DeleteReapAttempts >= maxDeleteReapAttempts {
			m.DeleteReapExhausted = true
		}
		return true
	})
}

// DeleteClusterChildren removes every KV key under clusters/{name}/ except
// the meta. It stops at the first failure, so a record whose children could not
// all be removed is still there to own them.
func DeleteClusterChildren(ctx context.Context, kv jetstream.KeyValue, name string) error {
	if name == "" {
		return errors.New("eks: DeleteClusterChildren empty name")
	}
	prefix := fmt.Sprintf("clusters/%s/", name)
	keys, err := kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("kv keys: %w", err)
	}
	meta := ClusterMetaKey(name)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || k == meta {
			continue
		}
		if err := kv.Delete(ctx, k); err != nil {
			return fmt.Errorf("kv delete %s: %w", k, err)
		}
	}
	return nil
}

// SetClusterCertificateAuthority does a CAS update of meta.CertificateAuthorityB64.
// Called by the bootstrap subscriber when the K3s VM publishes its CA.
func SetClusterCertificateAuthority(ctx context.Context, kv jetstream.KeyValue, name, caB64 string) error {
	if name == "" {
		return errors.New("eks: SetClusterCertificateAuthority empty name")
	}
	if caB64 == "" {
		return errors.New("eks: SetClusterCertificateAuthority empty CA")
	}
	return casUpdateMeta(ctx, kv, name, func(m *ClusterMeta) bool {
		if m.CertificateAuthorityB64 == caB64 {
			return false
		}
		m.CertificateAuthorityB64 = caB64
		return true
	})
}

// SetClusterHealth records the latest health outcome; issue="" means healthy.
// Only writes on a state change, stamping LastHealthProbe at the transition.
func SetClusterHealth(ctx context.Context, kv jetstream.KeyValue, name, issue string) error {
	if name == "" {
		return errors.New("eks: SetClusterHealth empty name")
	}
	return casUpdateMeta(ctx, kv, name, func(m *ClusterMeta) bool {
		if m.HealthIssue == issue {
			return false
		}
		m.HealthIssue = issue
		m.LastHealthProbe = time.Now().UTC()
		return true
	})
}

// SetClusterHealthState records health + node count (cluster-wide and, when the
// CP state report carries it, per-nodegroup). LastHealthProbe is stamped on any
// change; no KV write when nothing changed. nodegroupReady may be nil (older
// AMI / no report yet), in which case the persisted per-nodegroup counts are
// left untouched.
func SetClusterHealthState(ctx context.Context, kv jetstream.KeyValue, name, issue string, nodeCount int, nodegroupReady map[string]int) error {
	if name == "" {
		return errors.New("eks: SetClusterHealthState empty name")
	}
	return casUpdateMeta(ctx, kv, name, func(m *ClusterMeta) bool {
		changed := m.HealthIssue != issue || m.NodeCount != nodeCount
		if nodegroupReady != nil && !maps.Equal(m.NodegroupNodeCounts, nodegroupReady) {
			changed = true
		}
		if !changed {
			return false
		}
		m.HealthIssue = issue
		m.NodeCount = nodeCount
		if nodegroupReady != nil {
			m.NodegroupNodeCounts = nodegroupReady
		}
		m.LastHealthProbe = time.Now().UTC()
		return true
	})
}

// ClearClusterManagedCPVPC clears meta.ManagedCPVPC once the managed CP VPC's
// EC2 teardown has converged (including converging from an already-gone VPC).
// purgeClusterInfra calls it immediately after a successful DeleteClusterCPVPC
// so a re-drive of a wedged DELETING — with some unrelated step still failing —
// never retries EC2/OVN calls against a stale VpcId, which is what turns a
// single DependencyViolation into a permanent teardown loop. No-op if already
// cleared. Returns ErrClusterNotFound if the cluster was deleted concurrently.
func ClearClusterManagedCPVPC(ctx context.Context, kv jetstream.KeyValue, name string) error {
	if name == "" {
		return errors.New("eks: ClearClusterManagedCPVPC empty name")
	}
	return casUpdateMeta(ctx, kv, name, func(m *ClusterMeta) bool {
		if m.ManagedCPVPC == nil {
			return false
		}
		m.ManagedCPVPC = nil
		return true
	})
}

// SwapControlPlaneMember atomically replaces a lost control-plane member
// (deadInstanceID) with a freshly provisioned one in meta.ControlPlaneNodes and
// refreshes the scalar [0] mirrors if the primary changed. A no-op (no error)
// when the dead member is already gone from the list — a concurrent swap already
// won. Returns ErrClusterNotFound if the cluster was deleted concurrently.
func SwapControlPlaneMember(ctx context.Context, kv jetstream.KeyValue, name, deadInstanceID string, replacement ControlPlaneNode) error {
	if name == "" {
		return errors.New("eks: SwapControlPlaneMember empty name")
	}
	if replacement.InstanceID == "" {
		return errors.New("eks: SwapControlPlaneMember empty replacement instance id")
	}
	return casUpdateMeta(ctx, kv, name, func(m *ClusterMeta) bool {
		idx := -1
		for i, n := range m.ControlPlaneNodes {
			if n.InstanceID == deadInstanceID {
				idx = i
				break
			}
		}
		if idx == -1 {
			return false
		}
		m.ControlPlaneNodes[idx] = replacement
		primary := m.ControlPlaneNodes[0]
		m.ControlPlaneInstanceID = primary.InstanceID
		m.ControlPlaneENIID = primary.ENIID
		m.ControlPlaneENIIP = primary.ENIIP
		m.ControlPlaneMgmtIP = primary.MgmtIP
		return true
	})
}
