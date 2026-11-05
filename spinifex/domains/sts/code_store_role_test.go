package sts

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testCodeStoreRegion  = "ap-southeast-2"
	lambdaTrustPolicyDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
)

func newCodeStoreSetup(t *testing.T, trust string) *STSServiceImpl {
	t.Helper()
	svc, nc := newTestSetup(t)
	putTestAccount(t, nc, identifiers.GlobalAccountID, iamdomain.AccountStatusActive)
	createRoleInAccount(t, svc, identifiers.GlobalAccountID, CodeStoreRoleName(testCodeStoreRegion), trust)
	return svc
}

func TestAssumeCodeStoreRole_IssuesRegionalSystemRole(t *testing.T) {
	t.Parallel()

	svc := newCodeStoreSetup(t, lambdaTrustPolicyDoc)

	out, err := svc.AssumeCodeStoreRole(testCodeStoreRegion, "node-1")
	require.NoError(t, err)

	assert.Equal(t, "arn:aws:sts::000000000000:assumed-role/lambda-code-store-ap-southeast-2/node-1",
		aws.StringValue(out.AssumedRoleUser.Arn))
	stored, err := svc.LookupSessionCredential(aws.StringValue(out.Credentials.AccessKeyId))
	require.NoError(t, err)
	assert.Equal(t, identifiers.GlobalAccountID, stored.AccountID)
}

// The entry point asserts only lambda.amazonaws.com, so a role whose trust names
// another principal is refused even though its name is the code-store name.
func TestAssumeCodeStoreRole_RefusesOtherTrust(t *testing.T) {
	t.Parallel()

	svc := newCodeStoreSetup(t, trustPolicyAllowingEC2Service())

	_, err := svc.AssumeCodeStoreRole(testCodeStoreRegion, "node-1")
	assertErrorCode(t, err, awserrors.ErrorAccessDenied)
}

// No Region has a role yet, so another Region's name resolves to nothing.
func TestAssumeCodeStoreRole_OtherRegionHasNoRole(t *testing.T) {
	t.Parallel()

	svc := newCodeStoreSetup(t, lambdaTrustPolicyDoc)

	_, err := svc.AssumeCodeStoreRole("us-east-1", "node-1")
	assertErrorCode(t, err, awserrors.ErrorAccessDenied)
}

// An HTTPS caller is never attributed lambda.amazonaws.com, so even a system
// account principal cannot assume the code-store role over AssumeRole.
func TestAssumeCodeStoreRole_HTTPSAssumeRoleCannotMatch(t *testing.T) {
	t.Parallel()

	svc := newCodeStoreSetup(t, lambdaTrustPolicyDoc)

	caller := "arn:aws:iam::000000000000:user/operator"
	_, err := svc.AssumeRole(identifiers.GlobalAccountID, caller, "AIDAOPERATOR",
		basicAssumeRoleInput(CodeStoreRoleARN(testCodeStoreRegion), "s1"))
	assertErrorCode(t, err, awserrors.ErrorAccessDenied)
}

func TestAssumeCodeStoreRole_ValidatesInputs(t *testing.T) {
	t.Parallel()

	svc := newCodeStoreSetup(t, lambdaTrustPolicyDoc)

	_, err := svc.AssumeCodeStoreRole("", "node-1")
	assertErrorCode(t, err, awserrors.ErrorMissingParameter)
	_, err = svc.AssumeCodeStoreRole("ap/southeast", "node-1")
	assertErrorCode(t, err, awserrors.ErrorValidationError)
	_, err = svc.AssumeCodeStoreRole(testCodeStoreRegion, "bad name")
	assertErrorCode(t, err, awserrors.ErrorValidationError)
}
