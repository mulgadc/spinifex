package handlers_eks

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
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
	// PrivateEndpointIP is the customer-VPC (Set A) IP of the ENI threaded onto the
	// cluster NLB's LB VM when EndpointPrivateAccess is on. In-VPC workers + kubectl
	// reach the control plane here on :443 (no public hairpin / NAT GW). SANed on
	// the apiserver cert. PrivateEndpointENIID is its ENI, kept for teardown.
	PrivateEndpointIP       string            `json:"privateEndpointIp,omitempty"`
	PrivateEndpointENIID    string            `json:"privateEndpointEniId,omitempty"`
	OIDCIssuer              string            `json:"oidcIssuer,omitempty"`
	CertificateAuthorityB64 string            `json:"certificateAuthorityB64,omitempty"`
	ResourcesVpcConfig      *ClusterVpcConfig `json:"resourcesVpcConfig,omitempty"`
	ControlPlaneInstanceID  string            `json:"controlPlaneInstanceId,omitempty"`
	ControlPlaneENIID       string            `json:"controlPlaneEniId,omitempty"`
	ControlPlaneENIIP       string            `json:"controlPlaneEniIp,omitempty"`
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
	// DeletingSince stamps when the cluster entered DELETING. The teardown
	// backstop reaper waits out a healthy synchronous DeleteCluster (min-age)
	// before re-driving purgeClusterInfra, so it only ever re-drives a wedged
	// teardown, never one still in progress. No omitempty: encoding/json never
	// treats a time.Time as empty.
	DeletingSince time.Time `json:"deletingSince"`
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
func PutClusterMeta(kv nats.KeyValue, meta *ClusterMeta) error {
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
	if _, err := kv.Put(ClusterMetaKey(meta.Name), data); err != nil {
		return fmt.Errorf("kv put %s: %w", ClusterMetaKey(meta.Name), err)
	}
	return nil
}

// GetClusterMeta reads the meta record. Returns ErrClusterNotFound if the
// key is absent.
func GetClusterMeta(kv nats.KeyValue, name string) (*ClusterMeta, error) {
	if name == "" {
		return nil, errors.New("eks: GetClusterMeta empty name")
	}
	entry, err := kv.Get(ClusterMetaKey(name))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
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
// mutate returns true when a field changed. Retries on CAS conflict up to
// maxClusterStateCASRetries. Returns ErrClusterNotFound if deleted concurrently.
func casUpdateMeta(kv nats.KeyValue, name string, mutate func(*ClusterMeta) bool) error {
	if name == "" {
		return errors.New("eks: casUpdateMeta empty name")
	}
	for range maxClusterStateCASRetries {
		entry, err := kv.Get(ClusterMetaKey(name))
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				return ErrClusterNotFound
			}
			return fmt.Errorf("kv get %s: %w", ClusterMetaKey(name), err)
		}
		var meta ClusterMeta
		if err := json.Unmarshal(entry.Value(), &meta); err != nil {
			return fmt.Errorf("unmarshal cluster meta %s: %w", name, err)
		}
		if !mutate(&meta) {
			return nil
		}
		data, err := json.Marshal(&meta)
		if err != nil {
			return fmt.Errorf("marshal cluster meta %s: %w", name, err)
		}
		_, err = kv.Update(ClusterMetaKey(name), data, entry.Revision())
		if err == nil {
			return nil
		}
		if errors.Is(err, nats.ErrKeyExists) {
			continue
		}
		return fmt.Errorf("kv update %s: %w", ClusterMetaKey(name), err)
	}
	return fmt.Errorf("eks: casUpdateMeta %s exhausted CAS retries", name)
}

// SetClusterStatus does a CAS update of meta.Status. Returns ErrClusterNotFound if deleted concurrently.
func SetClusterStatus(kv nats.KeyValue, name string, status ClusterStatus) error {
	if name == "" {
		return errors.New("eks: SetClusterStatus empty name")
	}
	return casUpdateMeta(kv, name, func(m *ClusterMeta) bool {
		if m.Status == status {
			return false
		}
		m.Status = status
		if status == ClusterStatusDeleting && m.DeletingSince.IsZero() {
			m.DeletingSince = time.Now().UTC()
		}
		return true
	})
}

// MarkClusterFailed transitions the cluster to FAILED with a reason, but only
// from CREATING — so a late error cannot clobber a concurrent delete or an active cluster.
func MarkClusterFailed(kv nats.KeyValue, name, reason string) error {
	if name == "" {
		return errors.New("eks: MarkClusterFailed empty name")
	}
	return casUpdateMeta(kv, name, func(m *ClusterMeta) bool {
		if m.Status != ClusterStatusCreating {
			return false
		}
		m.Status = ClusterStatusFailed
		m.StatusReason = reason
		return true
	})
}

// SetClusterCertificateAuthority does a CAS update of meta.CertificateAuthorityB64.
// Called by the bootstrap subscriber when the K3s VM publishes its CA.
func SetClusterCertificateAuthority(kv nats.KeyValue, name, caB64 string) error {
	if name == "" {
		return errors.New("eks: SetClusterCertificateAuthority empty name")
	}
	if caB64 == "" {
		return errors.New("eks: SetClusterCertificateAuthority empty CA")
	}
	return casUpdateMeta(kv, name, func(m *ClusterMeta) bool {
		if m.CertificateAuthorityB64 == caB64 {
			return false
		}
		m.CertificateAuthorityB64 = caB64
		return true
	})
}

// SetClusterHealth records the latest health outcome; issue="" means healthy.
// Only writes on a state change, stamping LastHealthProbe at the transition.
func SetClusterHealth(kv nats.KeyValue, name, issue string) error {
	if name == "" {
		return errors.New("eks: SetClusterHealth empty name")
	}
	return casUpdateMeta(kv, name, func(m *ClusterMeta) bool {
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
func SetClusterHealthState(kv nats.KeyValue, name, issue string, nodeCount int, nodegroupReady map[string]int) error {
	if name == "" {
		return errors.New("eks: SetClusterHealthState empty name")
	}
	return casUpdateMeta(kv, name, func(m *ClusterMeta) bool {
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
func ClearClusterManagedCPVPC(kv nats.KeyValue, name string) error {
	if name == "" {
		return errors.New("eks: ClearClusterManagedCPVPC empty name")
	}
	return casUpdateMeta(kv, name, func(m *ClusterMeta) bool {
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
func SwapControlPlaneMember(kv nats.KeyValue, name, deadInstanceID string, replacement ControlPlaneNode) error {
	if name == "" {
		return errors.New("eks: SwapControlPlaneMember empty name")
	}
	if replacement.InstanceID == "" {
		return errors.New("eks: SwapControlPlaneMember empty replacement instance id")
	}
	return casUpdateMeta(kv, name, func(m *ClusterMeta) bool {
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

// DeleteClusterPrefix removes every KV key under clusters/{name}/.
// Returns the first error but continues sweeping.
func DeleteClusterPrefix(kv nats.KeyValue, name string) error {
	if name == "" {
		return errors.New("eks: DeleteClusterPrefix empty name")
	}
	prefix := fmt.Sprintf("clusters/%s/", name)
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("kv keys: %w", err)
	}
	var firstErr error
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if err := kv.Delete(k); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("kv delete %s: %w", k, err)
		}
	}
	return firstErr
}
