// Package handlers_eks implements the EKS control-plane API surface for Spinifex.
package handlers_eks

import "github.com/aws/aws-sdk-go/service/eks"

// EKSService is the EKS control-plane contract; one method per AWS EKS API action.
type EKSService interface {
	// Cluster. callerPrincipalARN is the IAM principal for the bootstrap AccessEntry; "" skips it.
	CreateCluster(input *eks.CreateClusterInput, accountID, callerPrincipalARN string) (*eks.CreateClusterOutput, error)
	DescribeCluster(input *eks.DescribeClusterInput, accountID string) (*eks.DescribeClusterOutput, error)
	ListClusters(input *eks.ListClustersInput, accountID string) (*eks.ListClustersOutput, error)
	UpdateClusterConfig(input *eks.UpdateClusterConfigInput, accountID string) (*eks.UpdateClusterConfigOutput, error)
	UpdateClusterVersion(input *eks.UpdateClusterVersionInput, accountID string) (*eks.UpdateClusterVersionOutput, error)
	DeleteCluster(input *eks.DeleteClusterInput, accountID string) (*eks.DeleteClusterOutput, error)

	// Nodegroup
	CreateNodegroup(input *eks.CreateNodegroupInput, accountID string) (*eks.CreateNodegroupOutput, error)
	DescribeNodegroup(input *eks.DescribeNodegroupInput, accountID string) (*eks.DescribeNodegroupOutput, error)
	ListNodegroups(input *eks.ListNodegroupsInput, accountID string) (*eks.ListNodegroupsOutput, error)
	UpdateNodegroupConfig(input *eks.UpdateNodegroupConfigInput, accountID string) (*eks.UpdateNodegroupConfigOutput, error)
	UpdateNodegroupVersion(input *eks.UpdateNodegroupVersionInput, accountID string) (*eks.UpdateNodegroupVersionOutput, error)
	DeleteNodegroup(input *eks.DeleteNodegroupInput, accountID string) (*eks.DeleteNodegroupOutput, error)

	// AccessEntry + AccessPolicy
	CreateAccessEntry(input *eks.CreateAccessEntryInput, accountID string) (*eks.CreateAccessEntryOutput, error)
	DescribeAccessEntry(input *eks.DescribeAccessEntryInput, accountID string) (*eks.DescribeAccessEntryOutput, error)
	ListAccessEntries(input *eks.ListAccessEntriesInput, accountID string) (*eks.ListAccessEntriesOutput, error)
	UpdateAccessEntry(input *eks.UpdateAccessEntryInput, accountID string) (*eks.UpdateAccessEntryOutput, error)
	DeleteAccessEntry(input *eks.DeleteAccessEntryInput, accountID string) (*eks.DeleteAccessEntryOutput, error)
	AssociateAccessPolicy(input *eks.AssociateAccessPolicyInput, accountID string) (*eks.AssociateAccessPolicyOutput, error)
	DisassociateAccessPolicy(input *eks.DisassociateAccessPolicyInput, accountID string) (*eks.DisassociateAccessPolicyOutput, error)
	ListAssociatedAccessPolicies(input *eks.ListAssociatedAccessPoliciesInput, accountID string) (*eks.ListAssociatedAccessPoliciesOutput, error)
	ListAccessPolicies(input *eks.ListAccessPoliciesInput, accountID string) (*eks.ListAccessPoliciesOutput, error)

	// Addons
	ListAddons(input *eks.ListAddonsInput, accountID string) (*eks.ListAddonsOutput, error)
	DescribeAddonVersions(input *eks.DescribeAddonVersionsInput, accountID string) (*eks.DescribeAddonVersionsOutput, error)
	CreateAddon(input *eks.CreateAddonInput, accountID string) (*eks.CreateAddonOutput, error)
	DeleteAddon(input *eks.DeleteAddonInput, accountID string) (*eks.DeleteAddonOutput, error)
	DescribeAddon(input *eks.DescribeAddonInput, accountID string) (*eks.DescribeAddonOutput, error)
	UpdateAddon(input *eks.UpdateAddonInput, accountID string) (*eks.UpdateAddonOutput, error)
	// ListStagedAddonManifests is an internal control-plane method (not an
	// AWS-SDK action): the on-VM addon-sync agent fetches staged manifests for a
	// cluster via the internal-addons gateway route.
	ListStagedAddonManifests(input *ListStagedAddonManifestsInput, accountID string) (*ListStagedAddonManifestsOutput, error)

	// OIDC identity-provider configs.
	AssociateIdentityProviderConfig(input *eks.AssociateIdentityProviderConfigInput, accountID string) (*eks.AssociateIdentityProviderConfigOutput, error)
	DescribeIdentityProviderConfig(input *eks.DescribeIdentityProviderConfigInput, accountID string) (*eks.DescribeIdentityProviderConfigOutput, error)
	ListIdentityProviderConfigs(input *eks.ListIdentityProviderConfigsInput, accountID string) (*eks.ListIdentityProviderConfigsOutput, error)
	DisassociateIdentityProviderConfig(input *eks.DisassociateIdentityProviderConfigInput, accountID string) (*eks.DisassociateIdentityProviderConfigOutput, error)

	// Tags
	TagResource(input *eks.TagResourceInput, accountID string) (*eks.TagResourceOutput, error)
	UntagResource(input *eks.UntagResourceInput, accountID string) (*eks.UntagResourceOutput, error)
	ListTagsForResource(input *eks.ListTagsForResourceInput, accountID string) (*eks.ListTagsForResourceOutput, error)
}
