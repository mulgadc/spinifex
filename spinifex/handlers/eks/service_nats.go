package handlers_eks

import (
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

func (s *NATSEKSService) CreateCluster(input *eks.CreateClusterInput, accountID string) (*eks.CreateClusterOutput, error) {
	return utils.NATSRequest[eks.CreateClusterOutput](s.natsConn, "eks.CreateCluster", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeCluster(input *eks.DescribeClusterInput, accountID string) (*eks.DescribeClusterOutput, error) {
	return utils.NATSRequest[eks.DescribeClusterOutput](s.natsConn, "eks.DescribeCluster", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListClusters(input *eks.ListClustersInput, accountID string) (*eks.ListClustersOutput, error) {
	return utils.NATSRequest[eks.ListClustersOutput](s.natsConn, "eks.ListClusters", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateClusterConfig(input *eks.UpdateClusterConfigInput, accountID string) (*eks.UpdateClusterConfigOutput, error) {
	return utils.NATSRequest[eks.UpdateClusterConfigOutput](s.natsConn, "eks.UpdateClusterConfig", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateClusterVersion(input *eks.UpdateClusterVersionInput, accountID string) (*eks.UpdateClusterVersionOutput, error) {
	return utils.NATSRequest[eks.UpdateClusterVersionOutput](s.natsConn, "eks.UpdateClusterVersion", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DeleteCluster(input *eks.DeleteClusterInput, accountID string) (*eks.DeleteClusterOutput, error) {
	return utils.NATSRequest[eks.DeleteClusterOutput](s.natsConn, "eks.DeleteCluster", input, defaultTimeout, accountID)
}

// --- Nodegroup ---

func (s *NATSEKSService) CreateNodegroup(input *eks.CreateNodegroupInput, accountID string) (*eks.CreateNodegroupOutput, error) {
	return utils.NATSRequest[eks.CreateNodegroupOutput](s.natsConn, "eks.CreateNodegroup", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeNodegroup(input *eks.DescribeNodegroupInput, accountID string) (*eks.DescribeNodegroupOutput, error) {
	return utils.NATSRequest[eks.DescribeNodegroupOutput](s.natsConn, "eks.DescribeNodegroup", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListNodegroups(input *eks.ListNodegroupsInput, accountID string) (*eks.ListNodegroupsOutput, error) {
	return utils.NATSRequest[eks.ListNodegroupsOutput](s.natsConn, "eks.ListNodegroups", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateNodegroupConfig(input *eks.UpdateNodegroupConfigInput, accountID string) (*eks.UpdateNodegroupConfigOutput, error) {
	return utils.NATSRequest[eks.UpdateNodegroupConfigOutput](s.natsConn, "eks.UpdateNodegroupConfig", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateNodegroupVersion(input *eks.UpdateNodegroupVersionInput, accountID string) (*eks.UpdateNodegroupVersionOutput, error) {
	return utils.NATSRequest[eks.UpdateNodegroupVersionOutput](s.natsConn, "eks.UpdateNodegroupVersion", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DeleteNodegroup(input *eks.DeleteNodegroupInput, accountID string) (*eks.DeleteNodegroupOutput, error) {
	return utils.NATSRequest[eks.DeleteNodegroupOutput](s.natsConn, "eks.DeleteNodegroup", input, defaultTimeout, accountID)
}

// --- AccessEntry + AccessPolicy ---

func (s *NATSEKSService) CreateAccessEntry(input *eks.CreateAccessEntryInput, accountID string) (*eks.CreateAccessEntryOutput, error) {
	return utils.NATSRequest[eks.CreateAccessEntryOutput](s.natsConn, "eks.CreateAccessEntry", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeAccessEntry(input *eks.DescribeAccessEntryInput, accountID string) (*eks.DescribeAccessEntryOutput, error) {
	return utils.NATSRequest[eks.DescribeAccessEntryOutput](s.natsConn, "eks.DescribeAccessEntry", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListAccessEntries(input *eks.ListAccessEntriesInput, accountID string) (*eks.ListAccessEntriesOutput, error) {
	return utils.NATSRequest[eks.ListAccessEntriesOutput](s.natsConn, "eks.ListAccessEntries", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateAccessEntry(input *eks.UpdateAccessEntryInput, accountID string) (*eks.UpdateAccessEntryOutput, error) {
	return utils.NATSRequest[eks.UpdateAccessEntryOutput](s.natsConn, "eks.UpdateAccessEntry", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DeleteAccessEntry(input *eks.DeleteAccessEntryInput, accountID string) (*eks.DeleteAccessEntryOutput, error) {
	return utils.NATSRequest[eks.DeleteAccessEntryOutput](s.natsConn, "eks.DeleteAccessEntry", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) AssociateAccessPolicy(input *eks.AssociateAccessPolicyInput, accountID string) (*eks.AssociateAccessPolicyOutput, error) {
	return utils.NATSRequest[eks.AssociateAccessPolicyOutput](s.natsConn, "eks.AssociateAccessPolicy", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DisassociateAccessPolicy(input *eks.DisassociateAccessPolicyInput, accountID string) (*eks.DisassociateAccessPolicyOutput, error) {
	return utils.NATSRequest[eks.DisassociateAccessPolicyOutput](s.natsConn, "eks.DisassociateAccessPolicy", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListAssociatedAccessPolicies(input *eks.ListAssociatedAccessPoliciesInput, accountID string) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	return utils.NATSRequest[eks.ListAssociatedAccessPoliciesOutput](s.natsConn, "eks.ListAssociatedAccessPolicies", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListAccessPolicies(input *eks.ListAccessPoliciesInput, accountID string) (*eks.ListAccessPoliciesOutput, error) {
	return utils.NATSRequest[eks.ListAccessPoliciesOutput](s.natsConn, "eks.ListAccessPolicies", input, defaultTimeout, accountID)
}

// --- Addons ---

func (s *NATSEKSService) ListAddons(input *eks.ListAddonsInput, accountID string) (*eks.ListAddonsOutput, error) {
	return utils.NATSRequest[eks.ListAddonsOutput](s.natsConn, "eks.ListAddons", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeAddonVersions(input *eks.DescribeAddonVersionsInput, accountID string) (*eks.DescribeAddonVersionsOutput, error) {
	return utils.NATSRequest[eks.DescribeAddonVersionsOutput](s.natsConn, "eks.DescribeAddonVersions", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) CreateAddon(input *eks.CreateAddonInput, accountID string) (*eks.CreateAddonOutput, error) {
	return utils.NATSRequest[eks.CreateAddonOutput](s.natsConn, "eks.CreateAddon", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DeleteAddon(input *eks.DeleteAddonInput, accountID string) (*eks.DeleteAddonOutput, error) {
	return utils.NATSRequest[eks.DeleteAddonOutput](s.natsConn, "eks.DeleteAddon", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeAddon(input *eks.DescribeAddonInput, accountID string) (*eks.DescribeAddonOutput, error) {
	return utils.NATSRequest[eks.DescribeAddonOutput](s.natsConn, "eks.DescribeAddon", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UpdateAddon(input *eks.UpdateAddonInput, accountID string) (*eks.UpdateAddonOutput, error) {
	return utils.NATSRequest[eks.UpdateAddonOutput](s.natsConn, "eks.UpdateAddon", input, defaultTimeout, accountID)
}

// --- OIDC identity-provider configs ---

func (s *NATSEKSService) AssociateIdentityProviderConfig(input *eks.AssociateIdentityProviderConfigInput, accountID string) (*eks.AssociateIdentityProviderConfigOutput, error) {
	return utils.NATSRequest[eks.AssociateIdentityProviderConfigOutput](s.natsConn, "eks.AssociateIdentityProviderConfig", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DescribeIdentityProviderConfig(input *eks.DescribeIdentityProviderConfigInput, accountID string) (*eks.DescribeIdentityProviderConfigOutput, error) {
	return utils.NATSRequest[eks.DescribeIdentityProviderConfigOutput](s.natsConn, "eks.DescribeIdentityProviderConfig", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListIdentityProviderConfigs(input *eks.ListIdentityProviderConfigsInput, accountID string) (*eks.ListIdentityProviderConfigsOutput, error) {
	return utils.NATSRequest[eks.ListIdentityProviderConfigsOutput](s.natsConn, "eks.ListIdentityProviderConfigs", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) DisassociateIdentityProviderConfig(input *eks.DisassociateIdentityProviderConfigInput, accountID string) (*eks.DisassociateIdentityProviderConfigOutput, error) {
	return utils.NATSRequest[eks.DisassociateIdentityProviderConfigOutput](s.natsConn, "eks.DisassociateIdentityProviderConfig", input, defaultTimeout, accountID)
}

// --- Tags ---

func (s *NATSEKSService) TagResource(input *eks.TagResourceInput, accountID string) (*eks.TagResourceOutput, error) {
	return utils.NATSRequest[eks.TagResourceOutput](s.natsConn, "eks.TagResource", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) UntagResource(input *eks.UntagResourceInput, accountID string) (*eks.UntagResourceOutput, error) {
	return utils.NATSRequest[eks.UntagResourceOutput](s.natsConn, "eks.UntagResource", input, defaultTimeout, accountID)
}

func (s *NATSEKSService) ListTagsForResource(input *eks.ListTagsForResourceInput, accountID string) (*eks.ListTagsForResourceOutput, error) {
	return utils.NATSRequest[eks.ListTagsForResourceOutput](s.natsConn, "eks.ListTagsForResource", input, defaultTimeout, accountID)
}
