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
// nodegroup.
type NodegroupRecord struct {
	ClusterName   string    `json:"clusterName"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	DesiredSize   int64     `json:"desiredSize"`
	InstanceTypes []string  `json:"instanceTypes,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

// AccessEntryRecord is the persisted-state envelope for an EKS API-mode
// AccessEntry (Q9).
type AccessEntryRecord struct {
	ClusterName        string    `json:"clusterName"`
	PrincipalARN       string    `json:"principalARN"`
	KubernetesUsername string    `json:"kubernetesUsername"`
	KubernetesGroups   []string  `json:"kubernetesGroups,omitempty"`
	Type               string    `json:"type"`
	CreatedAt          time.Time `json:"createdAt"`
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
