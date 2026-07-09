package handlers_eks

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const defaultTimeout = 30 * time.Second

// NATSEKSService is the gateway-side adapter that forwards every EKSService
// method as a NATS request to the daemon's matching subscriber.
type NATSEKSService struct {
	natsConn *nats.Conn
}

var _ EKSService = (*NATSEKSService)(nil)

// NewNATSEKSService returns an EKSService that uses NATS request-response.
func NewNATSEKSService(conn *nats.Conn) EKSService {
	return &NATSEKSService{natsConn: conn}
}

// --- Cluster ---

func (s *NATSEKSService) CreateCluster(ctx context.Context, input *eks.CreateClusterInput, accountID, callerPrincipalARN string) (*eks.CreateClusterOutput, error) {
	return utils.NatsRequest[eks.CreateClusterOutput](ctx, s.natsConn, "eks.CreateCluster", input, defaultTimeout, accountID,
		utils.NATSHeader{Key: utils.PrincipalARNHeader, Value: callerPrincipalARN})
}

func (s *NATSEKSService) DescribeCluster(ctx context.Context, input *eks.DescribeClusterInput, accountID string) (*eks.DescribeClusterOutput, error) {
	return utils.NatsRequest[eks.DescribeClusterOutput](ctx, s.natsConn, "eks.DescribeCluster", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListClusters(ctx context.Context, input *eks.ListClustersInput, accountID string) (*eks.ListClustersOutput, error) {
	return utils.NatsRequest[eks.ListClustersOutput](ctx, s.natsConn, "eks.ListClusters", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateClusterConfig(ctx context.Context, input *eks.UpdateClusterConfigInput, accountID string) (*eks.UpdateClusterConfigOutput, error) {
	return utils.NatsRequest[eks.UpdateClusterConfigOutput](ctx, s.natsConn, "eks.UpdateClusterConfig", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateClusterVersion(ctx context.Context, input *eks.UpdateClusterVersionInput, accountID string) (*eks.UpdateClusterVersionOutput, error) {
	return utils.NatsRequest[eks.UpdateClusterVersionOutput](ctx, s.natsConn, "eks.UpdateClusterVersion", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DeleteCluster(ctx context.Context, input *eks.DeleteClusterInput, accountID string) (*eks.DeleteClusterOutput, error) {
	return utils.NatsRequest[eks.DeleteClusterOutput](ctx, s.natsConn, "eks.DeleteCluster", input, defaultTimeout, accountID)
}

// --- Nodegroup ---

func (s *NATSEKSService) CreateNodegroup(ctx context.Context, input *eks.CreateNodegroupInput, accountID string) (*eks.CreateNodegroupOutput, error) {
	return utils.NatsRequest[eks.CreateNodegroupOutput](ctx, s.natsConn, "eks.CreateNodegroup", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeNodegroup(ctx context.Context, input *eks.DescribeNodegroupInput, accountID string) (*eks.DescribeNodegroupOutput, error) {
	return utils.NatsRequest[eks.DescribeNodegroupOutput](ctx, s.natsConn, "eks.DescribeNodegroup", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListNodegroups(ctx context.Context, input *eks.ListNodegroupsInput, accountID string) (*eks.ListNodegroupsOutput, error) {
	return utils.NatsRequest[eks.ListNodegroupsOutput](ctx, s.natsConn, "eks.ListNodegroups", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateNodegroupConfig(ctx context.Context, input *eks.UpdateNodegroupConfigInput, accountID string) (*eks.UpdateNodegroupConfigOutput, error) {
	return utils.NatsRequest[eks.UpdateNodegroupConfigOutput](ctx, s.natsConn, "eks.UpdateNodegroupConfig", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateNodegroupVersion(ctx context.Context, input *eks.UpdateNodegroupVersionInput, accountID string) (*eks.UpdateNodegroupVersionOutput, error) {
	return utils.NatsRequest[eks.UpdateNodegroupVersionOutput](ctx, s.natsConn, "eks.UpdateNodegroupVersion", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DeleteNodegroup(ctx context.Context, input *eks.DeleteNodegroupInput, accountID string) (*eks.DeleteNodegroupOutput, error) {
	return utils.NatsRequest[eks.DeleteNodegroupOutput](ctx, s.natsConn, "eks.DeleteNodegroup", input, defaultTimeout, accountID)
}

// --- AccessEntry + AccessPolicy ---

func (s *NATSEKSService) CreateAccessEntry(ctx context.Context, input *eks.CreateAccessEntryInput, accountID string) (*eks.CreateAccessEntryOutput, error) {
	return utils.NatsRequest[eks.CreateAccessEntryOutput](ctx, s.natsConn, "eks.CreateAccessEntry", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeAccessEntry(ctx context.Context, input *eks.DescribeAccessEntryInput, accountID string) (*eks.DescribeAccessEntryOutput, error) {
	return utils.NatsRequest[eks.DescribeAccessEntryOutput](ctx, s.natsConn, "eks.DescribeAccessEntry", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListAccessEntries(ctx context.Context, input *eks.ListAccessEntriesInput, accountID string) (*eks.ListAccessEntriesOutput, error) {
	return utils.NatsRequest[eks.ListAccessEntriesOutput](ctx, s.natsConn, "eks.ListAccessEntries", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateAccessEntry(ctx context.Context, input *eks.UpdateAccessEntryInput, accountID string) (*eks.UpdateAccessEntryOutput, error) {
	return utils.NatsRequest[eks.UpdateAccessEntryOutput](ctx, s.natsConn, "eks.UpdateAccessEntry", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DeleteAccessEntry(ctx context.Context, input *eks.DeleteAccessEntryInput, accountID string) (*eks.DeleteAccessEntryOutput, error) {
	return utils.NatsRequest[eks.DeleteAccessEntryOutput](ctx, s.natsConn, "eks.DeleteAccessEntry", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) AssociateAccessPolicy(ctx context.Context, input *eks.AssociateAccessPolicyInput, accountID string) (*eks.AssociateAccessPolicyOutput, error) {
	return utils.NatsRequest[eks.AssociateAccessPolicyOutput](ctx, s.natsConn, "eks.AssociateAccessPolicy", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DisassociateAccessPolicy(ctx context.Context, input *eks.DisassociateAccessPolicyInput, accountID string) (*eks.DisassociateAccessPolicyOutput, error) {
	return utils.NatsRequest[eks.DisassociateAccessPolicyOutput](ctx, s.natsConn, "eks.DisassociateAccessPolicy", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListAssociatedAccessPolicies(ctx context.Context, input *eks.ListAssociatedAccessPoliciesInput, accountID string) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	return utils.NatsRequest[eks.ListAssociatedAccessPoliciesOutput](ctx, s.natsConn, "eks.ListAssociatedAccessPolicies", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListAccessPolicies(ctx context.Context, input *eks.ListAccessPoliciesInput, accountID string) (*eks.ListAccessPoliciesOutput, error) {
	return utils.NatsRequest[eks.ListAccessPoliciesOutput](ctx, s.natsConn, "eks.ListAccessPolicies", input, defaultTimeout, accountID)
}

// --- Addons ---

func (s *NATSEKSService) ListAddons(ctx context.Context, input *eks.ListAddonsInput, accountID string) (*eks.ListAddonsOutput, error) {
	return utils.NatsRequest[eks.ListAddonsOutput](ctx, s.natsConn, "eks.ListAddons", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeAddonVersions(ctx context.Context, input *eks.DescribeAddonVersionsInput, accountID string) (*eks.DescribeAddonVersionsOutput, error) {
	return utils.NatsRequest[eks.DescribeAddonVersionsOutput](ctx, s.natsConn, "eks.DescribeAddonVersions", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) CreateAddon(ctx context.Context, input *eks.CreateAddonInput, accountID string) (*eks.CreateAddonOutput, error) {
	return utils.NatsRequest[eks.CreateAddonOutput](ctx, s.natsConn, "eks.CreateAddon", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DeleteAddon(ctx context.Context, input *eks.DeleteAddonInput, accountID string) (*eks.DeleteAddonOutput, error) {
	return utils.NatsRequest[eks.DeleteAddonOutput](ctx, s.natsConn, "eks.DeleteAddon", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeAddon(ctx context.Context, input *eks.DescribeAddonInput, accountID string) (*eks.DescribeAddonOutput, error) {
	return utils.NatsRequest[eks.DescribeAddonOutput](ctx, s.natsConn, "eks.DescribeAddon", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListStagedAddonManifests(ctx context.Context, input *ListStagedAddonManifestsInput, accountID string) (*ListStagedAddonManifestsOutput, error) {
	return utils.NatsRequest[ListStagedAddonManifestsOutput](ctx, s.natsConn, "eks.ListStagedAddonManifests", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateAddon(ctx context.Context, input *eks.UpdateAddonInput, accountID string) (*eks.UpdateAddonOutput, error) {
	return utils.NatsRequest[eks.UpdateAddonOutput](ctx, s.natsConn, "eks.UpdateAddon", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) GetRecoveryDirective(ctx context.Context, input *GetRecoveryDirectiveInput, accountID string) (*GetRecoveryDirectiveOutput, error) {
	return utils.NatsRequest[GetRecoveryDirectiveOutput](ctx, s.natsConn, "eks.GetRecoveryDirective", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) SetRecoveryDirective(ctx context.Context, input *SetRecoveryDirectiveInput, accountID string) (*SetRecoveryDirectiveOutput, error) {
	return utils.NatsRequest[SetRecoveryDirectiveOutput](ctx, s.natsConn, "eks.SetRecoveryDirective", input, defaultTimeout, accountID)
}

// --- OIDC identity-provider configs ---

func (s *NATSEKSService) AssociateIdentityProviderConfig(ctx context.Context, input *eks.AssociateIdentityProviderConfigInput, accountID string) (*eks.AssociateIdentityProviderConfigOutput, error) {
	return utils.NatsRequest[eks.AssociateIdentityProviderConfigOutput](ctx, s.natsConn, "eks.AssociateIdentityProviderConfig", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeIdentityProviderConfig(ctx context.Context, input *eks.DescribeIdentityProviderConfigInput, accountID string) (*eks.DescribeIdentityProviderConfigOutput, error) {
	return utils.NatsRequest[eks.DescribeIdentityProviderConfigOutput](ctx, s.natsConn, "eks.DescribeIdentityProviderConfig", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListIdentityProviderConfigs(ctx context.Context, input *eks.ListIdentityProviderConfigsInput, accountID string) (*eks.ListIdentityProviderConfigsOutput, error) {
	return utils.NatsRequest[eks.ListIdentityProviderConfigsOutput](ctx, s.natsConn, "eks.ListIdentityProviderConfigs", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DisassociateIdentityProviderConfig(ctx context.Context, input *eks.DisassociateIdentityProviderConfigInput, accountID string) (*eks.DisassociateIdentityProviderConfigOutput, error) {
	return utils.NatsRequest[eks.DisassociateIdentityProviderConfigOutput](ctx, s.natsConn, "eks.DisassociateIdentityProviderConfig", input, defaultTimeout, accountID)
}

// --- Tags ---

func (s *NATSEKSService) TagResource(ctx context.Context, input *eks.TagResourceInput, accountID string) (*eks.TagResourceOutput, error) {
	return utils.NatsRequest[eks.TagResourceOutput](ctx, s.natsConn, "eks.TagResource", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UntagResource(ctx context.Context, input *eks.UntagResourceInput, accountID string) (*eks.UntagResourceOutput, error) {
	return utils.NatsRequest[eks.UntagResourceOutput](ctx, s.natsConn, "eks.UntagResource", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListTagsForResource(ctx context.Context, input *eks.ListTagsForResourceInput, accountID string) (*eks.ListTagsForResourceOutput, error) {
	return utils.NatsRequest[eks.ListTagsForResourceOutput](ctx, s.natsConn, "eks.ListTagsForResource", input, defaultTimeout, accountID)
}
