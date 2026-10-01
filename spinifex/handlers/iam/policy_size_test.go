package handlers_iam

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sizedPolicy returns a valid identity policy whose size, as AWS counts it, is
// exactly size characters, padded through its resource ARN.
func sizedPolicy(t *testing.T, size int) string {
	t.Helper()
	const format = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::%s"}]}`
	base := policySize(fmt.Sprintf(format, ""))
	require.GreaterOrEqual(t, size, base)
	doc := fmt.Sprintf(format, strings.Repeat("a", size-base))
	require.Equal(t, size, policySize(doc))
	return doc
}

func TestPolicySize_IgnoresWhiteSpace(t *testing.T) {
	t.Parallel()
	compact := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`
	pretty := "{\n  \"Version\": \"2012-10-17\",\n\t\"Statement\": [ {\"Effect\": \"Allow\", \"Action\": \"s3:*\", \"Resource\": \"*\"} ]\r\n}"
	assert.Equal(t, len(compact), policySize(compact))
	assert.Equal(t, policySize(compact), policySize(pretty))
	assert.Equal(t, 3, policySize("aéb"), "size counts characters, not bytes")
}

// inlinePutter puts an inline policy on one entity type, for running the same
// quota cases against users, groups and roles.
type inlinePutter struct {
	entityType string
	quota      int
	create     func(t *testing.T, svc *IAMServiceImpl, name string)
	put        func(svc *IAMServiceImpl, name, policyName, doc string) error
}

var inlinePutters = []inlinePutter{
	{
		entityType: "user",
		quota:      2048,
		create:     func(t *testing.T, svc *IAMServiceImpl, name string) { createTestUser(t, svc, name) },
		put: func(svc *IAMServiceImpl, name, policyName, doc string) error {
			_, err := svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
				UserName: aws.String(name), PolicyName: aws.String(policyName), PolicyDocument: aws.String(doc),
			})
			return err
		},
	},
	{
		entityType: "group",
		quota:      5120,
		create:     func(t *testing.T, svc *IAMServiceImpl, name string) { createTestGroup(t, svc, name) },
		put: func(svc *IAMServiceImpl, name, policyName, doc string) error {
			_, err := svc.PutGroupPolicy(testAccountID, &iam.PutGroupPolicyInput{
				GroupName: aws.String(name), PolicyName: aws.String(policyName), PolicyDocument: aws.String(doc),
			})
			return err
		},
	},
	{
		entityType: "role",
		quota:      10240,
		create:     func(t *testing.T, svc *IAMServiceImpl, name string) { createTestRole(t, svc, name) },
		put: func(svc *IAMServiceImpl, name, policyName, doc string) error {
			_, err := svc.PutRolePolicy(testAccountID, &iam.PutRolePolicyInput{
				RoleName: aws.String(name), PolicyName: aws.String(policyName), PolicyDocument: aws.String(doc),
			})
			return err
		},
	},
}

func TestPutInlinePolicy_SingleDocumentQuota(t *testing.T) {
	t.Parallel()
	for _, p := range inlinePutters {
		t.Run(p.entityType, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			p.create(t, svc, "sized")

			err := p.put(svc, "sized", "Over", sizedPolicy(t, p.quota+1))
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorIAMLimitExceeded)
			assert.Contains(t, err.Error(), fmt.Sprintf("Maximum policy size of %d bytes exceeded for %s sized", p.quota, p.entityType))

			require.NoError(t, p.put(svc, "sized", "AtQuota", sizedPolicy(t, p.quota)))
		})
	}
}

// The quota applies to the sum of an entity's inline policies.
func TestPutInlinePolicy_AggregateQuota(t *testing.T) {
	t.Parallel()
	for _, p := range inlinePutters {
		t.Run(p.entityType, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			p.create(t, svc, "aggregate")

			first := p.quota / 2
			require.NoError(t, p.put(svc, "aggregate", "First", sizedPolicy(t, first)))

			err := p.put(svc, "aggregate", "Second", sizedPolicy(t, p.quota-first+1))
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorIAMLimitExceeded)

			require.NoError(t, p.put(svc, "aggregate", "Second", sizedPolicy(t, p.quota-first)))
		})
	}
}

// Replacing a policy counts only its new document, not the one it replaces.
func TestPutInlinePolicy_ReplacementCountsOnlyNewDocument(t *testing.T) {
	t.Parallel()
	for _, p := range inlinePutters {
		t.Run(p.entityType, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			p.create(t, svc, "replace")

			require.NoError(t, p.put(svc, "replace", "Only", sizedPolicy(t, p.quota-10)))
			require.NoError(t, p.put(svc, "replace", "Only", sizedPolicy(t, p.quota)))
		})
	}
}

func TestPutInlinePolicy_WhiteSpaceNotCounted(t *testing.T) {
	t.Parallel()
	for _, p := range inlinePutters {
		t.Run(p.entityType, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			p.create(t, svc, "spaced")

			doc := strings.Replace(sizedPolicy(t, p.quota), `"Statement":`, `"Statement":`+strings.Repeat(" ", p.quota), 1)
			require.NoError(t, p.put(svc, "spaced", "Spaced", doc))
		})
	}
}

func TestCreatePolicy_ManagedQuota(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)

	_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName: aws.String("Over"), PolicyDocument: aws.String(sizedPolicy(t, 6145)),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMLimitExceeded)
	assert.Contains(t, err.Error(), "Cannot exceed quota for PolicySize: 6144")

	out, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
		PolicyName: aws.String("AtQuota"), PolicyDocument: aws.String(sizedPolicy(t, 6144)),
	})
	require.NoError(t, err)

	_, err = svc.CreatePolicyVersion(testAccountID, &iam.CreatePolicyVersionInput{
		PolicyArn: out.Policy.Arn, PolicyDocument: aws.String(sizedPolicy(t, 6145)),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorIAMLimitExceeded)

	_, err = svc.CreatePolicyVersion(testAccountID, &iam.CreatePolicyVersionInput{
		PolicyArn: out.Policy.Arn, PolicyDocument: aws.String(sizedPolicy(t, 6144)),
	})
	require.NoError(t, err)
}

// Under 2008-10-17 a ${...} reference is literal text, so a key no request
// supplies is accepted rather than rejected as unresolvable.
func TestValidatePolicyDocument_Version2008(t *testing.T) {
	t.Parallel()
	const legacy = `{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject",` +
		`"Resource":"arn:aws:s3:::home/${unknown}/*","Condition":{"StringLike":{"s3:prefix":"${unknown}/*"}}}]}`
	_, err := ValidatePolicyDocument(legacy)
	require.NoError(t, err)

	_, err = ValidatePolicyDocument(strings.Replace(legacy, "2008-10-17", "2012-10-17", 1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no request supplies")

	_, err = ValidatePolicyDocument(strings.Replace(legacy, "2008-10-17", "2010-01-01", 1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported policy version")
}
