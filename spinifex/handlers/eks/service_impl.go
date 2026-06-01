package handlers_eks

import (
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/nats-io/nats.go"
)

// EKSServiceImpl is the daemon-side EKSService. All methods currently return
// NotImplemented; bodies land alongside the K3s control-plane work.
type EKSServiceImpl struct {
	config   *config.Config
	nc       *nats.Conn
	store    *Store
	leaderKV nats.KeyValue
}

var _ EKSService = (*EKSServiceImpl)(nil)

// NewEKSServiceImplWithNATS wires the Store handle and creates the shared
// spinifex-eks-leader bucket so the leader-lease CAS loop has a ready bucket
// to CAS against without a chicken-and-egg bootstrap.
func NewEKSServiceImplWithNATS(cfg *config.Config, nc *nats.Conn) (*EKSServiceImpl, error) {
	store, err := NewStore(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to create EKS store: %w", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}
	leaderKV, err := InitLeaderBucket(js)
	if err != nil {
		return nil, err
	}
	return &EKSServiceImpl{
		config:   cfg,
		nc:       nc,
		store:    store,
		leaderKV: leaderKV,
	}, nil
}

func notImpl() error { return errors.New(awserrors.ErrorNotImplemented) }

// --- Cluster ---

func (s *EKSServiceImpl) CreateCluster(_ *eks.CreateClusterInput, _ string) (*eks.CreateClusterOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeCluster(_ *eks.DescribeClusterInput, _ string) (*eks.DescribeClusterOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListClusters(_ *eks.ListClustersInput, _ string) (*eks.ListClustersOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateClusterConfig(_ *eks.UpdateClusterConfigInput, _ string) (*eks.UpdateClusterConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateClusterVersion(_ *eks.UpdateClusterVersionInput, _ string) (*eks.UpdateClusterVersionOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DeleteCluster(_ *eks.DeleteClusterInput, _ string) (*eks.DeleteClusterOutput, error) {
	return nil, notImpl()
}

// --- Nodegroup ---

func (s *EKSServiceImpl) CreateNodegroup(_ *eks.CreateNodegroupInput, _ string) (*eks.CreateNodegroupOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeNodegroup(_ *eks.DescribeNodegroupInput, _ string) (*eks.DescribeNodegroupOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListNodegroups(_ *eks.ListNodegroupsInput, _ string) (*eks.ListNodegroupsOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateNodegroupConfig(_ *eks.UpdateNodegroupConfigInput, _ string) (*eks.UpdateNodegroupConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateNodegroupVersion(_ *eks.UpdateNodegroupVersionInput, _ string) (*eks.UpdateNodegroupVersionOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DeleteNodegroup(_ *eks.DeleteNodegroupInput, _ string) (*eks.DeleteNodegroupOutput, error) {
	return nil, notImpl()
}

// --- AccessEntry + AccessPolicy ---

func (s *EKSServiceImpl) CreateAccessEntry(_ *eks.CreateAccessEntryInput, _ string) (*eks.CreateAccessEntryOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeAccessEntry(_ *eks.DescribeAccessEntryInput, _ string) (*eks.DescribeAccessEntryOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListAccessEntries(_ *eks.ListAccessEntriesInput, _ string) (*eks.ListAccessEntriesOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateAccessEntry(_ *eks.UpdateAccessEntryInput, _ string) (*eks.UpdateAccessEntryOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DeleteAccessEntry(_ *eks.DeleteAccessEntryInput, _ string) (*eks.DeleteAccessEntryOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) AssociateAccessPolicy(_ *eks.AssociateAccessPolicyInput, _ string) (*eks.AssociateAccessPolicyOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DisassociateAccessPolicy(_ *eks.DisassociateAccessPolicyInput, _ string) (*eks.DisassociateAccessPolicyOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListAssociatedAccessPolicies(_ *eks.ListAssociatedAccessPoliciesInput, _ string) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListAccessPolicies(_ *eks.ListAccessPoliciesInput, _ string) (*eks.ListAccessPoliciesOutput, error) {
	return nil, notImpl()
}

// --- Addons ---

func (s *EKSServiceImpl) ListAddons(_ *eks.ListAddonsInput, _ string) (*eks.ListAddonsOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeAddonVersions(_ *eks.DescribeAddonVersionsInput, _ string) (*eks.DescribeAddonVersionsOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) CreateAddon(_ *eks.CreateAddonInput, _ string) (*eks.CreateAddonOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DeleteAddon(_ *eks.DeleteAddonInput, _ string) (*eks.DeleteAddonOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeAddon(_ *eks.DescribeAddonInput, _ string) (*eks.DescribeAddonOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UpdateAddon(_ *eks.UpdateAddonInput, _ string) (*eks.UpdateAddonOutput, error) {
	return nil, notImpl()
}

// --- OIDC identity-provider configs ---

func (s *EKSServiceImpl) AssociateIdentityProviderConfig(_ *eks.AssociateIdentityProviderConfigInput, _ string) (*eks.AssociateIdentityProviderConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DescribeIdentityProviderConfig(_ *eks.DescribeIdentityProviderConfigInput, _ string) (*eks.DescribeIdentityProviderConfigOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListIdentityProviderConfigs(_ *eks.ListIdentityProviderConfigsInput, _ string) (*eks.ListIdentityProviderConfigsOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) DisassociateIdentityProviderConfig(_ *eks.DisassociateIdentityProviderConfigInput, _ string) (*eks.DisassociateIdentityProviderConfigOutput, error) {
	return nil, notImpl()
}

// --- Tags ---

func (s *EKSServiceImpl) TagResource(_ *eks.TagResourceInput, _ string) (*eks.TagResourceOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) UntagResource(_ *eks.UntagResourceInput, _ string) (*eks.UntagResourceOutput, error) {
	return nil, notImpl()
}

func (s *EKSServiceImpl) ListTagsForResource(_ *eks.ListTagsForResourceInput, _ string) (*eks.ListTagsForResourceOutput, error) {
	return nil, notImpl()
}
