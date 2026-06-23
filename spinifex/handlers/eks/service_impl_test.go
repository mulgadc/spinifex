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

	_, err := svc.CreateCluster(&eks.CreateClusterInput{Name: aws.String("c1")}, testAccountID, "")
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

// The API_AND_CONFIG_MAP hybrid still enables the unsupported aws-auth ConfigMap
// path, so it must be rejected the same as plain CONFIG_MAP — the sibling test
// only covers the CONFIG_MAP value.
func TestValidateCreateClusterInput_RejectsAPIAndConfigMapAuthMode(t *testing.T) {
	in := createInput("alpha")
	in.AccessConfig = &eks.CreateAccessConfigRequest{
		AuthenticationMode: aws.String(eks.AuthenticationModeApiAndConfigMap),
	}
	require.EqualError(t, validateCreateClusterInput(in), awserrors.ErrorInvalidParameter)
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

// DeleteCluster on an absent cluster must reach the KV lookup with full deps
// wired (the shim path short-circuits to ServiceUnavailable before the meta
// read) and return idempotent success (Common Resource Lifecycle Contract #1),
// not a teardown of nothing — so a tofu destroy retry converges.
func TestDeleteCluster_NotFoundWithFullDeps(t *testing.T) {
	f := newEKSServiceFixture(t)

	out, err := f.svc.DeleteCluster(deleteInput("ghost"), testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Empty(t, f.inst.terminateCalls, "absent cluster must trigger no teardown")
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

// In shim mode (orchestration deps absent) the mutating nodegroup methods
// short-circuit to ServiceUnavailable, the read methods reach an empty
// per-account bucket and surface ResourceNotFoundException, and
// UpdateNodegroupVersion stays NotImplemented (v1 doesn't do AMI upgrades).
func TestEKSServiceImpl_NodegroupMethodsShimMode(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateNodegroup(&eks.CreateNodegroupInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)

	_, err = svc.DescribeNodegroup(&eks.DescribeNodegroupInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)

	_, err = svc.ListNodegroups(&eks.ListNodegroupsInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)

	_, err = svc.UpdateNodegroupConfig(&eks.UpdateNodegroupConfigInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)

	_, err = svc.UpdateNodegroupVersion(&eks.UpdateNodegroupVersionInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DeleteNodegroup(&eks.DeleteNodegroupInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)
}

// seedTestCluster lays down a minimal ACTIVE cluster meta in the per-account
// bucket so the AccessEntry handlers (which gate on cluster existence) can run.
func seedTestCluster(t *testing.T, svc *EKSServiceImpl, cluster string) {
	t.Helper()
	js, err := svc.deps.NATSConn.JetStream()
	require.NoError(t, err)
	kv, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)
	require.NoError(t, PutClusterMeta(kv, &ClusterMeta{Name: cluster, Status: ClusterStatusActive}))
}

const testPrincipalARN = "arn:aws:iam::111122223333:role/dev"

func TestAccessEntry_UnknownClusterIsNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateAccessEntry(&eks.CreateAccessEntryInput{
		ClusterName: aws.String("missing"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestAccessEntry_CreateDescribeListDelete(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")

	out, err := svc.CreateAccessEntry(&eks.CreateAccessEntryInput{
		ClusterName:      aws.String("c1"),
		PrincipalArn:     aws.String(testPrincipalARN),
		KubernetesGroups: aws.StringSlice([]string{"system:masters"}),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.AccessEntry)
	// Username defaults to the principal ARN; type defaults to STANDARD.
	assert.Equal(t, testPrincipalARN, aws.StringValue(out.AccessEntry.Username))
	assert.Equal(t, AccessEntryTypeStandard, aws.StringValue(out.AccessEntry.Type))
	assert.Contains(t, aws.StringValue(out.AccessEntry.AccessEntryArn), ":access-entry/c1/")

	// Duplicate Create → ResourceInUseException.
	_, err = svc.CreateAccessEntry(&eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceInUse)

	desc, err := svc.DescribeAccessEntry(&eks.DescribeAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"system:masters"}, aws.StringValueSlice(desc.AccessEntry.KubernetesGroups))

	list, err := svc.ListAccessEntries(&eks.ListAccessEntriesInput{ClusterName: aws.String("c1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{testPrincipalARN}, aws.StringValueSlice(list.AccessEntries))

	_, err = svc.DeleteAccessEntry(&eks.DeleteAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DescribeAccessEntry(&eks.DescribeAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestAccessEntry_RejectsNodeType(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.CreateAccessEntry(&eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN), Type: aws.String("EC2_LINUX"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestAccessEntry_DescribeDeleteMissingIsNotFound(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.DescribeAccessEntry(&eks.DescribeAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
	_, err = svc.DeleteAccessEntry(&eks.DeleteAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestAccessPolicy_AssociateListDisassociate(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.CreateAccessEntry(&eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)

	const viewPolicy = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
	assoc, err := svc.AssociateAccessPolicy(&eks.AssociateAccessPolicyInput{
		ClusterName:  aws.String("c1"),
		PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:    aws.String(viewPolicy),
		AccessScope:  &eks.AccessScope{Type: aws.String("cluster")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, viewPolicy, aws.StringValue(assoc.AssociatedAccessPolicy.PolicyArn))

	listed, err := svc.ListAssociatedAccessPolicies(&eks.ListAssociatedAccessPoliciesInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.AssociatedAccessPolicies, 1)

	// Entry now discoverable by the associated-policy filter.
	filtered, err := svc.ListAccessEntries(&eks.ListAccessEntriesInput{
		ClusterName: aws.String("c1"), AssociatedPolicyArn: aws.String(viewPolicy),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{testPrincipalARN}, aws.StringValueSlice(filtered.AccessEntries))

	_, err = svc.DisassociateAccessPolicy(&eks.DisassociateAccessPolicyInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN), PolicyArn: aws.String(viewPolicy),
	}, testAccountID)
	require.NoError(t, err)
	listed, err = svc.ListAssociatedAccessPolicies(&eks.ListAssociatedAccessPoliciesInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, listed.AssociatedAccessPolicies)
}

func TestAccessPolicy_AssociateRejectsUnsupportedPolicyAndScope(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.CreateAccessEntry(&eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)

	// Unsupported policy ARN.
	_, err = svc.AssociateAccessPolicy(&eks.AssociateAccessPolicyInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:   aws.String("arn:aws:eks::aws:cluster-access-policy/MadeUpPolicy"),
		AccessScope: &eks.AccessScope{Type: aws.String("cluster")},
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	// namespace scope without namespaces.
	_, err = svc.AssociateAccessPolicy(&eks.AssociateAccessPolicyInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:   aws.String("arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"),
		AccessScope: &eks.AccessScope{Type: aws.String("namespace")},
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestListAccessPolicies_ReturnsSupportedCatalogue(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.ListAccessPolicies(&eks.ListAccessPoliciesInput{}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.AccessPolicies, len(supportedAccessPolicies))
	for _, p := range out.AccessPolicies {
		_, ok := supportedAccessPolicies[aws.StringValue(p.Arn)]
		assert.True(t, ok, "unexpected policy %s", aws.StringValue(p.Arn))
		assert.NotEmpty(t, aws.StringValue(p.Name))
	}
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

func TestEKSServiceImpl_ClusterTagRoundTrip(t *testing.T) {
	svc := setupTestService(t)
	js, err := svc.deps.NATSConn.JetStream()
	require.NoError(t, err)
	kv, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)

	const arn = "arn:aws:eks:us-east-1:111122223333:cluster/c1"
	require.NoError(t, PutClusterMeta(kv, &ClusterMeta{
		Name:   "c1",
		Status: ClusterStatusActive,
		Tags:   map[string]string{"env": "prod"},
	}))

	// Create-time tags are echoed (the drift-killing round-trip).
	lt, err := svc.ListTagsForResource(&eks.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "prod", aws.StringValue(lt.Tags["env"]))

	dc, err := svc.DescribeCluster(&eks.DescribeClusterInput{Name: aws.String("c1")}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "prod", aws.StringValue(dc.Cluster.Tags["env"]))

	// TagResource merges, UntagResource removes — both store-only.
	_, err = svc.TagResource(&eks.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags:        aws.StringMap(map[string]string{"team": "platform"}),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.UntagResource(&eks.UntagResourceInput{
		ResourceArn: aws.String(arn),
		TagKeys:     aws.StringSlice([]string{"env"}),
	}, testAccountID)
	require.NoError(t, err)

	lt, err = svc.ListTagsForResource(&eks.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "platform", aws.StringValue(lt.Tags["team"]))
	_, hasEnv := lt.Tags["env"]
	require.False(t, hasEnv)
}

func TestEKSServiceImpl_NodegroupTagRoundTrip(t *testing.T) {
	svc := setupTestService(t)
	js, err := svc.deps.NATSConn.JetStream()
	require.NoError(t, err)
	kv, err := GetOrCreateAccountBucket(js, testAccountID)
	require.NoError(t, err)

	const arn = "arn:aws:eks:us-east-1:111122223333:nodegroup/c1/ng1/abc123"
	require.NoError(t, PutNodegroupRecord(kv, &NodegroupRecord{
		ClusterName: "c1",
		Name:        "ng1",
		Arn:         arn,
		Status:      eks.NodegroupStatusActive,
		Tags:        map[string]string{"env": "prod"},
	}))

	// Create-time tags are echoed (the drift-killing round-trip).
	lt, err := svc.ListTagsForResource(&eks.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "prod", aws.StringValue(lt.Tags["env"]))

	dn, err := svc.DescribeNodegroup(&eks.DescribeNodegroupInput{
		ClusterName:   aws.String("c1"),
		NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "prod", aws.StringValue(dn.Nodegroup.Tags["env"]))

	// TagResource merges, UntagResource removes — both store-only.
	_, err = svc.TagResource(&eks.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags:        aws.StringMap(map[string]string{"team": "platform"}),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.UntagResource(&eks.UntagResourceInput{
		ResourceArn: aws.String(arn),
		TagKeys:     aws.StringSlice([]string{"env"}),
	}, testAccountID)
	require.NoError(t, err)

	lt, err = svc.ListTagsForResource(&eks.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "platform", aws.StringValue(lt.Tags["team"]))
	_, hasEnv := lt.Tags["env"]
	require.False(t, hasEnv)
}

func TestEKSServiceImpl_NodegroupTagMissingNotFound(t *testing.T) {
	svc := setupTestService(t)
	const arn = "arn:aws:eks:us-east-1:111122223333:nodegroup/c1/absent/abc123"

	_, err := svc.ListTagsForResource(&eks.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)

	_, err = svc.TagResource(&eks.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags:        aws.StringMap(map[string]string{"k": "v"}),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)

	_, err = svc.UntagResource(&eks.UntagResourceInput{
		ResourceArn: aws.String(arn),
		TagKeys:     aws.StringSlice([]string{"k"}),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestEKSServiceImpl_TagsUnsupportedARNNotImplemented(t *testing.T) {
	svc := setupTestService(t)
	const fpARN = "arn:aws:eks:us-east-1:111122223333:fargateprofile/c1/fp1/abc123"

	_, err := svc.TagResource(&eks.TagResourceInput{ResourceArn: aws.String(fpARN)}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.UntagResource(&eks.UntagResourceInput{ResourceArn: aws.String(fpARN)}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.ListTagsForResource(&eks.ListTagsForResourceInput{ResourceArn: aws.String(fpARN)}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}
