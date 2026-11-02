package sts

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

// lambdaServicePrincipal is the principal the Lambda code-store role trusts.
const lambdaServicePrincipal = "lambda.amazonaws.com"

// CodeStoreRoleName names the Region's Lambda code-store role in the system account.
func CodeStoreRoleName(region string) string {
	return "lambda-code-store-" + region
}

// CodeStoreRoleARN is the only role AssumeCodeStoreRole will issue.
func CodeStoreRoleARN(region string) string {
	return "arn:aws:iam::" + identifiers.GlobalAccountID + ":role/" + CodeStoreRoleName(region)
}

// AssumeCodeStoreRole mints a session for the Region's Lambda code-store role.
// In-process only: it is not on STSService, NATS or HTTPS, and takes no role
// ARN, so a caller can never use it to issue any other role.
func (s *STSServiceImpl) AssumeCodeStoreRole(region, sessionName string) (*sts.AssumeRoleOutput, error) {
	if region == "" || sessionName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if !roleSessionNameRegex.MatchString(sessionName) || !roleSessionNameRegex.MatchString(region) {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	roleARN := CodeStoreRoleARN(region)
	out, err := s.assumeRoleForCaller(context.Background(), "", lambdaServicePrincipal, roleARN, sessionName, "", 0)
	if err != nil {
		return nil, err
	}

	slog.Info("AssumeCodeStoreRole success",
		"role_arn", roleARN,
		"session_name", sessionName,
		"expires_at", aws.TimeValue(out.Credentials.Expiration),
	)
	return out, nil
}
