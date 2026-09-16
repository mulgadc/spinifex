package handlers_eks

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestService(t *testing.T) *EKSServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := NewEKSServiceImpl(EKSServiceDeps{NATSConn: nc})
	require.NoError(t, err)
	return svc
}

// TestEKSServiceImpl_ClusterLifecycleShimMode covers the four lifecycle
// methods when the service is constructed with only NATS wiring (the path the
// daemon-handler routing test uses): orchestration deps
// are absent so CreateCluster/DeleteCluster short-circuit to ServiceUnavailable
// (the missing deps are logged at ERROR), DescribeCluster hits an empty
// per-account bucket and surfaces ResourceNotFoundException, and ListClusters
// returns an empty list. The UpdateClusterConfig + UpdateClusterVersion paths
// stay NotImplemented.
func TestEKSServiceImpl_ClusterLifecycleShimMode(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateCluster(context.Background(), &eks.CreateClusterInput{Name: aws.String("c1")}, testAccountID, "")
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)

	_, err = svc.DescribeCluster(context.Background(), &eks.DescribeClusterInput{Name: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)

	out, err := svc.ListClusters(context.Background(), &eks.ListClustersInput{}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Empty(t, out.Clusters)

	_, err = svc.UpdateClusterConfig(context.Background(), &eks.UpdateClusterConfigInput{Name: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.UpdateClusterVersion(context.Background(), &eks.UpdateClusterVersionInput{Name: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DeleteCluster(context.Background(), &eks.DeleteClusterInput{Name: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)
}

// A daemon that loaded its master key but whose IAM service was not ready at
// boot (KV quorum still forming) must reject nodegroup orchestration rather than
// launch workers with no instance profile, which leaves IMDS roleless and blocks
// ALB creation. The gate reports IAM even though MasterKey is present.
func TestMissingOrchestrationDeps_NilIAMRejectsNodegroup(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.svc.deps.IAM = nil

	require.Equal(t, []string{"IAM"}, f.svc.missingOrchestrationDeps())

	_, err := f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 1), testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)
}

// Production wires the lazy IAMProvider and leaves deps.IAM nil. The gate must
// resolve the ensurer (provider included), not read deps.IAM directly, or it
// rejects every CreateCluster/CreateNodegroup with missing=[IAM] forever even
// though the IAM service is fully available via the provider.
func TestMissingOrchestrationDeps_IAMProviderSatisfiesGate(t *testing.T) {
	f := newEKSServiceFixture(t)
	f.svc.deps.IAM = nil
	f.svc.deps.IAMProvider = func() handlers_iam.SystemInstanceRoleEnsurer { return newFakeEnsurer() }

	require.Empty(t, f.svc.missingOrchestrationDeps())
}

// Bare CONFIG_MAP consults only the aws-auth ConfigMap, which Spinifex has no
// path for, so a cluster in that mode would have no working authentication at
// all. The refusal must name accessConfig.authenticationMode so a Terraform
// caller sees why, not a bare code.
func TestValidateCreateClusterInput_RejectsConfigMapAuthMode(t *testing.T) {
	in := createInput("alpha")
	in.AccessConfig = &eks.CreateAccessConfigRequest{
		AuthenticationMode: aws.String(eks.AuthenticationModeConfigMap),
	}
	err := validateCreateClusterInput(in)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameter, code)
	assert.Contains(t, message, "accessConfig.authenticationMode")
}

// A junk authentication mode must be rejected the same way as CONFIG_MAP, and
// must also name the parameter rather than surfacing a bare code.
func TestValidateCreateClusterInput_RejectsJunkAuthMode(t *testing.T) {
	in := createInput("alpha")
	in.AccessConfig = &eks.CreateAccessConfigRequest{
		AuthenticationMode: aws.String("NOT_A_REAL_MODE"),
	}
	err := validateCreateClusterInput(in)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorInvalidParameter, code)
	assert.Contains(t, message, "accessConfig.authenticationMode")
}

// API_AND_CONFIG_MAP is both AWS's default since 1.23 and the terraform-aws-eks
// module default. Its ConfigMap side grants nothing on Spinifex, exactly as it
// would on real AWS with an empty aws-auth ConfigMap, so it must be accepted.
func TestValidateCreateClusterInput_AcceptsAPIAndConfigMapAuthMode(t *testing.T) {
	in := createInput("alpha")
	in.AccessConfig = &eks.CreateAccessConfigRequest{
		AuthenticationMode: aws.String(eks.AuthenticationModeApiAndConfigMap),
	}
	require.NoError(t, validateCreateClusterInput(in))
}

func TestValidateCreateClusterInput_AcceptsAPIAuthMode(t *testing.T) {
	in := createInput("alpha")
	in.AccessConfig = &eks.CreateAccessConfigRequest{
		AuthenticationMode: aws.String(eks.AuthenticationModeApi),
	}
	require.NoError(t, validateCreateClusterInput(in))
}

// An unset accessConfig (or an unset authenticationMode within it) must stay
// valid — it defaults to API, exactly as an explicit "API" does.
func TestValidateCreateClusterInput_AcceptsUnsetAuthMode(t *testing.T) {
	in := createInput("alpha")
	require.NoError(t, validateCreateClusterInput(in))

	in.AccessConfig = &eks.CreateAccessConfigRequest{}
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

	_, err := f.svc.DescribeCluster(context.Background(), &eks.DescribeClusterInput{Name: aws.String("ghost")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

// DeleteCluster on an absent cluster must reach the KV lookup with full deps
// wired (the shim path short-circuits to ServiceUnavailable before the meta
// read) and return idempotent success (Common Resource Lifecycle Contract #1),
// not a teardown of nothing — so a tofu destroy retry converges.
func TestDeleteCluster_NotFoundWithFullDeps(t *testing.T) {
	f := newEKSServiceFixture(t)

	out, err := f.svc.DeleteCluster(context.Background(), deleteInput("ghost"), testAccountID)
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

	_, err := f.svc.DeleteCluster(context.Background(), deleteInput("alpha"), testAccountID)
	require.Error(t, err, "teardown failure must surface")

	_, getErr := f.kv.Get(t.Context(), OIDCSigningKeyKey("alpha"))
	assert.ErrorIs(t, getErr, jetstream.ErrKeyNotFound, "OIDC key must be zeroized before teardown, even when teardown fails")

	meta, metaErr := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, metaErr, "meta must survive a failed teardown for retry")
	assert.Equal(t, ClusterStatusDeleting, meta.Status)
}

// In shim mode (orchestration deps absent) the mutating nodegroup methods
// short-circuit to ServiceUnavailable, the read methods reach an empty
// per-account bucket and surface ResourceNotFoundException, and
// UpdateNodegroupVersion stays NotImplemented (v1 doesn't do AMI upgrades).
func TestEKSServiceImpl_NodegroupMethodsShimMode(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateNodegroup(context.Background(), &eks.CreateNodegroupInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)

	_, err = svc.DescribeNodegroup(context.Background(), &eks.DescribeNodegroupInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)

	_, err = svc.ListNodegroups(context.Background(), &eks.ListNodegroupsInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)

	_, err = svc.UpdateNodegroupConfig(context.Background(), &eks.UpdateNodegroupConfigInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)

	_, err = svc.UpdateNodegroupVersion(context.Background(), &eks.UpdateNodegroupVersionInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DeleteNodegroup(context.Background(), &eks.DeleteNodegroupInput{ClusterName: aws.String("c1"), NodegroupName: aws.String("ng1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorServiceUnavailable)
}

// seedTestCluster lays down a minimal ACTIVE cluster meta in the per-account
// bucket so the AccessEntry handlers (which gate on cluster existence) can run.
func seedTestCluster(t *testing.T, svc *EKSServiceImpl, cluster string) {
	t.Helper()
	js := testutil.NewJetStream(t, svc.deps.NATSConn)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID, 1)
	require.NoError(t, err)
	require.NoError(t, PutClusterMeta(t.Context(), kv, &ClusterMeta{Name: cluster, Status: ClusterStatusActive}))
}

const testPrincipalARN = "arn:aws:iam::111122223333:role/dev"

func TestAccessEntry_UnknownClusterIsNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName: aws.String("missing"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestAccessEntry_CreateDescribeListDelete(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")

	out, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
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
	_, err = svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceInUse)

	desc, err := svc.DescribeAccessEntry(context.Background(), &eks.DescribeAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"system:masters"}, aws.StringValueSlice(desc.AccessEntry.KubernetesGroups))

	list, err := svc.ListAccessEntries(context.Background(), &eks.ListAccessEntriesInput{ClusterName: aws.String("c1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{testPrincipalARN}, aws.StringValueSlice(list.AccessEntries))

	_, err = svc.DeleteAccessEntry(context.Background(), &eks.DeleteAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DescribeAccessEntry(context.Background(), &eks.DescribeAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestAccessEntry_RejectsNodeType(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN), Type: aws.String("EC2_LINUX"),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestAccessEntry_DescribeDeleteMissingIsNotFound(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.DescribeAccessEntry(context.Background(), &eks.DescribeAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
	_, err = svc.DeleteAccessEntry(context.Background(), &eks.DeleteAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestAccessPolicy_AssociateListDisassociate(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)

	const viewPolicy = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
	assoc, err := svc.AssociateAccessPolicy(context.Background(), &eks.AssociateAccessPolicyInput{
		ClusterName:  aws.String("c1"),
		PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:    aws.String(viewPolicy),
		AccessScope:  &eks.AccessScope{Type: aws.String("cluster")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, viewPolicy, aws.StringValue(assoc.AssociatedAccessPolicy.PolicyArn))

	listed, err := svc.ListAssociatedAccessPolicies(context.Background(), &eks.ListAssociatedAccessPoliciesInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.AssociatedAccessPolicies, 1)

	// Entry now discoverable by the associated-policy filter.
	filtered, err := svc.ListAccessEntries(context.Background(), &eks.ListAccessEntriesInput{
		ClusterName: aws.String("c1"), AssociatedPolicyArn: aws.String(viewPolicy),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{testPrincipalARN}, aws.StringValueSlice(filtered.AccessEntries))

	_, err = svc.DisassociateAccessPolicy(context.Background(), &eks.DisassociateAccessPolicyInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN), PolicyArn: aws.String(viewPolicy),
	}, testAccountID)
	require.NoError(t, err)
	listed, err = svc.ListAssociatedAccessPolicies(context.Background(), &eks.ListAssociatedAccessPoliciesInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, listed.AssociatedAccessPolicies)
}

func TestAccessPolicy_AssociateRejectsUnsupportedPolicyAndScope(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)

	// Unsupported policy ARN.
	_, err = svc.AssociateAccessPolicy(context.Background(), &eks.AssociateAccessPolicyInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:   aws.String("arn:aws:eks::aws:cluster-access-policy/MadeUpPolicy"),
		AccessScope: &eks.AccessScope{Type: aws.String("cluster")},
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)

	// namespace scope without namespaces.
	_, err = svc.AssociateAccessPolicy(context.Background(), &eks.AssociateAccessPolicyInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:   aws.String("arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"),
		AccessScope: &eks.AccessScope{Type: aws.String("namespace")},
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

// Namespace scope has no RBAC writer (Change 3): it must fail at association
// time with a message naming accessScope.type, not read back as a durable
// grant that never actually authorizes anything.
func TestAccessPolicy_AssociateRejectsNamespaceScope(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.AssociateAccessPolicy(context.Background(), &eks.AssociateAccessPolicyInput{
		ClusterName:  aws.String("c1"),
		PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:    aws.String("arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"),
		AccessScope:  &eks.AccessScope{Type: aws.String("namespace"), Namespaces: aws.StringSlice([]string{"team-a"})},
	}, testAccountID)
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorEKSInvalidParameter, code)
	assert.Contains(t, message, "accessScope.type")
}

// DisassociateAccessPolicy already removes the association from the record;
// this confirms the projection built on top of it reflects the removal too.
func TestAccessPolicy_DisassociateRemovesProjectedGroup(t *testing.T) {
	svc := setupTestService(t)
	seedTestCluster(t, svc, "c1")
	_, err := svc.CreateAccessEntry(context.Background(), &eks.CreateAccessEntryInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN),
	}, testAccountID)
	require.NoError(t, err)

	const viewPolicy = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
	_, err = svc.AssociateAccessPolicy(context.Background(), &eks.AssociateAccessPolicyInput{
		ClusterName:  aws.String("c1"),
		PrincipalArn: aws.String(testPrincipalARN),
		PolicyArn:    aws.String(viewPolicy),
		AccessScope:  &eks.AccessScope{Type: aws.String("cluster")},
	}, testAccountID)
	require.NoError(t, err)

	js := testutil.NewJetStream(t, svc.deps.NATSConn)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID, 1)
	require.NoError(t, err)
	rec, err := GetAccessEntryRecord(t.Context(), kv, "c1", testPrincipalARN)
	require.NoError(t, err)
	assert.Equal(t, []string{"mulga:eks-view"}, effectiveGroups(rec))

	_, err = svc.DisassociateAccessPolicy(context.Background(), &eks.DisassociateAccessPolicyInput{
		ClusterName: aws.String("c1"), PrincipalArn: aws.String(testPrincipalARN), PolicyArn: aws.String(viewPolicy),
	}, testAccountID)
	require.NoError(t, err)

	rec, err = GetAccessEntryRecord(t.Context(), kv, "c1", testPrincipalARN)
	require.NoError(t, err)
	assert.Empty(t, effectiveGroups(rec))
}

func TestListAccessPolicies_ReturnsSupportedCatalogue(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.ListAccessPolicies(context.Background(), &eks.ListAccessPoliciesInput{}, testAccountID)
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

	_, err := svc.AssociateIdentityProviderConfig(context.Background(), &eks.AssociateIdentityProviderConfigInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DescribeIdentityProviderConfig(context.Background(), &eks.DescribeIdentityProviderConfigInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.ListIdentityProviderConfigs(context.Background(), &eks.ListIdentityProviderConfigsInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.DisassociateIdentityProviderConfig(context.Background(), &eks.DisassociateIdentityProviderConfigInput{ClusterName: aws.String("c1")}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}

func TestEKSServiceImpl_ClusterTagRoundTrip(t *testing.T) {
	svc := setupTestService(t)
	js := testutil.NewJetStream(t, svc.deps.NATSConn)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID, 1)
	require.NoError(t, err)

	const arn = "arn:aws:eks:us-east-1:111122223333:cluster/c1"
	require.NoError(t, PutClusterMeta(t.Context(), kv, &ClusterMeta{
		Name:   "c1",
		Status: ClusterStatusActive,
		Tags:   map[string]string{"env": "prod"},
	}))

	// Create-time tags are echoed (the drift-killing round-trip).
	lt, err := svc.ListTagsForResource(context.Background(), &eks.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "prod", aws.StringValue(lt.Tags["env"]))

	dc, err := svc.DescribeCluster(context.Background(), &eks.DescribeClusterInput{Name: aws.String("c1")}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "prod", aws.StringValue(dc.Cluster.Tags["env"]))

	// TagResource merges, UntagResource removes — both store-only.
	_, err = svc.TagResource(context.Background(), &eks.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags:        aws.StringMap(map[string]string{"team": "platform"}),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.UntagResource(context.Background(), &eks.UntagResourceInput{
		ResourceArn: aws.String(arn),
		TagKeys:     aws.StringSlice([]string{"env"}),
	}, testAccountID)
	require.NoError(t, err)

	lt, err = svc.ListTagsForResource(context.Background(), &eks.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "platform", aws.StringValue(lt.Tags["team"]))
	_, hasEnv := lt.Tags["env"]
	require.False(t, hasEnv)
}

func TestEKSServiceImpl_NodegroupTagRoundTrip(t *testing.T) {
	svc := setupTestService(t)
	js := testutil.NewJetStream(t, svc.deps.NATSConn)
	kv, err := GetOrCreateAccountBucket(t.Context(), js, testAccountID, 1)
	require.NoError(t, err)

	const arn = "arn:aws:eks:us-east-1:111122223333:nodegroup/c1/ng1/abc123"
	require.NoError(t, PutNodegroupRecord(t.Context(), kv, &NodegroupRecord{
		ClusterName: "c1",
		Name:        "ng1",
		Arn:         arn,
		Status:      eks.NodegroupStatusActive,
		Tags:        map[string]string{"env": "prod"},
	}))

	// Create-time tags are echoed (the drift-killing round-trip).
	lt, err := svc.ListTagsForResource(context.Background(), &eks.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "prod", aws.StringValue(lt.Tags["env"]))

	dn, err := svc.DescribeNodegroup(context.Background(), &eks.DescribeNodegroupInput{
		ClusterName:   aws.String("c1"),
		NodegroupName: aws.String("ng1"),
	}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "prod", aws.StringValue(dn.Nodegroup.Tags["env"]))

	// TagResource merges, UntagResource removes — both store-only.
	_, err = svc.TagResource(context.Background(), &eks.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags:        aws.StringMap(map[string]string{"team": "platform"}),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.UntagResource(context.Background(), &eks.UntagResourceInput{
		ResourceArn: aws.String(arn),
		TagKeys:     aws.StringSlice([]string{"env"}),
	}, testAccountID)
	require.NoError(t, err)

	lt, err = svc.ListTagsForResource(context.Background(), &eks.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, "platform", aws.StringValue(lt.Tags["team"]))
	_, hasEnv := lt.Tags["env"]
	require.False(t, hasEnv)
}

func TestEKSServiceImpl_NodegroupTagMissingNotFound(t *testing.T) {
	svc := setupTestService(t)
	const arn = "arn:aws:eks:us-east-1:111122223333:nodegroup/c1/absent/abc123"

	_, err := svc.ListTagsForResource(context.Background(), &eks.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)

	_, err = svc.TagResource(context.Background(), &eks.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags:        aws.StringMap(map[string]string{"k": "v"}),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)

	_, err = svc.UntagResource(context.Background(), &eks.UntagResourceInput{
		ResourceArn: aws.String(arn),
		TagKeys:     aws.StringSlice([]string{"k"}),
	}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorEKSResourceNotFound)
}

func TestEKSServiceImpl_TagsUnsupportedARNNotImplemented(t *testing.T) {
	svc := setupTestService(t)
	const fpARN = "arn:aws:eks:us-east-1:111122223333:fargateprofile/c1/fp1/abc123"

	_, err := svc.TagResource(context.Background(), &eks.TagResourceInput{ResourceArn: aws.String(fpARN)}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.UntagResource(context.Background(), &eks.UntagResourceInput{ResourceArn: aws.String(fpARN)}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)

	_, err = svc.ListTagsForResource(context.Background(), &eks.ListTagsForResourceInput{ResourceArn: aws.String(fpARN)}, testAccountID)
	require.EqualError(t, err, awserrors.ErrorNotImplemented)
}
