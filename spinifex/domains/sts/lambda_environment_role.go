package sts

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	awsarn "github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/service/sts"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const (
	// lambdaMaxFunctionTimeoutSeconds is the longest timeout a function may set.
	lambdaMaxFunctionTimeoutSeconds = 900
	// lambdaCredentialMarginSeconds keeps the credential strictly past the last
	// moment an invocation started inside the environment's lifetime can run.
	lambdaCredentialMarginSeconds = 60
)

// These copy the Lambda function owner's accepted ID, name and region syntax, kept
// local so STS does not depend on the owner; a conformance test pins them to it.
var (
	lambdaFunctionIDRegex   = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	lambdaFunctionNameRegex = regexp.MustCompile(`^[a-zA-Z0-9-_]{1,64}$`)
	lambdaRegionRegex       = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
)

// LambdaEnvironmentSessionRequest names the execution environment a session is for.
// Every field is trusted as given: the caller is in-process, not authenticated here.
type LambdaEnvironmentSessionRequest struct {
	AccountID string
	RoleARN   string

	// FunctionARN is the function's unqualified base ARN, as
	// arn.FormatLambdaFunction(region, account, name) builds it; no version or alias.
	FunctionARN string

	// FunctionID is the owner's internal lifecycle ID for that function, not an ARN part.
	FunctionID    string
	EnvironmentID string

	// MaxLifetimeSeconds is the longest the environment may live, before the
	// function-timeout and margin are added.
	MaxLifetimeSeconds int64
}

// AssumeRoleForLambdaEnvironment issues a customer execution-role session for
// lambda.amazonaws.com, scoped to one environment. In-process only: it is not on
// STSService, NATS or HTTPS, and it does not authenticate its caller.
func (s *STSServiceImpl) AssumeRoleForLambdaEnvironment(ctx context.Context, req LambdaEnvironmentSessionRequest) (*sts.AssumeRoleOutput, error) {
	duration, err := validateLambdaEnvironmentRequest(req)
	if err != nil {
		return nil, err
	}

	// The one resolution supplies both the trust evaluated and the RoleID minted,
	// so a replaced role can never be issued under a trust that was not checked.
	roleAccountID, role, err := ResolveRoleByARN(s.iamSvc, req.RoleARN)
	if err != nil {
		if errors.Is(err, ErrRoleUnresolved) {
			return nil, errors.New(awserrors.ErrorAccessDenied)
		}
		return nil, err
	}
	if roleAccountID != req.AccountID {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}
	err = evalTrustPolicyFor(aws.StringValue(role.AssumeRolePolicyDocument), "", req.AccountID, []string{lambdaServicePrincipal})
	if err != nil {
		if code, ok := awserrors.ResolveErrorCode(err); ok && code == awserrors.ErrorAccessDenied {
			return nil, errors.New(awserrors.ErrorAccessDenied)
		}
		return nil, err
	}

	env := assumedRoleEnvelope(role, roleAccountID, req.EnvironmentID, "")
	env.LambdaFunctionID = req.FunctionID
	env.LambdaFunctionARN = req.FunctionARN
	env.LambdaEnvironmentID = req.EnvironmentID
	cred, plainSecret, plainToken, err := s.mintSession(ctx, env, duration)
	if err != nil {
		if errors.Is(err, errAccountNotActive) {
			return nil, errors.New(awserrors.ErrorAccessDenied)
		}
		return nil, err
	}

	slog.Info("AssumeRoleForLambdaEnvironment success",
		"account_id", req.AccountID,
		"role_arn", req.RoleARN,
		"function_arn", req.FunctionARN,
		"environment_id", req.EnvironmentID,
		"expires_at", cred.ExpiresAt,
	)
	return &sts.AssumeRoleOutput{
		Credentials: &sts.Credentials{
			AccessKeyId:     aws.String(cred.AccessKeyID),
			SecretAccessKey: aws.String(plainSecret),
			SessionToken:    aws.String(plainToken),
			Expiration:      aws.Time(cred.ExpiresAt),
		},
		AssumedRoleUser: &sts.AssumedRoleUser{
			AssumedRoleId: aws.String(cred.AssumedRoleID),
			Arn:           aws.String(cred.AssumedRoleARN),
		},
		PackedPolicySize: aws.Int64(0),
	}, nil
}

// validateLambdaEnvironmentRequest checks every field before any read or write
// and returns the credential lifetime. The role's MaxSessionDuration is not
// applied: AWS does not limit sessions assumed by AWS services with it.
func validateLambdaEnvironmentRequest(req LambdaEnvironmentSessionRequest) (int64, error) {
	if req.AccountID == "" || req.RoleARN == "" || req.FunctionARN == "" || req.FunctionID == "" || req.EnvironmentID == "" {
		return 0, errors.New(awserrors.ErrorMissingParameter)
	}
	if !lambdaFunctionIDRegex.MatchString(req.FunctionID) || !roleSessionNameRegex.MatchString(req.EnvironmentID) {
		return 0, errors.New(awserrors.ErrorValidationError)
	}
	fn, err := awsarn.Parse(req.FunctionARN)
	if err != nil || fn.Partition != "aws" || fn.Service != "lambda" || fn.AccountID != req.AccountID ||
		!lambdaRegionRegex.MatchString(fn.Region) {
		return 0, errors.New(awserrors.ErrorValidationError)
	}
	name, ok := strings.CutPrefix(fn.Resource, "function:")
	if !ok || !lambdaFunctionNameRegex.MatchString(name) {
		return 0, errors.New(awserrors.ErrorValidationError)
	}

	// Checked against the ceiling before adding, so the sum cannot overflow.
	const extra = lambdaMaxFunctionTimeoutSeconds + lambdaCredentialMarginSeconds
	if req.MaxLifetimeSeconds <= 0 || req.MaxLifetimeSeconds > maxDurationSeconds-extra {
		return 0, errors.New(awserrors.ErrorValidationError)
	}
	return req.MaxLifetimeSeconds + extra, nil
}
