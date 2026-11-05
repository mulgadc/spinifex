package credverify

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type noJWKS struct{}

func (noJWKS) LookupClusterJWKS(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, nil
}

const lambdaTrust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

// A Lambda environment session resolves through the real verifier, and stops
// resolving once its role is replaced under the same name.
func TestResolve_LambdaEnvironmentSessionFailsAfterRoleReplaced(t *testing.T) {
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

	createRole := func() *iam.Role {
		out, err := iamSvc.CreateRole(testAccount, &iam.CreateRoleInput{
			RoleName: aws.String("fn-exec"), AssumeRolePolicyDocument: aws.String(lambdaTrust),
		})
		require.NoError(t, err)
		return out.Role
	}
	role := createRole()

	out, err := stsSvc.AssumeRoleForLambdaEnvironment(t.Context(), stsdomain.LambdaEnvironmentSessionRequest{
		AccountID:          testAccount,
		RoleARN:            aws.StringValue(role.Arn),
		FunctionARN:        "arn:aws:lambda:ap-southeast-2:" + testAccount + ":function:hello",
		FunctionID:         "0123456789abcdef0123456789abcdef",
		EnvironmentID:      "env-0001",
		MaxLifetimeSeconds: 3600,
	})
	require.NoError(t, err)

	v := New(iamSvc, stsSvc)
	req := credential.Request{
		AccessKeyID:   aws.StringValue(out.Credentials.AccessKeyId),
		SecurityToken: aws.StringValue(out.Credentials.SessionToken),
	}
	secret := aws.StringValue(out.Credentials.SecretAccessKey)
	matchSecret := func(got string) error {
		if got != secret {
			return errors.New(awserrors.ErrorSignatureDoesNotMatch)
		}
		return nil
	}

	auth, f := v.Authenticate(req, matchSecret)
	require.Nil(t, f)
	p, f := v.Resolve(req, auth)
	require.Nil(t, f)
	assert.Equal(t, aws.StringValue(out.AssumedRoleUser.Arn), p.AssumedRoleARN)
	assert.Equal(t, stsdomain.PrincipalTypeAssumedRole, p.PrincipalType)

	_, err = iamSvc.DeleteRole(testAccount, &iam.DeleteRoleInput{RoleName: role.RoleName})
	require.NoError(t, err)
	replaced := createRole()
	require.NotEqual(t, aws.StringValue(role.RoleId), aws.StringValue(replaced.RoleId))

	auth, f = v.Authenticate(req, matchSecret)
	require.Nil(t, f, "the record still authenticates; the role check belongs to Resolve")
	_, f = v.Resolve(req, auth)
	require.NotNil(t, f)
	assert.Equal(t, awserrors.ErrorInvalidClientTokenId, f.Code)
}
