// Package access owns the EKS API-mode AccessEntry: its persisted record
// shape, KV storage, and the supported managed access-policy catalogue.
package access

import (
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
)

// Entry type and access-scope type values. v1 supports STANDARD principals
// only; node types (EC2_LINUX/EC2_WINDOWS/FARGATE_LINUX) are rejected until
// the nodegroup join path wires them.
const (
	EntryTypeStandard = "STANDARD"

	ScopeCluster   = "cluster"
	ScopeNamespace = "namespace"
)

// Record is the persisted-state envelope for an EKS API-mode AccessEntry (Q9).
type Record struct {
	ARN                string             `json:"arn"`
	ClusterName        string             `json:"clusterName"`
	PrincipalARN       string             `json:"principalARN"`
	KubernetesUsername string             `json:"kubernetesUsername"`
	KubernetesGroups   []string           `json:"kubernetesGroups,omitempty"`
	Type               string             `json:"type"`
	Tags               map[string]string  `json:"tags,omitempty"`
	AssociatedPolicies []AssociatedPolicy `json:"associatedPolicies,omitempty"`
	CreatedAt          time.Time          `json:"createdAt"`
	ModifiedAt         time.Time          `json:"modifiedAt"`
}

// AssociatedPolicy is a managed access policy bound to an AccessEntry with a
// cluster- or namespace-scoped grant (Q9).
type AssociatedPolicy struct {
	PolicyARN    string    `json:"policyARN"`
	AccessScope  Scope     `json:"accessScope"`
	AssociatedAt time.Time `json:"associatedAt"`
	ModifiedAt   time.Time `json:"modifiedAt"`
}

// Scope restricts an associated access policy to the whole cluster or a set
// of namespaces. Type is ScopeCluster or ScopeNamespace; Namespaces is
// required (and only meaningful) when Type is ScopeNamespace.
type Scope struct {
	Type       string   `json:"type"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// newRecord builds a Record; defaults username to principalARN when unset.
func newRecord(region, accountID, cluster, principalARN, username string, groups []string, entryType string, tags map[string]string, now time.Time) *Record {
	// AWS defaults the Kubernetes username to the principal ARN when omitted.
	if username == "" {
		username = principalARN
	}
	return &Record{
		ARN:                arn.FormatEKSAccessEntry(region, accountID, cluster, PrincipalARNHash(principalARN)),
		ClusterName:        cluster,
		PrincipalARN:       principalARN,
		KubernetesUsername: username,
		KubernetesGroups:   groups,
		Type:               entryType,
		Tags:               tags,
		CreatedAt:          now,
		ModifiedAt:         now,
	}
}
