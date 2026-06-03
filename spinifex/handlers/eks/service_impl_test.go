package handlers_eks

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestService(t *testing.T) *EKSServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := NewEKSServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	return svc
}

// TestEKSServiceImpl_ClusterLifecycleShimMode covers the four lifecycle
// methods when the service is constructed via NewEKSServiceImplWithNATS
// (the shim path the daemon-handler routing test uses): orchestration deps
// are absent so CreateCluster/DeleteCluster short-circuit to ServiceUnavailable
// (the missing deps are logged at ERROR), DescribeCluster hits an empty
// per-account bucket and surfaces ResourceNotFoundException, and ListClusters
// returns an empty list. The UpdateClusterConfig + UpdateClusterVersion paths
// stay NotImplemented.
func TestEKSServiceImpl_ClusterLifecycleShimMode(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateCluster(&eks.CreateClusterInput{Name: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)

	_, err = svc.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)

	out, err := svc.ListClusters(&eks.ListClustersInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Empty(t, out.Clusters)

	_, err = svc.UpdateClusterConfig(&eks.UpdateClusterConfigInput{Name: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.UpdateClusterVersion(&eks.UpdateClusterVersionInput{Name: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DeleteCluster(&eks.DeleteClusterInput{Name: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)
}

// EKS only supports the API authentication mode here; CONFIG_MAP (and the
// API_AND_CONFIG_MAP hybrid) must be rejected with InvalidParameterException.
func TestValidateCreateClusterInput_RejectsConfigMapAuthMode(t *testing.T) {
	in := createInput("alpha")
	in.AccessConfig = &eks.CreateAccessConfigRequest{
		AuthenticationMode: aws.String(eks.AuthenticationModeConfigMap),
	}
	err := validateCreateClusterInput(in)
	require.EqualError(t, err, awserrors.ErrorInvalidParameter)
}

func TestValidateCreateClusterInput_AcceptsAPIAuthMode(t *testing.T) {
	in := createInput("alpha")
	in.AccessConfig = &eks.CreateAccessConfigRequest{
		AuthenticationMode: aws.String(eks.AuthenticationModeApi),
	}
	require.NoError(t, validateCreateClusterInput(in))
}

// A create request with no subnets is malformed and must be rejected before any
// orchestration work (InvalidParameterValue → 400).
func TestValidateCreateClusterInput_RejectsMissingSubnetIds(t *testing.T) {
	in := createInput("alpha")
	in.ResourcesVpcConfig = &eks.VpcConfigRequest{}
	require.EqualError(t, validateCreateClusterInput(in), awserrors.ErrorInvalidParameterValue)

	in.ResourcesVpcConfig = nil
	require.EqualError(t, validateCreateClusterInput(in), awserrors.ErrorInvalidParameterValue)
}

// DescribeCluster on an absent cluster must reach the KV lookup (full deps
// wired, unlike the shim short-circuit) and surface ResourceNotFoundException.
func TestDescribeCluster_NotFoundWithFullDeps(t *testing.T) {
	f := newEKSServiceFixture(t)

	_, err := f.svc.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String("ghost")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

// Security guarantee: the OIDC signing key must be zeroized BEFORE infra
// teardown, so a teardown failure (which leaves the meta for retry) can never
// leave recoverable key material behind. Force VM terminate to fail and assert
// the key is already gone while the meta survives.
func TestDeleteCluster_ZeroizesOIDCKeyBeforeTeardown(t *testing.T) {
	f := newDeleteClusterFixture(t, "alpha")
	f.inst.terminateErr = errors.New("hypervisor unreachable")

	_, err := f.svc.DeleteCluster(deleteInput("alpha"), testAccountID)
	require.Error(t, err, "teardown failure must surface")

	_, getErr := f.kv.Get(OIDCSigningKeyKey("alpha"))
	assert.ErrorIs(t, getErr, nats.ErrKeyNotFound, "OIDC key must be zeroized before teardown, even when teardown fails")

	meta, metaErr := GetClusterMeta(f.kv, "alpha")
	require.NoError(t, metaErr, "meta must survive a failed teardown for retry")
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
}

func TestEKSServiceImpl_NodegroupMethodsReturnNotImplemented(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateNodegroup(&eks.CreateNodegroupInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DescribeNodegroup(&eks.DescribeNodegroupInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.ListNodegroups(&eks.ListNodegroupsInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.UpdateNodegroupConfig(&eks.UpdateNodegroupConfigInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.UpdateNodegroupVersion(&eks.UpdateNodegroupVersionInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DeleteNodegroup(&eks.DeleteNodegroupInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}

func TestEKSServiceImpl_AccessMethodsReturnNotImplemented(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateAccessEntry(&eks.CreateAccessEntryInput{ClusterName: aws.String("c1"), PrincipalArn: aws.String("arn:aws:iam::111122223333:user/dev")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DescribeAccessEntry(&eks.DescribeAccessEntryInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.ListAccessEntries(&eks.ListAccessEntriesInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.UpdateAccessEntry(&eks.UpdateAccessEntryInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DeleteAccessEntry(&eks.DeleteAccessEntryInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.AssociateAccessPolicy(&eks.AssociateAccessPolicyInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DisassociateAccessPolicy(&eks.DisassociateAccessPolicyInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.ListAssociatedAccessPolicies(&eks.ListAssociatedAccessPoliciesInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.ListAccessPolicies(&eks.ListAccessPoliciesInput{}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}

func TestEKSServiceImpl_AddonsMethodsReturnNotImplemented(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.ListAddons(&eks.ListAddonsInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DescribeAddonVersions(&eks.DescribeAddonVersionsInput{}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.CreateAddon(&eks.CreateAddonInput{ClusterName: aws.String("c1"), AddonName: aws.String("vpc-cni")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DeleteAddon(&eks.DeleteAddonInput{ClusterName: aws.String("c1"), AddonName: aws.String("vpc-cni")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DescribeAddon(&eks.DescribeAddonInput{ClusterName: aws.String("c1"), AddonName: aws.String("vpc-cni")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.UpdateAddon(&eks.UpdateAddonInput{ClusterName: aws.String("c1"), AddonName: aws.String("vpc-cni")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}

func TestEKSServiceImpl_OIDCMethodsReturnNotImplemented(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.AssociateIdentityProviderConfig(&eks.AssociateIdentityProviderConfigInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DescribeIdentityProviderConfig(&eks.DescribeIdentityProviderConfigInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.ListIdentityProviderConfigs(&eks.ListIdentityProviderConfigsInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DisassociateIdentityProviderConfig(&eks.DisassociateIdentityProviderConfigInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}

func TestEKSServiceImpl_TagsMethodsReturnNotImplemented(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.TagResource(&eks.TagResourceInput{ResourceArn: aws.String("arn:aws:eks:us-east-1:111122223333:cluster/c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.UntagResource(&eks.UntagResourceInput{ResourceArn: aws.String("arn:aws:eks:us-east-1:111122223333:cluster/c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.ListTagsForResource(&eks.ListTagsForResourceInput{ResourceArn: aws.String("arn:aws:eks:us-east-1:111122223333:cluster/c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}
