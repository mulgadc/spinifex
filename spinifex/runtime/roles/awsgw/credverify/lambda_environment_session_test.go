package credverify_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccount = "000000000001"

type noJWKS struct{}

func (noJWKS) LookupClusterJWKS(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, nil
}

const lambdaTrust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

type lambdaFixture struct {
	iamSvc      *iamdomain.IAMServiceImpl
	stsSvc      *stsdomain.STSServiceImpl
	role        *iam.Role
	createRole  func() *iam.Role
	out         *sts.AssumeRoleOutput
	req         credential.Request
	matchSecret func(string) error
}

func newLambdaFixture(t *testing.T) *lambdaFixture {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	iamSvc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)
	stsSvc, err := stsdomain.NewSTSServiceImpl(t.Context(), nc, iamSvc, masterKey, noJWKS{})
	require.NoError(t, err)

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	accounts, err := js.KeyValue(t.Context(), iamdomain.KVBucketAccounts)
	require.NoError(t, err)
	acct, err := json.Marshal(iamdomain.Account{AccountID: testAccount, AccountName: "lambda", Status: iamdomain.AccountStatusActive})
	require.NoError(t, err)
	_, err = accounts.Put(t.Context(), testAccount, acct)
	require.NoError(t, err)

	f := &lambdaFixture{iamSvc: iamSvc, stsSvc: stsSvc}
	f.createRole = func() *iam.Role {
		out, err := iamSvc.CreateRole(testAccount, &iam.CreateRoleInput{
			RoleName: aws.String("fn-exec"), AssumeRolePolicyDocument: aws.String(lambdaTrust),
		})
		require.NoError(t, err)
		return out.Role
	}
	f.role = f.createRole()

	f.out, err = stsSvc.AssumeRoleForLambdaEnvironment(t.Context(), stsdomain.LambdaEnvironmentSessionRequest{
		AccountID:          testAccount,
		RoleARN:            aws.StringValue(f.role.Arn),
		FunctionARN:        testFunctionARN,
		FunctionID:         testFunctionID,
		EnvironmentID:      testEnvironmentID,
		MaxLifetimeSeconds: 3600,
	})
	require.NoError(t, err)

	f.req = credential.Request{
		AccessKeyID:   aws.StringValue(f.out.Credentials.AccessKeyId),
		SecurityToken: aws.StringValue(f.out.Credentials.SessionToken),
	}
	secret := aws.StringValue(f.out.Credentials.SecretAccessKey)
	f.matchSecret = func(got string) error {
		if got != secret {
			return errors.New(awserrors.ErrorSignatureDoesNotMatch)
		}
		return nil
	}
	return f
}

const (
	testFunctionARN   = "arn:aws:lambda:ap-southeast-2:" + testAccount + ":function:hello"
	testFunctionID    = "0123456789abcdef0123456789abcdef"
	testEnvironmentID = "env-0001"
)

// A Lambda environment session passes the real verifier's Authenticate and Resolve,
// and stops resolving once its role is replaced under the same name. The secret
// callback stands in for SigV4; no request is signed here.
func TestResolve_LambdaEnvironmentSessionFailsAfterRoleReplaced(t *testing.T) {
	f := newLambdaFixture(t)
	v := credverify.New(f.iamSvc, f.stsSvc)

	auth, fail := v.Authenticate(f.req, f.matchSecret)
	require.Nil(t, fail)
	p, fail := v.Resolve(f.req, auth)
	require.Nil(t, fail)
	assert.Equal(t, aws.StringValue(f.out.AssumedRoleUser.Arn), p.AssumedRoleARN)
	assert.Equal(t, stsdomain.PrincipalTypeAssumedRole, p.PrincipalType)

	_, err := f.iamSvc.DeleteRole(testAccount, &iam.DeleteRoleInput{RoleName: f.role.RoleName})
	require.NoError(t, err)
	replaced := f.createRole()
	require.NotEqual(t, aws.StringValue(f.role.RoleId), aws.StringValue(replaced.RoleId))

	auth, fail = v.Authenticate(f.req, f.matchSecret)
	require.Nil(t, fail, "the record still authenticates; the role check belongs to Resolve")
	_, fail = v.Resolve(f.req, auth)
	require.NotNil(t, fail)
	assert.Equal(t, awserrors.ErrorInvalidClientTokenId, fail.Code)
}

// Once revoked for its environment, the session no longer authenticates.
func TestAuthenticate_LambdaEnvironmentSessionFailsAfterRevoke(t *testing.T) {
	f := newLambdaFixture(t)
	v := credverify.New(f.iamSvc, f.stsSvc)
	_, fail := v.Authenticate(f.req, f.matchSecret)
	require.Nil(t, fail)

	require.NoError(t, f.stsSvc.RevokeLambdaEnvironmentSession(t.Context(), f.req.AccessKeyID, stsdomain.LambdaEnvironmentSessionScope{
		AccountID: testAccount, FunctionID: testFunctionID, FunctionARN: testFunctionARN, EnvironmentID: testEnvironmentID,
	}))

	_, fail = v.Authenticate(f.req, f.matchSecret)
	require.NotNil(t, fail)
	assert.Equal(t, awserrors.ErrorInvalidClientTokenId, fail.Code)
}
