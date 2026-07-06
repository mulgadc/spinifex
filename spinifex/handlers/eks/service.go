// Package handlers_eks implements the EKS control-plane API surface for Spinifex.
package handlers_eks

import (
	"context"

	"github.com/aws/aws-sdk-go/service/eks"
)

// EKSService is the EKS control-plane contract; one method per AWS EKS API action.
type EKSService interface {
	// Cluster. callerPrincipalARN is the IAM principal for the bootstrap AccessEntry; "" skips it.
	CreateCluster(ctx context.Context, input *eks.CreateClusterInput, accountID, callerPrincipalARN string) (*eks.CreateClusterOutput, error)
	DescribeCluster(ctx context.Context, input *eks.DescribeClusterInput, accountID string) (*eks.DescribeClusterOutput, error)
	ListClusters(ctx context.Context, input *eks.ListClustersInput, accountID string) (*eks.ListClustersOutput, error)
	UpdateClusterConfig(ctx context.Context, input *eks.UpdateClusterConfigInput, accountID string) (*eks.UpdateClusterConfigOutput, error)
	UpdateClusterVersion(ctx context.Context, input *eks.UpdateClusterVersionInput, accountID string) (*eks.UpdateClusterVersionOutput, error)
	DeleteCluster(ctx context.Context, input *eks.DeleteClusterInput, accountID string) (*eks.DeleteClusterOutput, error)

	// Nodegroup
	CreateNodegroup(ctx context.Context, input *eks.CreateNodegroupInput, accountID string) (*eks.CreateNodegroupOutput, error)
	DescribeNodegroup(ctx context.Context, input *eks.DescribeNodegroupInput, accountID string) (*eks.DescribeNodegroupOutput, error)
	ListNodegroups(ctx context.Context, input *eks.ListNodegroupsInput, accountID string) (*eks.ListNodegroupsOutput, error)
	UpdateNodegroupConfig(ctx context.Context, input *eks.UpdateNodegroupConfigInput, accountID string) (*eks.UpdateNodegroupConfigOutput, error)
	UpdateNodegroupVersion(ctx context.Context, input *eks.UpdateNodegroupVersionInput, accountID string) (*eks.UpdateNodegroupVersionOutput, error)
	DeleteNodegroup(ctx context.Context, input *eks.DeleteNodegroupInput, accountID string) (*eks.DeleteNodegroupOutput, error)

	// AccessEntry + AccessPolicy
	CreateAccessEntry(ctx context.Context, input *eks.CreateAccessEntryInput, accountID string) (*eks.CreateAccessEntryOutput, error)
	DescribeAccessEntry(ctx context.Context, input *eks.DescribeAccessEntryInput, accountID string) (*eks.DescribeAccessEntryOutput, error)
	ListAccessEntries(ctx context.Context, input *eks.ListAccessEntriesInput, accountID string) (*eks.ListAccessEntriesOutput, error)
	UpdateAccessEntry(ctx context.Context, input *eks.UpdateAccessEntryInput, accountID string) (*eks.UpdateAccessEntryOutput, error)
	DeleteAccessEntry(ctx context.Context, input *eks.DeleteAccessEntryInput, accountID string) (*eks.DeleteAccessEntryOutput, error)
	AssociateAccessPolicy(ctx context.Context, input *eks.AssociateAccessPolicyInput, accountID string) (*eks.AssociateAccessPolicyOutput, error)
	DisassociateAccessPolicy(ctx context.Context, input *eks.DisassociateAccessPolicyInput, accountID string) (*eks.DisassociateAccessPolicyOutput, error)
	ListAssociatedAccessPolicies(ctx context.Context, input *eks.ListAssociatedAccessPoliciesInput, accountID string) (*eks.ListAssociatedAccessPoliciesOutput, error)
	ListAccessPolicies(ctx context.Context, input *eks.ListAccessPoliciesInput, accountID string) (*eks.ListAccessPoliciesOutput, error)

	// Addons
	ListAddons(ctx context.Context, input *eks.ListAddonsInput, accountID string) (*eks.ListAddonsOutput, error)
	DescribeAddonVersions(ctx context.Context, input *eks.DescribeAddonVersionsInput, accountID string) (*eks.DescribeAddonVersionsOutput, error)
	CreateAddon(ctx context.Context, input *eks.CreateAddonInput, accountID string) (*eks.CreateAddonOutput, error)
	DeleteAddon(ctx context.Context, input *eks.DeleteAddonInput, accountID string) (*eks.DeleteAddonOutput, error)
	DescribeAddon(ctx context.Context, input *eks.DescribeAddonInput, accountID string) (*eks.DescribeAddonOutput, error)
	UpdateAddon(ctx context.Context, input *eks.UpdateAddonInput, accountID string) (*eks.UpdateAddonOutput, error)
	// ListStagedAddonManifests is an internal control-plane method (not an
	// AWS-SDK action): the on-VM addon-sync agent fetches staged manifests for a
	// cluster via the internal-addons gateway route.
	ListStagedAddonManifests(ctx context.Context, input *ListStagedAddonManifestsInput, accountID string) (*ListStagedAddonManifestsOutput, error)

	// OIDC identity-provider configs.
	AssociateIdentityProviderConfig(ctx context.Context, input *eks.AssociateIdentityProviderConfigInput, accountID string) (*eks.AssociateIdentityProviderConfigOutput, error)
	DescribeIdentityProviderConfig(ctx context.Context, input *eks.DescribeIdentityProviderConfigInput, accountID string) (*eks.DescribeIdentityProviderConfigOutput, error)
	ListIdentityProviderConfigs(ctx context.Context, input *eks.ListIdentityProviderConfigsInput, accountID string) (*eks.ListIdentityProviderConfigsOutput, error)
	DisassociateIdentityProviderConfig(ctx context.Context, input *eks.DisassociateIdentityProviderConfigInput, accountID string) (*eks.DisassociateIdentityProviderConfigOutput, error)

	// Tags
	TagResource(ctx context.Context, input *eks.TagResourceInput, accountID string) (*eks.TagResourceOutput, error)
	UntagResource(ctx context.Context, input *eks.UntagResourceInput, accountID string) (*eks.UntagResourceOutput, error)
	ListTagsForResource(ctx context.Context, input *eks.ListTagsForResourceInput, accountID string) (*eks.ListTagsForResourceOutput, error)
}
