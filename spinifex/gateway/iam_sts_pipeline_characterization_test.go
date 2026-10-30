// Characterization tests for IAM and STS dispatch through the full SigV4
// pipeline as it exists in iam.go and sts.go today, pinned ahead of moving the
// dispatch behind a registration. They run on the real IAM and STS services and
// assert current behaviour, not AWS parity. Principal resolution, session
// credentials and identity-policy outcomes are pinned in
// auth_current_behaviour_test.go and are not repeated here.

package gateway

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	cbAllowAll = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`
	cbGetUser  = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"iam:GetUser","Resource":"*"}]}`
)

func cbDenied(service string) cbOutcome {
	return cbOutcome{
		status:  http.StatusForbidden,
		code:    awserrors.ErrorAccessDenied,
		message: awserrors.LookupErrorMessage(service, awserrors.ErrorAccessDenied).Message,
	}
}

// rawRequest signs and sends body exactly as given, so a malformed query string
// reaches the dispatcher instead of being re-encoded by url.Values.
func (e *cbEnv) rawRequest(t *testing.T, c cbCreds, service, body string) *httptest.ResponseRecorder {
	t.Helper()
	b := []byte(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Host = "localhost:9999"
	n := e.addr.Add(1)
	req.RemoteAddr = fmt.Sprintf("10.9.%d.%d:40000", (n>>8)&0xff, n&0xff)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.token != "" {
		req.Header.Set("X-Amz-Security-Token", c.token)
	}
	signTestRequestAs(t, req, b, c.akid, c.secret, service, time.Now().UTC())
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestIAMSTSPipeline_ErrorEnvelopes(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	e.putUserPolicy(t, "alice", cbAllowAll)

	for _, service := range []string{"iam", "sts"} {
		t.Run(service+"_missing_action", func(t *testing.T) {
			got := cbOutcomeOf(e.request(t, e.h, e.alice, service, url.Values{"Version": {"2011-06-15"}}))
			assert.Equal(t, cbRegistry(awserrors.ErrorMissingAction), got)
		})
		t.Run(service+"_unknown_action", func(t *testing.T) {
			got := cbOutcomeOf(e.request(t, e.h, e.alice, service, cbForm("NoSuchAction")))
			assert.Equal(t, cbRegistry(awserrors.ErrorInvalidAction), got)
		})
		t.Run(service+"_malformed_query", func(t *testing.T) {
			got := cbOutcomeOf(e.rawRequest(t, e.alice, service, "Action=%zz"))
			assert.Equal(t, cbRegistry(awserrors.ErrorMalformedQueryString), got)
		})
	}
}

// CURRENT: IAM authorizes a request that parses, whatever else it omits, so the
// denial is what the caller sees; STS names a missing RoleArn ahead of the gate.
func TestIAMSTSPipeline_AuthorizationAndMissingParameterOrder(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})

	t.Run("iam_create_user_without_name_and_no_policy_is_denied", func(t *testing.T) {
		got := cbOutcomeOf(e.request(t, e.h, e.alice, "iam", cbForm("CreateUser")))
		assert.Equal(t, cbDenied("iam"), got)
	})

	t.Run("sts_assume_role_without_role_arn", func(t *testing.T) {
		got := cbOutcomeOf(e.request(t, e.h, e.alice, "sts", cbForm("AssumeRole", "RoleSessionName", "s")))
		assert.Equal(t, cbRegistry(awserrors.ErrorMissingParameter), got)
	})
}

// CURRENT: GetUser without UserName describes the caller, and that default is
// applied before authorization so the policy is evaluated against the user.
func TestIAMSTSPipeline_GetUserWithoutUserName(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	getUser := cbForm("GetUser")

	t.Run("user_without_policy_is_denied", func(t *testing.T) {
		assert.Equal(t, cbDenied("iam"), cbOutcomeOf(e.request(t, e.h, e.alice, "iam", getUser)))
	})

	t.Run("user_describes_self", func(t *testing.T) {
		e.putUserPolicy(t, "alice", cbGetUser)
		rec := e.request(t, e.h, e.alice, "iam", getUser)
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.Equal(t, "alice", cbXML(rec.Body.String(), "UserName"))
		assert.Equal(t, e.aliceID, cbXML(rec.Body.String(), "UserId"))
	})

	t.Run("named_user_still_authorized_on_that_user", func(t *testing.T) {
		bob, _ := e.newUser(t, "bob")
		assert.Equal(t, cbDenied("iam"), cbOutcomeOf(e.request(t, e.h, bob, "iam", cbForm("GetUser", "UserName", "alice"))))
	})

	t.Run("global_root_describes_account_root", func(t *testing.T) {
		rec := e.request(t, e.h, cbCreds{akid: cbRootAKID, secret: cbRootSecret}, "iam", getUser)
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		body := rec.Body.String()
		assert.Equal(t, "arn:aws:iam::000000000000:root", cbXML(body, "Arn"))
	})

	t.Run("role_session_must_name_a_user", func(t *testing.T) {
		roleARN, _ := e.createAssumableRole(t, "app")
		s := e.assumeRole(t, roleARN, "s1")
		got := cbOutcomeOf(e.request(t, e.h, s, "iam", getUser))
		assert.Equal(t, cbRegistry(awserrors.ErrorValidationError).status, got.status)
		assert.Equal(t, awserrors.ErrorValidationError, got.code)
		assert.Equal(t, "Must specify userName when calling with non-User credentials", got.message)
	})
}

// CURRENT: AssumeRole is gated twice. The caller's identity policy is checked
// first, then the role's trust policy.
func TestIAMSTSPipeline_AssumeRoleGates(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	roleARN, _ := e.createAssumableRole(t, "app")

	t.Run("allowed", func(t *testing.T) {
		rec := e.request(t, e.h, e.alice, "sts", cbForm("AssumeRole", "RoleArn", roleARN, "RoleSessionName", "s1"))
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, cbXML(rec.Body.String(), "AccessKeyId"), "ASIA")
	})

	t.Run("identity_policy_denies", func(t *testing.T) {
		bob, _ := e.newUser(t, "bob")
		got := cbOutcomeOf(e.request(t, e.h, bob, "sts", cbForm("AssumeRole", "RoleArn", roleARN, "RoleSessionName", "s1")))
		assert.Equal(t, http.StatusForbidden, got.status)
		assert.Equal(t, awserrors.ErrorAccessDenied, got.code)
	})

	t.Run("trust_policy_denies", func(t *testing.T) {
		carol, _ := e.newUser(t, "carol")
		e.putUserPolicy(t, "carol", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"sts:AssumeRole","Resource":"*"}]}`)
		got := cbOutcomeOf(e.request(t, e.h, carol, "sts", cbForm("AssumeRole", "RoleArn", roleARN, "RoleSessionName", "s1")))
		assert.Equal(t, http.StatusForbidden, got.status)
		assert.Equal(t, awserrors.ErrorAccessDenied, got.code)
	})
}

// CURRENT: GetSessionToken carries no identity-policy gate, and a role session
// cannot mint one.
func TestIAMSTSPipeline_GetSessionToken(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})

	t.Run("user_without_policy_mints", func(t *testing.T) {
		rec := e.request(t, e.h, e.alice, "sts", cbForm("GetSessionToken"))
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, cbXML(rec.Body.String(), "AccessKeyId"), "ASIA")
	})

	t.Run("role_session_is_refused", func(t *testing.T) {
		roleARN, _ := e.createAssumableRole(t, "app")
		s := e.assumeRole(t, roleARN, "s1")
		got := cbOutcomeOf(e.request(t, e.h, s, "sts", cbForm("GetSessionToken")))
		assert.Equal(t, cbDenied("sts"), got)
	})
}

// CURRENT: AddRoleToInstanceProfile requires iam:PassRole on the role, checked
// after the iam:AddRoleToInstanceProfile gate and before the binding runs.
func TestIAMSTSPipeline_AddRoleToInstanceProfileRequiresPassRole(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})
	_, err := e.iam.CreateRole(e.accountID, &iam.CreateRoleInput{
		RoleName:                 aws.String("r"),
		AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`),
	})
	require.NoError(t, err)
	_, err = e.iam.CreateInstanceProfile(e.accountID, &iam.CreateInstanceProfileInput{InstanceProfileName: aws.String("p")})
	require.NoError(t, err)
	bind := cbForm("AddRoleToInstanceProfile", "InstanceProfileName", "p", "RoleName", "r")

	roleBound := func() bool {
		out, err := e.iam.GetInstanceProfile(e.accountID, &iam.GetInstanceProfileInput{InstanceProfileName: aws.String("p")})
		require.NoError(t, err)
		return len(out.InstanceProfile.Roles) > 0
	}

	t.Run("without_pass_role_is_denied_and_nothing_binds", func(t *testing.T) {
		e.putUserPolicy(t, "alice", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"iam:AddRoleToInstanceProfile","Resource":"*"}]}`)
		assert.Equal(t, cbDenied("iam"), cbOutcomeOf(e.request(t, e.h, e.alice, "iam", bind)))
		assert.False(t, roleBound())
	})

	t.Run("with_pass_role_binds", func(t *testing.T) {
		e.putUserPolicy(t, "alice", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["iam:AddRoleToInstanceProfile","iam:PassRole"],"Resource":"*"}]}`)
		rec := e.request(t, e.h, e.alice, "iam", bind)
		require.Equalf(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.True(t, roleBound())
	})
}

// CURRENT: AssumeRoleWithWebIdentity is served unsigned and ahead of SigV4. A
// token the issuer registry does not know is refused, and no other STS action
// is reachable without a signature.
func TestIAMSTSPipeline_AnonymousWebIdentityStaysAheadOfSigV4(t *testing.T) {
	e := newCBEnv(t, cbEnvOpts{})

	t.Run("unsigned_web_identity_reaches_the_handler", func(t *testing.T) {
		rec := e.request(t, e.h, cbCreds{}, "sts", cbForm("AssumeRoleWithWebIdentity",
			"RoleArn", "arn:aws:iam::"+e.accountID+":role/none", "RoleSessionName", "s", "WebIdentityToken", "not-a-jwt"))
		got := cbOutcomeOf(rec)
		assert.Equal(t, cbRegistry(awserrors.ErrorValidationError), got, "the handler answers, SigV4 does not")
	})

	t.Run("unsigned_get_caller_identity_is_stopped_by_sigv4", func(t *testing.T) {
		got := cbOutcomeOf(e.request(t, e.h, cbCreds{}, "sts", cbForm("GetCallerIdentity")))
		assert.Equal(t, cbRegistry(awserrors.ErrorMissingAuthenticationToken), got)
	})
}
