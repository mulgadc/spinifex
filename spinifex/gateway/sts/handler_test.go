package gateway_sts

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubSTSService implements handlers_sts.STSService for table tests. Each
// method delegates to an injectable function so individual cases can simulate
// the underlying service's behaviour without standing up NATS/JetStream.
type stubSTSService struct {
	assumeRoleFn        func(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error)
	getCallerIdentityFn func(callerAccountID, callerARN, callerUserID string, input *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error)
	getSessionTokenFn   func(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error)
	lookupSessionFn     func(akid string) (*handlers_sts.SessionCredential, error)
}

var _ handlers_sts.STSService = (*stubSTSService)(nil)

func (s *stubSTSService) AssumeRole(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
	if s.assumeRoleFn != nil {
		return s.assumeRoleFn(callerAccountID, callerARN, callerIdentity, input)
	}
	return &sts.AssumeRoleOutput{}, nil
}

// AssumeRoleForInstance is the in-process IMDS entry point and is never
// dispatched over the HTTPS gateway; the stub exists only to satisfy the
// interface.
func (s *stubSTSService) AssumeRoleForInstance(_, _, _ string, _ int64) (*sts.AssumeRoleOutput, error) {
	return &sts.AssumeRoleOutput{}, nil
}

func (s *stubSTSService) GetCallerIdentity(callerAccountID, callerARN, callerUserID string, input *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
	if s.getCallerIdentityFn != nil {
		return s.getCallerIdentityFn(callerAccountID, callerARN, callerUserID, input)
	}
	return &sts.GetCallerIdentityOutput{
		Account: aws.String(callerAccountID),
		Arn:     aws.String(callerARN),
		UserId:  aws.String(callerUserID),
	}, nil
}

func (s *stubSTSService) GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
	if s.getSessionTokenFn != nil {
		return s.getSessionTokenFn(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID, input)
	}
	return &sts.GetSessionTokenOutput{}, nil
}

func (s *stubSTSService) LookupSessionCredential(akid string) (*handlers_sts.SessionCredential, error) {
	if s.lookupSessionFn != nil {
		return s.lookupSessionFn(akid)
	}
	return nil, nil
}

func (s *stubSTSService) VerifySessionToken(_ *handlers_sts.SessionCredential, _ string) bool {
	return true
}

func (s *stubSTSService) AssumeRoleWithWebIdentity(_ *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

func (s *stubSTSService) VerifyPresignedGetCallerIdentity(_, _ string) (*handlers_sts.PresignedCallerIdentity, error) {
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

// stubIAMService satisfies the IAM interface to the extent needed by
// GetCallerIdentity (only GetUser is exercised). Every other method panics so
// a test that triggers an unexpected call fails loudly instead of silently
// returning zero values.
type stubIAMService struct {
	getUserFn func(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error)
}

var _ handlers_iam.IAMService = (*stubIAMService)(nil)

func (s *stubIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	if s.getUserFn != nil {
		return s.getUserFn(accountID, input)
	}
	return &iam.GetUserOutput{User: &iam.User{UserId: aws.String("AIDA00000000000000000")}}, nil
}

func (s *stubIAMService) CreateUser(string, *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	panic("unexpected CreateUser call")
}
func (s *stubIAMService) ListUsers(string, *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	panic("unexpected ListUsers call")
}
func (s *stubIAMService) DeleteUser(string, *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	panic("unexpected DeleteUser call")
}
func (s *stubIAMService) CreateAccessKey(string, *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	panic("unexpected CreateAccessKey call")
}
func (s *stubIAMService) ListAccessKeys(string, *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	panic("unexpected ListAccessKeys call")
}
func (s *stubIAMService) DeleteAccessKey(string, *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	panic("unexpected DeleteAccessKey call")
}
func (s *stubIAMService) UpdateAccessKey(string, *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	panic("unexpected UpdateAccessKey call")
}
func (s *stubIAMService) CreatePolicy(string, *iam.CreatePolicyInput) (*iam.CreatePolicyOutput, error) {
	panic("unexpected CreatePolicy call")
}
func (s *stubIAMService) GetPolicy(string, *iam.GetPolicyInput) (*iam.GetPolicyOutput, error) {
	panic("unexpected GetPolicy call")
}
func (s *stubIAMService) GetPolicyVersion(string, *iam.GetPolicyVersionInput) (*iam.GetPolicyVersionOutput, error) {
	panic("unexpected GetPolicyVersion call")
}
func (s *stubIAMService) ListPolicies(string, *iam.ListPoliciesInput) (*iam.ListPoliciesOutput, error) {
	panic("unexpected ListPolicies call")
}
func (s *stubIAMService) DeletePolicy(string, *iam.DeletePolicyInput) (*iam.DeletePolicyOutput, error) {
	panic("unexpected DeletePolicy call")
}
func (s *stubIAMService) AttachUserPolicy(string, *iam.AttachUserPolicyInput) (*iam.AttachUserPolicyOutput, error) {
	panic("unexpected AttachUserPolicy call")
}
func (s *stubIAMService) DetachUserPolicy(string, *iam.DetachUserPolicyInput) (*iam.DetachUserPolicyOutput, error) {
	panic("unexpected DetachUserPolicy call")
}
func (s *stubIAMService) ListAttachedUserPolicies(string, *iam.ListAttachedUserPoliciesInput) (*iam.ListAttachedUserPoliciesOutput, error) {
	panic("unexpected ListAttachedUserPolicies call")
}
func (s *stubIAMService) CreateRole(string, *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	panic("unexpected CreateRole call")
}
func (s *stubIAMService) GetRole(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	panic("unexpected GetRole call")
}
func (s *stubIAMService) ListRoles(string, *iam.ListRolesInput) (*iam.ListRolesOutput, error) {
	panic("unexpected ListRoles call")
}
func (s *stubIAMService) DeleteRole(string, *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	panic("unexpected DeleteRole call")
}
func (s *stubIAMService) UpdateRole(string, *iam.UpdateRoleInput) (*iam.UpdateRoleOutput, error) {
	panic("unexpected UpdateRole call")
}
func (s *stubIAMService) UpdateAssumeRolePolicy(string, *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	panic("unexpected UpdateAssumeRolePolicy call")
}
func (s *stubIAMService) AttachRolePolicy(string, *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	panic("unexpected AttachRolePolicy call")
}
func (s *stubIAMService) DetachRolePolicy(string, *iam.DetachRolePolicyInput) (*iam.DetachRolePolicyOutput, error) {
	panic("unexpected DetachRolePolicy call")
}
func (s *stubIAMService) ListAttachedRolePolicies(string, *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error) {
	panic("unexpected ListAttachedRolePolicies call")
}
func (s *stubIAMService) CreateInstanceProfile(string, *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	panic("unexpected CreateInstanceProfile call")
}
func (s *stubIAMService) GetInstanceProfile(string, *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	panic("unexpected GetInstanceProfile call")
}
func (s *stubIAMService) ListInstanceProfiles(string, *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error) {
	panic("unexpected ListInstanceProfiles call")
}
func (s *stubIAMService) DeleteInstanceProfile(string, *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	panic("unexpected DeleteInstanceProfile call")
}
func (s *stubIAMService) ListInstanceProfilesForRole(string, *iam.ListInstanceProfilesForRoleInput) (*iam.ListInstanceProfilesForRoleOutput, error) {
	panic("unexpected ListInstanceProfilesForRole call")
}
func (s *stubIAMService) AddRoleToInstanceProfile(string, *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	panic("unexpected AddRoleToInstanceProfile call")
}
func (s *stubIAMService) RemoveRoleFromInstanceProfile(string, *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	panic("unexpected RemoveRoleFromInstanceProfile call")
}
func (s *stubIAMService) ResolveInstanceProfile(string, string) (*handlers_iam.InstanceProfile, error) {
	panic("unexpected ResolveInstanceProfile call")
}
func (s *stubIAMService) GetUserPolicies(string, string) ([]handlers_iam.PolicyDocument, error) {
	panic("unexpected GetUserPolicies call")
}
func (s *stubIAMService) LookupAccessKey(string) (*handlers_iam.AccessKey, error) {
	panic("unexpected LookupAccessKey call")
}
func (s *stubIAMService) DecryptSecret(string) (string, error) {
	panic("unexpected DecryptSecret call")
}
func (s *stubIAMService) SeedBootstrap(*handlers_iam.BootstrapData) error {
	panic("unexpected SeedBootstrap call")
}
func (s *stubIAMService) IsEmpty() (bool, error) { panic("unexpected IsEmpty call") }
func (s *stubIAMService) CreateAccount(string) (*handlers_iam.Account, error) {
	panic("unexpected CreateAccount call")
}
func (s *stubIAMService) GetAccount(string) (*handlers_iam.Account, error) {
	panic("unexpected GetAccount call")
}
func (s *stubIAMService) ListAccounts() ([]*handlers_iam.Account, error) {
	panic("unexpected ListAccounts call")
}

func TestAssumeRole_HappyPath(t *testing.T) {
	var calledWith struct {
		callerAccountID, callerARN, callerIdentity string
		roleARN, sessionName                       string
	}
	svc := &stubSTSService{
		assumeRoleFn: func(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
			calledWith.callerAccountID = callerAccountID
			calledWith.callerARN = callerARN
			calledWith.callerIdentity = callerIdentity
			calledWith.roleARN = aws.StringValue(input.RoleArn)
			calledWith.sessionName = aws.StringValue(input.RoleSessionName)
			return &sts.AssumeRoleOutput{
				Credentials: &sts.Credentials{
					AccessKeyId:     aws.String("ASIAEXAMPLE"),
					SecretAccessKey: aws.String("secret"),
					SessionToken:    aws.String("token"),
				},
			}, nil
		},
	}
	out, err := AssumeRole(
		"000000000000",
		"arn:aws:iam::000000000000:user/alice",
		"alice",
		&sts.AssumeRoleInput{
			RoleArn:         aws.String("arn:aws:iam::000000000000:role/app"),
			RoleSessionName: aws.String("session-1"),
		},
		svc,
	)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "ASIAEXAMPLE", aws.StringValue(out.Credentials.AccessKeyId))
	assert.Equal(t, "000000000000", calledWith.callerAccountID)
	assert.Equal(t, "arn:aws:iam::000000000000:user/alice", calledWith.callerARN)
	assert.Equal(t, "alice", calledWith.callerIdentity)
	assert.Equal(t, "arn:aws:iam::000000000000:role/app", calledWith.roleARN)
	assert.Equal(t, "session-1", calledWith.sessionName)
}

func TestAssumeRole_MissingRequiredParam(t *testing.T) {
	tests := []struct {
		name  string
		input *sts.AssumeRoleInput
	}{
		{
			name:  "nil input",
			input: nil,
		},
		{
			name:  "missing RoleArn",
			input: &sts.AssumeRoleInput{RoleSessionName: aws.String("session-1")},
		},
		{
			name:  "empty RoleArn",
			input: &sts.AssumeRoleInput{RoleArn: aws.String(""), RoleSessionName: aws.String("session-1")},
		},
		{
			name:  "missing RoleSessionName",
			input: &sts.AssumeRoleInput{RoleArn: aws.String("arn:aws:iam::000000000000:role/app")},
		},
		{
			name:  "empty RoleSessionName",
			input: &sts.AssumeRoleInput{RoleArn: aws.String("arn:aws:iam::000000000000:role/app"), RoleSessionName: aws.String("")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &stubSTSService{
				assumeRoleFn: func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
					t.Fatal("AssumeRole should not be called when validation fails")
					return nil, nil
				},
			}
			_, err := AssumeRole("000000000000", "arn:aws:iam::000000000000:user/alice", "alice", tc.input, svc)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
		})
	}
}

func TestAssumeRole_PropagatesServiceError(t *testing.T) {
	svc := &stubSTSService{
		assumeRoleFn: func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
			return nil, errors.New(awserrors.ErrorAccessDenied)
		},
	}
	_, err := AssumeRole(
		"000000000000",
		"arn:aws:iam::000000000000:user/alice",
		"alice",
		&sts.AssumeRoleInput{
			RoleArn:         aws.String("arn:aws:iam::000000000000:role/app"),
			RoleSessionName: aws.String("session-1"),
		},
		svc,
	)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestGetCallerIdentity_User(t *testing.T) {
	iamSvc := &stubIAMService{
		getUserFn: func(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
			assert.Equal(t, "000000000000", accountID)
			assert.Equal(t, "alice", aws.StringValue(input.UserName))
			return &iam.GetUserOutput{User: &iam.User{UserId: aws.String("AIDAALICE0000000000")}}, nil
		},
	}
	stsSvc := &stubSTSService{}
	out, err := GetCallerIdentity(
		"000000000000",
		"arn:aws:iam::000000000000:user/alice",
		PrincipalTypeUser,
		"alice",
		"",
		&sts.GetCallerIdentityInput{},
		iamSvc, stsSvc,
	)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "000000000000", aws.StringValue(out.Account))
	assert.Equal(t, "arn:aws:iam::000000000000:user/alice", aws.StringValue(out.Arn))
	assert.Equal(t, "AIDAALICE0000000000", aws.StringValue(out.UserId))
}

func TestGetCallerIdentity_RootViaUserPath(t *testing.T) {
	// Root is currently encoded as principalType=user + identity="root";
	// UserId must collapse to the account ID without calling IAM GetUser.
	iamSvc := &stubIAMService{
		getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			t.Fatal("root path must not call IAM GetUser")
			return nil, nil
		},
	}
	out, err := GetCallerIdentity(
		"000000000000",
		"arn:aws:iam::000000000000:root",
		PrincipalTypeUser,
		"root",
		"",
		&sts.GetCallerIdentityInput{},
		iamSvc, &stubSTSService{},
	)
	require.NoError(t, err)
	assert.Equal(t, "000000000000", aws.StringValue(out.UserId))
}

func TestGetCallerIdentity_AssumedRole(t *testing.T) {
	// AssumedRoleID is propagated from the SigV4 middleware via context; the
	// handler must NOT issue a second LookupSessionCredential — the verifier
	// already loaded the record.
	stsSvc := &stubSTSService{
		lookupSessionFn: func(string) (*handlers_sts.SessionCredential, error) {
			t.Fatal("assumed-role path must not re-lookup the session credential")
			return nil, nil
		},
	}
	out, err := GetCallerIdentity(
		"000000000000",
		"arn:aws:sts::000000000000:assumed-role/app/session-1",
		PrincipalTypeAssumedRole,
		"session-1",
		"AROAEXAMPLE:session-1",
		&sts.GetCallerIdentityInput{},
		&stubIAMService{}, stsSvc,
	)
	require.NoError(t, err)
	assert.Equal(t, "AROAEXAMPLE:session-1", aws.StringValue(out.UserId))
	assert.Equal(t, "arn:aws:sts::000000000000:assumed-role/app/session-1", aws.StringValue(out.Arn))
}

func TestGetCallerIdentity_AssumedRoleSessionVanished(t *testing.T) {
	// Race: SigV4 verified the session, then the janitor swept it before
	// dispatch. The middleware would normally surface this earlier, but if the
	// AssumedRoleID arrives empty the handler must fail closed with
	// InvalidClientTokenId, not leak a 500.
	_, err := GetCallerIdentity(
		"000000000000",
		"arn:aws:sts::000000000000:assumed-role/app/session-1",
		PrincipalTypeAssumedRole,
		"session-1",
		"",
		&sts.GetCallerIdentityInput{},
		&stubIAMService{}, &stubSTSService{},
	)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidClientTokenId, err.Error())
}

func TestGetCallerIdentity_UserLookupError(t *testing.T) {
	iamSvc := &stubIAMService{
		getUserFn: func(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
			return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
		},
	}
	_, err := GetCallerIdentity(
		"000000000000",
		"arn:aws:iam::000000000000:user/alice",
		PrincipalTypeUser,
		"alice",
		"",
		&sts.GetCallerIdentityInput{},
		iamSvc, &stubSTSService{},
	)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error())
}

func TestGetCallerIdentity_UnknownPrincipalType(t *testing.T) {
	_, err := GetCallerIdentity(
		"000000000000",
		"arn:aws:iam::000000000000:user/alice",
		"saml-federated",
		"alice",
		"AKIAEXAMPLE",
		&sts.GetCallerIdentityInput{},
		&stubIAMService{}, &stubSTSService{},
	)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInternalError, err.Error())
}

func TestGetSessionToken_ForwardsCallerFields(t *testing.T) {
	var got struct {
		accountID     string
		userName      string
		principalType string
		accessKeyID   string
	}
	svc := &stubSTSService{
		getSessionTokenFn: func(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, _ *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
			got.accountID = callerAccountID
			got.userName = callerUserName
			got.principalType = callerPrincipalType
			got.accessKeyID = callerAccessKeyID
			return &sts.GetSessionTokenOutput{
				Credentials: &sts.Credentials{AccessKeyId: aws.String("ASIAEXAMPLE")},
			}, nil
		},
	}
	// The wrapper's job is to forward c.identity as the user name, plus
	// c.principalType and c.accessKey, through to the service so the handler can
	// enforce its user-only / no-session rule; assert that mapping holds.
	out, err := GetSessionToken("000000000000", "alice", PrincipalTypeUser, "AKIAEXAMPLE", &sts.GetSessionTokenInput{}, svc)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "ASIAEXAMPLE", aws.StringValue(out.Credentials.AccessKeyId))
	assert.Equal(t, "000000000000", got.accountID)
	assert.Equal(t, "alice", got.userName)
	assert.Equal(t, PrincipalTypeUser, got.principalType)
	assert.Equal(t, "AKIAEXAMPLE", got.accessKeyID)
}

func TestGetSessionToken_PropagatesServiceError(t *testing.T) {
	svc := &stubSTSService{
		getSessionTokenFn: func(string, string, string, string, *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
			return nil, errors.New(awserrors.ErrorAccessDenied)
		},
	}
	_, err := GetSessionToken("000000000000", "s1", PrincipalTypeAssumedRole, "ASIAEXAMPLE", &sts.GetSessionTokenInput{}, svc)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}
