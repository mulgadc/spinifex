package arn

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"uuid"
)

// EKSResourceType is the resource-type segment of an arn:aws:eks ARN.
type EKSResourceType string

const (
	EKSCluster     EKSResourceType = "cluster"
	EKSNodegroup   EKSResourceType = "nodegroup"
	EKSAddon       EKSResourceType = "addon"
	EKSAccessEntry EKSResourceType = "access-entry"
)

// FormatEKSCluster builds arn:aws:eks:<region>:<account>:cluster/<name>.
func FormatEKSCluster(region, accountID, name string) string {
	return fmt.Sprintf("arn:aws:eks:%s:%s:%s/%s", region, accountID, EKSCluster, name)
}

// FormatEKSNodegroup builds the AWS nodegroup shape,
// arn:aws:eks:<region>:<account>:nodegroup/<cluster>/<name>/<discriminator>.
// The discriminator is the per-nodegroup UUID AWS appends.
func FormatEKSNodegroup(region, accountID, cluster, name, discriminator string) string {
	return fmt.Sprintf("arn:aws:eks:%s:%s:%s/%s/%s/%s", region, accountID, EKSNodegroup, cluster, name, discriminator)
}

// Scopes the digest to this derivation so the value cannot collide with one
// derived elsewhere from the same identifiers.
const eksNodegroupNamespace = "mulga:eks:nodegroup"

// EKSNodegroupDiscriminator derives a nodegroup ARN's trailing segment from the
// tuple that already identifies the nodegroup. AWS puts a random UUID there,
// which only the stored record knows; deriving it lets the policy gate name the
// exact stored ARN without reading the record.
func EKSNodegroupDiscriminator(accountID, cluster, name string) string {
	// NUL-separated so ("a", "bc") and ("ab", "c") cannot digest alike.
	sum := sha256.Sum256([]byte(strings.Join([]string{eksNodegroupNamespace, accountID, cluster, name}, "\x00")))
	var u uuid.UUID
	copy(u[:], sum[:])
	u[6] = u[6]&0x0f | 0x80 // RFC 9562 version 8: custom, digest-derived.
	u[8] = u[8]&0x3f | 0x80 // RFC 9562 variant 10.
	return u.String()
}

// FormatEKSAddon builds arn:aws:eks:<region>:<account>:addon/<cluster>/<name>.
func FormatEKSAddon(region, accountID, cluster, name string) string {
	return fmt.Sprintf("arn:aws:eks:%s:%s:%s/%s/%s", region, accountID, EKSAddon, cluster, name)
}

// FormatEKSAccessEntry builds
// arn:aws:eks:<region>:<account>:access-entry/<cluster>/<discriminator>.
// The discriminator identifies the entry's principal; callers compute it.
func FormatEKSAccessEntry(region, accountID, cluster, discriminator string) string {
	return fmt.Sprintf("arn:aws:eks:%s:%s:%s/%s/%s", region, accountID, EKSAccessEntry, cluster, discriminator)
}

// FormatEKSResource builds an EKS ARN from an already-formed resource
// component, such as one parsed off a caller-supplied ARN.
func FormatEKSResource(region, accountID, resource string) string {
	return fmt.Sprintf("arn:aws:eks:%s:%s:%s", region, accountID, resource)
}
