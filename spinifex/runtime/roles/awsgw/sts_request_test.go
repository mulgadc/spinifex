package awsgw_test

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// flexMockSTSService is a configurable STSService mock with per-method
// overrides. It embeds the interface so it satisfies the full contract; only
// the fn-field methods below are wired, and any other method nil-panics if a
// test reaches an unmocked path.
type flexMockSTSService struct {
	stsdomain.STSService

	assumeRoleFn        func(string, string, string, *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error)
	getCallerIdentityFn func(string, string, string, *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error)
	getSessionTokenFn   func(string, string, string, string, *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error)
	lookupSessionFn     func(string) (*stsdomain.SessionCredential, error)
	assumeWebIdentityFn func(*sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error)
}

func (m *flexMockSTSService) AssumeRole(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
	if m.assumeRoleFn != nil {
		return m.assumeRoleFn(callerAccountID, callerARN, callerIdentity, input)
	}
	return &sts.AssumeRoleOutput{}, nil
}

func (m *flexMockSTSService) GetCallerIdentity(callerAccountID, callerARN, callerUserID string, input *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error) {
	if m.getCallerIdentityFn != nil {
		return m.getCallerIdentityFn(callerAccountID, callerARN, callerUserID, input)
	}
	return &sts.GetCallerIdentityOutput{
		Account: aws.String(callerAccountID),
		Arn:     aws.String(callerARN),
		UserId:  aws.String(callerUserID),
	}, nil
}

func (m *flexMockSTSService) GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
	if m.getSessionTokenFn != nil {
		return m.getSessionTokenFn(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID, input)
	}
	return &sts.GetSessionTokenOutput{}, nil
}

func (m *flexMockSTSService) LookupSessionCredential(akid string) (*stsdomain.SessionCredential, error) {
	if m.lookupSessionFn != nil {
		return m.lookupSessionFn(akid)
	}
	return nil, nil
}

func (m *flexMockSTSService) AssumeRoleWithWebIdentity(input *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	if m.assumeWebIdentityFn != nil {
		return m.assumeWebIdentityFn(input)
	}
	return nil, errors.New(awserrors.ErrorNotImplemented)
}
