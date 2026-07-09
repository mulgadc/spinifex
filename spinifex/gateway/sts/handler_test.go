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

// stubSTSService implements handlers_sts.STSService for table tests. It embeds
// the interface so it satisfies the full contract; each fn-field method
// delegates to an injectable function so individual cases can simulate the
// underlying service's behaviour without standing up NATS/JetStream. Any other
// method nil-panics if a test reaches an unmocked path.
type stubSTSService struct {
	handlers_sts.STSService

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

// stubIAMService embeds the IAM interface and wires only GetUser, the sole
// method GetCallerIdentity exercises. Every other method nil-panics so a test
// that triggers an unexpected call fails loudly instead of silently returning
// zero values.
type stubIAMService struct {
	handlers_iam.IAMService

	getUserFn func(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error)
}

var _ handlers_iam.IAMService = (*stubIAMService)(nil)

func (s *stubIAMService) GetUser(accountID string, input *iam.GetUserInput) (*iam.GetUserOutput, error) {
	if s.getUserFn != nil {
		return s.getUserFn(accountID, input)
	}
	return &iam.GetUserOutput{User: &iam.User{UserId: aws.String("AIDA00000000000000000")}}, nil
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
