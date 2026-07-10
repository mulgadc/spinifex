package handlers_eks

import "time"

// Cluster lifecycle status strings — values match the AWS EKS DescribeCluster
// status enum verbatim so clients can match against them without translation.
const (
	StatusCreating = "CREATING"
	StatusActive   = "ACTIVE"
	StatusUpdating = "UPDATING"
	StatusDeleting = "DELETING"
	StatusFailed   = "FAILED"
)

// ClusterRecord is the persisted-state envelope for an EKS cluster. Only the
// fields actually persisted by the in-tree handlers live here — the public
// AWS DescribeCluster shape is reconstructed at the gateway/service boundary
// from the aws-sdk-go eks types directly.
type ClusterRecord struct {
	AccountID string    `json:"accountID"`
	Name      string    `json:"name"`
	Region    string    `json:"region"`
	ARN       string    `json:"arn"`
	Status    string    `json:"status"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
}

// NodegroupRecord is the persisted-state envelope for an EKS managed
// nodegroup. It is the source of truth for Describe/ListNodegroups and tracks
// the worker EC2 instance IDs the nodegroup owns so Update/DeleteNodegroup can
// scale and tear them down.
type NodegroupRecord struct {
	ClusterName    string            `json:"clusterName"`
	Name           string            `json:"name"`
	Arn            string            `json:"arn"`
	Status         string            `json:"status"`
	StatusReason   string            `json:"statusReason,omitempty"`
	Subnets        []string          `json:"subnets,omitempty"`
	InstanceTypes  []string          `json:"instanceTypes,omitempty"`
	AMIType        string            `json:"amiType,omitempty"`
	DiskSize       int64             `json:"diskSize,omitempty"`
	ScalingMin     int64             `json:"scalingMin"`
	ScalingMax     int64             `json:"scalingMax"`
	ScalingDesired int64             `json:"scalingDesired"`
	Version        string            `json:"version,omitempty"`
	NodeRole       string            `json:"nodeRole,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	InstanceIDs    []string          `json:"instanceIds,omitempty"`
	Health         string            `json:"health,omitempty"`
	// GPUEnabled/GPUVendor cache whether InstanceTypes includes a GPU family and
	// which vendor, so worker launches resolve the matching GPU node AMI without
	// re-scanning InstanceTypes on every launch.
	GPUEnabled bool      `json:"gpuEnabled,omitempty"`
	GPUVendor  string    `json:"gpuVendor,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

// AccessEntryRecord is the persisted-state envelope for an EKS API-mode
// AccessEntry (Q9).
type AccessEntryRecord struct {
	ARN                string                   `json:"arn"`
	ClusterName        string                   `json:"clusterName"`
	PrincipalARN       string                   `json:"principalARN"`
	KubernetesUsername string                   `json:"kubernetesUsername"`
	KubernetesGroups   []string                 `json:"kubernetesGroups,omitempty"`
	Type               string                   `json:"type"`
	Tags               map[string]string        `json:"tags,omitempty"`
	AssociatedPolicies []AssociatedAccessPolicy `json:"associatedPolicies,omitempty"`
	CreatedAt          time.Time                `json:"createdAt"`
	ModifiedAt         time.Time                `json:"modifiedAt"`
}

// AssociatedAccessPolicy is a managed access policy bound to an AccessEntry
// with a cluster- or namespace-scoped grant (Q9).
type AssociatedAccessPolicy struct {
	PolicyARN    string      `json:"policyARN"`
	AccessScope  AccessScope `json:"accessScope"`
	AssociatedAt time.Time   `json:"associatedAt"`
	ModifiedAt   time.Time   `json:"modifiedAt"`
}

// AccessScope restricts an associated access policy to the whole cluster or a
// set of namespaces. Type is "cluster" or "namespace"; Namespaces is required
// (and only meaningful) when Type is "namespace".
type AccessScope struct {
	Type       string   `json:"type"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// OIDCProviderConfigRecord captures the minimum needed to register an OIDC
// identity provider against a cluster.
type OIDCProviderConfigRecord struct {
	ClusterName string    `json:"clusterName"`
	IssuerURL   string    `json:"issuerURL"`
	IssuerHash  string    `json:"issuerHash"`
	ClientID    string    `json:"clientID"`
	CreatedAt   time.Time `json:"createdAt"`
}
