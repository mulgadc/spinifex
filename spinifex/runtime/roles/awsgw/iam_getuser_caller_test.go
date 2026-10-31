package awsgw_test

import (
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GetUser with no UserName describes the user whose key signed the request,
// authorized against that user's own path-qualified ARN.
func TestIAMGetUser_NoUserName_UserCaller(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	_, err := e.iam.CreateUser(e.accountID, &iam.CreateUserInput{UserName: aws.String("dave"), Path: aws.String("/teams/")})
	require.NoError(t, err)
	dave := e.newKey(t, "dave")
	daveARN := "arn:aws:iam::" + e.accountID + ":user/teams/dave"
	e.putUserPolicy(t, "dave", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"iam:GetUser","Resource":"`+daveARN+`"}]}`)

	self := e.request(t, e.h, dave, "iam", cbForm("GetUser"))
	require.Equalf(t, http.StatusOK, self.Code, "body: %s", self.Body.String())
	named := e.request(t, e.h, dave, "iam", cbForm("GetUser", "UserName", "dave"))
	require.Equalf(t, http.StatusOK, named.Code, "body: %s", named.Body.String())

	assert.Equal(t, "dave", cbXML(self.Body.String(), "UserName"))
	assert.Equal(t, daveARN, cbXML(self.Body.String(), "Arn"))
	for _, tag := range []string{"UserName", "UserId", "Arn", "Path", "CreateDate"} {
		assert.Equal(t, cbXML(named.Body.String(), tag), cbXML(self.Body.String(), tag), tag)
	}
}

// A Deny on the caller's own ARN applies to the implicit form, while the same
// caller can still read another user, so the denial is scoped and not blanket.
func TestIAMGetUser_NoUserName_DenyOnSelf(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	carol, _ := e.newUser(t, "carol")
	e.putUserPolicy(t, "carol", `{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Action":"iam:GetUser","Resource":"*"},`+
		`{"Effect":"Deny","Action":"iam:GetUser","Resource":"arn:aws:iam::`+e.accountID+`:user/carol"}]}`)

	assert.Equal(t, awserrors.ErrorAccessDenied, cbOutcomeOf(e.request(t, e.h, carol, "iam", cbForm("GetUser"))).code)
	other := e.request(t, e.h, carol, "iam", cbForm("GetUser", "UserName", "alice"))
	assert.Equalf(t, http.StatusOK, other.Code, "body: %s", other.Body.String())
}

// Root gets the account root back: account ID as UserId, the root ARN, and no
// UserName or Path.
func TestIAMGetUser_NoUserName_RootCaller(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	rec := e.request(t, e.h, cbCreds{akid: cbRootAKID, secret: cbRootSecret}, "iam", cbForm("GetUser"))
	require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	body := rec.Body.String()
	assert.Equal(t, awsidentifiers.GlobalAccountID, cbXML(body, "UserId"))
	assert.Equal(t, "arn:aws:iam::"+awsidentifiers.GlobalAccountID+":root", cbXML(body, "Arn"))
	assert.NotEmpty(t, cbXML(body, "CreateDate"))
	assert.NotContains(t, body, "<UserName>")
	assert.NotContains(t, body, "<Path>")
}

// A role session has no user to default to, and is told to name one.
func TestIAMGetUser_NoUserName_RoleSessionCaller(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	roleARN, _ := e.createAssumableRole(t, "reader")
	_, err := e.iam.PutRolePolicy(e.accountID, &iam.PutRolePolicyInput{
		RoleName: aws.String("reader"), PolicyName: aws.String("p"),
		PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"iam:GetUser","Resource":"*"}]}`),
	})
	require.NoError(t, err)
	session := e.assumeRole(t, roleARN, "reader-session")

	got := cbOutcomeOf(e.request(t, e.h, session, "iam", cbForm("GetUser")))
	assert.Equal(t, http.StatusBadRequest, got.status)
	assert.Equal(t, awserrors.ErrorValidationError, got.code)
	assert.Equal(t, "Must specify userName when calling with non-User credentials", got.message)

	named := e.request(t, e.h, session, "iam", cbForm("GetUser", "UserName", "alice"))
	assert.Equalf(t, http.StatusOK, named.Code, "body: %s", named.Body.String())
}
