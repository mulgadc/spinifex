package gateway

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrauth "github.com/mulgadc/spinifex/spinifex/gateway/ecrauth"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ecrMockIAMService implements handlers_iam.IAMService for ECR principal
// rehydration tests. Only the methods resolveECRPrincipal calls are wired;
// any other method nil-panics on the embedded nil interface, surfacing an
// unexpected call rather than silently returning a zero value.
type ecrMockIAMService struct {
	handlers_iam.IAMService

	accessKeys   map[string]*handlers_iam.AccessKey
	users        map[string]*iam.User
	roles        map[string]*iam.Role
	accounts     map[string]*handlers_iam.Account
	userPolicies map[string][]handlers_iam.PolicyDocument
	rolePolicies map[string][]handlers_iam.PolicyDocument

	// genericErr, when set, is returned by LookupAccessKey/GetUser/GetRole/
	// GetAccount/GetUserPolicies/GetRolePolicies instead of the usual
	// not-found error, simulating an IAM backend outage rather than a
	// missing record.
	genericErr error
}

func newECRMockIAMService() *ecrMockIAMService {
	return &ecrMockIAMService{
		accessKeys:   map[string]*handlers_iam.AccessKey{},
		users:        map[string]*iam.User{},
		roles:        map[string]*iam.Role{},
		accounts:     map[string]*handlers_iam.Account{},
		userPolicies: map[string][]handlers_iam.PolicyDocument{},
		rolePolicies: map[string][]handlers_iam.PolicyDocument{},
	}
}

func (m *ecrMockIAMService) LookupAccessKey(accessKeyID string) (*handlers_iam.AccessKey, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	ak, ok := m.accessKeys[accessKeyID]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return ak, nil
}

func (m *ecrMockIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	u, ok := m.users[accountID+"|"+aws.StringValue(input.UserName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetUserOutput{User: u}, nil
}

func (m *ecrMockIAMService) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	r, ok := m.roles[accountID+"|"+aws.StringValue(input.RoleName)]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return &iam.GetRoleOutput{Role: r}, nil
}

func (m *ecrMockIAMService) GetAccount(accountID string) (*handlers_iam.Account, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	a, ok := m.accounts[accountID]
	if !ok {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}
	return a, nil
}

func (m *ecrMockIAMService) GetUserPolicies(accountID, userName string) ([]handlers_iam.PolicyDocument, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	return m.userPolicies[accountID+"|"+userName], nil
}

func (m *ecrMockIAMService) GetRolePolicies(accountID, roleName string) ([]handlers_iam.PolicyDocument, error) {
	if m.genericErr != nil {
		return nil, m.genericErr
	}
	return m.rolePolicies[accountID+"|"+roleName], nil
}

// ecrMockSTSService implements handlers_sts.STSService for ECR principal
// rehydration tests. Only LookupSessionCredential and VerifySessionPrincipal
// are wired.
type ecrMockSTSService struct {
	handlers_sts.STSService

	sessions map[string]*handlers_sts.SessionCredential

	// principalErr is the continuity verdict (or dependency fault) the check
	// returns. Detection itself is exercised against real IAM/STS services in
	// handlers/sts; what these tests pin is how this door maps the answer.
	principalErr error
}

func newECRMockSTSService() *ecrMockSTSService {
	return &ecrMockSTSService{sessions: map[string]*handlers_sts.SessionCredential{}}
}

func (m *ecrMockSTSService) LookupSessionCredential(accessKeyID string) (*handlers_sts.SessionCredential, error) {
	return m.sessions[accessKeyID], nil
}

func (m *ecrMockSTSService) VerifySessionPrincipal(cred *handlers_sts.SessionCredential) (*handlers_sts.SessionPrincipal, error) {
	if m.principalErr != nil {
		return nil, m.principalErr
	}
	if cred == nil {
		return nil, errors.New("nil session credential")
	}
	return &handlers_sts.SessionPrincipal{
		UserID:   cred.UserID,
		RoleID:   cred.RoleID,
		RoleName: cred.SessionName,
		RoleARN:  cred.UnderlyingRoleARN,
	}, nil
}

const (
	ecrPrincipalTestAccount = "000000000042"
	ecrPrincipalTestAKID    = "AKIATESTLONGLIVED0001"
	ecrPrincipalTestASID    = "ASIATESTSESSION000001"
)

func seedECRTestUser(iamSvc *ecrMockIAMService, accountID, userName, akid string) {
	iamSvc.accessKeys[akid] = &handlers_iam.AccessKey{
		AccessKeyID: akid,
		UserName:    userName,
		AccountID:   accountID,
		Status:      handlers_iam.AccessKeyStatusActive,
	}
	iamSvc.users[accountID+"|"+userName] = &iam.User{
		UserName: aws.String(userName),
		UserId:   aws.String("AIDA" + strings.ToUpper(userName)),
		Arn:      aws.String("arn:aws:iam::" + accountID + ":user/" + userName),
	}
	iamSvc.accounts[accountID] = &handlers_iam.Account{AccountID: accountID, Status: handlers_iam.AccountStatusActive}
}

func TestResolveECRPrincipal_LongLivedUser(t *testing.T) {
	iamSvc := newECRMockIAMService()
	seedECRTestUser(iamSvc, ecrPrincipalTestAccount, "dev", ecrPrincipalTestAKID)
	gw := &GatewayConfig{IAMService: iamSvc}

	claims := &gateway_ecrauth.Claims{
		AccountID:     ecrPrincipalTestAccount,
		PrincipalType: principalTypeUser,
		AccessKeyID:   ecrPrincipalTestAKID,
	}
	claims.Subject = "arn:aws:iam::" + ecrPrincipalTestAccount + ":user/dev"

	got, err := gw.resolveECRPrincipal(claims)
	require.NoError(t, err)
	assert.Equal(t, principalContext{identity: "dev", accountID: ecrPrincipalTestAccount,
		principalType: principalTypeUser, userID: "AIDADEV"}, got)
}

func TestResolveECRPrincipal_GlobalRoot(t *testing.T) {
	iamSvc := newECRMockIAMService()
	seedECRTestUser(iamSvc, utils.GlobalAccountID, "root", ecrPrincipalTestAKID)
	gw := &GatewayConfig{IAMService: iamSvc}

	claims := &gateway_ecrauth.Claims{
		AccountID:     utils.GlobalAccountID,
		PrincipalType: principalTypeUser,
		AccessKeyID:   ecrPrincipalTestAKID,
	}
	claims.Subject = "arn:aws:iam::" + utils.GlobalAccountID + ":root"

	got, err := gw.resolveECRPrincipal(claims)
	require.NoError(t, err)
	assert.Equal(t, principalTypeUser, got.principalType)
	assert.Equal(t, "root", got.identity)
}

func TestResolveECRPrincipal_LongLivedUser_Rejections(t *testing.T) {
	validClaims := func() *gateway_ecrauth.Claims {
		c := &gateway_ecrauth.Claims{
			AccountID:     ecrPrincipalTestAccount,
			PrincipalType: principalTypeUser,
			AccessKeyID:   ecrPrincipalTestAKID,
		}
		c.Subject = "arn:aws:iam::" + ecrPrincipalTestAccount + ":user/dev"
		return c
	}

	t.Run("unknown access key", func(t *testing.T) {
		iamSvc := newECRMockIAMService()
		gw := &GatewayConfig{IAMService: iamSvc}
		_, err := gw.resolveECRPrincipal(validClaims())
		require.Error(t, err)
		assert.False(t, isECRDependencyFailure(err))
	})

	t.Run("inactive access key", func(t *testing.T) {
		iamSvc := newECRMockIAMService()
		seedECRTestUser(iamSvc, ecrPrincipalTestAccount, "dev", ecrPrincipalTestAKID)
		iamSvc.accessKeys[ecrPrincipalTestAKID].Status = "Inactive"
		gw := &GatewayConfig{IAMService: iamSvc}
		_, err := gw.resolveECRPrincipal(validClaims())
		require.Error(t, err)
		assert.False(t, isECRDependencyFailure(err))
	})

	t.Run("access key account does not match claim account", func(t *testing.T) {
		iamSvc := newECRMockIAMService()
		seedECRTestUser(iamSvc, ecrPrincipalTestAccount, "dev", ecrPrincipalTestAKID)
		iamSvc.accessKeys[ecrPrincipalTestAKID].AccountID = "000000000099"
		gw := &GatewayConfig{IAMService: iamSvc}
		_, err := gw.resolveECRPrincipal(validClaims())
		require.Error(t, err)
	})

	t.Run("deleted user", func(t *testing.T) {
		iamSvc := newECRMockIAMService()
		seedECRTestUser(iamSvc, ecrPrincipalTestAccount, "dev", ecrPrincipalTestAKID)
		delete(iamSvc.users, ecrPrincipalTestAccount+"|dev")
		gw := &GatewayConfig{IAMService: iamSvc}
		_, err := gw.resolveECRPrincipal(validClaims())
		require.Error(t, err)
		assert.False(t, isECRDependencyFailure(err))
	})

	t.Run("suspended account", func(t *testing.T) {
		iamSvc := newECRMockIAMService()
		seedECRTestUser(iamSvc, ecrPrincipalTestAccount, "dev", ecrPrincipalTestAKID)
		iamSvc.accounts[ecrPrincipalTestAccount].Status = handlers_iam.AccountStatusSuspended
		gw := &GatewayConfig{IAMService: iamSvc}
		_, err := gw.resolveECRPrincipal(validClaims())
		require.Error(t, err)
		assert.False(t, isECRDependencyFailure(err))
	})

	t.Run("principalType claim mismatch", func(t *testing.T) {
		iamSvc := newECRMockIAMService()
		seedECRTestUser(iamSvc, ecrPrincipalTestAccount, "dev", ecrPrincipalTestAKID)
		gw := &GatewayConfig{IAMService: iamSvc}
		c := validClaims()
		c.PrincipalType = principalTypeAssumedRole
		_, err := gw.resolveECRPrincipal(c)
		require.Error(t, err)
	})

	t.Run("subject does not match canonical ARN", func(t *testing.T) {
		iamSvc := newECRMockIAMService()
		seedECRTestUser(iamSvc, ecrPrincipalTestAccount, "dev", ecrPrincipalTestAKID)
		gw := &GatewayConfig{IAMService: iamSvc}
		c := validClaims()
		c.Subject = "arn:aws:iam::" + ecrPrincipalTestAccount + ":user/someone-else"
		_, err := gw.resolveECRPrincipal(c)
		require.Error(t, err)
	})

	t.Run("nil IAM service is a dependency failure", func(t *testing.T) {
		gw := &GatewayConfig{}
		_, err := gw.resolveECRPrincipal(validClaims())
		require.Error(t, err)
		assert.True(t, isECRDependencyFailure(err))
	})

	t.Run("IAM backend outage is a dependency failure, not a denial", func(t *testing.T) {
		iamSvc := newECRMockIAMService()
		iamSvc.genericErr = errors.New("nats: no responders")
		gw := &GatewayConfig{IAMService: iamSvc}
		_, err := gw.resolveECRPrincipal(validClaims())
		require.Error(t, err)
		assert.True(t, isECRDependencyFailure(err))
	})
}

func seedECRSessionUser(iamSvc *ecrMockIAMService, stsSvc *ecrMockSTSService, accountID, userName, asid string, expiresAt time.Time) {
	iamSvc.users[accountID+"|"+userName] = &iam.User{
		UserName: aws.String(userName),
		UserId:   aws.String("AIDA" + strings.ToUpper(userName)),
		Arn:      aws.String("arn:aws:iam::" + accountID + ":user/" + userName),
	}
	iamSvc.accounts[accountID] = &handlers_iam.Account{AccountID: accountID, Status: handlers_iam.AccountStatusActive}
	stsSvc.sessions[asid] = &handlers_sts.SessionCredential{
		AccessKeyID:   asid,
		AccountID:     accountID,
		PrincipalType: principalTypeUser,
		SessionName:   userName,
		UserID:        "AIDA" + strings.ToUpper(userName),
		ExpiresAt:     expiresAt,
	}
}

func TestResolveECRPrincipal_SessionToken_User(t *testing.T) {
	iamSvc := newECRMockIAMService()
	stsSvc := newECRMockSTSService()
	seedECRSessionUser(iamSvc, stsSvc, ecrPrincipalTestAccount, "dev", ecrPrincipalTestASID, time.Now().Add(time.Hour))
	gw := &GatewayConfig{IAMService: iamSvc, STSService: stsSvc}

	claims := &gateway_ecrauth.Claims{
		AccountID:     ecrPrincipalTestAccount,
		PrincipalType: principalTypeUser,
		AccessKeyID:   ecrPrincipalTestASID,
	}
	claims.Subject = "arn:aws:iam::" + ecrPrincipalTestAccount + ":user/dev"

	got, err := gw.resolveECRPrincipal(claims)
	require.NoError(t, err)
	assert.Equal(t, principalContext{identity: "dev", accountID: ecrPrincipalTestAccount,
		principalType: principalTypeUser, userID: "AIDADEV"}, got)
}

// The user branch had no continuity check at all before: a GetSessionToken
// session survived its user being deleted and recreated under the same name.
func TestResolveECRPrincipal_SessionToken_User_StalePrincipalRejected(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"user deleted", handlers_sts.ErrSessionPrincipalGone},
		{"user recreated same name", handlers_sts.ErrSessionPrincipalReplaced},
		{"legacy record with no immutable UserID", handlers_sts.ErrSessionPrincipalLegacy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iamSvc := newECRMockIAMService()
			stsSvc := newECRMockSTSService()
			seedECRSessionUser(iamSvc, stsSvc, ecrPrincipalTestAccount, "dev", ecrPrincipalTestASID, time.Now().Add(time.Hour))
			stsSvc.principalErr = tc.err
			gw := &GatewayConfig{IAMService: iamSvc, STSService: stsSvc}

			claims := &gateway_ecrauth.Claims{
				AccountID:     ecrPrincipalTestAccount,
				PrincipalType: principalTypeUser,
				AccessKeyID:   ecrPrincipalTestASID,
			}
			claims.Subject = "arn:aws:iam::" + ecrPrincipalTestAccount + ":user/dev"

			_, err := gw.resolveECRPrincipal(claims)
			require.Error(t, err)
			assert.False(t, isECRDependencyFailure(err))
		})
	}
}

func TestResolveECRPrincipal_SessionToken_Expired(t *testing.T) {
	iamSvc := newECRMockIAMService()
	stsSvc := newECRMockSTSService()
	seedECRSessionUser(iamSvc, stsSvc, ecrPrincipalTestAccount, "dev", ecrPrincipalTestASID, time.Now().Add(-time.Hour))
	gw := &GatewayConfig{IAMService: iamSvc, STSService: stsSvc}

	claims := &gateway_ecrauth.Claims{
		AccountID:     ecrPrincipalTestAccount,
		PrincipalType: principalTypeUser,
		AccessKeyID:   ecrPrincipalTestASID,
	}
	claims.Subject = "arn:aws:iam::" + ecrPrincipalTestAccount + ":user/dev"

	_, err := gw.resolveECRPrincipal(claims)
	require.Error(t, err)
	assert.False(t, isECRDependencyFailure(err))
}

func TestResolveECRPrincipal_SessionToken_UnknownSession(t *testing.T) {
	gw := &GatewayConfig{IAMService: newECRMockIAMService(), STSService: newECRMockSTSService()}
	claims := &gateway_ecrauth.Claims{
		AccountID:     ecrPrincipalTestAccount,
		PrincipalType: principalTypeAssumedRole,
		AccessKeyID:   ecrPrincipalTestASID,
	}
	claims.Subject = "arn:aws:iam::" + ecrPrincipalTestAccount + ":assumed-role/deploy/session"
	_, err := gw.resolveECRPrincipal(claims)
	require.Error(t, err)
	assert.False(t, isECRDependencyFailure(err))
}

func seedECRAssumedRole(iamSvc *ecrMockIAMService, stsSvc *ecrMockSTSService, accountID, roleName, roleID, asid, sessionName string, expiresAt time.Time) string {
	roleARN := "arn:aws:iam::" + accountID + ":role/" + roleName
	iamSvc.roles[accountID+"|"+roleName] = &iam.Role{
		RoleName: aws.String(roleName),
		Arn:      aws.String(roleARN),
		RoleId:   aws.String(roleID),
	}
	iamSvc.accounts[accountID] = &handlers_iam.Account{AccountID: accountID, Status: handlers_iam.AccountStatusActive}
	// AssumeRole session ARNs use the "assumed-role" resource type; the
	// underlying role (resolved via GetRole) stays a role/ ARN, matching the
	// production shape.
	assumedRoleARN := "arn:aws:sts::" + accountID + ":assumed-role/" + roleName + "/" + sessionName
	stsSvc.sessions[asid] = &handlers_sts.SessionCredential{
		AccessKeyID:       asid,
		AccountID:         accountID,
		PrincipalType:     principalTypeAssumedRole,
		SessionName:       sessionName,
		AssumedRoleARN:    assumedRoleARN,
		UnderlyingRoleARN: roleARN,
		RoleID:            roleID,
		ExpiresAt:         expiresAt,
	}
	return assumedRoleARN
}

func TestResolveECRPrincipal_AssumedRole(t *testing.T) {
	iamSvc := newECRMockIAMService()
	stsSvc := newECRMockSTSService()
	assumedARN := seedECRAssumedRole(iamSvc, stsSvc, ecrPrincipalTestAccount, "deploy", "AROATESTROLE0001", ecrPrincipalTestASID, "session-1", time.Now().Add(time.Hour))
	gw := &GatewayConfig{IAMService: iamSvc, STSService: stsSvc}

	claims := &gateway_ecrauth.Claims{
		AccountID:     ecrPrincipalTestAccount,
		PrincipalType: principalTypeAssumedRole,
		AccessKeyID:   ecrPrincipalTestASID,
	}
	claims.Subject = assumedARN

	got, err := gw.resolveECRPrincipal(claims)
	require.NoError(t, err)
	assert.Equal(t, principalTypeAssumedRole, got.principalType)
	assert.Equal(t, "session-1", got.identity)
	assert.Equal(t, assumedARN, got.assumedRoleARN)
	assert.Equal(t, "arn:aws:iam::"+ecrPrincipalTestAccount+":role/deploy", got.underlyingRoleARN)
}

func TestResolveECRPrincipal_AssumedRole_LegacyEmptyPrincipalType(t *testing.T) {
	iamSvc := newECRMockIAMService()
	stsSvc := newECRMockSTSService()
	assumedARN := seedECRAssumedRole(iamSvc, stsSvc, ecrPrincipalTestAccount, "deploy", "AROATESTROLE0001", ecrPrincipalTestASID, "session-1", time.Now().Add(time.Hour))
	stsSvc.sessions[ecrPrincipalTestASID].PrincipalType = "" // pre-PrincipalType legacy record
	gw := &GatewayConfig{IAMService: iamSvc, STSService: stsSvc}

	claims := &gateway_ecrauth.Claims{
		AccountID:     ecrPrincipalTestAccount,
		PrincipalType: principalTypeAssumedRole,
		AccessKeyID:   ecrPrincipalTestASID,
	}
	claims.Subject = assumedARN

	got, err := gw.resolveECRPrincipal(claims)
	require.NoError(t, err)
	assert.Equal(t, principalTypeAssumedRole, got.principalType)
}

func TestResolveECRPrincipal_AssumedRole_Rejections(t *testing.T) {
	baseClaims := func(assumedARN string) *gateway_ecrauth.Claims {
		c := &gateway_ecrauth.Claims{
			AccountID:     ecrPrincipalTestAccount,
			PrincipalType: principalTypeAssumedRole,
			AccessKeyID:   ecrPrincipalTestASID,
		}
		c.Subject = assumedARN
		return c
	}

	// Every continuity verdict is client-visible (401 + re-auth challenge), never
	// a 503: the identity really is invalid, and answering 503 would tell a
	// client to retry a credential that will never work again.
	t.Run("continuity verdicts are invalid-principal, not dependency failures", func(t *testing.T) {
		verdicts := map[string]error{
			"role deleted":                           handlers_sts.ErrSessionPrincipalGone,
			"role replaced (RoleId changed)":         handlers_sts.ErrSessionPrincipalReplaced,
			"legacy record with no immutable RoleID": handlers_sts.ErrSessionPrincipalLegacy,
		}
		for name, verdict := range verdicts {
			t.Run(name, func(t *testing.T) {
				iamSvc := newECRMockIAMService()
				stsSvc := newECRMockSTSService()
				assumedARN := seedECRAssumedRole(iamSvc, stsSvc, ecrPrincipalTestAccount, "deploy", "AROATESTROLE0001", ecrPrincipalTestASID, "session-1", time.Now().Add(time.Hour))
				stsSvc.principalErr = verdict
				gw := &GatewayConfig{IAMService: iamSvc, STSService: stsSvc}
				_, err := gw.resolveECRPrincipal(baseClaims(assumedARN))
				require.Error(t, err)
				assert.False(t, isECRDependencyFailure(err))
			})
		}
	})

	t.Run("continuity check fault is a dependency failure", func(t *testing.T) {
		iamSvc := newECRMockIAMService()
		stsSvc := newECRMockSTSService()
		assumedARN := seedECRAssumedRole(iamSvc, stsSvc, ecrPrincipalTestAccount, "deploy", "AROATESTROLE0001", ecrPrincipalTestASID, "session-1", time.Now().Add(time.Hour))
		stsSvc.principalErr = errors.New("nats: no responders")
		gw := &GatewayConfig{IAMService: iamSvc, STSService: stsSvc}
		_, err := gw.resolveECRPrincipal(baseClaims(assumedARN))
		require.Error(t, err)
		assert.True(t, isECRDependencyFailure(err))
	})

	t.Run("expired session", func(t *testing.T) {
		iamSvc := newECRMockIAMService()
		stsSvc := newECRMockSTSService()
		assumedARN := seedECRAssumedRole(iamSvc, stsSvc, ecrPrincipalTestAccount, "deploy", "AROATESTROLE0001", ecrPrincipalTestASID, "session-1", time.Now().Add(-time.Minute))
		gw := &GatewayConfig{IAMService: iamSvc, STSService: stsSvc}
		_, err := gw.resolveECRPrincipal(baseClaims(assumedARN))
		require.Error(t, err)
	})

	t.Run("attacker-controlled session name cannot rename into another role's ARN shape", func(t *testing.T) {
		// resolveECRPrincipal must derive the ARN from the stored underlying
		// role, never trust an attacker-influenced session name for anything
		// beyond the ARN's trailing session-name segment.
		iamSvc := newECRMockIAMService()
		stsSvc := newECRMockSTSService()
		assumedARN := seedECRAssumedRole(iamSvc, stsSvc, ecrPrincipalTestAccount, "deploy", "AROATESTROLE0001", ecrPrincipalTestASID, "session-1", time.Now().Add(time.Hour))
		gw := &GatewayConfig{IAMService: iamSvc, STSService: stsSvc}

		claims := baseClaims(assumedARN)
		// Subject claims a completely different role than the session's stored
		// underlying role — must be rejected even though PrincipalType matches.
		claims.Subject = "arn:aws:sts::" + ecrPrincipalTestAccount + ":assumed-role/other-role/session-1"
		_, err := gw.resolveECRPrincipal(claims)
		require.Error(t, err)
	})

	t.Run("nil STS service is a dependency failure", func(t *testing.T) {
		gw := &GatewayConfig{IAMService: newECRMockIAMService()}
		_, err := gw.resolveECRPrincipal(baseClaims("arn:aws:sts::" + ecrPrincipalTestAccount + ":assumed-role/deploy/session-1"))
		require.Error(t, err)
		assert.True(t, isECRDependencyFailure(err))
	})
}

func TestResolveECRPrincipal_UnrecognizedAccessKeyPrefix(t *testing.T) {
	gw := &GatewayConfig{IAMService: newECRMockIAMService()}
	claims := &gateway_ecrauth.Claims{
		AccountID:     ecrPrincipalTestAccount,
		PrincipalType: principalTypeUser,
		AccessKeyID:   "NOTAVALIDPREFIX",
	}
	claims.Subject = "arn:aws:iam::" + ecrPrincipalTestAccount + ":user/dev"
	_, err := gw.resolveECRPrincipal(claims)
	require.Error(t, err)
	assert.False(t, isECRDependencyFailure(err))
}
