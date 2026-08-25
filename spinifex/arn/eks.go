package arn

import "fmt"

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
