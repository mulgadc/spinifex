package handlers_iam

import (
	"fmt"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const s3PolicyDocument = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`

func createPolicyVersion(t *testing.T, svc *IAMServiceImpl, policyArn *string, setAsDefault bool) *iam.PolicyVersion {
	t.Helper()
	out, err := svc.CreatePolicyVersion(testAccountID, &iam.CreatePolicyVersionInput{
		PolicyArn:      policyArn,
		PolicyDocument: aws.String(s3PolicyDocument),
		SetAsDefault:   aws.Bool(setAsDefault),
	})
	require.NoError(t, err)
	return out.PolicyVersion
}

func deletePolicyVersion(t *testing.T, svc *IAMServiceImpl, policyArn *string, versionID string) {
	t.Helper()
	_, err := svc.DeletePolicyVersion(testAccountID, &iam.DeletePolicyVersionInput{
		PolicyArn: policyArn,
		VersionId: aws.String(versionID),
	})
	require.NoError(t, err)
}

func listVersionIDs(t *testing.T, svc *IAMServiceImpl, policyArn *string) (ids []string, defaultID string) {
	t.Helper()
	out, err := svc.ListPolicyVersions(testAccountID, &iam.ListPolicyVersionsInput{PolicyArn: policyArn})
	require.NoError(t, err)
	for _, v := range out.Versions {
		assert.Nil(t, v.Document, "list entries carry no document")
		ids = append(ids, *v.VersionId)
		if *v.IsDefaultVersion {
			defaultID = *v.VersionId
		}
	}
	return ids, defaultID
}

func getVersionDocument(t *testing.T, svc *IAMServiceImpl, policyArn *string, versionID string) (string, bool) {
	t.Helper()
	out, err := svc.GetPolicyVersion(testAccountID, &iam.GetPolicyVersionInput{
		PolicyArn: policyArn,
		VersionId: aws.String(versionID),
	})
	require.NoError(t, err)
	return *out.PolicyVersion.Document, *out.PolicyVersion.IsDefaultVersion
}

func TestCreatePolicyVersion_SetAsDefault(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	created := createPolicyVersion(t, svc, policy.Arn, true)
	assert.Equal(t, "v2", *created.VersionId)
	assert.True(t, *created.IsDefaultVersion)
	assert.NotNil(t, created.CreateDate)
	assert.Nil(t, created.Document)

	got, err := svc.GetPolicy(testAccountID, &iam.GetPolicyInput{PolicyArn: policy.Arn})
	require.NoError(t, err)
	assert.Equal(t, "v2", *got.Policy.DefaultVersionId)

	doc, isDefault := getVersionDocument(t, svc, policy.Arn, "v2")
	assert.Equal(t, s3PolicyDocument, doc)
	assert.True(t, isDefault)
	doc, isDefault = getVersionDocument(t, svc, policy.Arn, "v1")
	assert.Equal(t, validPolicyDocument(), doc)
	assert.False(t, isDefault)
}

func TestCreatePolicyVersion_NotDefault(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	created := createPolicyVersion(t, svc, policy.Arn, false)
	assert.Equal(t, "v2", *created.VersionId)
	assert.False(t, *created.IsDefaultVersion)

	ids, defaultID := listVersionIDs(t, svc, policy.Arn)
	assert.Equal(t, []string{"v2", "v1"}, ids)
	assert.Equal(t, "v1", defaultID)
}

func TestCreatePolicyVersion_FiveVersionLimit(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	for range 4 {
		createPolicyVersion(t, svc, policy.Arn, false)
	}
	_, err := svc.CreatePolicyVersion(testAccountID, &iam.CreatePolicyVersionInput{
		PolicyArn:      policy.Arn,
		PolicyDocument: aws.String(s3PolicyDocument),
		SetAsDefault:   aws.Bool(true),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMLimitExceeded)

	ids, defaultID := listVersionIDs(t, svc, policy.Arn)
	assert.Len(t, ids, 5)
	assert.Equal(t, "v1", defaultID, "a rejected version must not change the default")
}

func TestCreatePolicyVersion_MalformedDocument(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	_, err := svc.CreatePolicyVersion(testAccountID, &iam.CreatePolicyVersionInput{
		PolicyArn:      policy.Arn,
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[]}`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
}

func TestCreatePolicyVersion_PolicyNotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	for _, policyArn := range []string{
		"arn:aws:iam::" + testAccountID + ":policy/Ghost",
		// The stored key omits the path, so a wrong path must not reach the record.
		"arn:aws:iam::" + testAccountID + ":policy/other/Versioned",
		"not-an-arn",
	} {
		_, err := svc.CreatePolicyVersion(testAccountID, &iam.CreatePolicyVersionInput{
			PolicyArn:      aws.String(policyArn),
			PolicyDocument: aws.String(s3PolicyDocument),
		})
		require.Error(t, err, policyArn)
		assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity, policyArn)
	}

	ids, _ := listVersionIDs(t, svc, policy.Arn)
	assert.Equal(t, []string{"v1"}, ids)
}

func TestCreatePolicyVersion_IDsAreNeverReused(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	createPolicyVersion(t, svc, policy.Arn, false)
	deletePolicyVersion(t, svc, policy.Arn, "v2")

	created := createPolicyVersion(t, svc, policy.Arn, false)
	assert.Equal(t, "v3", *created.VersionId)
}

// Terraform prunes the oldest version while creating the next; neither write may be lost.
func TestCreatePolicyVersion_ConcurrentWritersAllLand(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = svc.CreatePolicyVersion(testAccountID, &iam.CreatePolicyVersionInput{
				PolicyArn:      policy.Arn,
				PolicyDocument: aws.String(s3PolicyDocument),
			})
		})
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	ids, _ := listVersionIDs(t, svc, policy.Arn)
	assert.Equal(t, []string{"v5", "v4", "v3", "v2", "v1"}, ids)
}

func TestListPolicyVersions_NewestFirstByNumber(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	for i := 2; i <= 8; i++ {
		createPolicyVersion(t, svc, policy.Arn, false)
		deletePolicyVersion(t, svc, policy.Arn, fmt.Sprintf("v%d", i))
	}
	createPolicyVersion(t, svc, policy.Arn, false)
	createPolicyVersion(t, svc, policy.Arn, true)

	// Sorted as strings, v9 would come before v10.
	ids, defaultID := listVersionIDs(t, svc, policy.Arn)
	assert.Equal(t, []string{"v10", "v9", "v1"}, ids)
	assert.Equal(t, "v10", defaultID)
}

func TestSetDefaultPolicyVersion_AuthorizationFollowsDefault(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")
	createPolicyVersion(t, svc, policy.Arn, false)

	evaluate := func(action string) iampolicy.Decision {
		doc, include, err := svc.resolveAttachedPolicy(t.Context(), testAccountID, *policy.Arn)
		require.NoError(t, err)
		require.True(t, include)
		return iampolicy.EvaluateWithKeys(action, "*", []PolicyDocument{doc}, nil)
	}
	setDefault := func(versionID string) {
		_, err := svc.SetDefaultPolicyVersion(testAccountID, &iam.SetDefaultPolicyVersionInput{
			PolicyArn: policy.Arn,
			VersionId: aws.String(versionID),
		})
		require.NoError(t, err)
	}

	setDefault("v2")
	assert.Equal(t, iampolicy.Allow, evaluate("s3:GetObject"))
	assert.Equal(t, iampolicy.Deny, evaluate("ec2:DescribeInstances"))

	setDefault("v1")
	assert.Equal(t, iampolicy.Deny, evaluate("s3:GetObject"))
	assert.Equal(t, iampolicy.Allow, evaluate("ec2:DescribeInstances"))

	ids, defaultID := listVersionIDs(t, svc, policy.Arn)
	assert.Equal(t, []string{"v2", "v1"}, ids)
	assert.Equal(t, "v1", defaultID)
	doc, _ := getVersionDocument(t, svc, policy.Arn, "v2")
	assert.Equal(t, s3PolicyDocument, doc)
}

func TestSetDefaultPolicyVersion_AlreadyDefault(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	_, err := svc.SetDefaultPolicyVersion(testAccountID, &iam.SetDefaultPolicyVersionInput{
		PolicyArn: policy.Arn,
		VersionId: aws.String("v1"),
	})
	require.NoError(t, err)
}

func TestSetDefaultPolicyVersion_UnknownVersion(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	_, err := svc.SetDefaultPolicyVersion(testAccountID, &iam.SetDefaultPolicyVersionInput{
		PolicyArn: policy.Arn,
		VersionId: aws.String("v2"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeletePolicyVersion_DefaultIsAConflict(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")
	createPolicyVersion(t, svc, policy.Arn, true)

	_, err := svc.DeletePolicyVersion(testAccountID, &iam.DeletePolicyVersionInput{
		PolicyArn: policy.Arn,
		VersionId: aws.String("v2"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)
	assert.Contains(t, err.Error(), "Cannot delete the default version of a policy.")

	deletePolicyVersion(t, svc, policy.Arn, "v1")
	ids, _ := listVersionIDs(t, svc, policy.Arn)
	assert.Equal(t, []string{"v2"}, ids)
}

func TestDeletePolicyVersion_UnknownVersion(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")

	_, err := svc.DeletePolicyVersion(testAccountID, &iam.DeletePolicyVersionInput{
		PolicyArn: policy.Arn,
		VersionId: aws.String("v2"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMNoSuchEntity)
}

func TestDeletePolicy_NonDefaultVersionsAreAConflict(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policy := createTestPolicy(t, svc, "Versioned")
	createPolicyVersion(t, svc, policy.Arn, true)

	_, err := svc.DeletePolicy(testAccountID, &iam.DeletePolicyInput{PolicyArn: policy.Arn})
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMDeleteConflict)

	deletePolicyVersion(t, svc, policy.Arn, "v1")
	_, err = svc.DeletePolicy(testAccountID, &iam.DeletePolicyInput{PolicyArn: policy.Arn})
	require.NoError(t, err)
}

// A record stored before versioning has none of the version fields.
func TestPolicyVersions_RecordWithoutVersionFields(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	policyArn := arn.FormatIAMPath(arn.IAMPolicy, testAccountID, "/", "Legacy")
	record := `{"policy_name":"Legacy","policy_id":"ANPALEGACY","arn":"` + policyArn + `","path":"/",` +
		`"policy_document":` + fmt.Sprintf("%q", validPolicyDocument()) + `,` +
		`"created_at":"2025-01-02T03:04:05Z","default_version":"v1","tags":[]}`
	_, err := svc.policiesBucket.Put(t.Context(), testAccountID+".Legacy", []byte(record))
	require.NoError(t, err)

	out, err := svc.ListPolicyVersions(testAccountID, &iam.ListPolicyVersionsInput{PolicyArn: aws.String(policyArn)})
	require.NoError(t, err)
	require.Len(t, out.Versions, 1)
	assert.Equal(t, "v1", *out.Versions[0].VersionId)
	assert.True(t, *out.Versions[0].IsDefaultVersion)
	assert.Equal(t, "2025-01-02T03:04:05Z", out.Versions[0].CreateDate.UTC().Format("2006-01-02T15:04:05Z"))

	created := createPolicyVersion(t, svc, aws.String(policyArn), true)
	assert.Equal(t, "v2", *created.VersionId)
	doc, isDefault := getVersionDocument(t, svc, aws.String(policyArn), "v1")
	assert.Equal(t, validPolicyDocument(), doc)
	assert.False(t, isDefault)
}
