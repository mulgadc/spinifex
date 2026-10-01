package handlers_sts

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const (
	// GetSessionToken allows up to 36h (vs. the 12h role-session ceiling), defaulting to 12h.
	getSessionTokenMaxDuration     int64 = 129600 // 36h
	getSessionTokenDefaultDuration int64 = 43200  // 12h
)

// GetSessionToken exchanges a long-lived IAM user credential for short-lived ASIA
// credentials that still resolve to the same user identity. Only long-lived users
// (AKIA prefix, principalType "user") may call this; assumed-role and session callers
// are rejected. Caller identity is resolved by the gateway and passed as plain strings.
func (s *STSServiceImpl) GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error) {
	ctx := context.Background()
	if input == nil {
		// An absent body is the common case from `aws sts get-session-token` with no args.
		input = &sts.GetSessionTokenInput{}
	}

	// Model constraints are checked before the caller, as AWS does for AssumeRole.
	violations := durationViolations(input.DurationSeconds, getSessionTokenMaxDuration)
	violations = append(violations, mfaViolations(input.SerialNumber, input.TokenCode)...)
	if err := validationError(violations); err != nil {
		return nil, err
	}

	if callerPrincipalType != principalTypeUser {
		slog.Warn("GetSessionToken denied: caller is not a long-lived user principal",
			"account_id", callerAccountID, "principal_type", callerPrincipalType)
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}
	if strings.HasPrefix(callerAccessKeyID, SessionAccessKeyIDPrefix) {
		// A GetSessionToken session resolves to principalType "user", so the ASIA prefix
		// is the only signal the caller holds temporary credentials. Rejecting prevents
		// a session from minting a fresh session and rolling its lifetime forward.
		slog.Warn("GetSessionToken denied: caller is using temporary (session) credentials",
			"account_id", callerAccountID, "akid", callerAccessKeyID)
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}
	if callerAccountID == "" || callerUserName == "" {
		// Mis-wired caller, not a client error — fail loud rather than mint a nameless session.
		slog.Error("GetSessionToken: user principal missing account or name",
			"account_id", callerAccountID, "user_name", callerUserName)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	// MFA is out of scope: reject rather than silently ignore, so callers don't believe MFA was enforced.
	if input.SerialNumber != nil || input.TokenCode != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"MFA is not supported in this release; omit SerialNumber and TokenCode")
	}

	duration := getSessionTokenDefaultDuration
	if input.DurationSeconds != nil {
		duration = *input.DurationSeconds
	}

	// Resolved here rather than threaded from the gateway: the gateway's value is
	// aws:userid, which reports the account ID for root. Both mint and verify read
	// the record's own UserId, so the two ends compare like with like.
	userOut, err := s.iamSvc.GetUser(callerAccountID, &iam.GetUserInput{UserName: aws.String(callerUserName)})
	if err != nil || userOut == nil || userOut.User == nil {
		slog.Error("GetSessionToken: cannot resolve caller user record",
			"account_id", callerAccountID, "user_name", callerUserName, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	userID := aws.StringValue(userOut.User.UserId)
	if userID == "" {
		// Same fail-loud treatment as a caller missing an account or name: a session
		// with no immutable ID cannot be verified against the live record later.
		slog.Error("GetSessionToken: caller user record carries no UserId",
			"account_id", callerAccountID, "user_name", callerUserName)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	cred, plainSecret, plainToken, err := s.mintSession(ctx, userEnvelope(callerAccountID, callerUserName, userID), duration)
	if err != nil {
		return nil, err
	}

	slog.Info("GetSessionToken success",
		"account_id", callerAccountID,
		"user_name", callerUserName,
		"akid", cred.AccessKeyID,
		"expires_at", cred.ExpiresAt,
	)

	return &sts.GetSessionTokenOutput{
		Credentials: &sts.Credentials{
			AccessKeyId:     aws.String(cred.AccessKeyID),
			SecretAccessKey: aws.String(plainSecret),
			SessionToken:    aws.String(plainToken),
			Expiration:      aws.Time(cred.ExpiresAt),
		},
	}, nil
}

// userEnvelope is the session envelope for a GetSessionToken user session:
// PrincipalType "user", SessionName = the IAM user name, no assumed-role fields.
func userEnvelope(accountID, userName, userID string) sessionEnvelope {
	return sessionEnvelope{
		PrincipalType: principalTypeUser,
		AccountID:     accountID,
		SessionName:   userName,
		UserID:        userID,
	}
}
