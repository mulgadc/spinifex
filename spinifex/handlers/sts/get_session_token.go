package handlers_sts

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
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
	if aws.StringValue(input.SerialNumber) != "" || aws.StringValue(input.TokenCode) != "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	duration := getSessionTokenDefaultDuration
	if input.DurationSeconds != nil {
		duration = clampGetSessionTokenDuration(*input.DurationSeconds)
	}

	cred, plainSecret, plainToken, err := s.mintSession(ctx, userEnvelope(callerAccountID, callerUserName), duration)
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

// clampGetSessionTokenDuration coerces a duration into [900, 129600].
// A nil DurationSeconds is handled by the caller; AWS clamps explicit values rather than rejecting them.
func clampGetSessionTokenDuration(requested int64) int64 {
	if requested < minDurationSeconds {
		return minDurationSeconds
	}
	if requested > getSessionTokenMaxDuration {
		return getSessionTokenMaxDuration
	}
	return requested
}

// userEnvelope is the session envelope for a GetSessionToken user session:
// PrincipalType "user", SessionName = the IAM user name, no assumed-role fields.
func userEnvelope(accountID, userName string) sessionEnvelope {
	return sessionEnvelope{
		PrincipalType: principalTypeUser,
		AccountID:     accountID,
		SessionName:   userName,
	}
}
